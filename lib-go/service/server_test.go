package service

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	servicev1 "github.com/smukherj1/jj-workspaces/gen-go/jjcw/service/v1"
)

// startServer serves a new Server on a free localhost port and returns a health client for it.
func startServer(t *testing.T) (*Server, healthpb.HealthClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer()
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
	for _, svc := range []string{"", servicev1.WorkspaceService_ServiceDesc.ServiceName} {
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
	stream, err := client.Watch(t.Context(), &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := stream.Recv(); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("first Watch update = %v, %v; want SERVING", resp, err)
	}

	// The open Watch stream keeps GracefulStop waiting, so Shutdown also exercises the timeout.
	go s.Shutdown(100 * time.Millisecond)
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Watch after Shutdown: %v", err)
	}
	if got := resp.GetStatus(); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("Watch after Shutdown = %v, want NOT_SERVING", got)
	}
}
