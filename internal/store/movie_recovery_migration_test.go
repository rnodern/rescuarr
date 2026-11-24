package store

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// deployedMovieRecoveryChecksum is the revision 12 checksum recorded by
// candidate build f426b9a, which shipped this migration before its source
// reached main. The file was recovered byte for byte from that executable, so
// databases that build created open with this one. Editing the file, even its
// whitespace, makes those databases incompatible again.
const deployedMovieRecoveryChecksum = "7d9a913335c2440112299b5f76e333bfd35680b14c130ad8a583a32cf4b52c10"

func TestMovieRecoveryRevisionMatchesDeployedBuild(t *testing.T) {
	body, err := fs.ReadFile(schema, "migrations/0012_movie_recovery.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != deployedMovieRecoveryChecksum {
		t.Fatalf("revision 12 checksum %s differs from the deployed %s", got, deployedMovieRecoveryChecksum)
	}
}

// A database from a build that stopped at revision 11 upgrades with a
// pre-migration copy, and a database already at revision 12 reopens as is.
func TestOpensDatabasesAtRevisionsElevenAndTwelve(t *testing.T) {
	ctx := t.Context()
	eleven := fstest.MapFS{}
	paths, err := fs.Glob(schema, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(path), "0012_") {
			continue
		}
		body, err := fs.ReadFile(schema, path)
		if err != nil {
			t.Fatal(err)
		}
		eleven[path] = &fstest.MapFile{Data: body}
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := privateDatabase(DatabasePath(dir)); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(DatabasePath(dir), "rw", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, eleven, false); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, step := range []string{"upgrade from revision 11", "reopen at revision 12"} {
		s, err := Open(ctx, dir)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		var latest int
		var checksum string
		if err := s.writer.QueryRowContext(ctx, `SELECT revision, checksum FROM schema_migrations
			ORDER BY revision DESC LIMIT 1`).Scan(&latest, &checksum); err != nil {
			t.Fatal(err)
		}
		if latest != 12 || checksum != deployedMovieRecoveryChecksum {
			t.Fatalf("%s: revision %d checksum %s", step, latest, checksum)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		copies, err := filepath.Glob(filepath.Join(dir, "pre-migration-*.sqlite3"))
		if err != nil || len(copies) != 1 || !strings.HasPrefix(filepath.Base(copies[0]), "pre-migration-0011-") {
			t.Fatalf("%s: pre-migration copies %v, %v", step, copies, err)
		}
	}
}
