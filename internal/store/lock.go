package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrLocked = errors.New("data directory is already in use")

type dataLock struct{ file *os.File }

func acquire(dir string) (*dataLock, error) {
	if err := makeDataDirectory(dir); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := secureDirectory(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "rescuarr.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("data lock path must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open data lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &dataLock{file: f}, nil
}

// Closing the handle releases the OS lock. Keep the lock file itself in place,
// because another process might open the same path during shutdown.
func (l *dataLock) Close() error { return l.file.Close() }
