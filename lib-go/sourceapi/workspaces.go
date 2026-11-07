package sourceapi

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// maxUserLen is the longest user the service accepts, in bytes.
const maxUserLen = 254

// namePattern is the rule for repo and workspace names.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// workspaceService serves the Workspace service.
type workspaceService struct {
	sourceapiv1.UnimplementedWorkspaceServer
	meta *metastore.Store
}

func (s *workspaceService) CreateWorkspace(ctx context.Context,
	req *sourceapiv1.CreateWorkspaceRequest) (*sourceapiv1.CreateWorkspaceResponse, error) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	if err := checkName("workspace", req.GetName()); err != nil {
		return nil, err
	}
	key := metastore.WorkspaceKey{Repo: req.GetRepo(), User: req.GetUser(), Name: req.GetName()}
	if err := s.meta.CreateWorkspace(ctx, key); err != nil {
		return nil, metaError(err, "creating %s", key)
	}
	return &sourceapiv1.CreateWorkspaceResponse{}, nil
}

func (s *workspaceService) ListWorkspaces(ctx context.Context,
	req *sourceapiv1.ListWorkspacesRequest) (*sourceapiv1.ListWorkspacesResponse, error) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	names, err := s.meta.ListWorkspaces(ctx, req.GetRepo(), req.GetUser())
	if err != nil {
		return nil, metaError(err, "listing workspaces in repo %q", req.GetRepo())
	}
	return &sourceapiv1.ListWorkspacesResponse{Names: names}, nil
}

func (s *workspaceService) DeleteWorkspace(ctx context.Context,
	req *sourceapiv1.DeleteWorkspaceRequest) (*sourceapiv1.DeleteWorkspaceResponse, error) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	key := metastore.WorkspaceKey{Repo: req.GetRepo(), User: req.GetUser(), Name: req.GetName()}
	if err := s.meta.DeleteWorkspace(ctx, key); err != nil {
		return nil, metaError(err, "deleting %s", key)
	}
	return &sourceapiv1.DeleteWorkspaceResponse{}, nil
}

// opHeadsService serves the OpHeads service.
type opHeadsService struct {
	sourceapiv1.UnimplementedOpHeadsServer
	meta *metastore.Store
}

func (s *opHeadsService) GetOpHeads(ctx context.Context, req *sourceapiv1.GetOpHeadsRequest) (
	*sourceapiv1.GetOpHeadsResponse, error,
) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	key := metastore.WorkspaceKey{Repo: req.GetRepo(), User: req.GetUser(), Name: req.GetWorkspace()}
	heads, err := s.meta.GetOpHeads(ctx, key)
	if err != nil {
		return nil, metaError(err, "reading op heads of %s", key)
	}
	resp := &sourceapiv1.GetOpHeadsResponse{OpIds: make([][]byte, len(heads))}
	for i, id := range heads {
		resp.OpIds[i] = id[:]
	}
	return resp, nil
}

func (s *opHeadsService) UpdateOpHeads(ctx context.Context, req *sourceapiv1.UpdateOpHeadsRequest) (
	*sourceapiv1.UpdateOpHeadsResponse, error,
) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	oldIDs, err := parseIDs(req.GetOldIds())
	if err != nil {
		return nil, wrapStatus(err, "parsing old op heads")
	}
	newID, err := parseID(req.GetNewId())
	if err != nil {
		return nil, wrapStatus(err, "parsing new op head")
	}
	key := metastore.WorkspaceKey{Repo: req.GetRepo(), User: req.GetUser(), Name: req.GetWorkspace()}
	if err := s.meta.UpdateOpHeads(ctx, key, oldIDs, newID); err != nil {
		return nil, metaError(err, "updating op heads of %s", key)
	}
	return &sourceapiv1.UpdateOpHeadsResponse{}, nil
}

// checkName returns InvalidArgument, stating the rule, if name isn't a valid name for what, which
// is "repo" or "workspace".
func checkName(what, name string) error {
	if !namePattern.MatchString(name) {
		return status.Errorf(codes.InvalidArgument,
			"invalid %s name %q: must start with a letter or digit, contain only letters, digits, "+
				"'.', '_' and '-', and be at most 100 characters", what, name)
	}
	return nil
}

// checkUser returns InvalidArgument if user isn't an acceptable user. The service treats it as an
// opaque key, so it doesn't check that it's an email.
func checkUser(user string) error {
	switch {
	case user == "":
		return status.Error(codes.InvalidArgument, "invalid user: must not be empty")
	case len(user) > maxUserLen:
		return status.Errorf(codes.InvalidArgument, "invalid user: %d bytes is over the limit of %d",
			len(user), maxUserLen)
	case strings.ContainsFunc(user, unicode.IsControl):
		return status.Errorf(codes.InvalidArgument, "invalid user %q: has a control character", user)
	}
	return nil
}

// metaError turns an error from the metadata store into a gRPC status. The store's own errors name
// the entity, so they go out as they are. Anything else is storageError's, with the format's context.
func metaError(err error, format string, args ...any) error {
	switch {
	case errors.Is(err, metastore.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, metastore.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, metastore.ErrFailedPrecondition):
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return storageError(fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err))
}
