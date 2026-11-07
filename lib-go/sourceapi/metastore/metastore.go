// Package metastore is source-api's metadata: everything except object content, in meta.db. An
// object's row is what makes it stored; the content lives in an objstore, which this package never
// touches. See docs/plan/m2-implementation.md#storage-on-disk.
package metastore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/metadb"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
)

// Errors a caller can act on, returned wrapped in a message naming the entity, which reads as a
// whole sentence, e.g. `workspace "w" in repo "r" not found`.
var (
	// ErrNotFound is returned for a repo or workspace that doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists is returned when creating a repo or workspace that exists.
	ErrAlreadyExists = errors.New("already exists")
	// ErrFailedPrecondition is returned when the database lacks a row an operation depends on, such
	// as UpdateOpHeads's new op head having no operation row.
	ErrFailedPrecondition = errors.New("precondition not met")
)

// WorkspaceKey identifies a workspace.
type WorkspaceKey struct {
	Repo string
	// User is the owner's email, an opaque key here.
	User string
	Name string
}

// String names the workspace for error messages.
func (k WorkspaceKey) String() string {
	return fmt.Sprintf("workspace %q in repo %q", k.Name, k.Repo)
}

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
	found map[obj.ID]ObjectMeta,
) error {
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

// CreateRepo records a repo. It fails with ErrAlreadyExists if the repo exists.
func (s *Store) CreateRepo(ctx context.Context, name string) error {
	return s.runInTxn(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO repos (name, created_at) VALUES (?, ?)
			ON CONFLICT (name) DO NOTHING`, name, s.now().UnixMilli())
		if err != nil {
			return fmt.Errorf("inserting repo %q: %w", name, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking insert of repo %q: %w", name, err)
		}
		if n == 0 {
			return fmt.Errorf("repo %q %w", name, ErrAlreadyExists)
		}
		return nil
	})
}

// CreateWorkspace registers a workspace with op heads {root op ID}. It fails with ErrNotFound for
// an unknown repo and ErrAlreadyExists if the user has the workspace.
func (s *Store) CreateWorkspace(ctx context.Context, key WorkspaceKey) error {
	return s.runInTxn(ctx, func(tx *sql.Tx) error {
		var repoID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM repos WHERE name = ?", key.Repo).Scan(&repoID)
		if errors.Is(err, sql.ErrNoRows) {
			return repoNotFound(key.Repo)
		}
		if err != nil {
			return fmt.Errorf("looking up repo %q: %w", key.Repo, err)
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO workspaces (repo_id, user_email, name, created_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (repo_id, user_email, name) DO NOTHING RETURNING id`,
			repoID, key.User, key.Name, s.now().UnixMilli()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s %w", key, ErrAlreadyExists)
		}
		if err != nil {
			return fmt.Errorf("inserting %s: %w", key, err)
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO op_heads (workspace_id, op_id) VALUES (?, ?)", id,
			obj.RootID[:])
		if err != nil {
			return fmt.Errorf("inserting root op head of %s: %w", key, err)
		}
		return nil
	})
}

// ListWorkspaces returns the names of the user's workspaces in the repo, in no particular order. It
// fails with ErrNotFound for an unknown repo.
func (s *Store) ListWorkspaces(ctx context.Context, repo, user string) ([]string, error) {
	// One statement, so the repo check and the names are one snapshot. No row means no repo, and a
	// repo where the user has no workspaces gives one row with a NULL ID and name.
	rows, err := s.db.QueryContext(ctx, `SELECT w.id, w.name FROM repos r
		LEFT JOIN workspaces w ON w.repo_id = r.id AND w.user_email = ?
		WHERE r.name = ?`, user, repo)
	if err != nil {
		return nil, fmt.Errorf("listing workspaces in repo %q: %w", repo, err)
	}
	defer rows.Close()
	found := false
	names := []string{}
	for rows.Next() {
		found = true
		var id sql.NullInt64
		var name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("reading workspace name in repo %q: %w", repo, err)
		}
		if !id.Valid {
			// The repo exists, but the user has no workspaces in it.
			continue
		}
		if !name.Valid {
			return nil, fmt.Errorf("reading workspace %d in repo %q: name is unexpectedly NULL", id.Int64,
				repo)
		}
		names = append(names, name.String)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing workspaces in repo %q: %w", repo, err)
	}
	if !found {
		return nil, repoNotFound(repo)
	}
	return names, nil
}

// DeleteWorkspace deletes a workspace and, by cascade, its op heads. It fails with ErrNotFound for
// an unknown repo or workspace.
func (s *Store) DeleteWorkspace(ctx context.Context, key WorkspaceKey) error {
	return s.runInTxn(ctx, func(tx *sql.Tx) error {
		id, err := workspaceID(ctx, tx, key)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM workspaces WHERE id = ?", id); err != nil {
			return fmt.Errorf("deleting %s: %w", key, err)
		}
		return nil
	})
}

// GetOpHeads returns a workspace's op heads, in no particular order. It fails with ErrNotFound for an unknown repo
// or workspace.
func (s *Store) GetOpHeads(ctx context.Context, key WorkspaceKey) ([]obj.ID, error) {
	// One statement, so the checks and the heads are one snapshot, and a concurrent delete can't
	// give an empty set. Each NULL marks what's missing, as in workspaceID.
	rows, err := s.db.QueryContext(ctx, `SELECT w.id, h.op_id FROM repos r
		LEFT JOIN workspaces w ON w.repo_id = r.id AND w.user_email = ? AND w.name = ?
		LEFT JOIN op_heads h ON h.workspace_id = w.id
		WHERE r.name = ?`, key.User, key.Name, key.Repo)
	if err != nil {
		return nil, fmt.Errorf("reading op heads of %s: %w", key, err)
	}
	defer rows.Close()
	foundRepo := false
	var heads []obj.ID
	for rows.Next() {
		foundRepo = true
		var workspace sql.NullInt64
		var b []byte
		if err := rows.Scan(&workspace, &b); err != nil {
			return nil, fmt.Errorf("reading op head of %s: %w", key, err)
		}
		if !workspace.Valid {
			// The repo exists, but the user has no workspace with that name in it.
			return nil, workspaceNotFound(key)
		}
		id, err := obj.IDFromBytes(b)
		if err != nil {
			return nil, fmt.Errorf("parsing op head of %s: %w", key, err)
		}
		heads = append(heads, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading op heads of %s: %w", key, err)
	}
	if !foundRepo {
		return nil, repoNotFound(key.Repo)
	}
	return heads, nil
}

// UpdateOpHeads removes oldIDs from a workspace's op heads, ignoring ones already gone, and adds
// newID, in one transaction. It fails with ErrNotFound for an unknown repo or workspace, and with
// ErrFailedPrecondition if newID has no operation row.
func (s *Store) UpdateOpHeads(ctx context.Context, key WorkspaceKey, oldIDs []obj.ID,
	newID obj.ID,
) error {
	return s.runInTxn(ctx, func(tx *sql.Tx) error {
		id, err := workspaceID(ctx, tx, key)
		if err != nil {
			return err
		}
		var stored int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM objects WHERE id = ? AND kind = ?", newID[:],
			obj.Operation.String()).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("can't update op head to %s that wasn't found as an operation object: %w",
				newID, ErrFailedPrecondition)
		}
		if err != nil {
			return fmt.Errorf("looking up operation %s: %w", newID, err)
		}
		stmt, err := tx.PrepareContext(ctx, "DELETE FROM op_heads WHERE workspace_id = ? AND op_id = ?")
		if err != nil {
			return fmt.Errorf("preparing op head removal: %w", err)
		}
		defer stmt.Close()
		for _, old := range oldIDs {
			if _, err := stmt.ExecContext(ctx, id, old[:]); err != nil {
				return fmt.Errorf("removing op head %s of %s: %w", old, key, err)
			}
		}
		// A retry after a lost reply adds a head that's already there.
		_, err = tx.ExecContext(ctx, `INSERT INTO op_heads (workspace_id, op_id) VALUES (?, ?)
			ON CONFLICT (workspace_id, op_id) DO NOTHING`, id, newID[:])
		if err != nil {
			return fmt.Errorf("adding op head %s of %s: %w", newID, key, err)
		}
		return nil
	})
}

// runInTxn runs fn in one transaction, committing it if fn returns nil. It wraps fn's error, which
// callers can still match with errors.Is.
func (s *Store) runInTxn(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		// After a commit, Rollback returns sql.ErrTxDone and does nothing.
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "rolling back transaction", "err", err)
		}
	}()
	if err := fn(tx); err != nil {
		return fmt.Errorf("running transaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// workspaceID returns the row ID of a workspace, or ErrNotFound for an unknown repo or workspace.
func workspaceID(ctx context.Context, tx *sql.Tx, key WorkspaceKey) (int64, error) {
	// No row means no repo, and a repo without the workspace gives one row with a NULL ID.
	var id sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT w.id FROM repos r
		LEFT JOIN workspaces w ON w.repo_id = r.id AND w.user_email = ? AND w.name = ?
		WHERE r.name = ?`, key.User, key.Name, key.Repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, repoNotFound(key.Repo)
	}
	if err != nil {
		return 0, fmt.Errorf("looking up %s: %w", key, err)
	}
	if !id.Valid {
		// The repo exists, but the user has no workspace with that name in it.
		return 0, workspaceNotFound(key)
	}
	return id.Int64, nil
}

// repoNotFound is the ErrNotFound of an unknown repo.
func repoNotFound(repo string) error {
	return fmt.Errorf("repo %q %w", repo, ErrNotFound)
}

// workspaceNotFound is the ErrNotFound of a workspace the user doesn't have in a repo that exists.
func workspaceNotFound(key WorkspaceKey) error {
	return fmt.Errorf("%s %w", key, ErrNotFound)
}
