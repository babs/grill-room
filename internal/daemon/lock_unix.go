//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive lock on path, or fails with ErrLocked while another process holds
// it. The kernel drops the lock when the process dies, so a crash never leaves a stale one.
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // our own state dir
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { //nolint:gosec // fd fits an int
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
