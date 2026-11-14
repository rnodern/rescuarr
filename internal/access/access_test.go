package access

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/store"
)

func TestOwnerLifecycle(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	auth := New(s, dir, func() time.Time { return now })
	const password = "first synthetic passphrase"
	const recovered = "different synthetic passphrase"

	if err := auth.PrepareSetup(ctx); err != nil {
		t.Fatal(err)
	}
	original := readSetupToken(t, dir)
	now = now.Add(tokenLifetime)
	if err := auth.SetOwner(ctx, original, password); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired setup token returned %v", err)
	}
	if err := auth.PrepareSetup(ctx); err != nil {
		t.Fatal(err)
	}
	replacement := readSetupToken(t, dir)
	if original == replacement {
		t.Fatal("expired setup token was reused")
	}
	if err := auth.SetOwner(ctx, replacement, password); err != nil {
		t.Fatal(err)
	}
	if err := auth.SetOwner(ctx, replacement, password); !errors.Is(err, ErrDenied) {
		t.Fatalf("replayed setup token returned %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, TokenFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("used setup token still on disk: %v", err)
	}
	if _, err := auth.Login(ctx, "incorrect synthetic passphrase", ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong password returned %v", err)
	}
	first, err := auth.Login(ctx, password, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.Login(ctx, password, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.CSRF == second.CSRF {
		t.Fatal("login did not rotate session and CSRF tokens")
	}
	if _, err := auth.Authenticate(ctx, first.Token, false); !errors.Is(err, ErrDenied) {
		t.Fatalf("rotated-out session returned %v", err)
	}
	now = now.Add(idleLimit)
	if _, err := auth.Authenticate(ctx, second.Token, true); !errors.Is(err, ErrDenied) {
		t.Fatalf("idle-expired session returned %v", err)
	}
	third, err := auth.Login(ctx, password, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.IssueRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, third.Token, false); !errors.Is(err, ErrDenied) {
		t.Fatalf("recovery left an existing session active: %v", err)
	}
	if _, err := auth.Login(ctx, password, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("old password allowed a session during recovery: %v", err)
	}
	if err := auth.SetOwner(ctx, readSetupToken(t, dir), recovered); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Login(ctx, password, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("old password returned %v", err)
	}
	if _, err := auth.Login(ctx, recovered, ""); err != nil {
		t.Fatalf("recovered credentials could not log in: %v", err)
	}
}

func readSetupToken(t *testing.T, dir string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(dir, TokenFilename))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(value))
}
