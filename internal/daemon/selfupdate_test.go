package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/trust/trusttest"
)

// fakeGitHub publishes version as the latest release, with bin and its bundle (none when nil).
func fakeGitHub(t *testing.T, version string, bin, bundle []byte) *httptest.Server {
	t.Helper()
	file := releaseFile(runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest/download/VERSION":
			_, _ = w.Write([]byte(version + "\n"))
		case "/releases/download/" + version + "/" + file:
			_, _ = w.Write(bin)
		case "/releases/download/" + version + "/" + file + bundleExt:
			if bundle == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// updateSupervisor is a supervisor at v1.0.0 whose binary is a temp file, trusting key, with
// GitHub at gh.
func updateSupervisor(t *testing.T, gh string, key *trusttest.Key, set ...func(*SupervisorOptions)) (*Supervisor, string, *atomic.Bool) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "grill-room-daemon")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil { //nolint:gosec // an executable fake
		t.Fatal(err)
	}
	bins := testBins(t)
	if key != nil {
		if err := bins.Keys.Add("ci", key.PEM); err != nil {
			t.Fatal(err)
		}
	}
	var restarted atomic.Bool
	s := testSupervisor(t, strconv.Itoa(Proto), nil, append([]func(*SupervisorOptions){func(o *SupervisorOptions) {
		o.Version, o.Bins, o.Executable, o.GitHub = "v1.0.0", bins, exe, gh
		o.Restart = func() { restarted.Store(true) }
		// Checks run when the test asks.
		o.UpdateEvery = time.Hour
	}}, set...)...)
	return s, exe, &restarted
}

func assertBinary(t *testing.T, exe string, want []byte) {
	t.Helper()
	b, err := os.ReadFile(exe) //nolint:gosec // test temp file
	if err != nil || !bytes.Equal(b, want) {
		t.Fatalf("binary is %d bytes, %v; want %d bytes", len(b), err, len(want))
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".dl-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// 003-AC5, 001-AC37: on the default GitHub channel, a newer release is offered; approving it on the
// page replaces the binary once its bundle verifies, keeps the bundle beside it, and restarts.
func TestSelfUpdateFromGitHub(t *testing.T) {
	key := trusttest.NewKey(t)
	next := ownExe(t)
	gh := fakeGitHub(t, "v1.1.0", next, key.Sign(t, next))
	s, exe, restarted := updateSupervisor(t, gh.URL, key)
	s.checkUpdate(context.Background())
	if u := s.Status().Update; u.Available != "v1.1.0" || u.From != "GitHub" {
		t.Fatalf("offer %+v", u)
	}
	e := newAdminEnv(t, s)
	if code, body := e.req(t, "POST", "/api/update", e.login(t), "", nil); code != http.StatusNoContent {
		t.Fatalf("update: %d %s", code, body)
	}
	assertBinary(t, exe, next)
	assertPerm(t, exe, 0o755)
	if _, err := os.Stat(exe + bundleExt); err != nil || !restarted.Load() {
		t.Fatalf("bundle %v, restarted %v", err, restarted.Load())
	}
}

// 003-AC5, 001-AC37: an unsigned, tampered or foreign-signed release is refused with its reason;
// the binary stays and nothing restarts. An older or equal release is not offered.
func TestSelfUpdateRefuses(t *testing.T) {
	key := trusttest.NewKey(t)
	next := ownExe(t)
	for _, tc := range []struct {
		name   string
		bin    []byte
		bundle []byte
		want   string
	}{
		{"unsigned", next, nil, "no signature bundle"},
		{"tampered", append(append([]byte{}, next...), 'x'), key.Sign(t, next), "signature refused"},
		{"foreign key", next, trusttest.NewKey(t).Sign(t, next), "no trusted dev key matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, exe, restarted := updateSupervisor(t, fakeGitHub(t, "v1.1.0", tc.bin, tc.bundle).URL, key)
			s.checkUpdate(context.Background())
			e := newAdminEnv(t, s)
			code, body := e.req(t, "POST", "/api/update", e.login(t), "", nil)
			if code != http.StatusBadGateway || !strings.Contains(body, tc.want) {
				t.Fatalf("update: %d %s", code, body)
			}
			assertBinary(t, exe, []byte("old binary"))
			if u := s.Status().Update; restarted.Load() || u.Running || !strings.Contains(u.Error, tc.want) {
				t.Fatalf("restarted %v, update %+v", restarted.Load(), u)
			}
		})
	}
	for _, v := range []string{"v1.0.0", "v0.9.0", "garbage"} {
		s, _, _ := updateSupervisor(t, fakeGitHub(t, v, next, key.Sign(t, next)).URL, key)
		s.checkUpdate(context.Background())
		if u := s.Status().Update; u.Available != "" {
			t.Fatalf("%s offered: %+v", v, u)
		}
		if code, _ := newAdminEnvLogin(t, s, "POST", "/api/update"); code != http.StatusConflict {
			t.Fatalf("%s: update without offer: %d", v, code)
		}
	}
	// GitHub unreachable: the page says why, and nothing is offered.
	s, _, _ := updateSupervisor(t, "http://127.0.0.1:1", key)
	s.checkUpdate(context.Background())
	if tv := s.Trust(); tv.CheckError == "" || s.Status().Update.Available != "" {
		t.Fatalf("unreachable: %+v", tv)
	}
}

func newAdminEnvLogin(t *testing.T, b Backend, method, path string) (int, string) {
	t.Helper()
	e := newAdminEnv(t, b)
	return e.req(t, method, path, e.login(t), "", nil)
}

// 003-AC5: with the "follow instance" channel, the supervisor takes the version its instance
// serves, older ones included, under that instance's trust rules: an unsigned binary needs the
// instance's relax toggle.
func TestSelfUpdateFollowsInstance(t *testing.T) {
	next := ownExe(t)
	srv, rel := serve(t, release{version: "v0.9.0", bin: next}, nil)
	s, exe, restarted := updateSupervisor(t, "http://127.0.0.1:1", nil)
	if err := s.SetUpdateChannel("instance", ""); err == nil {
		t.Fatal("followed an instance before any")
	}
	if err := s.SetUpdateChannel("nightly", ""); err == nil {
		t.Fatal("unknown channel accepted")
	}
	s.SetRoom(context.Background(), Room{Base: srv.URL, ID: "abc"})
	id, _ := InstanceID(srv.URL)
	e := newAdminEnv(t, s)
	tok := e.login(t)
	if code, _ := e.req(t, "POST", "/api/update-channel", tok, `{"channel":"instance","instance":"unknown"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown instance followed: %d", code)
	}
	if code, b := e.req(t, "POST", "/api/update-channel", tok, `{"channel":"instance"}`, nil); code != http.StatusNoContent {
		t.Fatalf("channel: %d %s", code, b)
	}
	if tv := s.Trust(); tv.Channel != "instance" || tv.Follow != id {
		t.Fatalf("trust view %+v", tv)
	}
	b, err := os.ReadFile(s.o.Paths.supervisorSettings())
	if err != nil || !strings.Contains(string(b), `"update_channel": "instance"`) {
		t.Fatalf("settings %s %v", b, err)
	}
	s.checkUpdate(context.Background())
	if u := s.Status().Update; u.Available != "v0.9.0" || u.From != srv.URL {
		t.Fatalf("offer %+v", u)
	}
	if err := s.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "no signature bundle") {
		t.Fatalf("unsigned update: %v", err)
	}
	assertBinary(t, exe, []byte("old binary"))
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := s.SetRelax(id, true, host); err != nil {
		t.Fatal(err)
	}
	s.checkUpdate(context.Background())
	if err := s.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBinary(t, exe, next)
	if _, err := os.Stat(exe + bundleExt); !os.IsNotExist(err) || restarted.Load() {
		t.Fatalf("unsigned update left a bundle (%v) or restarted without the page", err)
	}
	// The instance's own version is never offered again.
	rel.Store(&release{version: "v1.0.0", bin: next})
	s.checkUpdate(context.Background())
	if u := s.Status().Update; u.Available != "" {
		t.Fatalf("offer %+v", u)
	}
	// A settings file a newer or broken supervisor wrote falls back to GitHub.
	if err := os.WriteFile(s.o.Paths.supervisorSettings(), []byte(`{"update_channel":"carrier-pigeon"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := loadSupervisorSettings(s.o.Paths.supervisorSettings()); err == nil || set.UpdateChannel != "github" {
		t.Fatalf("bad settings: %+v %v", set, err)
	}
}

// 003-AC5: the page says whether the running supervisor is project-signed. A dev build says so;
// a bundle beside the binary is checked against it.
func TestSelfSignature(t *testing.T) {
	key := trusttest.NewKey(t)
	gh := fakeGitHub(t, "v9.9.9", nil, nil)
	for _, tc := range []struct {
		name, version string
		bundle        func(exe []byte) []byte
		want          string
	}{
		{"dev build", "v0.0.0", nil, "dev build"},
		{"prerelease", "v1.0.0-rc1", nil, "dev build"},
		{"release missing on GitHub", "v1.0.0", nil, "dev build"},
		{"signed with a trusted dev key", "v1.0.0", func(exe []byte) []byte { return key.Sign(t, exe) }, "dev-key"},
		{"bundle of another binary", "v1.0.0", func([]byte) []byte { return key.Sign(t, []byte("other")) }, "unsigned"},
		{"bundle of an untrusted key", "v1.0.0", func(exe []byte) []byte { return trusttest.NewKey(t).Sign(t, exe) }, "unsigned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, exe, _ := updateSupervisor(t, gh.URL, key, func(o *SupervisorOptions) { o.Version = tc.version })
			if tc.bundle != nil {
				b, _ := os.ReadFile(exe) //nolint:gosec // test temp file
				if err := os.WriteFile(exe+bundleExt, tc.bundle(b), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := s.checkSelf(context.Background()); got.State != tc.want {
				t.Fatalf("self %+v, want %s", got, tc.want)
			}
		})
	}
	s, _, _ := updateSupervisor(t, "http://127.0.0.1:1", key)
	if got := s.checkSelf(context.Background()); got.State != "unknown" {
		t.Fatalf("offline release: %+v", got)
	}
}

// relaxSupervisor runs workers whose Binary refuses unless the instance accepts unsigned
// binaries, logging to an observer.
func relaxSupervisor(t *testing.T) (*Supervisor, *observer.ObservedLogs, *atomic.Int32) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	var calls atomic.Int32
	s := testSupervisor(t, strconv.Itoa(Proto), nil, func(o *SupervisorOptions) {
		o.Log = zap.New(core)
		o.Binary = func(_ context.Context, in Instance) (Resolved, error) {
			calls.Add(1)
			if !in.Relax {
				return Resolved{}, errRefused{"v1.2.3 refused: no signature bundle"}
			}
			return Resolved{Path: os.Args[0], Version: "v1.2.3", Trust: "unsigned, accepted", Relaxed: true}, nil
		}
	})
	return s, logs, &calls
}

// 003-AC2, 003-AC4: a refused binary stops its worker with the reason on the page. The "accept
// unsigned" toggle is off by default, switches on only with the instance's host typed exactly,
// shows on the page, logs a warning at every worker start, and switching it off refuses again.
func TestRelaxToggle(t *testing.T) {
	s, logs, calls := relaxSupervisor(t)
	e := newAdminEnv(t, s)
	tok := e.login(t)
	base := "https://rooms.example:8443"
	s.SetRoom(context.Background(), Room{Base: base, ID: "abc"})
	id, _ := InstanceID(base)
	waitFor(t, "the refusal", func() bool {
		in := instance(t, s)
		return in.State == "refused" && strings.Contains(in.Error, "no signature bundle")
	})
	if in := instance(t, s); in.AcceptUnsigned {
		t.Fatal("toggle on by default")
	}
	if !strings.Contains(s.Status().Error, "no signature bundle") {
		t.Fatalf("page status %+v", s.Status())
	}
	for _, host := range []string{"", "rooms.example", "ROOMS.EXAMPLE:8443", "rooms.example:8443 ", "https://rooms.example:8443"} {
		body, _ := json.Marshal(map[string]any{"instance": id, "enabled": true, "host": host})
		if code, _ := e.req(t, "POST", "/api/relax", tok, string(body), nil); code != http.StatusBadRequest {
			t.Fatalf("host %q: %d", host, code)
		}
	}
	if in := instance(t, s); in.AcceptUnsigned || in.State != "refused" || calls.Load() != 1 {
		t.Fatalf("after wrong hosts: %+v, %d resolves", in, calls.Load())
	}
	body, _ := json.Marshal(map[string]any{"instance": id, "enabled": true, "host": "rooms.example:8443"})
	if code, b := e.req(t, "POST", "/api/relax", tok, string(body), nil); code != http.StatusNoContent {
		t.Fatalf("relax: %d %s", code, b)
	}
	waitFor(t, "the relaxed worker", func() bool { in := instance(t, s); return in.State == "running" && in.AcceptUnsigned })
	saved, err := os.ReadFile(filepath.Join(s.o.Paths.InstanceConfig(id), instanceFile))
	if err != nil || !strings.Contains(string(saved), `"accept_unsigned": true`) {
		t.Fatalf("instance.json %s %v", saved, err)
	}
	warnings := func() int {
		return logs.FilterMessage("worker_accepts_unsigned").FilterField(zap.String("version", "v1.2.3")).Len()
	}
	if warnings() != 1 {
		t.Fatalf("%d warnings after the first start", warnings())
	}
	// Every start warns: a crash restarts the worker.
	pid := instance(t, s).PID
	p, _ := os.FindProcess(pid)
	_ = p.Kill()
	waitFor(t, "the restarted worker", func() bool { in := instance(t, s); return in.State == "running" && in.PID != pid })
	if warnings() != 2 {
		t.Fatalf("%d warnings after a restart", warnings())
	}
	off, _ := json.Marshal(map[string]any{"instance": id, "enabled": false})
	if code, _ := e.req(t, "POST", "/api/relax", tok, string(off), nil); code != http.StatusNoContent {
		t.Fatal("relax off refused")
	}
	waitFor(t, "the refusal again", func() bool { in := instance(t, s); return in.State == "refused" && !in.AcceptUnsigned })
	if code, _ := e.req(t, "POST", "/api/relax", tok, `{"instance":"nope","enabled":false}`, nil); code != http.StatusBadRequest {
		t.Fatal("unknown instance accepted")
	}
}

// 003-AC4: adding a dev key on the page restarts the workers its absence refused; removing it
// restarts the workers running a binary it signed.
func TestDevKeyOnPage(t *testing.T) {
	key := trusttest.NewKey(t)
	bins := testBins(t)
	s := testSupervisor(t, strconv.Itoa(Proto), nil, func(o *SupervisorOptions) {
		o.Bins = bins
		o.Executable = os.Args[0]
		o.UpdateEvery = time.Hour
		o.Binary = func(context.Context, Instance) (Resolved, error) {
			if keys := bins.Keys.List(); len(keys) > 0 {
				return Resolved{Path: os.Args[0], Version: "v1.2.3", Trust: "signed with the dev key " + keys[0].Name, DevKey: keys[0].Name}, nil
			}
			return Resolved{}, errRefused{"v1.2.3 refused: no trusted dev key matches"}
		}
	})
	e := newAdminEnv(t, s)
	tok := e.login(t)
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the refusal", func() bool { return instance(t, s).State == "refused" })
	if code, _ := e.req(t, "POST", "/api/dev-keys", tok, `{"name":"laptop","pem":"garbage"}`, nil); code != http.StatusBadRequest {
		t.Fatal("garbage key accepted")
	}
	body, _ := json.Marshal(map[string]string{"name": "laptop", "pem": string(key.PEM)})
	if code, b := e.req(t, "POST", "/api/dev-keys", tok, string(body), nil); code != http.StatusNoContent {
		t.Fatalf("add key: %d %s", code, b)
	}
	waitFor(t, "the worker running", func() bool {
		in := instance(t, s)
		return in.State == "running" && strings.Contains(in.Trust, "laptop")
	})
	_, st := e.req(t, "GET", "/api/status", tok, "", nil)
	if !strings.Contains(st, `"name":"laptop"`) || !strings.Contains(st, `"fingerprint"`) {
		t.Fatalf("status lacks the key: %s", st)
	}
	if code, _ := e.req(t, "POST", "/api/dev-keys/remove", tok, `{"name":"laptop"}`, nil); code != http.StatusNoContent {
		t.Fatal("remove refused")
	}
	waitFor(t, "the refusal again", func() bool { return instance(t, s).State == "refused" })
}

// 003-AC7: when a worker reports that its instance serves another version, the worker restarts
// on it, once.
func TestInstanceVersionChangeRestartsWorker(t *testing.T) {
	var calls atomic.Int32
	s := testSupervisor(t, "served", nil, func(o *SupervisorOptions) {
		o.Binary = func(context.Context, Instance) (Resolved, error) {
			if calls.Add(1) == 1 {
				return Resolved{Path: os.Args[0], Version: "v1.0.0", Served: "v1.0.0"}, nil
			}
			return Resolved{Path: os.Args[0], Version: "v9.9.9", Served: "v9.9.9"}, nil
		}
	})
	id, _ := InstanceID("https://rooms.example")
	// 003-AC5: the supervisor follows this instance, so its new version is checked at once.
	s.mu.Lock()
	s.set = SupervisorSettings{UpdateChannel: "instance", UpdateInstance: id}
	s.mu.Unlock()
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the worker on v9.9.9", func() bool { in := instance(t, s); return in.State == "running" && in.Version == "v9.9.9" })
	// Its status, which names v9.9.9 again, came before its last log line: once that is in, a
	// second restart would already be under way.
	waitStatusSeen(t, s, 2)
	if in := instance(t, s); calls.Load() != 2 || in.Restarts != 0 || len(s.checkNow) != 1 || reloading(s) {
		t.Fatalf("%d resolves, %d update checks asked, %+v", calls.Load(), len(s.checkNow), in)
	}
	in, err := loadInstance(s.o.Paths.InstanceConfig(id))
	if err != nil || in.Version != "v9.9.9" {
		t.Fatalf("instance.json %+v %v", in, err)
	}
	// Started offline on the copy it ran last, a worker whose instance serves that very version
	// is left alone.
	var offline atomic.Int32
	s = testSupervisor(t, "served", nil, func(o *SupervisorOptions) {
		o.Binary = func(context.Context, Instance) (Resolved, error) {
			offline.Add(1)
			return Resolved{Path: os.Args[0], Version: "v9.9.9"}, nil
		}
	})
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the worker", func() bool { return instance(t, s).State == "running" })
	waitStatusSeen(t, s, 1)
	if offline.Load() != 1 || reloading(s) {
		t.Fatalf("%d resolves for an unchanged version", offline.Load())
	}
}

// 005-AC45: a supervisor downloaded under a versioned name updates to the version-neutral name,
// and the old file goes: grill-room-daemon-v1.0.0 never runs v1.1.0.
func TestSelfUpdateDropsVersionFromName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the name: it cannot remove a running binary nor exec in place")
	}
	key := trusttest.NewKey(t)
	next := ownExe(t)
	gh := fakeGitHub(t, "v1.1.0", next, key.Sign(t, next))
	versioned := filepath.Join(t.TempDir(), "grill-room-daemon-v1.0.0")
	if err := os.WriteFile(versioned, []byte("old binary"), 0o755); err != nil { //nolint:gosec // an executable fake
		t.Fatal(err)
	}
	s, _, restarted := updateSupervisor(t, gh.URL, key, func(o *SupervisorOptions) { o.Executable = versioned })
	s.checkUpdate(context.Background())
	e := newAdminEnv(t, s)
	if code, body := e.req(t, "POST", "/api/update", e.login(t), "", nil); code != http.StatusNoContent {
		t.Fatalf("update: %d %s", code, body)
	}
	neutral := filepath.Join(filepath.Dir(versioned), "grill-room-daemon")
	assertBinary(t, neutral, next)
	if _, err := os.Stat(versioned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old binary still there: %v", err)
	}
	if got := s.Executable(); got != neutral || !restarted.Load() {
		t.Fatalf("executable %s, restarted %v", got, restarted.Load())
	}
}

func TestNeutralName(t *testing.T) {
	for _, c := range []struct{ exe, want string }{
		{"/x/grill-room-daemon-v0.0.3", "/x/grill-room-daemon"},
		{"/x/grill-room-daemon_v0.0.3.exe", "/x/grill-room-daemon.exe"},
		{"/x/grill-room-daemon-v0.0.3-rc.1.exe", "/x/grill-room-daemon.exe"},
		// Named after an older version: in-place updates before 005-AC45 left such names.
		{"/x/grill-room-daemon-v0.0.2", "/x/grill-room-daemon"},
		{"/x/grill-room-daemon", "/x/grill-room-daemon"},
		{"/x/v0.0.3", "/x/v0.0.3"},
		{"/x/grill-room-daemon-linux-amd64", "/x/grill-room-daemon-linux-amd64"},
	} {
		if got := neutralName(filepath.FromSlash(c.exe)); got != filepath.FromSlash(c.want) {
			t.Errorf("%s: %s, want %s", c.exe, got, c.want)
		}
	}
}

// 005-AC45: a file already at the neutral name is someone's install: the update stays in place.
func TestSelfUpdateKeepsNameWhenTaken(t *testing.T) {
	key := trusttest.NewKey(t)
	next := ownExe(t)
	gh := fakeGitHub(t, "v1.1.0", next, key.Sign(t, next))
	dir := t.TempDir()
	versioned, other := filepath.Join(dir, "grill-room-daemon-v1.0.0"), filepath.Join(dir, "grill-room-daemon")
	for _, f := range []string{versioned, other} {
		if err := os.WriteFile(f, []byte("old binary"), 0o755); err != nil { //nolint:gosec // an executable fake
			t.Fatal(err)
		}
	}
	s, _, _ := updateSupervisor(t, gh.URL, key, func(o *SupervisorOptions) { o.Executable = versioned })
	s.checkUpdate(context.Background())
	e := newAdminEnv(t, s)
	if code, body := e.req(t, "POST", "/api/update", e.login(t), "", nil); code != http.StatusNoContent {
		t.Fatalf("update: %d %s", code, body)
	}
	assertBinary(t, versioned, next)
	assertBinary(t, other, []byte("old binary"))
	if s.Executable() != versioned {
		t.Fatalf("executable %s", s.Executable())
	}
}

// waitStatusSeen waits until n fake workers sent their last start-up log line: the supervisor reads
// a worker's lines in order, so the status sent before it was handled.
func waitStatusSeen(t *testing.T, s *Supervisor, n int) {
	t.Helper()
	waitFor(t, "the fake workers' start-up lines", func() bool {
		return strings.Count(strings.Join(s.o.Ring.Lines(), "\n"), "fake_limits") >= n
	})
}

// reloading says whether a worker restart was asked and not yet taken.
func reloading(s *Supervisor) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		if w.reload {
			return true
		}
	}
	return false
}
