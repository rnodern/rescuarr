// Package config resolves local bootstrap settings.
package config

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const DefaultListen = "127.0.0.1:8787"

type Config struct {
	Listen                 string
	DataDir                string
	BasePath               string
	PublicURL              *url.URL
	TrustedProxies         []netip.Prefix
	Input                  string
	Output                 string
	ProwlDelivery          bool
	SonarrConnection       string
	ManualPackConnection   string
	ManualPackProfile      [32]byte
	ManualImportConnection string
	ManualImportProfile    [32]byte
	ImportAudioSonarrRoot  string
	ImportAudioLocalRoot   string
	Sources                map[string]string
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
	publicURL := flags.String("public-url", resolve("RESCUARR_PUBLIC_URL", ""), "public HTTP or HTTPS URL")
	proxies := flags.String("trusted-proxies", resolve("RESCUARR_TRUSTED_PROXIES", ""), "trusted proxy CIDRs")
	prowl := flags.String("prowl-delivery", resolve("RESCUARR_PROWL_DELIVERY", "false"), "enable Prowl delivery (true or false)")
	flags.StringVar(&cfg.Input, "input", "", "backup database to restore")
	flags.StringVar(&cfg.Output, "output", "", "database backup destination")
	flags.StringVar(&cfg.SonarrConnection, "sonarr-connection", "", "local Sonarr connection ID for recovery checks")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("unexpected positional argument")
	}
	keys := map[string]string{
		"listen": "RESCUARR_LISTEN", "data-dir": "RESCUARR_DATA_DIR", "base-path": "RESCUARR_BASE_PATH",
		"public-url": "RESCUARR_PUBLIC_URL", "trusted-proxies": "RESCUARR_TRUSTED_PROXIES",
		"prowl-delivery": "RESCUARR_PROWL_DELIVERY",
	}
	flags.Visit(func(f *flag.Flag) {
		if key := keys[f.Name]; key != "" {
			cfg.Sources[key] = "flag"
		}
	})

	if *prowl != "true" && *prowl != "false" {
		return Config{}, errors.New("Prowl delivery must be true or false")
	}
	cfg.ProwlDelivery = *prowl == "true"
	var qualificationErr error
	cfg.ManualPackConnection, cfg.ManualPackProfile, qualificationErr = manualRoutePin(getenv, "RESCUARR_MANUAL_EMPTY_PACK", "empty-pack")
	if qualificationErr != nil {
		return Config{}, qualificationErr
	}
	cfg.ManualImportConnection, cfg.ManualImportProfile, qualificationErr = manualRoutePin(getenv, "RESCUARR_MANUAL_IMPORT", "import")
	if qualificationErr != nil {
		return Config{}, qualificationErr
	}

	cfg.ImportAudioSonarrRoot = getenv("RESCUARR_MANUAL_IMPORT_AUDIO_SONARR_ROOT")
	cfg.ImportAudioLocalRoot = getenv("RESCUARR_MANUAL_IMPORT_AUDIO_LOCAL_ROOT")
	if (cfg.ImportAudioSonarrRoot != "" || cfg.ImportAudioLocalRoot != "") &&
		(cfg.ManualImportConnection == "" || cfg.ImportAudioSonarrRoot == "" || !filepath.IsAbs(cfg.ImportAudioLocalRoot)) {
		return Config{}, errors.New("download audio requires a qualified Sonarr import connection and both download roots, with an absolute local path")
	}

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
	if cfg.BasePath == "/healthz" || cfg.BasePath == "/readyz" {
		return Config{}, errors.New("base path conflicts with a public health route")
	}
	if *publicURL != "" {
		u, err := url.Parse(*publicURL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
			u.ForceQuery || u.RawPath != "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, errors.New("public URL requires an HTTP or HTTPS host and the configured base path")
		}
		if cfg.BasePath != strings.TrimSuffix(u.Path, "/") {
			return Config{}, errors.New("public URL path must match the configured base path")
		}
		if u.Scheme == "http" && !LoopbackHost(u.Hostname()) {
			return Config{}, errors.New("public URL for a non-loopback host requires HTTPS")
		}
		cfg.PublicURL = u
	}
	if *proxies != "" {
		if cfg.PublicURL == nil {
			return Config{}, errors.New("trusted proxies require a configured public URL")
		}
		for _, value := range strings.Split(*proxies, ",") {
			ip, err := netip.ParsePrefix(strings.TrimSpace(value))
			if err != nil || ip.Bits() == 0 {
				return Config{}, errors.New("trusted proxies must be specific CIDRs")
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, ip.Masked())
		}
	}
	return cfg, nil
}

func manualRoutePin(getenv func(string) string, prefix, route string) (string, [32]byte, error) {
	connection, digest, sab := getenv(prefix+"_CONNECTION"), getenv(prefix+"_PROFILE"), getenv(prefix+"_SAB_VERSION")
	var fingerprint [32]byte
	if connection == "" && digest == "" && sab == "" {
		return "", fingerprint, nil
	}
	decoded, err := hex.DecodeString(digest)
	if !validManualConnectionID(connection) || err != nil || len(decoded) != len(fingerprint) || sab != "5.1.3" {
		return "", fingerprint, fmt.Errorf("manual %s recovery requires a connection ID, profile SHA-256 and verified SABnzbd 5.1.3", route)
	}
	copy(fingerprint[:], decoded)
	if fingerprint == ([32]byte{}) {
		return "", fingerprint, fmt.Errorf("manual %s profile SHA-256 is empty", route)
	}
	return connection, fingerprint, nil
}

func validManualConnectionID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, ch := range value {
		if !strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-", ch) {
			return false
		}
	}
	return true
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

func LoopbackHost(value string) bool {
	if strings.EqualFold(value, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(value)
	return err == nil && ip.Unmap().IsLoopback()
}
