//go:build unix

package daemon

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a session of its own: with no controlling terminal, ssh cannot ask for a
// passphrase or a host key on the terminal the daemon started from, and fails instead. Cancel kills
// the whole process group, git and what it started (ssh, a credential or remote helper), so none
// outlives the fetch's deadline.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
