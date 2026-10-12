package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/babs/grill-room/internal/sandbox"
)

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeRoomRelease is a room server handing out version, with sum published for this platform
// and body served as the binary.
func fakeRoomRelease(t *testing.T, version, sum string, body []byte, image string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemon/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version, "sha256": map[string]string{target: sum}, "sandbox_image": image})
		case "/download/" + releaseFile(runtime.GOOS, runtime.GOARCH):
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 001-AC37: the daemon offers an update only for a newer, valid version built for its platform.
func TestUseServerOffersNewerVersionOnly(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, tt := range []struct {
		name, version string
		sums          map[string]string
		want          string
	}{
		{"newer", "v1.1.0", map[string]string{target: sum}, "v1.1.0"},
		{"same", "v1.0.0", map[string]string{target: sum}, ""},
		{"older", "v0.9.0", map[string]string{target: sum}, ""},
		{"not semver", "latest", map[string]string{target: sum}, ""},
		{"no build for this platform", "v1.1.0", map[string]string{"plan9-mips": sum}, ""},
		{"no release", "", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDaemon(t, dockerUp)
			d.o.Version = "v1.0.0"
			d.useServer(context.Background(), "https://rooms.example", serverInfo{Version: tt.version, SHA256: tt.sums})
			if got := d.Status().Update; got.Available != tt.want || got.Served != tt.version {
				t.Fatalf("update = %+v, want available %q, served %q", got, tt.want, tt.version)
			}
		})
	}
}

// imageDaemon has no pinned image: it takes the room server's, through a fake docker.
func imageDaemon(t *testing.T, run sandbox.Runner, stream sandbox.Streamer) *Daemon {
	t.Helper()
	d := newTestDaemon(t, run)
	d.o.Image = ""
	d.st.Image, d.img = Image{}, imageJob{}
	d.o.Stream = stream
	return d
}

const pinnedImage = "registry.example/grill-room/sandbox:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The room server's sandbox image is pulled before the daemon offers to host; the local page shows
// the pull, and a daemon that cannot get any image does not offer to host.
func TestSandboxImageFromRoomServer(t *testing.T) {
	pulls := atomic.Int32{}
	release := make(chan struct{})
	stream := func(_ context.Context, _ io.Reader, line func(string), args ...string) error {
		pulls.Add(1)
		line("layer 1: downloading")
		<-release
		return nil
	}
	missing := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "image" {
			return nil, errors.New("No such image")
		}
		return dockerUp(context.Background(), nil)
	}
	d := imageDaemon(t, missing, stream)
	d.Refresh(context.Background())
	if s := d.Status(); s.CanHost || s.Image.State != "" {
		t.Fatalf("before the room answered: %+v", s)
	}
	srv := fakeRoomRelease(t, "v1.0.0", "", nil, pinnedImage)
	(&roomRun{d: d, room: Room{Base: srv.URL, ID: "abc"}}).checkServer(context.Background())
	waitFor(t, "pull progress", func() bool {
		s := d.Status().Image
		return s.State == "pulling" && s.Progress == "layer 1: downloading"
	})
	if s := d.Status(); s.CanHost || !strings.Contains(s.Reason, "not ready") {
		t.Fatalf("while pulling: %+v", s)
	}
	// The same answer again, on a reconnect, does not start a second pull.
	(&roomRun{d: d, room: Room{Base: srv.URL, ID: "abc"}}).checkServer(context.Background())
	close(release)
	waitFor(t, "image ready", func() bool { return d.Status().Image.State == "ready" })
	if s := d.Status(); !s.CanHost || s.Image.Ref != pinnedImage || pulls.Load() != 1 {
		t.Fatalf("after the pull: %+v, %d pulls", s, pulls.Load())
	}
	d.Wait()
}

func TestSandboxImageWaitsForDockerAndRetries(t *testing.T) {
	var up, fail atomic.Bool
	run := func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if !up.Load() {
			return dockerDown(ctx, stdin, args...)
		}
		if args[0] == "image" {
			return nil, errors.New("No such image")
		}
		return dockerUp(ctx, stdin)
	}
	builds := atomic.Int32{}
	stream := func(_ context.Context, _ io.Reader, _ func(string), args ...string) error {
		builds.Add(1)
		if fail.Load() {
			return errors.New("no network")
		}
		return nil
	}
	d := imageDaemon(t, run, stream)
	d.useServer(context.Background(), "https://rooms.example", serverInfo{})
	d.Refresh(context.Background())
	if s := d.Status().Image; s.State != "pending" || builds.Load() != 0 {
		t.Fatalf("docker down: %+v", s)
	}
	up.Store(true)
	fail.Store(true)
	d.Refresh(context.Background())
	waitFor(t, "build failed", func() bool { return d.Status().Image.State == "failed" })
	if s := d.Status(); s.CanHost || !strings.Contains(s.Reason, "no network") {
		t.Fatalf("failed build: %+v", s)
	}
	// A periodic check does not retry a failed build; "Check again" does.
	d.Refresh(context.Background())
	if builds.Load() != 1 {
		t.Fatalf("builds = %d", builds.Load())
	}
	fail.Store(false)
	d.Recheck(context.Background())
	waitFor(t, "image ready", func() bool { return d.Status().Image.State == "ready" })
	if s := d.Status(); !s.CanHost || s.Image.Ref != sandbox.LocalImage() {
		t.Fatalf("after retry: %+v", s)
	}
	d.Wait()
}

func TestSandboxImageCancelledWithRoom(t *testing.T) {
	missing := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "image" {
			return nil, errors.New("No such image")
		}
		return dockerUp(context.Background(), nil)
	}
	var pulls atomic.Int32
	stream := func(ctx context.Context, _ io.Reader, _ func(string), _ ...string) error {
		pulls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
	d := imageDaemon(t, missing, stream)
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	d.useServer(ctx, "https://rooms.example", serverInfo{SandboxImage: pinnedImage})
	waitFor(t, "pulling", func() bool { return d.Status().Image.State == "pulling" })
	// The next room run, on the same server, takes the job over at once.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	d.useServer(ctx2, "https://rooms.example", serverInfo{SandboxImage: pinnedImage})
	cancel()
	waitFor(t, "a second pull", func() bool { return pulls.Load() == 2 })
	cancel2()
	waitFor(t, "back to pending", func() bool { return d.Status().Image.State == "pending" })
	d.Wait()
}

func TestTurnWithoutImageFails(t *testing.T) {
	d := imageDaemon(t, dockerUp, nil)
	r := &roomRun{d: d}
	if _, err := r.agent(); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("agent() err = %v", err)
	}
}

// The room server names the windows builds with .exe (internal/web.DaemonFile).
func TestReleaseFile(t *testing.T) {
	for _, tt := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "grill-room-daemon-linux-amd64"},
		{"darwin", "arm64", "grill-room-daemon-darwin-arm64"},
		{"windows", "arm64", "grill-room-daemon-windows-arm64.exe"},
	} {
		if got := releaseFile(tt.goos, tt.goarch); got != tt.want {
			t.Errorf("releaseFile(%s, %s) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}

// assertPerm checks path's Unix mode. Windows has no mode bits: Go reports 0666 or 0444 there.
func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != want {
		t.Fatalf("%s: mode %v, want %v", path, fi.Mode().Perm(), want)
	}
}
