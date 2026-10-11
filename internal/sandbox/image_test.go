package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

const pinned = "registry.example/grill-room/sandbox:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeImages is a Docker that holds the images in have and answers pulls and builds per its errors.
type fakeImages struct {
	have     map[string]bool
	pullErr  error
	buildErr error
	calls    []string
	built    []byte
}

func (f *fakeImages) run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	if args[0] == "image" && f.have[args[len(args)-1]] {
		return []byte("sha256:x"), nil
	}
	return nil, errors.New("No such image")
}

func (f *fakeImages) stream(_ context.Context, stdin io.Reader, line func(string), args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	line("step 1/2")
	switch args[0] {
	case "pull":
		return f.pullErr
	case "build":
		f.built, _ = io.ReadAll(stdin)
		return f.buildErr
	}
	return nil
}

// The sandbox image comes from the room server's pinned reference, or from the
// Dockerfile the daemon embeds; the local page shows progress for both.
func TestEnsureImage(t *testing.T) {
	local := LocalImage()
	tests := []struct {
		name      string
		want      string
		img       fakeImages
		ref       string
		calls     []string
		states    string
		errSubstr string
	}{
		{name: "pinned and present", want: pinned, img: fakeImages{have: map[string]bool{pinned: true}}, ref: pinned},
		{name: "pinned, pulled", want: pinned, img: fakeImages{}, ref: pinned, calls: []string{"pull " + pinned}, states: "pulling"},
		{name: "pull fails, local build", want: pinned, img: fakeImages{pullErr: errors.New("unauthorized")}, ref: local,
			calls: []string{"pull " + pinned, "build --progress=plain -t " + local + " -"}, states: "pulling,building"},
		{name: "pull fails, local image present", want: pinned, img: fakeImages{pullErr: errors.New("unauthorized"), have: map[string]bool{local: true}}, ref: local,
			calls: []string{"pull " + pinned}, states: "pulling"},
		{name: "not pinned: never pulled", want: "registry.example/grill-room/sandbox:v1", img: fakeImages{}, ref: local,
			calls: []string{"build --progress=plain -t " + local + " -"}, states: "building"},
		{name: "flag-shaped: never passed to docker", want: "--privileged@sha256:" + strings.Repeat("a", 64), img: fakeImages{}, ref: local,
			calls: []string{"build --progress=plain -t " + local + " -"}, states: "building"},
		{name: "nothing advertised, built", img: fakeImages{}, ref: local, calls: []string{"build --progress=plain -t " + local + " -"}, states: "building"},
		{name: "nothing advertised, local present", img: fakeImages{have: map[string]bool{local: true}}, ref: local},
		{name: "build fails", img: fakeImages{buildErr: errors.New("no network")}, errSubstr: "local build: no network",
			calls: []string{"build --progress=plain -t " + local + " -"}, states: "building"},
		{name: "pull and build fail", want: pinned, img: fakeImages{pullErr: errors.New("unauthorized"), buildErr: errors.New("no network")},
			errSubstr: "pull: unauthorized; local build: no network", calls: []string{"pull " + pinned, "build --progress=plain -t " + local + " -"}, states: "pulling,building"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var states []string
			ref, err := EnsureImage(context.Background(), tt.img.run, tt.img.stream, tt.want, func(state, _ string) {
				if len(states) == 0 || states[len(states)-1] != state {
					states = append(states, state)
				}
			})
			if tt.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("err = %v, want %q", err, tt.errSubstr)
				}
			} else if err != nil || ref != tt.ref {
				t.Fatalf("EnsureImage = %q, %v; want %q", ref, err, tt.ref)
			}
			if strings.Join(tt.img.calls, "|") != strings.Join(tt.calls, "|") || strings.Join(states, ",") != tt.states {
				t.Fatalf("calls %q states %q, want %q %q", tt.img.calls, states, tt.calls, tt.states)
			}
			if tt.img.built != nil && string(tt.img.built) != string(Dockerfile) {
				t.Fatal("the build did not get the embedded Dockerfile")
			}
		})
	}
}

func TestEnsureImageCancelledPullDoesNotBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	img := fakeImages{}
	stream := func(ctx context.Context, stdin io.Reader, line func(string), args ...string) error {
		cancel()
		return img.stream(ctx, stdin, line, args...)
	}
	img.pullErr = context.Canceled
	if _, err := EnsureImage(ctx, img.run, stream, pinned, func(string, string) {}); !errors.Is(err, context.Canceled) || len(img.calls) != 1 {
		t.Fatalf("err %v calls %q", err, img.calls)
	}
}

func TestLocalImageFollowsDockerfile(t *testing.T) {
	a := LocalImage()
	saved := Dockerfile
	t.Cleanup(func() { Dockerfile = saved })
	Dockerfile = append([]byte("# changed\n"), saved...)
	if b := LocalImage(); a == b || !strings.HasPrefix(b, "grill-room-sandbox:local-") {
		t.Fatalf("LocalImage %q then %q", a, b)
	}
}

func TestDockerStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a sh script")
	}
	dir := t.TempDir()
	// A fake docker: prints its args and one line on stderr, fails when asked to.
	script := "#!/bin/sh\necho \"args $*\"\necho 'to stderr' >&2\n[ \"$1\" = fail ] && { echo 'boom happened' >&2; exit 3; }\nexit 0\n"
	if err := os.WriteFile(dir+"/docker", []byte(script), 0o755); err != nil { //nolint:gosec // an executable fake
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var lines []string
	if err := DockerStream(context.Background(), nil, func(l string) { lines = append(lines, l) }, "pull", "x"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "args pull x|to stderr" {
		t.Fatalf("lines %q", lines)
	}
	err := DockerStream(context.Background(), nil, func(string) {}, "fail")
	if err == nil || !strings.Contains(err.Error(), "boom happened") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := DockerStream(context.Background(), nil, func(string) {}, "pull"); !errors.Is(err, ErrNoDocker) {
		t.Fatalf("no docker: %v", err)
	}
}

// The daemon's fallback for real: the embedded Dockerfile builds with no context, and the
// result is found again without a rebuild.
func TestDockerEnsureImageBuildsEmbedded(t *testing.T) {
	dockerTest(t)
	ctx := context.Background()
	local := LocalImage()
	_, _ = Docker(ctx, nil, "image", "rm", local)
	var lines int
	ref, err := EnsureImage(ctx, Docker, DockerStream, "", func(string, string) { lines++ })
	if err != nil || ref != local || lines == 0 {
		t.Fatalf("EnsureImage = %q, %v (%d lines)", ref, err, lines)
	}
	lines = 0
	if ref, err := EnsureImage(ctx, Docker, DockerStream, "", func(string, string) { lines++ }); err != nil || ref != local || lines != 0 {
		t.Fatalf("second EnsureImage = %q, %v (%d lines)", ref, err, lines)
	}
	out, err := Docker(ctx, nil, "run", "--rm", "--entrypoint", "claude", local, "--version")
	if err != nil || !strings.Contains(string(out), "Claude Code") {
		t.Fatalf("claude in the built image: %q, %v", out, err)
	}
}
