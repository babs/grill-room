package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/store/storetest"
	"github.com/babs/grill-room/internal/trust/trusttest"
	"github.com/babs/grill-room/internal/web"
)

// releaseServer is a room server handing out the daemon release in a directory; swap changes it,
// as a redeploy would.
type releaseServer struct {
	*roomServer
	st *store.Store
	h  atomic.Pointer[http.Handler]
}

func newReleaseServer(t *testing.T, dir string) *releaseServer {
	t.Helper()
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rs := &releaseServer{roomServer: &roomServer{admin: web.NewToken()}, st: st}
	rs.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*rs.h.Load()).ServeHTTP(w, r) }))
	t.Cleanup(rs.ts.Close)
	rs.swap(t, dir)
	var room store.Room
	rs.call(t, "POST", "/api/rooms", rs.admin, map[string]string{"name": "R"}, http.StatusCreated, &room)
	rs.room = room.ID
	return rs
}

func (rs *releaseServer) swap(t *testing.T, dir string) {
	t.Helper()
	rel, err := web.LoadRelease(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := web.New(bus.New(rs.st), web.HashToken(rs.admin), rs.ts.URL,
		web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20, MaxAttachmentBytes: 1 << 20, Release: rel}, zap.NewNop()).Handler()
	rs.h.Store(&h)
}

// fakeRelease writes a release at version whose binary for this platform is bin and whose bundle,
// when set, is bundle; the other platforms get placeholders.
func fakeRelease(t *testing.T, version string, bin, bundle []byte) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range web.DaemonTargets {
		f, b := web.DaemonFile(target), []byte("placeholder")
		if target == runtime.GOOS+"-"+runtime.GOARCH {
			b = bin
			if bundle != nil {
				write(f+".sigstore.json", bundle)
			}
		}
		sum := sha256.Sum256(b)
		write(f, b)
		write(f+".sha256", []byte(hex.EncodeToString(sum[:])+"  "+f+"\n"))
	}
	write("VERSION", []byte(version+"\n"))
	return dir
}

// devBuild builds this daemon at version and signs it with key, as `make dev-release` does.
func devBuild(t *testing.T, version string, key *trusttest.Key) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "daemon")
	out, err := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.Version="+version, "-o", exe, ".").CombinedOutput() //nolint:gosec,noctx // a fixed build of this package
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	bin, err := os.ReadFile(exe) //nolint:gosec // test build
	if err != nil {
		t.Fatal(err)
	}
	return fakeRelease(t, version, bin, key.Sign(t, bin))
}

// 003-AC2, 003-AC7, 003-AC4: one supervisor, one instance per real GitHub release in
// GRILL_ROOM_TEST_RELEASES (space-separated directories, as `make test-release` downloads them)
// plus one serving a dev-signed local build. Each release from v0.0.3 runs as its own worker,
// verified keyless as a project release; an older one is refused as too old; the dev build runs
// with its key trusted. Each worker runs from bin/<version>/, and when an instance moves to the
// supervisor's own version, its stored copy goes.
func TestReleases(t *testing.T) {
	dirs := strings.Fields(os.Getenv("GRILL_ROOM_TEST_RELEASES"))
	if len(dirs) == 0 {
		t.Skip("run by make test-release: it downloads real releases")
	}
	key := trusttest.NewKey(t)
	dev := newReleaseServer(t, devBuild(t, "v0.0.9-dev", key))
	env := daemonEnv(t)
	state := strings.TrimPrefix(env[0], "GRILL_ROOM_DAEMON_STATE_DIR=")
	if err := os.MkdirAll(filepath.Join(state, "dev-keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "dev-keys", "dev.pub"), key.PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{dev.ts.URL: "v0.0.9-dev"}
	servers := []*releaseServer{dev}
	for _, d := range dirs {
		v, err := os.ReadFile(filepath.Join(d, "VERSION")) //nolint:gosec // test input
		if err != nil {
			t.Fatal(err)
		}
		rs := newReleaseServer(t, d)
		want[rs.ts.URL] = strings.TrimSpace(string(v))
		servers = append(servers, rs)
	}
	env = append(env, "GRILL_ROOM_GITHUB_URL=http://127.0.0.1:9")
	sup := startProc(t, env, dev.url())
	pg := sup.open(t)
	for _, rs := range servers[1:] {
		if code := startProc(t, env, rs.url()).exit(t); code != 0 {
			t.Fatalf("handoff exit %d", code)
		}
	}
	byURL := func() map[string]daemon.InstanceView {
		m := map[string]daemon.InstanceView{}
		for _, in := range pg.status(t).Instances {
			m[in.URL] = in
		}
		return m
	}
	settled := func(in daemon.InstanceView, v string) bool {
		if semver.Compare(v, "v0.0.3") < 0 && !strings.HasSuffix(v, "-dev") {
			return in.State == "refused"
		}
		return in.State == "running" && in.Version == v
	}
	waitFor(t, "every instance settled", func() bool {
		m := byURL()
		for u, v := range want {
			if !settled(m[u], v) {
				return false
			}
		}
		return true
	})
	file := daemonFile()
	for u, v := range want {
		in := byURL()[u]
		t.Logf("%s: %s %s, trust %q, error %q", v, in.State, in.Version, in.Trust, in.Error)
		switch {
		case in.State == "refused":
			if !strings.Contains(in.Error, v+", older than v0.0.3") {
				t.Errorf("%s refused with %q", v, in.Error)
			}
			continue
		case v == "v0.0.9-dev":
			if in.Trust != "signed with the dev key dev" {
				t.Errorf("dev build trust %q", in.Trust)
			}
		default:
			if in.Trust != "signed by the project (https://github.com/babs/grill-room/.github/workflows/release.yml@refs/tags/"+v+")" {
				t.Errorf("%s trust %q", v, in.Trust)
			}
		}
		path := filepath.Join(state, "bin", v, file)
		if _, err := os.Stat(path + ".sigstore.json"); err != nil {
			t.Errorf("%s: %v", v, err)
		}
		if runtime.GOOS == "linux" {
			if exe, err := os.Readlink("/proc/" + strconv.Itoa(in.PID) + "/exe"); err != nil || exe != path {
				t.Errorf("%s worker runs %q (%v), want %s", v, exe, err, path)
			}
		}
	}
	// The dev instance moves to the supervisor's own version: its worker restarts on the
	// supervisor's binary, and the stored v0.0.9-dev goes.
	dev.swap(t, fakeRelease(t, Version, []byte("never downloaded"), nil))
	pid := byURL()[dev.ts.URL].PID
	p, _ := os.FindProcess(pid)
	_ = p.Kill()
	waitFor(t, "the dev instance on the supervisor's binary", func() bool {
		in := byURL()[dev.ts.URL]
		return in.State == "running" && in.PID != pid && in.Version == Version
	})
	if _, err := os.Stat(filepath.Join(state, "bin", "v0.0.9-dev")); !os.IsNotExist(err) {
		t.Fatalf("v0.0.9-dev still stored: %v", err)
	}
	for u, v := range want {
		if u != dev.ts.URL && byURL()[u].State == "running" {
			if _, err := os.Stat(filepath.Join(state, "bin", v, file)); err != nil {
				t.Errorf("%s removed while in use: %v", v, err)
			}
		}
	}
	if pg.post(t, "/api/stop", nil) != http.StatusNoContent {
		t.Fatal("stop refused")
	}
	if code := sup.exit(t); code != 0 {
		t.Fatalf("supervisor exit %d", code)
	}
}

func daemonFile() string { return web.DaemonFile(runtime.GOOS + "-" + runtime.GOARCH) }
