package sourceapi

import (
	"context"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// repoService serves the Repo service.
type repoService struct {
	sourceapiv1.UnimplementedRepoServer
	meta *metastore.Store
}

func (s *repoService) CreateRepo(ctx context.Context, req *sourceapiv1.CreateRepoRequest) (
	*sourceapiv1.CreateRepoResponse, error,
) {
	if err := checkName("repo", req.GetRepo()); err != nil {
		return nil, err
	}
	if err := s.meta.CreateRepo(ctx, req.GetRepo()); err != nil {
		return nil, metaError(err, "creating repo %q", req.GetRepo())
	}
	return &sourceapiv1.CreateRepoResponse{}, nil
}
