// Command jjw-daemon serves the jjw CLI on a Unix socket and talks to jjw-workspace-service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/daemon"
	"github.com/smukherj1/jj-workspaces/lib-go/daemon/serviceclient"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

const shutdownTimeout = 5 * time.Second

func main() {
	socket := flag.String("socket", defaultSocket(), "Unix socket to listen on (default $XDG_RUNTIME_DIR/jjw/daemon.sock)")
	serviceAddr := flag.String("service-addr", "", "host:port of jjw-workspace-service (required)")
	serviceTimeout := flag.Duration("service-timeout", 10*time.Second, "bound on every call to the service")
	printVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *printVersion {
		fmt.Printf("jjw-daemon %s\n", version.Version)
		return
	}
	if *socket == "" {
		usageError("XDG_RUNTIME_DIR is not set; pass --socket")
	}
	if *serviceAddr == "" {
		usageError("--service-addr is required")
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*socket, *serviceAddr, *serviceTimeout); err != nil {
		slog.Error("jjw-daemon failed", "err", err)
		os.Exit(1)
	}
}

func defaultSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "jjw", "daemon.sock")
}

func usageError(msg string) {
	fmt.Fprintf(os.Stderr, "jjw-daemon: %s\n", msg)
	flag.Usage()
	os.Exit(2)
}

func run(socket, serviceAddr string, serviceTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := serviceclient.New(serviceAddr, serviceTimeout)
	if err != nil {
		return err
	}
	defer svc.Close()

	lis, err := daemon.Listen(socket)
	if err != nil {
		return err
	}
	// The e2e harness reads this line to know the daemon is ready.
	fmt.Printf("listening on %s\n", socket)

	srv := daemon.NewServer(svc)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	srv.Shutdown(shutdownTimeout)
	return <-served
}
