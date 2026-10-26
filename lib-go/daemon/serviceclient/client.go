// Package serviceclient is the daemon's connection to jjw-workspace-service.
package serviceclient

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// Client calls the service at one address. Every call is bounded by the client's timeout.
type Client struct {
	addr    string
	timeout time.Duration
	conn    *grpc.ClientConn
	health  healthpb.HealthClient
}

// New returns a client for the service at addr. It doesn't connect: the connection is made on the
// first call, so the daemon starts even when the service is down.
func New(addr string, timeout time.Duration) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &Client{addr: addr, timeout: timeout, conn: conn, health: healthpb.NewHealthClient(conn)}, nil
}

// Addr returns the service address.
func (c *Client) Addr() string { return c.addr }

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Health returns the service's overall health status (service ""). An error means the service
// couldn't be reached or didn't answer within the timeout.
func (c *Client) Health(ctx context.Context) (healthpb.HealthCheckResponse_ServingStatus, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.health.Check(callCtx, &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		return healthpb.HealthCheckResponse_UNKNOWN, c.callError(ctx, err)
	}
	return resp.GetStatus(), nil
}

// callError turns an RPC error into a short message, naming the timeout when it was ours that fired
// rather than the caller's.
func (c *Client) callError(ctx context.Context, err error) error {
	st := status.Convert(err)
	if st.Code() == codes.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("deadline exceeded after %s", c.timeout)
	}
	return fmt.Errorf("%v: %s", st.Code(), st.Message())
}
