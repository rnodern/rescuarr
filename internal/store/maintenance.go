package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type BackupInfo struct {
	SchemaRevision int
}

// Backup snapshots the active database and verifies the resulting database before publishing it.
func (s *Store) Backup(ctx context.Context, output string) (BackupInfo, error) {
	return publishSnapshot(ctx, s.writer, output)
}

func publishSnapshot(ctx context.Context, db *sql.DB, output string) (BackupInfo, error) {
	path, err := snapshot(ctx, db, filepath.Dir(output))
	if err != nil {
		return BackupInfo{}, err
	}
	defer os.Remove(path)
	info, err := InspectBackup(ctx, path)
	if err != nil {
		return BackupInfo{}, err
	}
	// Link publishes the private, verified file atomically without overwriting an existing backup.
	if err := os.Link(path, output); err != nil {
		return BackupInfo{}, fmt.Errorf("publish backup: %w", err)
	}
	if err := syncDirectory(filepath.Dir(output)); err != nil {
		return BackupInfo{}, fmt.Errorf("sync backup directory: %w", err)
	}
	return info, nil
}

func snapshot(ctx context.Context, db *sql.DB, dir string) (string, error) {
	tmp, err := os.CreateTemp(dir, ".rescuarr-snapshot-")
	if err != nil {
		return "", fmt.Errorf("open temporary snapshot: %w", err)
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("snapshot SQLite database: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("sync snapshot: %w", err)
	}
	return path, nil
}

func InspectBackup(ctx context.Context, input string) (BackupInfo, error) {
	db, err := openDB(input, "ro", 1)
	if err != nil {
		return BackupInfo{}, err
	}
	defer db.Close()
	return inspectDatabase(ctx, db)
}

func inspectDatabase(ctx context.Context, db *sql.DB) (BackupInfo, error) {
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return BackupInfo{}, fmt.Errorf("check backup integrity: %w", err)
	}
	if check != "ok" {
		return BackupInfo{}, errors.New("backup failed SQLite integrity check")
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return BackupInfo{}, err
	}
	broken := rows.Next()
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return BackupInfo{}, err
	}
	if broken {
		return BackupInfo{}, errors.New("backup failed SQLite foreign-key check")
	}
	known, err := revisions(schema)
	if err != nil {
		return BackupInfo{}, err
	}
	applied, err := checkMigrations(ctx, db, known)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("check backup schema: %w", err)
	}
	if applied == 0 {
		return BackupInfo{}, errors.New("backup has no recognised Rescuarr schema")
	}
	var installationID string
	if err := db.QueryRowContext(ctx, "SELECT installation_id FROM app_state WHERE singleton = 1").Scan(&installationID); err != nil {
		return BackupInfo{}, fmt.Errorf("check backup installation: %w", err)
	}
	if installationID == "" {
		return BackupInfo{}, errors.New("backup has an empty installation ID")
	}
	return BackupInfo{SchemaRevision: known[applied-1].number}, nil
}

// Restore requires the service to be stopped. It stages and checks the replacement
// before touching the live file, then forces Observe and revokes browser sessions.
func Restore(ctx context.Context, dir, input string, now time.Time) (string, error) {
	lock, err := acquire(dir)
	if err != nil {
		return "", err
	}
	defer lock.Close()

	source, err := openDB(input, "ro", 1)
	if err != nil {
		return "", err
	}
	if _, err := inspectDatabase(ctx, source); err != nil {
		_ = source.Close()
		return "", err
	}
	staging, shotErr := snapshot(ctx, source, dir)
	if err := errors.Join(shotErr, source.Close()); err != nil {
		return "", err
	}
	defer os.Remove(staging)
	if err := prepareRestore(ctx, staging, now); err != nil {
		return "", err
	}
	if _, err := InspectBackup(ctx, staging); err != nil {
		return "", fmt.Errorf("verify staged restore: %w", err)
	}

	destination := filepath.Join(dir, DatabaseName)
	var previous string
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("restore destination must be a regular file")
		}
		previous = filepath.Join(dir, "pre-restore-"+NewID()+".sqlite3")
		if err := preserveLive(ctx, destination, previous); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(destination + suffix); !errors.Is(err, os.ErrNotExist) {
				return "", errors.New("restore destination has an SQLite sidecar without its database")
			}
		}
	}
	if err := replaceFile(staging, destination); err != nil {
		return previous, fmt.Errorf("replace local database: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return previous, fmt.Errorf("sync restored database directory: %w", err)
	}
	// A setup token file belongs to the pre-restore authentication state.
	if err := os.Remove(filepath.Join(dir, "setup-token")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return previous, fmt.Errorf("remove previous setup token file: %w", err)
	}
	return previous, nil
}

func preserveLive(ctx context.Context, path, output string) error {
	db, err := openDB(path, "rw", 1)
	if err != nil {
		return err
	}
	_, saveErr := publishSnapshot(ctx, db, output)
	if saveErr == nil {
		var busy, logFrames, checkpoints int
		saveErr = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpoints)
		if saveErr == nil && busy != 0 {
			saveErr = errors.New("live SQLite database has active readers or writers")
		}
	}
	if err := errors.Join(saveErr, db.Close()); err != nil {
		return fmt.Errorf("preserve pre-restore database: %w", err)
	}
	// Sidecars must never be applied to the replacement. A remaining WAL with
	// data means we cannot prove the previous database closed cleanly.
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		info, err := os.Lstat(sidecar)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || suffix == "-wal" && info.Size() != 0 {
			return errors.New("previous SQLite database has an unsafe sidecar")
		}
		if err := os.Remove(sidecar); err != nil {
			return err
		}
	}
	return nil
}

func prepareRestore(ctx context.Context, path string, now time.Time) (err error) {
	db, err := openDB(path, "rw", 1)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := migrate(ctx, db, schema, true); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE app_state SET mode = 'observe', reconcile_required = 1 WHERE singleton = 1;
		DELETE FROM owner_sessions;
		DELETE FROM setup_token;
	`); err != nil {
		return err
	}
	if err := AppendEvent(ctx, tx, domain.Event{
		ID: domain.EventID(NewID()), Actor: "local-cli", Kind: "database_restored", OccurredAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
