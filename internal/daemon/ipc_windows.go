package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeName is per state dir, as the Unix socket is: two supervisors with their own state each
// get their own pipe.
func pipeName(stateDir string) string {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		abs = stateDir
	}
	sum := sha256.Sum256([]byte(abs))
	return `\\.\pipe\grill-room-` + hex.EncodeToString(sum[:6])
}

// listenLocal opens the supervisor's handoff pipe, reachable by this user only.
func listenLocal(_ context.Context, stateDir string) (net.Listener, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}
	// Security: the default pipe DACL lets every user read; this one admits only the owner.
	sddl := "D:P(A;;GA;;;" + user.User.Sid.String() + ")"
	ln, err := winio.ListenPipe(pipeName(stateDir), &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", pipeName(stateDir), err)
	}
	return ln, nil
}

// dialLocal connects to the supervisor's pipe. Security: pipe names are global, so another user
// may have created this one first to catch the room link; its server process must run as us.
func dialLocal(ctx context.Context, stateDir string) (net.Conn, error) {
	c, err := winio.DialPipeContext(ctx, pipeName(stateDir))
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err == nil {
		err = checkPipeServer(c, user.User.Sid)
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// checkPipeServer fails unless the process serving pipe c runs as want.
func checkPipeServer(c net.Conn, want *windows.SID) error {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("pipe: no handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return fmt.Errorf("pipe server process: %w", err)
	}
	return checkProcessOwner(pid, want)
}

// checkProcessOwner fails unless process pid runs as want, or when its owner cannot be read.
func checkProcessOwner(pid uint32, want *windows.SID) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("pipe %w: process %d unreadable: %w", errForeignPipe, pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("pipe %w: process %d unreadable: %w", errForeignPipe, pid, err)
	}
	defer func() { _ = tok.Close() }()
	owner, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("pipe server owner: %w", err)
	}
	if !owner.User.Sid.Equals(want) {
		return fmt.Errorf("pipe %w: process %d runs as %s", errForeignPipe, pid, owner.User.Sid)
	}
	return nil
}
