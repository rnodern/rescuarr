package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func TestRestartAndExclusiveProcess(t *testing.T) {
	if childDir := os.Getenv("RESCUARR_TEST_SECOND_PROCESS"); childDir != "" {
		child, err := Open(t.Context(), childDir)
		if child != nil {
			_ = child.Close()
		}
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("second process acquired the lock or failed unexpectedly: %v", err)
		}
		return
	}
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.State(t.Context())
	if err != nil || initial.InstallationID == "" || initial.Mode != domain.ModeObserve {
		t.Fatalf("unexpected initial state: %+v, %v", initial, err)
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestRestartAndExclusiveProcess$")
	command.Env = append(os.Environ(), "RESCUARR_TEST_SECOND_PROCESS="+dir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("second process: %v: %s", err, output)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, err := s.State(t.Context())
	if err != nil || restarted != initial {
		t.Fatalf("restart lost state: %+v, %v", restarted, err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	if _, err := privateDatabase(path); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(path, "rw", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := fstest.MapFS{"migrations/0001_invalid.sql": &fstest.MapFile{
		Data: []byte("CREATE TABLE created (id INTEGER); INSERT INTO missing VALUES (1);"),
	}}
	if err := migrate(ctx, db, source, false); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name = 'created'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration was partially applied: %d, %v", count, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration was recorded: %d, %v", count, err)
	}
}

func TestDegradedStore(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.writer.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if err := s.Probe(t.Context()); err == nil {
		t.Fatal("write succeeded with read-only writer")
	}
	if !errors.Is(s.Ready(t.Context()), ErrDegraded) {
		t.Fatal("degraded data store remained ready")
	}
	if _, err := s.State(t.Context()); err != nil {
		t.Fatalf("read-only state became unavailable: %v", err)
	}
	if err := s.Write(t.Context(), func(*sql.Tx) error {
		t.Fatal("write executed while degraded")
		return nil
	}); !errors.Is(err, ErrDegraded) {
		t.Fatalf("unexpected write error: %v", err)
	}
	if _, err := s.writer.Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	if err := s.Probe(t.Context()); err != nil || s.Ready(t.Context()) != nil {
		t.Fatalf("writer failed to recover: %v", err)
	}
}

func TestBackupRestore(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE app_state SET mode = 'assist'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx, backup); !errors.Is(err, os.ErrExist) {
		t.Fatalf("backup should not overwrite existing file: %v", err)
	}
	if _, err := Restore(ctx, dir, backup, time.Now()); !errors.Is(err, ErrLocked) {
		t.Fatalf("restore should require an exclusive lock: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err := Restore(ctx, dir, backup, time.Now())
	if err != nil || previous == "" {
		t.Fatalf("restore failed to preserve live state: %q, %v", previous, err)
	}
	if _, err := InspectBackup(ctx, previous); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restored, err := s.State(ctx)
	if err != nil || restored.InstallationID != initial.InstallationID ||
		restored.Mode != domain.ModeObserve || !restored.ReconcileRequired {
		t.Fatalf("unexpected restored state: %+v, %v", restored, err)
	}
}

func TestAuditOutboxRollBackTogether(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event := domain.Event{ID: "event", Kind: "case_changed", OccurredAt: time.Now()}
	bad := domain.OutboxItem{ID: "outbox", EventID: "different"}
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		return AppendEvent(ctx, tx, event, bad)
	}); err == nil {
		t.Fatal("invalid outbox item succeeded")
	}
	var count int
	if err := s.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), "SELECT count(*) FROM audit_events").Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("audit/outbox write was partial: %d, %v", count, err)
	}
}

func TestSecretReferences(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	secrets := filepath.Join(dir, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	const key = "synthetic-credential"
	if err := os.WriteFile(filepath.Join(secrets, "fixture-key"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadSecret("fixture-key"); err != nil || got != key {
		t.Fatal("could not resolve protected local secret")
	}
	for _, name := range []string{"../fixture-key", "a/b", "a\\b", "", ".", "https:secret"} {
		if _, err := s.ReadSecret(name); err == nil {
			t.Fatalf("accepted unsafe secret name %q", name)
		}
	}
}

func TestMalformedBackupDoesNotReplaceLocalState(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE app_state SET mode = 'assist'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, change := range []struct {
		name string
		sql  string
	}{
		{"missing owner table", "DROP TABLE owner"},
		{"changed owner table", "ALTER TABLE owner RENAME COLUMN password_hash TO wrong_hash"},
		{"unexpected view", "CREATE VIEW unexpected AS SELECT * FROM app_state"},
	} {
		t.Run(change.name, func(t *testing.T) {
			backup := filepath.Join(t.TempDir(), "backup.sqlite")
			if _, err := s.Backup(t.Context(), backup); err != nil {
				t.Fatal(err)
			}
			db, err := openDB(backup, "rw", 1)
			if err != nil {
				t.Fatal(err)
			}
			_, editErr := db.ExecContext(t.Context(), change.sql)
			if err := errors.Join(editErr, db.Close()); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectBackup(t.Context(), backup); err == nil {
				t.Fatal("doctor accepted an altered schema with an intact migration ledger")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			previous, restoreErr := Restore(t.Context(), dir, backup, time.Now())
			s, err = Open(t.Context(), dir)
			if err != nil {
				t.Fatalf("valid destination became unusable: %v", err)
			}
			if restoreErr == nil || previous != "" {
				t.Fatalf("malformed backup reached the replacement step: %q, %v", previous, restoreErr)
			}
			after, err := s.State(t.Context())
			if err != nil || after != before {
				t.Fatalf("valid destination was changed: %+v, %v", after, err)
			}
		})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(DatabasePath(dir), "rw", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, editErr := db.ExecContext(ctx, "DROP TABLE owner")
	if err := errors.Join(editErr, db.Close()); err != nil {
		t.Fatal(err)
	}
	if broken, err := Open(ctx, dir); err == nil {
		_ = broken.Close()
		t.Fatal("startup accepted an altered schema with an intact migration ledger")
	}
}
