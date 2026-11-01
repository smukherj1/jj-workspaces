// Command srcd serves the jjc CLI on a Unix socket and talks to source-api.
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

	"github.com/smukherj1/jj-workspaces/lib-go/srcd"
	"github.com/smukherj1/jj-workspaces/lib-go/srcd/sourceapiclient"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

const shutdownTimeout = 5 * time.Second

func main() {
	socket := flag.String("socket", defaultSocket(), "Unix socket to listen on (default $XDG_RUNTIME_DIR/jjc/srcd.sock)")
	sourceAPIAddr := flag.String("source-api-addr", "", "host:port of source-api (required)")
	sourceAPITimeout := flag.Duration("source-api-timeout", 10*time.Second, "bound on every call to source-api")
	printVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *printVersion {
		fmt.Printf("srcd %s\n", version.Version)
		return
	}
	if *socket == "" {
		usageError("XDG_RUNTIME_DIR is not set; pass --socket")
	}
	if *sourceAPIAddr == "" {
		usageError("--source-api-addr is required")
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*socket, *sourceAPIAddr, *sourceAPITimeout); err != nil {
		slog.Error("srcd failed", "err", err)
		os.Exit(1)
	}
}

func defaultSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "jjc", "srcd.sock")
}

func usageError(msg string) {
	fmt.Fprintf(os.Stderr, "srcd: %s\n", msg)
	flag.Usage()
	os.Exit(2)
}

func run(socket, sourceAPIAddr string, sourceAPITimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sourceAPI, err := sourceapiclient.New(sourceAPIAddr, sourceAPITimeout)
	if err != nil {
		return err
	}
	defer sourceAPI.Close()

	lis, err := srcd.Listen(socket)
	if err != nil {
		return err
	}
	// The e2e harness reads this line to know the daemon is ready.
	fmt.Printf("listening on %s\n", socket)

	srv := srcd.NewServer(sourceAPI)
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
