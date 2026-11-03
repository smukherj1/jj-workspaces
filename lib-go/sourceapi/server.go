// Package sourceapi implements the gRPC servers of source-api.
package sourceapi

import (
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// MaxMessageContent is the most object content one RPC message carries, in a batch or as a stream
// chunk. An object over it can only go through UploadObject and DownloadObject.
const MaxMessageContent = 4 << 20

// MaxMessageSize is the largest gRPC message the server sends or receives. It's over
// MaxMessageContent to leave room for IDs and framing.
const MaxMessageSize = 8 << 20

// serviceNames are the fully qualified names of the source API's gRPC services.
var serviceNames = []string{
	sourceapiv1.Repo_ServiceDesc.ServiceName,
	sourceapiv1.Workspace_ServiceDesc.ServiceName,
	sourceapiv1.Objects_ServiceDesc.ServiceName,
	sourceapiv1.OpHeads_ServiceDesc.ServiceName,
	sourceapiv1.Trunk_ServiceDesc.ServiceName,
}

// Server is a gRPC server serving the source API, health and reflection.
type Server struct {
	grpc   *grpc.Server
	health *health.Server
}

// Services with no RPCs yet.
type (
	repoService struct {
		sourceapiv1.UnimplementedRepoServer
	}
	workspaceService struct {
		sourceapiv1.UnimplementedWorkspaceServer
	}
	opHeadsService struct {
		sourceapiv1.UnimplementedOpHeadsServer
	}
	trunkService struct {
		sourceapiv1.UnimplementedTrunkServer
	}
)

// NewServer returns a server over meta and objects whose health status is SERVING for "" and for
// each of serviceNames. The caller closes meta after the server stops.
func NewServer(meta *metastore.Store, objects *objstore.Store) *Server {
	s := &Server{
		grpc: grpc.NewServer(
			grpc.MaxRecvMsgSize(MaxMessageSize),
			grpc.MaxSendMsgSize(MaxMessageSize),
		),
		health: health.NewServer(),
	}
	sourceapiv1.RegisterRepoServer(s.grpc, &repoService{})
	sourceapiv1.RegisterWorkspaceServer(s.grpc, &workspaceService{})
	sourceapiv1.RegisterObjectsServer(s.grpc, &objectsService{meta: meta, objects: objects})
	sourceapiv1.RegisterOpHeadsServer(s.grpc, &opHeadsService{})
	sourceapiv1.RegisterTrunkServer(s.grpc, &trunkService{})
	healthpb.RegisterHealthServer(s.grpc, s.health)
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	for _, name := range serviceNames {
		s.health.SetServingStatus(name, healthpb.HealthCheckResponse_SERVING)
	}
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
