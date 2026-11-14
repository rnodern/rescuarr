package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestBrowserUsesOwnerSessionAndLocalState(t *testing.T) {
	const base = "/rescuarr"
	cfg, err := config.Parse([]string{"--base-path", base, "--data-dir", filepath.Join(t.TempDir(), "state")}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	local, err := store.Open(t.Context(), cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	auth := access.New(local, cfg.DataDir, time.Now)
	if err := auth.PrepareSetup(t.Context()); err != nil {
		t.Fatal(err)
	}
	handler := browserHandler(cfg, auth, local)
	perform := func(path string, token string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
		if token != "" {
			request.AddCookie(&http.Cookie{Name: "rescuarr_owner", Value: token})
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		return out
	}
	if response := perform(base+"/", ""); response.Code != http.StatusSeeOther || response.Header().Get("Location") != base+"/setup" {
		t.Fatalf("fresh console returned %d", response.Code)
	}
	for _, route := range []string{"/cases", "/assets/console.css", "/fragments/status"} {
		if status := perform(base+route, "").Code; status != http.StatusUnauthorized {
			t.Errorf("anonymous %s returned %d", route, status)
		}
	}
	if err := auth.IssueRecovery(t.Context()); err != access.ErrNoOwner {
		t.Fatalf("recovery before setup: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, access.TokenFilename))
	if err != nil {
		t.Fatal(err)
	}
	const password = "synthetic console test password"
	if err := auth.SetOwner(t.Context(), strings.TrimSpace(string(data)), password); err != nil {
		t.Fatal(err)
	}
	session, err := auth.Login(t.Context(), password, "")
	if err != nil {
		t.Fatal(err)
	}
	home := perform(base+"/", session.Token)
	if home.Code != http.StatusOK {
		t.Fatalf("authenticated overview returned %d", home.Code)
	}
	for _, expected := range []string{"<h1>Overview</h1>", "Observe", `action="/rescuarr/logout"`, fmt.Sprintf(`value="%s"`, session.CSRF)} {
		if !strings.Contains(home.Body.String(), expected) {
			t.Fatal("overview did not render the current owner shell")
		}
	}
	for _, route := range []string{"/cases", "/assets/console.css", "/fragments/status"} {
		if status := perform(base+route, session.Token).Code; status != http.StatusOK {
			t.Errorf("authenticated %s returned %d", route, status)
		}
	}
}
