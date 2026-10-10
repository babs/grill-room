package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed" // the sandbox Dockerfile
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Dockerfile is the sandbox image recipe. It needs no build context, so a daemon can build it from
// this copy when the room server advertises no image or its pull fails.
//
//go:embed Dockerfile
var Dockerfile []byte

// LocalImage is the tag of the image built from Dockerfile, named after its content: a daemon
// whose Dockerfile changed builds again, one whose did not reuses the image.
func LocalImage() string {
	sum := sha256.Sum256(Dockerfile)
	return "grill-room-sandbox:local-" + hex.EncodeToString(sum[:6])
}

// Streamer runs the docker CLI with args, feeding stdin, and hands each output line to line.
// Injected for tests.
type Streamer func(ctx context.Context, stdin io.Reader, line func(string), args ...string) error

// DockerStream is the real Streamer; stdout and stderr are merged, as docker splits progress
// between them.
func DockerStream(ctx context.Context, stdin io.Reader, line func(string), args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // argv, no shell: args cannot inject commands
	cmd.Stdin = stdin
	cmd.WaitDelay = 2 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	last := ""
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" {
				last = l
				line(l)
			}
		}
		// A line past the buffer stops the scanner: drain, or docker blocks on a full pipe.
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := cmd.Run()
	_ = pw.Close()
	<-done
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return ErrNoDocker
		}
		if len(last) > 500 {
			last = "..." + last[len(last)-500:]
		}
		return fmt.Errorf("docker %s: %w: %s", args[0], err, last)
	}
	return nil
}

// Image progress states handed to EnsureImage's progress callback.
const (
	StatePulling  = "pulling"
	StateBuilding = "building"
)

// EnsureImage returns an image turns can run in. want, a digest-pinned reference the room server
// advertises, is used when present or pulled; otherwise, or when the pull fails, the image is
// built from Dockerfile. progress receives the state and each line docker prints.
func EnsureImage(ctx context.Context, run Runner, stream Streamer, want string, progress func(state, line string)) (string, error) {
	present := func(ref string) bool {
		_, err := run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", ref)
		return err == nil
	}
	var pullErr error
	switch {
	case want == "":
	case !PinnedImage(want):
		pullErr = fmt.Errorf("advertised image %q is not pinned by digest", want)
	case present(want):
		return want, nil
	default:
		progress(StatePulling, "docker pull "+want)
		if pullErr = stream(ctx, nil, func(l string) { progress(StatePulling, l) }, "pull", want); pullErr == nil {
			return want, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	local := LocalImage()
	if present(local) {
		return local, nil
	}
	if pullErr != nil {
		progress(StateBuilding, "falling back to a local build: "+pullErr.Error())
	}
	progress(StateBuilding, "docker build -t "+local)
	if err := stream(ctx, bytes.NewReader(Dockerfile), func(l string) { progress(StateBuilding, l) },
		"build", "--progress=plain", "-t", local, "-"); err != nil {
		if pullErr != nil {
			return "", fmt.Errorf("pull: %w; local build: %w", pullErr, err)
		}
		return "", fmt.Errorf("local build: %w", err)
	}
	return local, nil
}
