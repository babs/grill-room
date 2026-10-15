package devicestt

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockDir waits for an exclusive lock on path; Windows drops it when the process dies.
func lockDir(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the operator's directory
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
