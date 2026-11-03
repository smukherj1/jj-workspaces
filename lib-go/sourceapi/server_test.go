package sourceapi

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// healthNames are "" and the fully qualified name of each source API service.
var healthNames = []string{
	"",
	"jjc.sourceapi.v1.Repo",
	"jjc.sourceapi.v1.Workspace",
	"jjc.sourceapi.v1.Objects",
	"jjc.sourceapi.v1.OpHeads",
	"jjc.sourceapi.v1.Trunk",
}

// startServer serves a new Server on a free localhost port and returns a health client for it.
func startServer(t *testing.T) (*Server, healthpb.HealthClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	objects, err := objstore.Open(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := metastore.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })
	s := NewServer(meta, objects)
	served := make(chan error, 1)
	go func() { served <- s.Serve(lis) }()
	t.Cleanup(func() {
		s.Shutdown(time.Second)
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return s, healthpb.NewHealthClient(conn)
}

func TestHealthServing(t *testing.T) {
	_, client := startServer(t)
	for _, svc := range healthNames {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: svc})
		cancel()
		if err != nil {
			t.Fatalf("Check(%q): %v", svc, err)
		}
		if got := resp.GetStatus(); got != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("Check(%q) = %v, want SERVING", svc, got)
		}
	}
}

func TestShutdownSetsNotServing(t *testing.T) {
	s, client := startServer(t)
	streams := make(map[string]healthpb.Health_WatchClient)
	for _, svc := range healthNames {
		stream, err := client.Watch(t.Context(), &healthpb.HealthCheckRequest{Service: svc})
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := stream.Recv(); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("first Watch(%q) update = %v, %v; want SERVING", svc, resp, err)
		}
		streams[svc] = stream
	}

	// The open Watch streams keep GracefulStop waiting, so Shutdown also exercises the timeout.
	go s.Shutdown(100 * time.Millisecond)
	for svc, stream := range streams {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Watch(%q) after Shutdown: %v", svc, err)
		}
		if got := resp.GetStatus(); got != healthpb.HealthCheckResponse_NOT_SERVING {
			t.Errorf("Watch(%q) after Shutdown = %v, want NOT_SERVING", svc, got)
		}
	}
}
