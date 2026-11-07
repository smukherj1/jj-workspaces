package sourceapi_test

import (
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
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

func TestHealthServing(t *testing.T) {
	h := newHarness(t)
	for _, svc := range healthNames {
		resp, err := h.Health().Check(testCtx(t), &healthpb.HealthCheckRequest{Service: svc})
		if err != nil {
			t.Fatalf("Check(%q): %v", svc, err)
		}
		if got := resp.GetStatus(); got != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("Check(%q) = %v, want SERVING", svc, got)
		}
	}
}

func TestShutdownSetsNotServing(t *testing.T) {
	h := newHarness(t)
	streams := make(map[string]healthpb.Health_WatchClient)
	for _, svc := range healthNames {
		stream, err := h.Health().Watch(t.Context(), &healthpb.HealthCheckRequest{Service: svc})
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := stream.Recv(); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("first Watch(%q) update = %v, %v; want SERVING", svc, resp, err)
		}
		streams[svc] = stream
	}

	// The open Watch streams keep GracefulStop waiting, so Shutdown also exercises the timeout.
	go h.Server().Shutdown(100 * time.Millisecond)
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
