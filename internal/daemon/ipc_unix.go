//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// socketPath sits in run/, kept owner-only: other users cannot reach the socket inside it.
func socketPath(stateDir string) string { return filepath.Join(stateDir, "run", "supervisor.sock") }

// listenLocal opens the supervisor's handoff socket. The caller holds the supervisor lock, so a
// socket file already there is a dead supervisor's.
func listenLocal(ctx context.Context, stateDir string) (net.Listener, error) {
	p := socketPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	// Security: MkdirAll keeps the mode of a directory that already exists.
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil { //nolint:gosec // a directory needs its search bit
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	_ = os.Remove(p) // a stale socket from a crash; absent is fine
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", p)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", p, err)
	}
	return ln, nil
}

func dialLocal(ctx context.Context, stateDir string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", socketPath(stateDir))
}
