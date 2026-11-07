package metastore

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/obj"
)

var (
	blobID = obj.ID{1}
	treeID = obj.ID{2}
	opID   = obj.ID{4}
	op2ID  = obj.ID{5}
	op3ID  = obj.ID{6}
)

// createRepo creates a repo, failing the test if it can't.
func createRepo(t *testing.T, s *Store, name string) {
	t.Helper()
	if err := s.CreateRepo(t.Context(), name); err != nil {
		t.Fatalf("CreateRepo(%q): %v", name, err)
	}
}

// createWorkspace creates a workspace, failing the test if it can't.
func createWorkspace(t *testing.T, s *Store, key WorkspaceKey) {
	t.Helper()
	if err := s.CreateWorkspace(t.Context(), key); err != nil {
		t.Fatalf("CreateWorkspace(%v): %v", key, err)
	}
}

// insertOps records operation objects with the given IDs, failing the test if it can't.
func insertOps(t *testing.T, s *Store, ids ...obj.ID) {
	t.Helper()
	records := make([]ObjectRecord, len(ids))
	for i, id := range ids {
		records[i] = ObjectRecord{ID: id, Kind: obj.Operation}
	}
	if err := s.InsertObjects(t.Context(), records); err != nil {
		t.Fatalf("InsertObjects: %v", err)
	}
}

// checkOpHeads checks a workspace's op heads, in any order, against want.
func checkOpHeads(t *testing.T, s *Store, key WorkspaceKey, want ...obj.ID) {
	t.Helper()
	got, err := s.GetOpHeads(t.Context(), key)
	if err != nil {
		t.Fatalf("GetOpHeads(%v): %v", key, err)
	}
	cmp := func(a, b obj.ID) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(got, cmp)
	want = slices.Clone(want)
	slices.SortFunc(want, cmp)
	if !slices.Equal(got, want) {
		t.Errorf("GetOpHeads(%v) = %v, want %v", key, got, want)
	}
}

// checkErr checks that err matches want with errors.Is, or is nil if want is nil.
func checkErr(t *testing.T, call string, err, want error) {
	t.Helper()
	if want == nil && err != nil {
		t.Errorf("%s: %v", call, err)
	}
	if want != nil && !errors.Is(err, want) {
		t.Errorf("%s = %v, want %v", call, err, want)
	}
}

// openStore opens an empty store whose clock reads the returned time, which a test may move.
func openStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.UnixMilli(1_000_000)
	s.now = func() time.Time { return now }
	return s, &now
}

// checkMeta checks one looked-up entry against what's expected.
func checkMeta(t *testing.T, got, want ObjectMeta) {
	t.Helper()
	if got.ID != want.ID || got.Present != want.Present || got.Kind != want.Kind ||
		got.Size != want.Size || !bytes.Equal(got.SHA256, want.SHA256) ||
		(got.SHA256 == nil) != (want.SHA256 == nil) || !got.UploadedAt.Equal(want.UploadedAt) {
		t.Errorf("GetObjectsMeta entry = %+v, want %+v", got, want)
	}
}

func TestInsertAndGetObjectsMeta(t *testing.T) {
	s, now := openStore(t)
	sum := bytes.Repeat([]byte{7}, 32)
	err := s.InsertObjects(t.Context(), []ObjectRecord{
		{ID: blobID, Kind: obj.Blob, Size: 13, SHA256: sum},
		{ID: treeID, Kind: obj.Tree},
	})
	if err != nil {
		t.Fatalf("InsertObjects: %v", err)
	}

	blob := ObjectMeta{ID: blobID, Present: true, Kind: obj.Blob, Size: 13, SHA256: sum, UploadedAt: *now}
	tree := ObjectMeta{ID: treeID, Present: true, Kind: obj.Tree, UploadedAt: *now}
	absent := ObjectMeta{ID: obj.ID{3}}
	for _, tc := range []struct {
		name string
		want []ObjectMeta
	}{
		{"blob with its SHA-256", []ObjectMeta{blob}},
		{"tree without a SHA-256", []ObjectMeta{tree}},
		{"absent", []ObjectMeta{absent}},
		{"request order, repeats included", []ObjectMeta{absent, tree, blob, tree}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idList := make([]obj.ID, len(tc.want))
			for i, m := range tc.want {
				idList[i] = m.ID
			}
			got, err := s.GetObjectsMeta(t.Context(), idList)
			if err != nil {
				t.Fatalf("GetObjectsMeta: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("GetObjectsMeta returned %d entries, want %d", len(got), len(tc.want))
			}
			for i := range got {
				checkMeta(t, got[i], tc.want[i])
			}
		})
	}
}

func TestInsertObjects_keepsExistingRow(t *testing.T) {
	s, now := openStore(t)
	first := *now
	records := []ObjectRecord{{ID: treeID, Kind: obj.Tree}}
	if err := s.InsertObjects(t.Context(), records); err != nil {
		t.Fatalf("InsertObjects: %v", err)
	}
	*now = now.Add(time.Hour)
	if err := s.InsertObjects(t.Context(), records); err != nil {
		t.Fatalf("second InsertObjects: %v", err)
	}

	got, err := s.GetObjectsMeta(t.Context(), []obj.ID{treeID})
	if err != nil {
		t.Fatalf("GetObjectsMeta: %v", err)
	}
	checkMeta(t, got[0], ObjectMeta{ID: treeID, Present: true, Kind: obj.Tree, UploadedAt: first})
}

// TestCreateRepo checks that a new repo is created and that creating an existing one fails with
// ErrAlreadyExists.
func TestCreateRepo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []string
		create   string
		wantErr  error
	}{
		{name: "new repo", create: "r"},
		{name: "beside another repo", existing: []string{"other"}, create: "r"},
		{name: "existing repo", existing: []string{"r"}, create: "r", wantErr: ErrAlreadyExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Set up the existing repos, then create the repo under test.
			s, _ := openStore(t)
			for _, r := range tc.existing {
				createRepo(t, s, r)
			}
			err := s.CreateRepo(t.Context(), tc.create)
			checkErr(t, "CreateRepo", err, tc.wantErr)
			// Whether created now or before, the repo exists, so listing in it succeeds.
			if _, err := s.ListWorkspaces(t.Context(), tc.create, "u"); err != nil {
				t.Errorf("ListWorkspaces after CreateRepo: %v", err)
			}
		})
	}
}

// TestCreateWorkspace checks that a new workspace starts with the root op as its only op head, that
// workspace names are scoped per repo and user, and that an unknown repo or an existing workspace
// fails.
func TestCreateWorkspace(t *testing.T) {
	key := WorkspaceKey{Repo: "r", User: "u@x", Name: "w"}
	for _, tc := range []struct {
		name     string
		repos    []string
		existing []WorkspaceKey
		create   WorkspaceKey
		wantErr  error
	}{
		{name: "new workspace", repos: []string{"r"}, create: key},
		{
			name: "same name for another user", repos: []string{"r"},
			existing: []WorkspaceKey{{Repo: "r", User: "v@x", Name: "w"}}, create: key,
		},
		{
			name: "same name in another repo", repos: []string{"r", "r2"},
			existing: []WorkspaceKey{{Repo: "r2", User: "u@x", Name: "w"}}, create: key,
		},
		{name: "unknown repo", repos: []string{"other"}, create: key, wantErr: ErrNotFound},
		{
			name: "existing workspace", repos: []string{"r"}, existing: []WorkspaceKey{key},
			create: key, wantErr: ErrAlreadyExists,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Set up the repos and existing workspaces, then create the workspace under test.
			s, _ := openStore(t)
			for _, r := range tc.repos {
				createRepo(t, s, r)
			}
			for _, k := range tc.existing {
				createWorkspace(t, s, k)
			}
			err := s.CreateWorkspace(t.Context(), tc.create)
			checkErr(t, "CreateWorkspace", err, tc.wantErr)
			if tc.wantErr == nil {
				// A created workspace's only op head is the root op.
				checkOpHeads(t, s, tc.create, obj.RootID)
			}
		})
	}
}

// TestListWorkspaces checks that listing returns only the user's workspaces in the repo, in any
// order, an empty non-nil list if there are none, and ErrNotFound for an unknown repo.
func TestListWorkspaces(t *testing.T) {
	// Set up two repos with workspaces for two users.
	s, _ := openStore(t)
	createRepo(t, s, "r")
	createRepo(t, s, "r2")
	createRepo(t, s, "empty")
	for _, k := range []WorkspaceKey{
		{Repo: "r", User: "u@x", Name: "b"},
		{Repo: "r", User: "u@x", Name: "a"},
		{Repo: "r", User: "u@x", Name: "c"},
		{Repo: "r", User: "v@x", Name: "d"},
		{Repo: "r2", User: "u@x", Name: "e"},
	} {
		createWorkspace(t, s, k)
	}

	for _, tc := range []struct {
		name    string
		repo    string
		user    string
		want    []string
		wantErr error
	}{
		{name: "user's workspaces", repo: "r", user: "u@x", want: []string{"a", "b", "c"}},
		{name: "other user", repo: "r", user: "v@x", want: []string{"d"}},
		{name: "other repo", repo: "r2", user: "u@x", want: []string{"e"}},
		{name: "user without workspaces", repo: "r", user: "w@x", want: []string{}},
		{name: "repo without workspaces", repo: "empty", user: "u@x", want: []string{}},
		{name: "unknown repo", repo: "nope", user: "u@x", wantErr: ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// List and compare names in any order; an empty list must be non-nil.
			got, err := s.ListWorkspaces(t.Context(), tc.repo, tc.user)
			checkErr(t, "ListWorkspaces", err, tc.wantErr)
			if tc.wantErr != nil {
				return
			}
			slices.Sort(got)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("ListWorkspaces = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestDeleteWorkspace checks that deleting removes the workspace and its op heads while leaving
// others alone, and that an unknown repo or workspace fails with ErrNotFound.
func TestDeleteWorkspace(t *testing.T) {
	key := WorkspaceKey{Repo: "r", User: "u@x", Name: "w"}
	other := WorkspaceKey{Repo: "r", User: "u@x", Name: "other"}
	for _, tc := range []struct {
		name    string
		del     WorkspaceKey
		wantErr error
	}{
		{name: "existing workspace", del: key},
		{name: "unknown workspace", del: WorkspaceKey{Repo: "r", User: "u@x", Name: "nope"},
			wantErr: ErrNotFound},
		{name: "other user's workspace", del: WorkspaceKey{Repo: "r", User: "v@x", Name: "w"},
			wantErr: ErrNotFound},
		{name: "unknown repo", del: WorkspaceKey{Repo: "nope", User: "u@x", Name: "w"},
			wantErr: ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Set up two workspaces of one user, then delete the one under test.
			s, _ := openStore(t)
			createRepo(t, s, "r")
			createWorkspace(t, s, key)
			createWorkspace(t, s, other)
			err := s.DeleteWorkspace(t.Context(), tc.del)
			checkErr(t, "DeleteWorkspace", err, tc.wantErr)

			// The other workspace is untouched either way.
			checkOpHeads(t, s, other, obj.RootID)
			if tc.wantErr != nil {
				checkOpHeads(t, s, key, obj.RootID)
				return
			}
			// The deleted workspace is gone from lookups and listings.
			if _, err := s.GetOpHeads(t.Context(), key); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetOpHeads after delete = %v, want %v", err, ErrNotFound)
			}
			names, err := s.ListWorkspaces(t.Context(), "r", "u@x")
			if err != nil {
				t.Fatalf("ListWorkspaces: %v", err)
			}
			if !slices.Equal(names, []string{"other"}) {
				t.Errorf("ListWorkspaces after delete = %v, want [other]", names)
			}
			// The cascade removed the deleted workspace's op heads, leaving only the other's.
			var n int
			if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM op_heads").
				Scan(&n); err != nil {
				t.Fatalf("counting op heads: %v", err)
			}
			if n != 1 {
				t.Errorf("op_heads has %d rows after delete, want 1", n)
			}
			// The name can be reused, starting again from the root op.
			createWorkspace(t, s, key)
			checkOpHeads(t, s, key, obj.RootID)
		})
	}
}

// TestGetOpHeads checks that a workspace's op heads are returned and that an unknown repo or
// workspace fails with ErrNotFound.
func TestGetOpHeads(t *testing.T) {
	// Set up a workspace whose heads are moved off the root to two concurrent ops, and a second
	// workspace left at the root.
	s, _ := openStore(t)
	createRepo(t, s, "r")
	key := WorkspaceKey{Repo: "r", User: "u@x", Name: "w"}
	fresh := WorkspaceKey{Repo: "r", User: "u@x", Name: "fresh"}
	createWorkspace(t, s, key)
	createWorkspace(t, s, fresh)
	insertOps(t, s, opID, op2ID)
	if err := s.UpdateOpHeads(t.Context(), key, []obj.ID{obj.RootID}, opID); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}
	if err := s.UpdateOpHeads(t.Context(), key, []obj.ID{obj.RootID}, op2ID); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}

	for _, tc := range []struct {
		name    string
		key     WorkspaceKey
		want    []obj.ID
		wantErr error
	}{
		{name: "multiple heads", key: key, want: []obj.ID{opID, op2ID}},
		{name: "root head", key: fresh, want: []obj.ID{obj.RootID}},
		{name: "unknown workspace", key: WorkspaceKey{Repo: "r", User: "u@x", Name: "nope"},
			wantErr: ErrNotFound},
		{name: "other user's workspace", key: WorkspaceKey{Repo: "r", User: "v@x", Name: "w"},
			wantErr: ErrNotFound},
		{name: "unknown repo", key: WorkspaceKey{Repo: "nope", User: "u@x", Name: "w"},
			wantErr: ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Look up the heads and compare them in any order.
			if tc.wantErr != nil {
				_, err := s.GetOpHeads(t.Context(), tc.key)
				checkErr(t, "GetOpHeads", err, tc.wantErr)
				return
			}
			checkOpHeads(t, s, tc.key, tc.want...)
		})
	}
}

// TestUpdateOpHeads checks that the old heads are swapped for the new one, that old heads already
// gone and a repeated update are tolerated, and that a new head without an operation row or an
// unknown workspace fails, leaving the heads unchanged.
func TestUpdateOpHeads(t *testing.T) {
	key := WorkspaceKey{Repo: "r", User: "u@x", Name: "w"}
	for _, tc := range []struct {
		name string
		// before are the heads to set up, besides the root, each by an update from the root.
		before  []obj.ID
		key     WorkspaceKey
		oldIDs  []obj.ID
		newID   obj.ID
		want    []obj.ID
		wantErr error
	}{
		{name: "replace root", key: key, oldIDs: []obj.ID{obj.RootID}, newID: opID,
			want: []obj.ID{opID}},
		{name: "merge two heads", before: []obj.ID{opID, op2ID}, key: key,
			oldIDs: []obj.ID{opID, op2ID}, newID: op3ID, want: []obj.ID{op3ID}},
		{name: "old head already gone", key: key, oldIDs: []obj.ID{op2ID}, newID: opID,
			want: []obj.ID{obj.RootID, opID}},
		{name: "no old heads", key: key, newID: opID, want: []obj.ID{obj.RootID, opID}},
		{name: "retry of applied update", before: []obj.ID{opID}, key: key,
			oldIDs: []obj.ID{obj.RootID}, newID: opID, want: []obj.ID{opID}},
		{name: "new head not stored", key: key, oldIDs: []obj.ID{obj.RootID}, newID: obj.ID{9},
			want: []obj.ID{obj.RootID}, wantErr: ErrFailedPrecondition},
		{name: "new head not an operation", key: key, oldIDs: []obj.ID{obj.RootID}, newID: treeID,
			want: []obj.ID{obj.RootID}, wantErr: ErrFailedPrecondition},
		{name: "unknown workspace", key: WorkspaceKey{Repo: "r", User: "u@x", Name: "nope"},
			oldIDs: []obj.ID{obj.RootID}, newID: opID, want: []obj.ID{obj.RootID},
			wantErr: ErrNotFound},
		{name: "unknown repo", key: WorkspaceKey{Repo: "nope", User: "u@x", Name: "w"},
			oldIDs: []obj.ID{obj.RootID}, newID: opID, want: []obj.ID{obj.RootID},
			wantErr: ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Set up a workspace, operation objects and a tree, then the starting heads: each of
			// before is added from the root, and the root is dropped if any were added.
			s, _ := openStore(t)
			createRepo(t, s, "r")
			createWorkspace(t, s, key)
			insertOps(t, s, opID, op2ID, op3ID)
			if err := s.InsertObjects(t.Context(),
				[]ObjectRecord{{ID: treeID, Kind: obj.Tree}}); err != nil {
				t.Fatalf("InsertObjects: %v", err)
			}
			for _, h := range tc.before {
				if err := s.UpdateOpHeads(t.Context(), key, []obj.ID{obj.RootID}, h); err != nil {
					t.Fatalf("setting up head %s: %v", h, err)
				}
			}

			// Run the update, then check the error and the resulting heads of key's workspace,
			// which a failed update must leave as they were.
			err := s.UpdateOpHeads(t.Context(), tc.key, tc.oldIDs, tc.newID)
			checkErr(t, "UpdateOpHeads", err, tc.wantErr)
			checkOpHeads(t, s, key, tc.want...)
		})
	}
}
