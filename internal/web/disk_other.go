//go:build !linux && !darwin

package web

import (
	"errors"
	"os"
)

// diskSpace is unknown off Linux and macOS: the room server only ships for those.
func diskSpace(string) (free, total int64, err error) {
	return 0, 0, errors.New("disk space: unsupported platform")
}

// fileOwner is unknown off Linux and macOS.
func fileOwner(os.FileInfo) (uid int, known bool) { return 0, false }
