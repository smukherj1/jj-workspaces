package sourceapi_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/obj"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/sourceapitest"
)

const (
	alice = "alice@example.com"
	bob   = "bob@example.com"
)

// wsRef names a workspace in calls from tests.
type wsRef struct {
	repo, user, name string
}

// startWithRepo serves source-api on a new data dir with one repo, "r".
func startWithRepo(t *testing.T) *sourceapitest.Harness {
	t.Helper()
	h := newHarness(t)
	if _, err := h.Repos().CreateRepo(testCtx(t), &sourceapiv1.CreateRepoRequest{Repo: "r"}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return h
}

func createWorkspace(t *testing.T, h *sourceapitest.Harness, w wsRef) error {
	t.Helper()
	_, err := h.Workspaces().CreateWorkspace(testCtx(t), &sourceapiv1.CreateWorkspaceRequest{
		Repo: w.repo, User: w.user, Name: w.name,
	})
	return err
}

func mustCreateWorkspace(t *testing.T, h *sourceapitest.Harness, w wsRef) {
	t.Helper()
	if err := createWorkspace(t, h, w); err != nil {
		t.Fatalf("CreateWorkspace(%+v): %v", w, err)
	}
}

func deleteWorkspace(t *testing.T, h *sourceapitest.Harness, w wsRef) error {
	t.Helper()
	_, err := h.Workspaces().DeleteWorkspace(testCtx(t), &sourceapiv1.DeleteWorkspaceRequest{
		Repo: w.repo, User: w.user, Name: w.name,
	})
	return err
}

func listWorkspaces(t *testing.T, h *sourceapitest.Harness, repo, user string) ([]string, error) {
	t.Helper()
	resp, err := h.Workspaces().ListWorkspaces(testCtx(t), &sourceapiv1.ListWorkspacesRequest{
		Repo: repo, User: user,
	})
	return resp.GetNames(), err
}

func getOpHeads(t *testing.T, h *sourceapitest.Harness, w wsRef) ([][]byte, error) {
	t.Helper()
	resp, err := h.OpHeads().GetOpHeads(testCtx(t), &sourceapiv1.GetOpHeadsRequest{
		Repo: w.repo, User: w.user, Workspace: w.name,
	})
	return resp.GetOpIds(), err
}

func updateOpHeads(t *testing.T, h *sourceapitest.Harness, w wsRef, oldIDs [][]byte, newID []byte) error {
	t.Helper()
	_, err := h.OpHeads().UpdateOpHeads(testCtx(t), &sourceapiv1.UpdateOpHeadsRequest{
		Repo: w.repo, User: w.user, Workspace: w.name, OldIds: oldIDs, NewId: newID,
	})
	return err
}

// wantHeads checks that a workspace's op heads are exactly want, in any order.
func wantHeads(t *testing.T, h *sourceapitest.Harness, w wsRef, want ...[]byte) {
	t.Helper()
	got, err := getOpHeads(t, h, w)
	if err != nil {
		t.Fatalf("GetOpHeads(%+v): %v", w, err)
	}
	slices.SortFunc(got, bytes.Compare)
	want = slices.Clone(want)
	slices.SortFunc(want, bytes.Compare)
	if !slices.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("GetOpHeads(%+v) = %x, want %x", w, got, want)
	}
}

// storedOp stores an operation with content derived from seed and returns its ID.
func storedOp(t *testing.T, h *sourceapitest.Harness, seed string) []byte {
	t.Helper()
	op := newObject(t, obj.Operation, []byte("operation "+seed))
	put(t, h, op)
	return op.GetId()
}

// TestCreateWorkspace checks that a new workspace starts at the root operation, that a user can't
// create the same name twice, and each way a create can be rejected.
func TestCreateWorkspace(t *testing.T) {
	h := startWithRepo(t)

	// A new workspace reads {root}.
	w := wsRef{"r", alice, "w"}
	mustCreateWorkspace(t, h, w)
	wantHeads(t, h, w, obj.RootID[:])

	for _, tc := range []struct {
		name    string
		ws      wsRef
		code    codes.Code
		msgPart string
	}{
		{"second create", w, codes.AlreadyExists, `workspace "w" in repo "r" already exists`},
		{"unknown repo", wsRef{"nope", alice, "w"}, codes.NotFound, `repo "nope" not found`},
		{"empty user", wsRef{"r", "", "w2"}, codes.InvalidArgument, "invalid user"},
		{"user with a newline", wsRef{"r", "a\nb", "w2"}, codes.InvalidArgument, "control character"},
		{"user of 255 bytes", wsRef{"r", strings.Repeat("a", 255), "w2"}, codes.InvalidArgument, "over the limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantStatus(t, "CreateWorkspace", createWorkspace(t, h, tc.ws), tc.code, tc.msgPart)
		})
	}
	for _, name := range invalidNames {
		t.Run(fmt.Sprintf("name %q", name), func(t *testing.T) {
			err := createWorkspace(t, h, wsRef{"r", alice, name})
			wantStatus(t, "CreateWorkspace", err, codes.InvalidArgument, "must start with a letter or digit")
		})
	}

	// None of the rejected creates registered anything.
	names, err := listWorkspaces(t, h, "r", alice)
	if err != nil || !slices.Equal(names, []string{"w"}) {
		t.Errorf("ListWorkspaces = %q, %v; want just w", names, err)
	}
}

// TestCreateWorkspace_userIsOpaque checks that the service stores a user with characters that need
// escaping in a path or SQL string, and one of the longest allowed length.
func TestCreateWorkspace_userIsOpaque(t *testing.T) {
	h := startWithRepo(t)
	for _, user := range []string{"a/b%2f'; --@example.com", strings.Repeat("a", 254)} {
		w := wsRef{"r", user, "w"}
		mustCreateWorkspace(t, h, w)
		wantHeads(t, h, w, obj.RootID[:])
	}
}

// TestListWorkspaces checks that a user sees only their own workspaces in that repo, in any order,
// and the errors for an unknown repo and an invalid user.
func TestListWorkspaces(t *testing.T) {
	h := startWithRepo(t)
	if _, err := h.Repos().CreateRepo(testCtx(t), &sourceapiv1.CreateRepoRequest{Repo: "other"}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	// Alice has two workspaces in r, and each user has one more elsewhere.
	for _, w := range []wsRef{
		{"r", alice, "b"}, {"r", alice, "a"}, {"r", bob, "c"}, {"other", alice, "d"},
	} {
		mustCreateWorkspace(t, h, w)
	}

	for _, tc := range []struct {
		name       string
		repo, user string
		want       []string
	}{
		{"two workspaces", "r", alice, []string{"a", "b"}},
		{"another user", "r", bob, []string{"c"}},
		{"another repo", "other", alice, []string{"d"}},
		{"a user with none", "other", bob, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listWorkspaces(t, h, tc.repo, tc.user)
			if err != nil {
				t.Fatalf("ListWorkspaces: %v", err)
			}
			// Compare in any order.
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ListWorkspaces = %q, want %q", got, tc.want)
			}
		})
	}

	_, err := listWorkspaces(t, h, "nope", alice)
	wantStatus(t, "ListWorkspaces", err, codes.NotFound, `repo "nope" not found`)
	_, err = listWorkspaces(t, h, "r", "")
	wantStatus(t, "ListWorkspaces", err, codes.InvalidArgument, "invalid user")
}

// TestDeleteWorkspace checks that a deleted workspace is gone from every RPC, that its name can be
// created again from {root}, and that a delete leaves other workspaces alone.
func TestDeleteWorkspace(t *testing.T) {
	h := startWithRepo(t)
	w, other := wsRef{"r", alice, "w"}, wsRef{"r", bob, "w"}
	// w is created last so that recreating it reuses its row ID, where op heads left behind by the
	// delete would show.
	mustCreateWorkspace(t, h, other)
	mustCreateWorkspace(t, h, w)
	op := storedOp(t, h, "1")
	if err := updateOpHeads(t, h, w, [][]byte{obj.RootID[:]}, op); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}
	if err := updateOpHeads(t, h, other, [][]byte{obj.RootID[:]}, op); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}

	if err := deleteWorkspace(t, h, w); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}

	// The workspace is gone from the list and both op-heads RPCs.
	names, err := listWorkspaces(t, h, "r", alice)
	if err != nil || len(names) != 0 {
		t.Errorf("ListWorkspaces = %q, %v; want none", names, err)
	}
	_, err = getOpHeads(t, h, w)
	wantStatus(t, "GetOpHeads", err, codes.NotFound, `workspace "w" in repo "r" not found`)
	err = updateOpHeads(t, h, w, nil, op)
	wantStatus(t, "UpdateOpHeads", err, codes.NotFound, `workspace "w" in repo "r" not found`)

	// The other user's workspace of the same name is untouched, and the op stayed stored.
	wantHeads(t, h, other, op)
	checkGot(t, h, []*sourceapiv1.Object{newObject(t, obj.Operation, []byte("operation 1"))})

	// The name can be created again, starting over at {root}.
	mustCreateWorkspace(t, h, w)
	wantHeads(t, h, w, obj.RootID[:])

	// Errors: an unknown workspace, an unknown repo and an invalid user.
	err = deleteWorkspace(t, h, wsRef{"r", alice, "nope"})
	wantStatus(t, "DeleteWorkspace", err, codes.NotFound, `workspace "nope" in repo "r" not found`)
	err = deleteWorkspace(t, h, wsRef{"nope", alice, "w"})
	wantStatus(t, "DeleteWorkspace", err, codes.NotFound, `repo "nope" not found`)
	err = deleteWorkspace(t, h, wsRef{"r", "", "w"})
	wantStatus(t, "DeleteWorkspace", err, codes.InvalidArgument, "invalid user")
}

// TestGetOpHeads_errors checks the errors for an unknown repo, workspace and invalid user. A
// workspace of another user is unknown too.
func TestGetOpHeads_errors(t *testing.T) {
	h := startWithRepo(t)
	mustCreateWorkspace(t, h, wsRef{"r", alice, "w"})
	for _, tc := range []struct {
		name    string
		ws      wsRef
		code    codes.Code
		msgPart string
	}{
		{"unknown repo", wsRef{"nope", alice, "w"}, codes.NotFound, `repo "nope" not found`},
		{"unknown workspace", wsRef{"r", alice, "nope"}, codes.NotFound, `workspace "nope" in repo "r" not found`},
		{"another user's workspace", wsRef{"r", bob, "w"}, codes.NotFound, `workspace "w" in repo "r" not found`},
		{"empty user", wsRef{"r", "", "w"}, codes.InvalidArgument, "invalid user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := getOpHeads(t, h, tc.ws)
			wantStatus(t, "GetOpHeads", err, tc.code, tc.msgPart)
		})
	}
}

// TestUpdateOpHeads checks that an update removes the old heads and adds the new one, in the cases
// jj hits: a linear publish, sibling publishes from one old head, a merge of them, an old head
// that's already gone, and a retry.
func TestUpdateOpHeads(t *testing.T) {
	h := startWithRepo(t)
	w := wsRef{"r", alice, "w"}
	mustCreateWorkspace(t, h, w)
	root := obj.RootID[:]
	a, b, c, d := storedOp(t, h, "a"), storedOp(t, h, "b"), storedOp(t, h, "c"), storedOp(t, h, "d")

	// Each step applies to the heads the previous one left.
	for _, step := range []struct {
		name string
		old  [][]byte
		new  []byte
		want [][]byte
	}{
		{"linear publish", [][]byte{root}, a, [][]byte{a}},
		{"first sibling", [][]byte{a}, b, [][]byte{b}},
		{"second sibling, whose old head is gone", [][]byte{a}, c, [][]byte{b, c}},
		{"retry of the same update", [][]byte{a}, c, [][]byte{b, c}},
		{"merge of the siblings", [][]byte{b, c}, d, [][]byte{d}},
		{"new head also in old", [][]byte{d}, d, [][]byte{d}},
	} {
		t.Run(step.name, func(t *testing.T) {
			if err := updateOpHeads(t, h, w, step.old, step.new); err != nil {
				t.Fatalf("UpdateOpHeads: %v", err)
			}
			wantHeads(t, h, w, step.want...)
		})
	}
}

// TestUpdateOpHeads_errors checks each rejected update, and that none changes the op heads.
func TestUpdateOpHeads_errors(t *testing.T) {
	h := startWithRepo(t)
	w := wsRef{"r", alice, "w"}
	mustCreateWorkspace(t, h, w)
	root := obj.RootID[:]
	op := storedOp(t, h, "a")
	blob := newObject(t, obj.Blob, []byte("not an operation"))
	put(t, h, blob)
	unstored := newObject(t, obj.Operation, []byte("never uploaded")).GetId()

	for _, tc := range []struct {
		name    string
		ws      wsRef
		old     [][]byte
		new     []byte
		code    codes.Code
		msgPart string
	}{
		{"unknown repo", wsRef{"nope", alice, "w"}, [][]byte{root}, op, codes.NotFound, `repo "nope" not found`},
		{"unknown workspace", wsRef{"r", alice, "nope"}, [][]byte{root}, op, codes.NotFound, `workspace "nope"`},
		{"invalid user", wsRef{"r", "a\nb", "w"}, [][]byte{root}, op, codes.InvalidArgument, "invalid user"},
		{"operation not stored", w, [][]byte{root}, unstored, codes.FailedPrecondition, "wasn't found as an operation object"},
		{"new head is a blob", w, [][]byte{root}, blob.GetId(), codes.FailedPrecondition, "wasn't found as an operation object"},
		{"root as the new head", w, nil, root, codes.FailedPrecondition, "wasn't found as an operation object"},
		{"malformed new ID", w, [][]byte{root}, []byte{1}, codes.InvalidArgument, "parsing new op head"},
		{"malformed old ID", w, [][]byte{{1}}, op, codes.InvalidArgument, "parsing old op heads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := updateOpHeads(t, h, tc.ws, tc.old, tc.new)
			wantStatus(t, "UpdateOpHeads", err, tc.code, tc.msgPart)
			wantHeads(t, h, w, root)
		})
	}
}

// TestUpdateOpHeads_workspacesAreIndependent checks that two workspaces of one user and a workspace
// of another user with the same name each keep their own op heads.
func TestUpdateOpHeads_workspacesAreIndependent(t *testing.T) {
	h := startWithRepo(t)
	first, second, bobs := wsRef{"r", alice, "w"}, wsRef{"r", alice, "w2"}, wsRef{"r", bob, "w"}
	for _, w := range []wsRef{first, second, bobs} {
		mustCreateWorkspace(t, h, w)
	}
	root := obj.RootID[:]

	// Publishing in the first leaves the others at {root}.
	op1 := storedOp(t, h, "1")
	if err := updateOpHeads(t, h, first, [][]byte{root}, op1); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}
	wantHeads(t, h, first, op1)
	wantHeads(t, h, second, root)
	wantHeads(t, h, bobs, root)

	// An update naming the first's head as old in the second adds a head to the second only.
	op2 := storedOp(t, h, "2")
	if err := updateOpHeads(t, h, second, [][]byte{op1}, op2); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}
	wantHeads(t, h, first, op1)
	wantHeads(t, h, second, root, op2)
	wantHeads(t, h, bobs, root)

	// A workspace created after those publishes still starts at {root}.
	late := wsRef{"r", alice, "late"}
	mustCreateWorkspace(t, h, late)
	wantHeads(t, h, late, root)
}

// TestUpdateOpHeads_concurrent checks that parallel publishes from one old head lose nothing: every
// new op ends up a head, and the old head is gone.
func TestUpdateOpHeads_concurrent(t *testing.T) {
	h := startWithRepo(t)
	w := wsRef{"r", alice, "w"}
	mustCreateWorkspace(t, h, w)

	// Twenty ops are stored up front so that only the updates race.
	root := obj.RootID[:]
	ops := make([][]byte, 20)
	for i := range ops {
		ops[i] = storedOp(t, h, fmt.Sprint(i))
	}

	var wg sync.WaitGroup
	errs := make([]error, len(ops))
	for i, op := range ops {
		wg.Go(func() {
			_, errs[i] = h.OpHeads().UpdateOpHeads(testCtx(t), &sourceapiv1.UpdateOpHeadsRequest{
				Repo: w.repo, User: w.user, Workspace: w.name, OldIds: [][]byte{root}, NewId: op,
			})
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("UpdateOpHeads %d: %v", i, err)
		}
	}
	wantHeads(t, h, w, ops...)
}

// TestWorkspaces_surviveRestart checks that repos, the registry and op heads are on disk: a new
// server on the same data dir serves them.
func TestWorkspaces_surviveRestart(t *testing.T) {
	h := startWithRepo(t)
	w := wsRef{"r", alice, "w"}
	mustCreateWorkspace(t, h, w)
	op := storedOp(t, h, "1")
	if err := updateOpHeads(t, h, w, [][]byte{obj.RootID[:]}, op); err != nil {
		t.Fatalf("UpdateOpHeads: %v", err)
	}

	restart(t, h)

	// The repo, the workspace and the op heads are all back.
	_, err := h.Repos().CreateRepo(testCtx(t), &sourceapiv1.CreateRepoRequest{Repo: "r"})
	wantStatus(t, "CreateRepo", err, codes.AlreadyExists, "already exists")
	names, err := listWorkspaces(t, h, "r", alice)
	if err != nil || !slices.Equal(names, []string{"w"}) {
		t.Errorf("ListWorkspaces = %q, %v; want just w", names, err)
	}
	wantHeads(t, h, w, op)
}
