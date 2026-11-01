// Package metadb opens a binary's SQLite metadata database, <data-dir>/meta.db, with the settings
// the service and the daemon share, and creates or checks its schema. See
// docs/plan/m2-implementation.md#storage-on-disk.
package metadb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // Registers the "sqlite" driver.
)

// ErrSchemaVersionMismatch is returned by Open when the database holds another schema version, or
// holds tables without one. The wrapping error names the versions and the data dir.
var ErrSchemaVersionMismatch = errors.New("schema version mismatch")

// busyTimeoutMillis is how long a statement waits for a lock held by another connection.
const busyTimeoutMillis = 5000

// Open opens the SQLite database at path, which lives in a binary's data dir.
//
// Every connection uses WAL, synchronous=NORMAL, foreign keys and a busy timeout, and every
// transaction begins with BEGIN IMMEDIATE. If the database is empty, Open runs schema in one
// transaction and sets the database's user_version to version. If the database has another
// version, or has tables but no version, Open fails with ErrSchemaVersionMismatch without changing
// it: binary names the program in the error.
func Open(path, binary string, version int, schema string) (*sql.DB, error) {
	if version < 1 {
		return nil, fmt.Errorf("schema version must be at least 1, got %d", version)
	}
	// The driver applies each _pragma to every new connection in the pool, since most of these are
	// per-connection settings that SQLite doesn't store in the file.
	q := url.Values{}
	// Write-ahead logging: readers don't block the writer and the writer doesn't block readers, so
	// the server can serve reads while a publish commits. Also makes commits cheaper, since a commit
	// appends to the log instead of rewriting pages in the main file.
	q.Add("_pragma", "journal_mode(WAL)")
	// In WAL mode, NORMAL syncs the log at checkpoints rather than at every commit. A committed
	// transaction survives a process crash; only an OS crash or power loss can roll back the last
	// few commits, never corrupt the database. FULL would fsync every commit for little gain here.
	q.Add("_pragma", "synchronous(NORMAL)")
	// SQLite ignores REFERENCES clauses unless this is on, so without it the schemas' foreign keys
	// and ON DELETE CASCADE would do nothing.
	q.Add("_pragma", "foreign_keys(ON)")
	// When another connection holds the lock, wait up to this long for it instead of failing at
	// once with "database is locked".
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	// Start every transaction with BEGIN IMMEDIATE, which takes the write lock up front. With the
	// default BEGIN DEFERRED, a transaction that reads and then writes must upgrade its lock, and
	// if another writer got there first SQLite fails the upgrade at once, ignoring busy_timeout.
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := initSchema(context.Background(), db, path, binary, version, schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// initSchema creates the schema in an empty database, or checks the version of an existing one.
func initSchema(ctx context.Context, db *sql.DB, path, binary string, version int,
	schema string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer tx.Rollback()

	// user_version is an integer SQLite keeps in the file header for applications to use. SQLite
	// never sets it itself, so it's 0 in a new database, and Open sets it to the schema version
	// after creating the schema below.
	var have int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
		return fmt.Errorf("read schema version of %s: %w", path, err)
	}
	if have == version {
		return nil
	}
	// A database at version 0 is new only if it has no tables, indexes or other objects, which
	// sqlite_schema lists. Opening a path that doesn't exist creates an empty file, so this is the
	// first open of a data dir. A database at version 0 with objects was created by something else,
	// or by a binary that didn't record a version, and is rejected like any other version.
	var objects int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&objects); err != nil {
		return fmt.Errorf("read schema of %s: %w", path, err)
	}
	if have != 0 || objects != 0 {
		dataDir := filepath.Dir(path)
		return fmt.Errorf("%w: %s has schema v%d, this %s needs v%d; delete %s to start over",
			ErrSchemaVersionMismatch, dataDir, have, binary, version, dataDir)
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create schema in %s: %w", path, err)
	}
	// PRAGMA takes no parameters; version is an int.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("set schema version of %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create schema in %s: %w", path, err)
	}
	return nil
}
