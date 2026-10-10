//go:build darwin

package creds

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

var platformKeychain KeychainFunc = securityKeychain

// securityKeychain reads the item with the system's security tool, which only reads.
func securityKeychain(ctx context.Context, service, account string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w") //nolint:gosec // argv, no shell
	// A killed security process can leave a pipe open; WaitDelay bounds the wait for it.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		// Only the exit status: on success stdout holds the token, so never quote output.
		return nil, fmt.Errorf("security find-generic-password -s %q: %w", service, err)
	}
	return out, nil
}
