package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const dialTimeout = time.Second

// Listen listens on the Unix socket at path, creating its parent directory with mode 0700 if needed.
// If a socket file is already there, Listen fails when a daemon answers on it and removes it when it
// is stale. The socket gets mode 0600. Closing the listener unlinks it.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close()
		return nil, err
	}
	return lis, nil
}

// removeStale removes the socket file at path if nothing is listening on it.
func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err == nil {
		conn.Close()
		return fmt.Errorf("jjw-daemon is already running on %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("checking for a running daemon on %s: %w", path, err)
	}
	return os.Remove(path)
}
