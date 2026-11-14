package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

const migrationLedger = `CREATE TABLE schema_migrations (
	revision INTEGER PRIMARY KEY, checksum TEXT NOT NULL) STRICT`

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
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := checkSchema(ctx, db, known[:applied]); err != nil {
		return 0, err
	}
	return applied, nil
}

type schemaObject struct {
	kind, name, table string
	definition        sql.NullString
}

func schemaObjects(ctx context.Context, db *sql.DB) ([]schemaObject, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM main.sqlite_schema ORDER BY type, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.table, &object.definition); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

// The migration ledger identifies the applied SQL revisions. Compare the actual
// catalogue with the embedded migrations replayed in an isolated database.
func checkSchema(ctx context.Context, actual *sql.DB, applied []revision) error {
	expected, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer expected.Close()
	expected.SetMaxOpenConns(1)
	if _, err := expected.ExecContext(ctx, "PRAGMA foreign_keys=ON; PRAGMA trusted_schema=OFF; "+migrationLedger); err != nil {
		return err
	}
	if err := applyMigrations(ctx, expected, applied); err != nil {
		return fmt.Errorf("rebuild expected schema: %w", err)
	}
	want, err := schemaObjects(ctx, expected)
	if err != nil {
		return err
	}
	have, err := schemaObjects(ctx, actual)
	if err != nil {
		return err
	}
	for i, object := range want {
		if i >= len(have) || object != have[i] {
			return fmt.Errorf("SQLite schema differs at expected %s %q", object.kind, object.name)
		}
	}
	if len(have) != len(want) {
		return fmt.Errorf("SQLite schema contains unexpected %s %q", have[len(want)].kind, have[len(want)].name)
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB, source fs.FS, existed bool) error {
	known, err := revisions(source)
	if err != nil {
		return err
	}
	if !existed {
		if _, err := db.ExecContext(ctx, migrationLedger); err != nil {
			return err
		}
	}
	applied, err := checkMigrations(ctx, db, known)
	if err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if applied == len(known) {
		return nil
	}
	if applied > 0 {
		var path string
		if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
			return err
		}
		output := filepath.Join(filepath.Dir(path), fmt.Sprintf("pre-migration-%04d-%s.sqlite3", known[applied-1].number, NewID()))
		if _, err := publishSnapshot(ctx, db, output); err != nil {
			return fmt.Errorf("preserve pre-migration database: %w", err)
		}
	}
	if err := applyMigrations(ctx, db, known[applied:]); err != nil {
		return err
	}
	return checkSchema(ctx, db, known)
}

func applyMigrations(ctx context.Context, db *sql.DB, pending []revision) error {
	for _, migration := range pending {
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
