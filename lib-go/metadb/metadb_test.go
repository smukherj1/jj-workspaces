package metadb

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

const testSchema = `
CREATE TABLE parents (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE children (
  parent_id INTEGER NOT NULL REFERENCES parents (id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  PRIMARY KEY (parent_id, name)
);
`

func open(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := Open(path, "test-binary", version, testSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreatesSchemaOnceAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db := open(t, path, 3)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("user_version = %d, %v, want 3", version, err)
	}
	if _, err := db.Exec("INSERT INTO parents (name) VALUES ('p')"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopening keeps the data and doesn't run the schema again, which would fail on the existing
	// tables.
	db = open(t, path, 3)
	var n int
	if err := db.QueryRow("SELECT count(*) FROM parents").Scan(&n); err != nil || n != 1 {
		t.Fatalf("parents after reopen = %d, %v, want 1", n, err)
	}
}

func TestSchemaVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	open(t, path, 1).Close()

	if _, err := Open(path, "test-binary", 2, testSchema); !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("Open error = %v, want ErrSchemaVersionMismatch", err)
	}

	// The database is unchanged and still opens at its own version.
	open(t, path, 1)
}

// A database with tables but no version wasn't created by Open, so it's rejected like any other
// version rather than having the schema created on top of it.
func TestRefusesNonEmptyDatabaseWithoutVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (x)"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(path, "test-binary", 1, testSchema); !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("Open error = %v, want ErrSchemaVersionMismatch", err)
	}
}

func TestRejectsVersionZero(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "meta.db"), "test-binary", 0, testSchema); err == nil {
		t.Fatal("Open with version 0 succeeded")
	}
}
