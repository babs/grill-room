package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func dialLocal(ctx context.Context, stateDir string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipeName(stateDir))
}
