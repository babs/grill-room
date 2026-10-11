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
	"path/filepath"
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
			if got := d.Status().Update.Available; got != tt.want {
				t.Fatalf("available = %q, want %q", got, tt.want)
			}
		})
	}
}

// updateDaemon is a test daemon whose "binary" is a temp file, paired with srv's release.
func updateDaemon(t *testing.T, srv *httptest.Server) (*Daemon, string) {
	t.Helper()
	d := newTestDaemon(t, dockerUp)
	d.o.Version = "v1.0.0"
	exe := filepath.Join(t.TempDir(), "grill-room-daemon")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil { //nolint:gosec // an executable fake
		t.Fatal(err)
	}
	d.o.Executable = exe
	(&roomRun{d: d, room: Room{Base: srv.URL, ID: "abc"}}).checkServer(context.Background())
	if d.Status().Update.Available != "v1.1.0" {
		t.Fatalf("no update offered: %+v", d.Status().Update)
	}
	return d, exe
}

func assertOnlyBinary(t *testing.T, exe string, want string) {
	t.Helper()
	b, err := os.ReadFile(exe) //nolint:gosec // test temp file
	if err != nil || string(b) != want {
		t.Fatalf("binary = %q, %v; want %q", b, err, want)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// 001-AC37: approving in the local page replaces the binary with the room server's, checked
// against its published sha256, and restarts the daemon.
func TestUpdateReplacesBinaryAndRestarts(t *testing.T) {
	newBin := []byte("new binary v1.1.0")
	srv := fakeRoomRelease(t, "v1.1.0", hexSHA(newBin), newBin, "")
	d, exe := updateDaemon(t, srv)
	var restarted atomic.Bool
	d.o.Restart = func() { restarted.Store(true) }
	e := newAdminEnv(t, d)
	tok := e.login(t)
	if s, body := e.req(t, "POST", "/api/update", tok, "", nil); s != http.StatusNoContent {
		t.Fatalf("update: %d %s", s, body)
	}
	assertOnlyBinary(t, exe, string(newBin))
	if st, _ := os.Stat(exe); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode())
	}
	if !restarted.Load() {
		t.Fatal("no restart")
	}
}

// 001-AC37: a download whose sha256 differs from the published one is refused, the current
// binary stays, and nothing restarts.
func TestUpdateRefusesTamperedBinary(t *testing.T) {
	newBin := []byte("new binary v1.1.0")
	for _, tt := range []struct {
		name string
		srv  *httptest.Server
		want string
	}{
		{"tampered", fakeRoomRelease(t, "v1.1.0", hexSHA(newBin), []byte("evil binary"), ""), "sha256 mismatch"},
		{"truncated", fakeRoomRelease(t, "v1.1.0", hexSHA(newBin), newBin[:5], ""), "sha256 mismatch"},
		{"published digest wrong", fakeRoomRelease(t, "v1.1.0", strings.Repeat("0", 64), newBin, ""), "sha256 mismatch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, exe := updateDaemon(t, tt.srv)
			var restarted atomic.Bool
			d.o.Restart = func() { restarted.Store(true) }
			e := newAdminEnv(t, d)
			s, body := e.req(t, "POST", "/api/update", e.login(t), "", nil)
			if s != http.StatusBadGateway || !strings.Contains(body, tt.want) {
				t.Fatalf("update: %d %s", s, body)
			}
			assertOnlyBinary(t, exe, "old binary")
			if restarted.Load() || !strings.Contains(d.Status().Update.Error, tt.want) || d.Status().Update.Running {
				t.Fatalf("restarted %v, status %+v", restarted.Load(), d.Status().Update)
			}
		})
	}
}

func TestUpdateFailures(t *testing.T) {
	newBin := []byte("new binary")
	t.Run("nothing to update", func(t *testing.T) {
		d := newTestDaemon(t, dockerUp)
		e := newAdminEnv(t, d)
		if s, _ := e.req(t, "POST", "/api/update", e.login(t), "", nil); s != http.StatusConflict {
			t.Fatalf("update: %d", s)
		}
	})
	t.Run("download missing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/daemon/version" {
				_ = json.NewEncoder(w).Encode(map[string]any{"version": "v1.1.0", "sha256": map[string]string{target: hexSHA(newBin)}})
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		d, exe := updateDaemon(t, srv)
		if err := d.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err = %v", err)
		}
		assertOnlyBinary(t, exe, "old binary")
	})
	t.Run("binary dir not writable", func(t *testing.T) {
		d, _ := updateDaemon(t, fakeRoomRelease(t, "v1.1.0", hexSHA(newBin), newBin, ""))
		d.o.Executable = filepath.Join(t.TempDir(), "missing-dir", "grill-room-daemon")
		if err := d.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot write next to") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("own binary unknown", func(t *testing.T) {
		d, _ := updateDaemon(t, fakeRoomRelease(t, "v1.1.0", hexSHA(newBin), newBin, ""))
		d.o.Executable = ""
		if err := d.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot locate") {
			t.Fatalf("err = %v", err)
		}
	})
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
