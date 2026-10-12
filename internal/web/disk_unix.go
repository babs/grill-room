//go:build linux || darwin

package web

import (
	"fmt"
	"syscall"
)

// diskSpace returns the free and total bytes of the filesystem holding dir.
func diskSpace(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	// Bsize is int64 on Linux and uint32 on macOS; block counts fit an int64 on any real disk.
	bs := int64(st.Bsize) //nolint:unconvert // not a no-op on darwin

	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil //nolint:gosec // see above
}
