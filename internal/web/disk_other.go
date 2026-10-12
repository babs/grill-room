//go:build !linux && !darwin

package web

import "errors"

// diskSpace is unknown off Linux and macOS: the room server only ships for those.
func diskSpace(string) (free, total int64, err error) {
	return 0, 0, errors.New("disk space: unsupported platform")
}
