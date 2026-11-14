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
