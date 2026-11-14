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
	if name == "" || name[0] == '.' || strings.HasSuffix(name, ".") || !filepath.IsLocal(name) {
		return "", errors.New("secret reference must be a filename")
	}
	for _, char := range name {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._", char) {
			return "", errors.New("secret reference must be a filename")
		}
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
