// Package sourceapitest runs a source-api server for tests: a server on a data dir, a client for
// each service it serves, and the stores it serves from.
package sourceapitest

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	sourceapiv1 "github.com/smukherj1/jj-workspaces/gen-go/jjc/sourceapi/v1"
	"github.com/smukherj1/jj-workspaces/lib-go/objstore"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi"
	"github.com/smukherj1/jj-workspaces/lib-go/sourceapi/metastore"
)

// shutdownTimeout bounds how long Close waits for in-flight RPCs before stopping the server.
const shutdownTimeout = time.Second

// Harness is a source-api server on a data dir, connected to by one client per service. A Harness
// isn't safe for concurrent use, but its clients are.
type Harness struct {
	dataDir string

	server    *sourceapi.Server
	served    chan error // Receives Serve's result once the server stops.
	conn      *grpc.ClientConn
	metaStore *metastore.Store
	objStore  *objstore.Store
	running   bool
}

// Start serves source-api on dataDir, creating its stores there if needed, and connects to it. The
// caller must Close the harness.
func Start(dataDir string) (*Harness, error) {
	h := &Harness{dataDir: dataDir}
	if err := h.start(); err != nil {
		return nil, fmt.Errorf("starting source-api on %s: %w", dataDir, err)
	}
	return h, nil
}

// Restart stops the server and starts a new one on the same data dir, so everything it serves
// afterwards comes back from disk. Clients and stores from before the restart are closed.
func (h *Harness) Restart() error {
	if err := h.Close(); err != nil {
		return fmt.Errorf("stopping source-api for a restart: %w", err)
	}
	if err := h.start(); err != nil {
		return fmt.Errorf("restarting source-api on %s: %w", h.dataDir, err)
	}
	return nil
}

// Close closes the client connection, shuts the server down and closes its metadata store. It
// returns an error if the server stopped serving with one. Closing a closed harness does nothing.
func (h *Harness) Close() error {
	if !h.running {
		return nil
	}
	h.running = false
	var errs []error
	if err := h.conn.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing the client connection: %w", err))
	}
	h.server.Shutdown(shutdownTimeout)
	if err := <-h.served; err != nil {
		errs = append(errs, fmt.Errorf("serving source-api: %w", err))
	}
	if err := h.metaStore.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing the metadata store: %w", err))
	}
	return errors.Join(errs...)
}

// Server returns the server under test.
func (h *Harness) Server() *sourceapi.Server { return h.server }

// MetaStore returns the metadata store the server serves from.
func (h *Harness) MetaStore() *metastore.Store { return h.metaStore }

// ObjStore returns the object store the server serves from.
func (h *Harness) ObjStore() *objstore.Store { return h.objStore }

// Objects returns a client of the Objects service.
func (h *Harness) Objects() sourceapiv1.ObjectsClient { return sourceapiv1.NewObjectsClient(h.conn) }

// Repos returns a client of the Repo service.
func (h *Harness) Repos() sourceapiv1.RepoClient { return sourceapiv1.NewRepoClient(h.conn) }

// Workspaces returns a client of the Workspace service.
func (h *Harness) Workspaces() sourceapiv1.WorkspaceClient {
	return sourceapiv1.NewWorkspaceClient(h.conn)
}

// OpHeads returns a client of the OpHeads service.
func (h *Harness) OpHeads() sourceapiv1.OpHeadsClient { return sourceapiv1.NewOpHeadsClient(h.conn) }

// Trunk returns a client of the Trunk service.
func (h *Harness) Trunk() sourceapiv1.TrunkClient { return sourceapiv1.NewTrunkClient(h.conn) }

// Health returns a client of the gRPC health service.
func (h *Harness) Health() healthpb.HealthClient { return healthpb.NewHealthClient(h.conn) }

// start opens the stores in h.dataDir, serves source-api from them on a free port and connects to
// it. On an error, it closes whatever it opened.
func (h *Harness) start() error {
	objStore, err := objstore.Open(filepath.Join(h.dataDir, "objects"))
	if err != nil {
		return fmt.Errorf("opening the object store: %w", err)
	}
	metaStore, err := metastore.Open(filepath.Join(h.dataDir, "meta.db"))
	if err != nil {
		return fmt.Errorf("opening the metadata store: %w", err)
	}
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		metaStore.Close()
		return fmt.Errorf("listening on a free port: %w", err)
	}
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(sourceapi.MaxMessageSize),
			grpc.MaxCallSendMsgSize(sourceapi.MaxMessageSize)))
	if err != nil {
		lis.Close()
		metaStore.Close()
		return fmt.Errorf("creating a client for %s: %w", lis.Addr(), err)
	}
	server := sourceapi.NewServer(metaStore, objStore)
	served := make(chan error, 1)
	go func() { served <- server.Serve(lis) }()

	h.server, h.served, h.conn = server, served, conn
	h.metaStore, h.objStore = metaStore, objStore
	h.running = true
	return nil
}
