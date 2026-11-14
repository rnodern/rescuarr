// Package store manages the exclusive local SQLite data directory.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	_ "modernc.org/sqlite"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	DatabaseName  = "rescuarr.sqlite3"
	sqliteVersion = "3.54.1"
)

func DatabasePath(dir string) string { return filepath.Join(dir, DatabaseName) }

//go:embed migrations/*.sql
var schema embed.FS

type Store struct {
	dir      string
	lock     *dataLock
	writer   *sql.DB
	reader   *sql.DB
	degraded atomic.Bool
	gate     chan struct{}
}

type State struct {
	InstallationID    string
	Mode              domain.Mode
	ReconcileRequired bool
}

func Open(ctx context.Context, dir string) (*Store, error) {
	lock, err := acquire(dir)
	if err != nil {
		return nil, err
	}
	s, err := openLocked(ctx, dir, lock)
	if err != nil {
		_ = lock.Close()
	}
	return s, err
}

func openLocked(ctx context.Context, dir string, lock *dataLock) (*Store, error) {
	path := DatabasePath(dir)
	existed, err := privateDatabase(path)
	if err != nil {
		return nil, err
	}
	writer, err := openDB(path, "rw", 1)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = writer.Close()
		}
	}()
	if err := writer.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open local SQLite store: %w", err)
	}
	var version string
	if err := writer.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return nil, fmt.Errorf("read SQLite runtime version: %w", err)
	}
	if version != sqliteVersion {
		return nil, fmt.Errorf("SQLite runtime %q differs from pinned version %s", version, sqliteVersion)
	}
	if err := migrate(ctx, writer, schema, existed); err != nil {
		return nil, err
	}
	var mode string
	if err := writer.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return nil, fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if mode != "wal" {
		return nil, fmt.Errorf("SQLite did not enable WAL, got %q", mode)
	}
	reader, err := openDB(path, "ro", 4)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, writer: writer, reader: reader, gate: make(chan struct{}, 1)}
	if err := s.Ready(ctx); err != nil {
		_ = reader.Close()
		return nil, err
	}
	success = true
	return s, nil
}

func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close(), s.lock.Close())
}

func (s *Store) State(ctx context.Context) (State, error) {
	var state State
	err := s.reader.QueryRowContext(ctx, "SELECT installation_id, mode, reconcile_required FROM app_state WHERE singleton = 1").
		Scan(&state.InstallationID, &state.Mode, &state.ReconcileRequired)
	return state, err
}

var ErrDegraded = errors.New("local SQLite writes are degraded")

func (s *Store) Ready(ctx context.Context) error {
	if s.degraded.Load() {
		return ErrDegraded
	}
	_, err := s.State(ctx)
	return err
}

// Probe commits a small local write. Only a successful probe clears degraded state.
func (s *Store) Probe(ctx context.Context) error {
	if err := s.acquireWriter(ctx); err != nil {
		return err
	}
	defer s.releaseWriter()
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE app_state SET health_probe = 1 - health_probe WHERE singleton = 1")
		return err
	})
	s.degraded.Store(err != nil)
	return err
}

// Write serialises short local transactions. External requests belong after commit.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := s.acquireWriter(ctx); err != nil {
		return err
	}
	defer s.releaseWriter()
	if s.degraded.Load() {
		return ErrDegraded
	}
	err := s.transaction(ctx, fn)
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 8, 10, 11, 13, 14, 26: // SQLITE_READONLY, IOERR, CORRUPT, FULL, CANTOPEN, NOTADB
			s.degraded.Store(true)
		}
	}
	return err
}

func (s *Store) acquireWriter(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) releaseWriter() { <-s.gate }

func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Read starts a bounded read transaction in the read-only pool.
func (s *Store) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func NewID() string {
	// rand.Text uses at least 128 bits and panics only on operating-system entropy failure.
	return rand.Text()
}

func privateDatabase(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return false, f.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("create database: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("database must be a regular file")
	}
	return info.Size() > 0, nil
}

func openDB(path, mode string, connections int) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = filepath.ToSlash(absolute)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {mode}, "_pragma": {"foreign_keys(ON)", "busy_timeout(4000)", "synchronous(FULL)", "trusted_schema(OFF)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(connections)
	db.SetMaxIdleConns(connections)
	return db, nil
}

type revision struct {
	number int
	body   string
	hash   string
}

func revisions(source fs.FS) ([]revision, error) {
	paths, err := fs.Glob(source, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var result []revision
	for _, path := range paths {
		name := filepath.Base(path)
		prefix, _, ok := strings.Cut(name, "_")
		number, err := strconv.Atoi(prefix)
		if err != nil || !ok || len(prefix) != 4 || number <= 0 ||
			len(result) > 0 && result[len(result)-1].number >= number {
			return nil, fmt.Errorf("invalid migration order or filename %q", name)
		}
		body, err := fs.ReadFile(source, path)
		if err != nil {
			return nil, err
		}
		result = append(result, revision{number: number, body: string(body), hash: fmt.Sprintf("%x", sha256.Sum256(body))})
	}
	return result, nil
}

func checkMigrations(ctx context.Context, db *sql.DB, known []revision) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT revision, checksum FROM schema_migrations ORDER BY revision")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var applied int
	for rows.Next() {
		var number int
		var checksum string
		if err := rows.Scan(&number, &checksum); err != nil {
			return 0, err
		}
		if applied == len(known) || known[applied].number != number || known[applied].hash != checksum {
			return 0, fmt.Errorf("schema revision %d is incompatible with this binary", number)
		}
		applied++
	}
	return applied, rows.Err()
}

func migrate(ctx context.Context, db *sql.DB, source fs.FS, existed bool) error {
	known, err := revisions(source)
	if err != nil {
		return err
	}
	if !existed {
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
			revision INTEGER PRIMARY KEY, checksum TEXT NOT NULL) STRICT`); err != nil {
			return err
		}
	}
	applied, err := checkMigrations(ctx, db, known)
	if err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if applied > 0 && applied < len(known) {
		var path string
		if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
			return err
		}
		output := filepath.Join(filepath.Dir(path), fmt.Sprintf("pre-migration-%04d-%s.sqlite3", known[applied-1].number, NewID()))
		if _, err := publishSnapshot(ctx, db, output); err != nil {
			return fmt.Errorf("preserve pre-migration database: %w", err)
		}
	}
	for _, migration := range known[applied:] {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, migration.body)
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations (revision, checksum) VALUES (?, ?)", migration.number, migration.hash)
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %04d: %w", migration.number, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %04d: %w", migration.number, err)
		}
	}
	return nil
}
