//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// alive reports whether pid still runs.
func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
