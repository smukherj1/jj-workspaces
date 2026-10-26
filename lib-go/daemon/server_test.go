package daemon

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	daemonv1 "github.com/smukherj1/jj-workspaces/gen-go/jjcw/daemon/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/daemon/serviceclient"
	"github.com/smukherj1/jj-workspaces/lib-go/service"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

// startDaemon serves a new daemon Server on a Unix socket, pointed at the service at serviceAddr,
// and returns a Daemon client for it.
func startDaemon(t *testing.T, serviceAddr string, serviceTimeout time.Duration) daemonv1.DaemonClient {
	t.Helper()
	svc, err := serviceclient.New(serviceAddr, serviceTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })

	lis, err := Listen(filepath.Join(t.TempDir(), "d.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(svc)
	served := make(chan error, 1)
	go func() { served <- s.Serve(lis) }()
	t.Cleanup(func() {
		s.Shutdown(time.Second)
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	conn, err := grpc.NewClient("unix://"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return daemonv1.NewDaemonClient(conn)
}

// serve serves srv on a free localhost port until the test ends and returns its address.
func serve(t *testing.T, srv interface {
	Serve(net.Listener) error
}, stop func()) string {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(stop)
	return lis.Addr().String()
}

// unusedAddr returns a localhost address that nothing listens on.
func unusedAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// silentListener returns the address of a TCP listener that accepts connections and never writes
// to them, so an HTTP/2 handshake with it never completes.
func silentListener(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		lis.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return lis.Addr().String()
}

func getStatus(t *testing.T, client daemonv1.DaemonClient) (*daemonv1.GetStatusResponse, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := client.GetStatus(ctx, &daemonv1.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	return resp, time.Since(start)
}

func TestGetStatusServing(t *testing.T) {
	svc := service.NewServer()
	addr := serve(t, svc, func() { svc.Shutdown(time.Second) })
	resp, _ := getStatus(t, startDaemon(t, addr, 10*time.Second))

	if resp.GetDaemonVersion() != version.Version {
		t.Errorf("daemon_version = %q, want %q", resp.GetDaemonVersion(), version.Version)
	}
	if resp.GetServiceAddress() != addr {
		t.Errorf("service_address = %q, want %q", resp.GetServiceAddress(), addr)
	}
	if got := resp.GetServiceStatus().GetCode(); got != daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_OK {
		t.Errorf("service status = %v, want OK", resp.GetServiceStatus())
	}
}

func TestGetStatusNotServing(t *testing.T) {
	srv := grpc.NewServer()
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	addr := serve(t, srv, srv.Stop)
	resp, _ := getStatus(t, startDaemon(t, addr, 10*time.Second))

	st := resp.GetServiceStatus()
	if st.GetCode() != daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_UNHEALTHY || st.GetDetails() != "NOT_SERVING" {
		t.Errorf("service status = %v, want UNHEALTHY with details NOT_SERVING", st)
	}
}

func TestGetStatusNothingListening(t *testing.T) {
	resp, elapsed := getStatus(t, startDaemon(t, unusedAddr(t), 10*time.Second))

	st := resp.GetServiceStatus()
	if st.GetCode() != daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_UNREACHABLE || !strings.Contains(st.GetDetails(), "Unavailable") {
		t.Errorf("service status = %v, want UNREACHABLE with an Unavailable error", st)
	}
	if elapsed > 2*time.Second {
		t.Errorf("GetStatus took %v, want well under the 10s timeout", elapsed)
	}
}

func TestGetStatusServiceHangs(t *testing.T) {
	const timeout = 500 * time.Millisecond
	resp, elapsed := getStatus(t, startDaemon(t, silentListener(t), timeout))

	st := resp.GetServiceStatus()
	want := "deadline exceeded after 500ms"
	if st.GetCode() != daemonv1.ServiceStatusCode_SERVICE_STATUS_CODE_UNREACHABLE || st.GetDetails() != want {
		t.Errorf("service status = %v, want UNREACHABLE with details %q", st, want)
	}
	if elapsed < timeout || elapsed > timeout+500*time.Millisecond {
		t.Errorf("GetStatus took %v, want about %v", elapsed, timeout)
	}
}
