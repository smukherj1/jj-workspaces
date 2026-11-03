// Command source-api serves the source API to srcd daemons.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

const shutdownTimeout = 5 * time.Second

func main() {
	listen := flag.String("listen", "localhost:7470", "address to listen on; port 0 picks a free port")
	dataDir := flag.String("data-dir", "", "directory holding objects and meta.db (required)")
	printVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *printVersion {
		fmt.Printf("source-api %s\n", version.Version)
		return
	}

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "source-api: --data-dir is required")
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*listen, *dataDir); err != nil {
		slog.Error("source-api failed", "err", err)
		os.Exit(1)
	}
}

func run(listen, dataDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	objects, err := objstore.Open(filepath.Join(dataDir, "objects"))
	if err != nil {
		return err
	}
	meta, err := metastore.Open(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		return err
	}
	defer meta.Close()

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	// The e2e harness reads this line to find the resolved port.
	fmt.Printf("listening on %s\n", lis.Addr())

	srv := sourceapi.NewServer(meta, objects)
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
