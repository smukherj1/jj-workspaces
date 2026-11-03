// Package metastore is source-api's metadata: everything except object content, in meta.db. An
// object's row is what makes it stored; the content lives in an objstore, which this package never
// touches. See docs/plan/m2-implementation.md#storage-on-disk.
package metastore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/metadb"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
)

// ObjectMeta is what meta.db records about a looked-up ID.
type ObjectMeta struct {
	ID obj.ID
	// Present says whether the object is stored.
	Present bool
	// Kind is the object's kind, or zero if the object isn't present.
	Kind obj.Kind
	// Size is the content's length in bytes, or zero if the object isn't present.
	Size uint64
	// SHA256 is the SHA-256 of a blob's content, and nil for other kinds or if the object isn't
	// present.
	SHA256 []byte
	// UploadedAt is when the object was first stored, or zero if the object isn't present.
	UploadedAt time.Time
}

// ObjectRecord is the row of an object whose content is already written.
type ObjectRecord struct {
	ID   obj.ID
	Kind obj.Kind
	// Size is the content's length in bytes.
	Size uint64
	// SHA256 is the SHA-256 of a blob's content, and nil for other kinds.
	SHA256 []byte
}

// schemaVersion is the version of schema, recorded in meta.db's user_version.
const schemaVersion = 1

// schema is meta.db's schema. Object IDs are 20-byte blobs and times are Unix milliseconds.
const schema = `
CREATE TABLE objects (
  id BLOB PRIMARY KEY,
  kind TEXT NOT NULL,
  size INTEGER NOT NULL,
  -- The SHA-256 of a blob's content; NULL for other kinds.
  sha256 BLOB,
  uploaded_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE repos (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);

CREATE TABLE workspaces (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos (id),
  user_email TEXT NOT NULL,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (repo_id, user_email, name)
);

CREATE TABLE op_heads (
  workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
  op_id BLOB NOT NULL,
  PRIMARY KEY (workspace_id, op_id)
) WITHOUT ROWID;
`

// maxQueryParams bounds the IDs bound in one query, well under SQLite's limit on parameters.
const maxQueryParams = 500

// Store is source-api's metadata store. Its methods are safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens the store in the database at path, creating it if needed. It fails with
// metadb.ErrSchemaVersionMismatch if the database has another schema version.
func Open(path string) (*Store, error) {
	db, err := metadb.Open(path, "source-api", schemaVersion, schema)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// InsertObjects records objects in one transaction, which makes them stored. Objects already
// stored are left as they are, keeping their upload time.
func (s *Store) InsertObjects(ctx context.Context, records []ObjectRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store objects: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO objects (id, kind, size, sha256, uploaded_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store objects: %w", err)
	}
	defer stmt.Close()
	now := s.now().UnixMilli()
	for _, r := range records {
		if _, err := stmt.ExecContext(ctx, r.ID[:], r.Kind.String(), r.Size, r.SHA256,
			now); err != nil {
			return fmt.Errorf("store object %s: %w", r.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store objects: %w", err)
	}
	return nil
}

// GetObjectsMeta returns one ObjectMeta per ID, in order.
func (s *Store) GetObjectsMeta(ctx context.Context, ids []obj.ID) ([]ObjectMeta, error) {
	found := make(map[obj.ID]ObjectMeta)
	for start := 0; start < len(ids); start += maxQueryParams {
		chunk := ids[start:min(start+maxQueryParams, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id[:]
		}
		q := "SELECT id, kind, size, sha256, uploaded_at FROM objects WHERE id IN (?" +
			strings.Repeat(", ?", len(chunk)-1) + ")"
		if err := s.queryMetas(ctx, q, args, found); err != nil {
			return nil, err
		}
	}
	metas := make([]ObjectMeta, len(ids))
	for i, id := range ids {
		m, present := found[id]
		if !present {
			m = ObjectMeta{ID: id}
		}
		metas[i] = m
	}
	return metas, nil
}

// queryMetas runs a query returning the columns of objects and adds them to found.
func (s *Store) queryMetas(ctx context.Context, q string, args []any,
	found map[obj.ID]ObjectMeta) error {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("look up objects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var word string
		var size uint64
		var sum []byte
		var uploadedAt int64
		if err := rows.Scan(&b, &word, &size, &sum, &uploadedAt); err != nil {
			return fmt.Errorf("look up objects: %w", err)
		}
		id, err := obj.IDFromBytes(b)
		if err != nil {
			return fmt.Errorf("look up objects: %w", err)
		}
		kind, err := obj.ParseKind(word)
		if err != nil {
			return fmt.Errorf("look up object %s: %w", id, err)
		}
		found[id] = ObjectMeta{
			ID: id, Present: true, Kind: kind, Size: size, SHA256: sum,
			UploadedAt: time.UnixMilli(uploadedAt),
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("look up objects: %w", err)
	}
	return nil
}
