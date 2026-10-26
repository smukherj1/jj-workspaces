// Package daemon implements the gRPC servers of jjw-daemon and its Unix socket handling.
package daemon

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	daemonv1 "github.com/smukherj1/jj-workspaces/gen-go/jjcw/daemon/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/daemon/serviceclient"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

// Server is a gRPC server serving the daemon API, health and reflection.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
}

type daemonService struct {
	daemonv1.UnimplementedDaemonServer
	service *serviceclient.Client
}

// Services with no RPCs yet.
type (
	storeService struct {
		daemonv1.UnimplementedStoreServer
	}
	opStoreService struct {
		daemonv1.UnimplementedOpStoreServer
	}
	opHeadsService struct {
		daemonv1.UnimplementedOpHeadsServer
	}
	workingCopyService struct {
		daemonv1.UnimplementedWorkingCopyServer
	}
	workspaceService struct {
		daemonv1.UnimplementedWorkspaceServer
	}
	prefetchService struct {
		daemonv1.UnimplementedPrefetchServer
	}
)

// NewServer returns a server that calls the workspace service through service. Its health status is
// SERVING for "".
func NewServer(service *serviceclient.Client) *Server {
	s := &Server{grpc: grpc.NewServer(), health: health.NewServer()}
	daemonv1.RegisterDaemonServer(s.grpc, &daemonService{service: service})
	daemonv1.RegisterStoreServer(s.grpc, &storeService{})
	daemonv1.RegisterOpStoreServer(s.grpc, &opStoreService{})
	daemonv1.RegisterOpHeadsServer(s.grpc, &opHeadsService{})
	daemonv1.RegisterWorkingCopyServer(s.grpc, &workingCopyService{})
	daemonv1.RegisterWorkspaceServer(s.grpc, &workspaceService{})
	daemonv1.RegisterPrefetchServer(s.grpc, &prefetchService{})
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

func (d *daemonService) GetStatus(ctx context.Context, _ *daemonv1.GetStatusRequest) (*daemonv1.GetStatusResponse, error) {
	return &daemonv1.GetStatusResponse{
		DaemonVersion:  version.Version,
		ServiceAddress: d.service.Addr(),
		ServiceStatus:  d.serviceStatus(ctx),
	}, nil
}

func (d *daemonService) serviceStatus(ctx context.Context) *daemonv1.ServiceStatus {
	st, err := d.service.Health(ctx)
	switch {
	case err != nil:
		return &daemonv1.ServiceStatus{Code: daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_UNREACHABLE, Details: err.Error()}
	case st == healthpb.HealthCheckResponse_SERVING:
		return &daemonv1.ServiceStatus{Code: daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_OK}
	default:
		return &daemonv1.ServiceStatus{Code: daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_UNHEALTHY, Details: st.String()}
	}
}
