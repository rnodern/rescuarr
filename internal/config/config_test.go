package config

import (
	"path/filepath"
	"testing"
)

func TestParsePrecedence(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	env := map[string]string{
		"RESCUARR_DATA_DIR": state,
		"RESCUARR_LISTEN":   "127.0.0.1:9000",
	}
	cfg, err := Parse([]string{"--listen", "127.0.0.1:9001", "--base-path", "/tools/rescuarr"}, func(key string) string {
		return env[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9001" || cfg.DataDir != state || cfg.BasePath != "/tools/rescuarr" {
		t.Fatalf("unexpected configuration: %+v", cfg)
	}
	if cfg.Sources["RESCUARR_LISTEN"] != "flag" || cfg.Sources["RESCUARR_DATA_DIR"] != "environment" {
		t.Fatalf("unexpected sources: %+v", cfg.Sources)
	}
}

func TestInvalidBasePath(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"//host", "/../foo", "/foo%2fbar", "/foo?bar", "/foo/"} {
		if _, err := Parse([]string{"--data-dir", ".", "--base-path", path}, func(string) string { return "" }); err == nil {
			t.Errorf("accepted base path %q", path)
		}
	}
}

func TestManualRouteAcceptsLaterSABnzbd5(t *testing.T) {
	t.Parallel()
	profile := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for version, ok := range map[string]bool{
		"5.1.3": true, "5.1.4": true, "5.2.0": true, "5.10.0": true,
		"5.1.2": false, "5.0.9": false, "4.5.3": false, "6.0.0": false,
		"": false, "5.1": false, "5.1.3.1": false, "5.1.03": false, "5.1.3Beta1": false, "v5.1.3": false,
	} {
		env := map[string]string{
			"RESCUARR_DATA_DIR":                  t.TempDir(),
			"RESCUARR_MANUAL_IMPORT_CONNECTION":  "connection-1",
			"RESCUARR_MANUAL_IMPORT_PROFILE":     profile,
			"RESCUARR_MANUAL_IMPORT_SAB_VERSION": version,
		}
		cfg, err := Parse(nil, func(key string) string { return env[key] })
		if ok && (err != nil || cfg.ManualImportConnection != "connection-1") {
			t.Errorf("SABnzbd %q rejected: %v", version, err)
		}
		if !ok && err == nil {
			t.Errorf("SABnzbd %q accepted", version)
		}
	}
}
