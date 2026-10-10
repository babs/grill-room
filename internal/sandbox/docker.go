// Package sandbox runs Claude Code in a Docker container that holds no credential and whose only
// route out is the daemon's bearer-swap proxy (D11, D21, D22).
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Runner runs the docker CLI with args, feeding stdin, and returns its stdout. Injected for tests.
type Runner func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)

// ErrNoDocker is returned when the docker CLI is not installed.
var ErrNoDocker = errors.New("docker CLI not found")

// Docker is the real Runner. Errors carry the end of stderr: the docker CLI explains itself there.
func Docker(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // argv, no shell: args cannot inject commands
	cmd.Stdin = stdin
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrNoDocker
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = "..." + msg[len(msg)-500:]
		}
		return stdout.Bytes(), fmt.Errorf("docker %s: %w: %s", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// Readiness is whether this machine can host the agent sandbox.
type Readiness struct {
	OK bool `json:"ok"`
	// Reason says what is missing when OK is false.
	Reason string `json:"reason,omitempty"`
	// Desktop is Docker Desktop: its bridges live in a VM, so the proxy listens on loopback.
	Desktop bool   `json:"desktop"`
	Version string `json:"version,omitempty"`
}

// Check asks the Docker daemon for its version.
func Check(ctx context.Context, run Runner) Readiness {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := run(ctx, nil, "info", "--format", "{{.ServerVersion}}|{{.OperatingSystem}}")
	switch {
	case errors.Is(err, ErrNoDocker):
		return Readiness{Reason: "Docker is not installed"}
	case err != nil:
		return Readiness{Reason: "Docker is not running: " + err.Error()}
	}
	version, osName, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	if version == "" {
		// docker info prints client info and exits 0 on some versions when the daemon is down.
		return Readiness{Reason: "Docker is not running"}
	}
	return Readiness{OK: true, Version: version, Desktop: strings.Contains(osName, "Docker Desktop")}
}
