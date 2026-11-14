//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func makeDataDirectory(path string) error {
	return os.MkdirAll(path, 0o700)
}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("data directory must be owned by the current user with permissions 0700")
	}
	return nil
}

func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
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

func checkSecretFile(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func replaceFile(source, destination string) error { return os.Rename(source, destination) }
