package sourceapi_test

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
)

// invalidNames break the name rule that repos and workspaces share.
var invalidNames = []string{"", ".x", "a/b", "a\n", strings.Repeat("a", 101)}

// TestCreateRepo checks that valid names are accepted, a second create of a name is AlreadyExists,
// and a name breaking the rule is InvalidArgument and stores nothing.
func TestCreateRepo(t *testing.T) {
	h := newHarness(t)
	create := func(repo string) error {
		_, err := h.Repos().CreateRepo(testCtx(t), &sourceapiv1.CreateRepoRequest{Repo: repo})
		return err
	}

	// The extremes of the rule are accepted.
	for _, repo := range []string{"a", "0._-Z", strings.Repeat("a", 100)} {
		if err := create(repo); err != nil {
			t.Errorf("CreateRepo(%q): %v", repo, err)
		}
	}

	// The repo created above exists, so a second create fails.
	wantStatus(t, "second CreateRepo", create("a"), codes.AlreadyExists, `repo "a" already exists`)

	// Each invalid name is rejected with the rule, and no repo appears under it.
	for _, repo := range invalidNames {
		wantStatus(t, "CreateRepo", create(repo), codes.InvalidArgument, "must start with a letter or digit")
		_, err := h.Workspaces().ListWorkspaces(testCtx(t),
			&sourceapiv1.ListWorkspacesRequest{Repo: repo, User: alice})
		wantStatus(t, "ListWorkspaces", err, codes.NotFound, "not found")
	}
}
