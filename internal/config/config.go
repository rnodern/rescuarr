// Package config resolves local bootstrap settings.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const DefaultListen = "127.0.0.1:8787"

type Config struct {
	Listen   string
	DataDir  string
	BasePath string
	Sources  map[string]string
}

// Parse applies explicit flags over nonempty environment values and platform defaults.
func Parse(args []string, getenv func(string) string) (Config, error) {
	cfg := Config{Sources: make(map[string]string)}
	resolve := func(key, fallback string) string {
		if value := getenv(key); value != "" {
			cfg.Sources[key] = "environment"
			return value
		}
		cfg.Sources[key] = "default"
		return fallback
	}
	flags := flag.NewFlagSet("rescuarr", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.Listen, "listen", resolve("RESCUARR_LISTEN", DefaultListen), "listen address")
	flags.StringVar(&cfg.DataDir, "data-dir", resolve("RESCUARR_DATA_DIR", ""), "state directory")
	flags.StringVar(&cfg.BasePath, "base-path", resolve("RESCUARR_BASE_PATH", ""), "browser route prefix")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("unexpected positional argument")
	}
	keys := map[string]string{
		"listen": "RESCUARR_LISTEN", "data-dir": "RESCUARR_DATA_DIR", "base-path": "RESCUARR_BASE_PATH",
	}
	flags.Visit(func(f *flag.Flag) { cfg.Sources[keys[f.Name]] = "flag" })

	if cfg.DataDir == "" && cfg.Sources["RESCUARR_DATA_DIR"] == "default" {
		var err error
		cfg.DataDir, err = defaultDataDir(getenv)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.DataDir == "" {
		return Config{}, errors.New("data directory is empty")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host == "" {
		return Config{}, errors.New("listen address requires a host and port")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return Config{}, errors.New("listen address requires a nonzero numeric port")
	}
	if cfg.BasePath == "/" {
		cfg.BasePath = ""
	}
	if err := validateBasePath(cfg.BasePath); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateBasePath(path string) error {
	if path == "" {
		return nil
	}
	if !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return errors.New("base path must begin with a slash and have no trailing slash")
	}
	for _, part := range strings.Split(path[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("base path has an invalid segment")
		}
		for _, c := range part {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~", c) {
				return errors.New("base path has an invalid character")
			}
		}
	}
	return nil
}

func defaultDataDir(getenv func(string) string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if local := getenv("LOCALAPPDATA"); filepath.IsAbs(local) {
			return filepath.Join(local, "Rescuarr"), nil
		}
		return "", errors.New("LOCALAPPDATA is unavailable; set RESCUARR_DATA_DIR")
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "Rescuarr"), nil
	default:
		if state := getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
			return filepath.Join(state, "rescuarr"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		return filepath.Join(home, ".local", "state", "rescuarr"), nil
	}
}
