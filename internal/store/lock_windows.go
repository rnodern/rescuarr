package store

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

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
	flags := ""
	if directory {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + owner.String() + ")(A;" + flags + ";FA;;;SY)")
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
