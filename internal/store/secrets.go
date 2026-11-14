package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ReadSecret resolves a filename within the owner's local secrets directory.
// The store keeps credential values out of the SQLite backup.
func (s *Store) ReadSecret(name string) (string, error) {
	if !validSecretName(name) {
		return "", errors.New("secret reference must be a filename")
	}
	directory := filepath.Join(s.dir, "secrets")
	if err := checkSecretDirectory(directory); err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", errors.New("secret reference must point to a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("could not open secret reference")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !checkSecretFile(path, after) {
		return "", errors.New("secret reference must point to a private regular file")
	}
	const maxBytes = 4096
	body, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || len(body) > maxBytes {
		return "", errors.New("secret could not be read within the size limit")
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(body), "\n"), "\r")
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("secret must contain a single nonempty line")
	}
	return value, nil
}

// WriteSecret creates a new private credential file without replacing an
// existing file. The value never enters SQLite or its backups.
func (s *Store) WriteSecret(name, value string) error {
	if !validSecretName(name) || value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("invalid credential")
	}
	directory := filepath.Join(s.dir, "secrets")
	if err := makeDataDirectory(directory); err != nil {
		return errors.New("could not create private secrets directory")
	}
	if err := checkSecretDirectory(directory); err != nil {
		return err
	}
	path := filepath.Join(directory, name)
	f, err := createSecretFile(path)
	if err != nil {
		return errors.New("could not create private credential file")
	}
	info, statErr := f.Stat()
	if statErr != nil || !checkSecretFile(path, info) {
		_ = f.Close()
		_ = os.Remove(path)
		return errors.New("could not secure private credential file")
	}
	_, writeErr := f.WriteString(value)
	if err := errors.Join(writeErr, f.Sync(), f.Close(), syncDirectory(directory)); err != nil {
		_ = os.Remove(path)
		return errors.New("could not save private credential file")
	}
	return nil
}

// RemoveSecret unlinks the named credential from the private directory. Callers
// must confirm ownership and that no other connection still uses the reference.
func (s *Store) RemoveSecret(name string) error {
	if !validSecretName(name) {
		return errors.New("secret reference must be a filename")
	}
	directory := filepath.Join(s.dir, "secrets")
	if err := checkSecretDirectory(directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	err := os.Remove(filepath.Join(directory, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || syncDirectory(directory) != nil {
		return errors.New("could not remove private credential file")
	}
	return nil
}

func validSecretName(name string) bool {
	if name == "" || name[0] == '.' || strings.HasSuffix(name, ".") || !filepath.IsLocal(name) {
		return false
	}
	for _, char := range name {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._", char) {
			return false
		}
	}
	return true
}
