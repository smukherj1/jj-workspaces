// Command jjw-workspace-service serves the workspace service API to jjw daemons.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smukherj1/jj-workspaces/lib-go/service"
	"github.com/smukherj1/jj-workspaces/lib-go/version"
)

const shutdownTimeout = 5 * time.Second

func main() {
	listen := flag.String("listen", "localhost:7470", "address to listen on; port 0 picks a free port")
	printVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *printVersion {
		fmt.Printf("jjw-workspace-service %s\n", version.Version)
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*listen); err != nil {
		slog.Error("jjw-workspace-service failed", "err", err)
		os.Exit(1)
	}
}

func run(listen string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	// The e2e harness reads this line to find the resolved port.
	fmt.Printf("listening on %s\n", lis.Addr())

	srv := service.NewServer()
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
