// Package service implements the gRPC servers of jjw-workspace-service.
package service

import (
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	servicev1 "github.com/smukherj1/jj-workspaces/gen-go/jjcw/service/v1"
)

// Server is a gRPC server serving WorkspaceService, health and reflection.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
}

type workspaceService struct {
	servicev1.UnimplementedWorkspaceServiceServer
}

// NewServer returns a server whose health status is SERVING for "" and for WorkspaceService.
func NewServer() *Server {
	s := &Server{grpc: grpc.NewServer(), health: health.NewServer()}
	servicev1.RegisterWorkspaceServiceServer(s.grpc, &workspaceService{})
	healthpb.RegisterHealthServer(s.grpc, s.health)
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	s.health.SetServingStatus(servicev1.WorkspaceService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
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
