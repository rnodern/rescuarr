package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func makeDataDirectory(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	attributes, err := ownerAttributes(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(name, attributes)
	if errors.Is(err, os.ErrExist) {
		// A simultaneous startup may have created it. secureDirectory checks ownership.
		return nil
	}
	return err
}

func ownerAttributes(directory bool) (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// An elevated token can default to Administrators ownership. Explicitly
	// assign the current user and a private DACL when creating private state.
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + ownerDACL(user.User.Sid, directory))
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, nil
}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("data directory must be a directory")
	}
	return restrictOwner(path, true)
}

func restrictOwner(path string, directory bool) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := current.Owner()
	if err != nil {
		return err
	}
	if !owner.Equals(user.User.Sid) {
		return errors.New("local state must be owned by the current user")
	}
	descriptor, err := windows.SecurityDescriptorFromString(ownerDACL(owner, directory))
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func ownerDACL(owner *windows.SID, directory bool) string {
	flags := ""
	if directory {
		flags = "OICI"
	}
	return "D:P(A;" + flags + ";FA;;;" + owner.String() + ")(A;" + flags + ";FA;;;SY)"
}

func lockFile(f *os.File) error {
	var overlap windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	if err != nil {
		return fmt.Errorf("lock data directory: %w", err)
	}
	return nil
}

func checkSecretDirectory(path string) error {
	return secureDirectory(path)
}

func createSecretFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes, err := ownerAttributes(false)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ,
		attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func checkSecretFile(path string, _ os.FileInfo) bool {
	// The private directory ACL protects inherited files. Reapply the same
	// controls to imported files, which may have retained the source ACL.
	return restrictOwner(path, false) == nil
}

// Replacing a file on NTFS requests a write-through move, including metadata.
func replaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows cannot flush a directory handle with os.File.Sync.
func syncDirectory(string) error { return nil }
