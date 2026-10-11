package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on path, or fails with ErrLocked while another process holds
// it. Windows drops the lock when the process dies, so a crash never leaves a stale one.
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // our own state dir
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
