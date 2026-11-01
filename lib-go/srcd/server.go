// Package srcd implements the gRPC servers of srcd and its Unix socket handling.
package srcd

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	srcdv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/srcd/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/srcd/sourceapiclient"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

// Server is a gRPC server serving the daemon API, health and reflection.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
}

type daemonService struct {
	srcdv1.UnimplementedDaemonServer
	sourceAPI *sourceapiclient.Client
}

// Services with no RPCs yet.
type (
	storeService struct {
		srcdv1.UnimplementedStoreServer
	}
	opStoreService struct {
		srcdv1.UnimplementedOpStoreServer
	}
	opHeadsService struct {
		srcdv1.UnimplementedOpHeadsServer
	}
	workingCopyService struct {
		srcdv1.UnimplementedWorkingCopyServer
	}
	repoService struct {
		srcdv1.UnimplementedRepoServer
	}
	workspaceService struct {
		srcdv1.UnimplementedWorkspaceServer
	}
	prefetchService struct {
		srcdv1.UnimplementedPrefetchServer
	}
)

// NewServer returns a server that calls source-api through sourceAPI. Its health status is SERVING
// for "".
func NewServer(sourceAPI *sourceapiclient.Client) *Server {
	s := &Server{grpc: grpc.NewServer(), health: health.NewServer()}
	srcdv1.RegisterDaemonServer(s.grpc, &daemonService{sourceAPI: sourceAPI})
	srcdv1.RegisterStoreServer(s.grpc, &storeService{})
	srcdv1.RegisterOpStoreServer(s.grpc, &opStoreService{})
	srcdv1.RegisterOpHeadsServer(s.grpc, &opHeadsService{})
	srcdv1.RegisterWorkingCopyServer(s.grpc, &workingCopyService{})
	srcdv1.RegisterRepoServer(s.grpc, &repoService{})
	srcdv1.RegisterWorkspaceServer(s.grpc, &workspaceService{})
	srcdv1.RegisterPrefetchServer(s.grpc, &prefetchService{})
	healthpb.RegisterHealthServer(s.grpc, s.health)
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	reflection.Register(s.grpc)
	return s
}

// Serve accepts connections on lis until Shutdown is called.
func (s *Server) Serve(lis net.Listener) error {
	return s.grpc.Serve(lis)
}

// Shutdown sets every health status to NOT_SERVING, then stops gracefully, waiting at most timeout
// for in-flight RPCs before closing the remaining connections.
func (s *Server) Shutdown(timeout time.Duration) {
	s.health.Shutdown()
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.grpc.Stop()
		<-done
	}
}

func (d *daemonService) GetStatus(ctx context.Context, _ *srcdv1.GetStatusRequest) (*srcdv1.GetStatusResponse, error) {
	return &srcdv1.GetStatusResponse{
		DaemonVersion:    version.Version,
		SourceApiAddress: d.sourceAPI.Addr(),
		SourceApiStatus:  d.sourceAPIStatus(ctx),
	}, nil
}

func (d *daemonService) sourceAPIStatus(ctx context.Context) *srcdv1.SourceApiStatus {
	st, err := d.sourceAPI.Health(ctx)
	switch {
	case err != nil:
		return &srcdv1.SourceApiStatus{Code: srcdv1.SourceApiStatusCode_SOURCE_API_STATUS_CODE_UNREACHABLE, Details: err.Error()}
	case st == healthpb.HealthCheckResponse_SERVING:
		return &srcdv1.SourceApiStatus{Code: srcdv1.SourceApiStatusCode_SOURCE_API_STATUS_CODE_OK}
	default:
		return &srcdv1.SourceApiStatus{Code: srcdv1.SourceApiStatusCode_SOURCE_API_STATUS_CODE_UNHEALTHY, Details: st.String()}
	}
}
