//go:build windows

package daemon

import "os/exec"

// detach leaves cmd as it is on Windows: the worker has no console for ssh to prompt on. The
// context's end kills git only; a process it started may outlive it, and gitWaitDelay stops the
// fetch from waiting on its pipes.
// ponytail: no job object; add one if a helper left behind on Windows ever matters.
func detach(*exec.Cmd) {}
