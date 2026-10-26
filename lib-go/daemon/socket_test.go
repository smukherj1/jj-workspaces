package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenCreatesPrivateDirAndSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jjw")
	path := filepath.Join(dir, "d.sock")
	lis, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %v, want %v", p, got, want)
		}
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}

	lis, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	lis.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Close: %v", err)
	}
}

func TestListenRefusesWhenDaemonRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.sock")
	lis, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	_, err = Listen(path)
	if err == nil || err.Error() != "jjw-daemon is already running on "+path {
		t.Errorf("second Listen = %v, want already running", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("first daemon's socket was removed: %v", err)
	}
}

func TestListenRefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("Listen over a regular file = %v, want not a socket", err)
	}
}
