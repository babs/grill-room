//go:build !windows

package devicestt

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockDir waits for an exclusive lock on path; the kernel drops it when the process dies.
func lockDir(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the operator's directory
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { //nolint:gosec // fd fits an int
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
