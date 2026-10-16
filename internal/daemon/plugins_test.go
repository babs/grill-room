package daemon

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
	"github.com/babs/grill-room/internal/web"
)

// fakeInspector stands in for docker during inspections: it unpacks the snapshot it is given and
// answers claude's plugin commands from validate and details.
type fakeInspector struct {
	mu    sync.Mutex
	calls [][]string
	// running are the inspection containers up, by name; removed counts those removed.
	running   map[string]bool
	removed   int
	maxActive int
	// unpacked are the files of the last snapshot, by path.
	unpacked map[string]string
	validate func(files map[string]string) ([]byte, error)
	details  string
	// gate, when set, holds each inspection's container start until it is closed.
	gate chan struct{}
}

func (f *fakeInspector) run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if args[0] == "info" {
		return dockerUp(context.Background(), nil)
	}
	if args[0] == "run" && f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	switch {
	case args[0] == "run":
		if f.running == nil {
			f.running = map[string]bool{}
		}
		f.running[args[slices.Index(args, "--name")+1]] = true
		f.maxActive = max(f.maxActive, len(f.running))
	case args[0] == "rm":
		if f.running[args[len(args)-1]] {
			delete(f.running, args[len(args)-1])
			f.removed++
		}
	case slices.Contains(args, "tar"):
		f.unpacked = map[string]string{}
		tr := tar.NewReader(stdin)
		for h, err := tr.Next(); err == nil; h, err = tr.Next() {
			b, _ := io.ReadAll(tr)
			f.unpacked[h.Name] = string(b)
		}
	case slices.Contains(args, "validate"):
		return f.validate(f.unpacked)
	case slices.Contains(args, "details"):
		return []byte(f.details), nil
	}
	return nil, nil
}

func (f *fakeInspector) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c[0] == "run" {
			n++
		}
	}
	return n
}

func validOutput(map[string]string) ([]byte, error) {
	return os.ReadFile("../plugin/testdata/validate-rich-2.1.296.json")
}

// snapshotOf packs files as the server stores a plugin.
func snapshotOf(t *testing.T, files map[string]string) *plugin.Snapshot {
	t.Helper()
	var es []plugin.Entry
	for p, c := range files {
		es = append(es, plugin.Entry{Path: p, Kind: plugin.Regular, Data: []byte(c)})
	}
	s, err := plugin.FromTree(es, "", nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// addPlugin stores plugin name at its version 1 in the room, on for personas, and wakes the
// room's streams with a chat line so the host daemon hears of it.
func (rs *roomServer) addPlugin(t *testing.T, tok, name string, s *plugin.Snapshot, personas ...int64) {
	t.Helper()
	ps, err := rs.st.Participants(t.Context(), rs.room)
	if err != nil || len(ps) == 0 {
		t.Fatalf("participants: %v", err)
	}
	files, _ := json.Marshal(s.Files)
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(m)
	if _, err := rs.st.AddPlugins(t.Context(), rs.room, []store.NewPlugin{{Name: name, URL: "https://example.com/" + name, Ref: "main", RefName: "main",
		PluginSnapshot: store.PluginSnapshot{Commit: strings.Repeat("a", 40), SHA256: s.SHA256, Size: s.Size, Files: len(s.Files),
			FileList: files, Manifest: manifest, Data: s.Tar}}}, personas, &ps[0], 1<<20); err != nil {
		t.Fatal(err)
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", tok, map[string]string{"body": "plugin " + name}, http.StatusCreated, nil)
}

// version returns plugin name's latest version.
func (rs *roomServer) version(t *testing.T, name string) store.PluginVersion {
	t.Helper()
	ps, err := rs.st.RoomPlugins(t.Context(), rs.room)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == name {
			return *p.Latest()
		}
	}
	t.Fatalf("no plugin %s", name)
	return store.PluginVersion{}
}

// 009-AC13: the host's daemon inspects each new version in a throwaway container, with no network
// and a read-only root, from the sandbox image, removed after: it unpacks the snapshot, validates
// it, reads its details, and posts the report. A passed validation is approved (the host does not
// approve everything), a failed one is refused by Claude Code with its first error. Inspections
// run one at a time and a version reported already is not inspected again.
func TestDaemonInspectsPlugins(t *testing.T) {
	rs := newRoomServer(t)
	details, err := os.ReadFile("../plugin/testdata/details-2.1.296.txt")
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeInspector{details: string(details), gate: make(chan struct{})}
	fi.validate = func(files map[string]string) ([]byte, error) {
		if strings.Contains(files[plugin.ManifestPath], `"bad"`) {
			return []byte(`{"success":false,"manifest":{"errors":[{"path":"name","message":"Plugin name cannot be empty"}]}}`), errors.New("exit status 1")
		}
		return validOutput(files)
	}
	alice, d, _ := pairedDaemon(t, rs, "Alice", &fakeAgent{}, func(d *Daemon) { d.o.Docker, d.o.Labels = fi.run, []string{"grill-room-test=1"} })
	rs.addPlugin(t, alice, "good", snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"good"}`, "skills/x/SKILL.md": "x"}))
	rs.addPlugin(t, alice, "broken", snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"broken","x":"bad"}`}))
	close(fi.gate)
	waitFor(t, "both inspected", func() bool {
		return rs.version(t, "good").State != store.PluginInspecting && rs.version(t, "broken").State != store.PluginInspecting
	})

	good := rs.version(t, "good")
	var rep pluginReport
	if err := json.Unmarshal(good.Report, &rep); err != nil {
		t.Fatal(err)
	}
	if good.State != store.PluginApproved || !rep.OK || rep.Details == nil || rep.Details.AlwaysOnTokens != 1513 || !rep.Details.CostKnown ||
		!strings.Contains(string(rep.Validate), `"No author information provided.`) {
		t.Fatalf("good: %s %s", good.State, good.Report)
	}
	if bad := rs.version(t, "broken"); bad.State != store.PluginInvalid || bad.Reason != "refused by Claude Code: name: Plugin name cannot be empty" {
		t.Fatalf("broken: %s %q", bad.State, bad.Reason)
	}
	fi.mu.Lock()
	var run, details2 []string
	removed := fi.removed
	for _, c := range fi.calls {
		switch {
		case c[0] == "run":
			run = c
		case slices.Contains(c, "details"):
			details2 = c
		}
	}
	if fi.maxActive != 1 || removed != 2 {
		t.Fatalf("inspections at once %d, containers removed %d", fi.maxActive, removed)
	}
	fi.mu.Unlock()
	for _, want := range [][]string{{"--network", "none"}, {"--read-only"}, {"--cap-drop", "ALL"}, {"test-image"},
		{"--memory", "1g", "--memory-swap", "1g", "--cpus", "1"}, {"--tmpfs", "/tmp:size=400m"}} {
		if !containsSeq(run, want) {
			t.Fatalf("run %q lacks %q", run, want)
		}
	}
	name := run[slices.Index(run, "--name")+1]
	if !strings.HasPrefix(name, "grill-room-inspect-"+rs.room+"-") || !containsSeq(run, []string{"--label", sandbox.Label + "=" + name}) ||
		!containsSeq(run, []string{"--label", "grill-room-test=1"}) {
		t.Fatalf("container name and labels: %q", run)
	}
	// A container left by a crash under the same name goes first.
	fi.mu.Lock()
	first := slices.IndexFunc(fi.calls, func(c []string) bool { return c[0] == "rm" && c[len(c)-1] == name })
	started := slices.IndexFunc(fi.calls, func(c []string) bool { return c[0] == "run" && slices.Contains(c, name) })
	fi.mu.Unlock()
	if first < 0 || first > started {
		t.Fatalf("no removal before the start of %s", name)
	}
	if !containsSeq(details2, []string{"claude", "--plugin-dir", "/tmp/p", "plugin", "details", "good"}) {
		t.Fatalf("details %q", details2)
	}

	// Announced again, a version reported already is skipped.
	tok, _ := d.o.Tokens.Get(d.runs()[0].room)
	d.runs()[0].inspect(t.Context(), tok, good.ID)
	if n := fi.runs(); n != 2 {
		t.Fatalf("%d inspections, want 2", n)
	}
}

// containsSeq reports whether want appears in args as consecutive items.
func containsSeq(args, want []string) bool {
	for i := range args {
		if i+len(want) <= len(args) && slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// 009-AC13: a plugin whose validation prints no JSON is tried again after a backoff; failing
// again, the daemon reports it failed with the error, so the room shows why instead of waiting. A
// failure followed by a success reports the success.
func TestDaemonInspectionRetries(t *testing.T) {
	rs := newRoomServer(t)
	var mu sync.Mutex
	fails := map[string]int{"flaky": 1, "broken": 99}
	fi := &fakeInspector{details: "x 1\n"}
	fi.validate = func(files map[string]string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		for name := range fails {
			if strings.Contains(files[plugin.ManifestPath], `"`+name+`"`) && fails[name] > 0 {
				fails[name]--
				return []byte("Error: boom"), errors.New("exit status 1")
			}
		}
		return validOutput(files)
	}
	alice, _, _ := pairedDaemon(t, rs, "Alice", &fakeAgent{}, func(d *Daemon) { d.o.Docker = fi.run })
	rs.addPlugin(t, alice, "flaky", snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"flaky"}`}))
	rs.addPlugin(t, alice, "broken", snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"broken"}`}))
	waitFor(t, "both reported", func() bool {
		return rs.version(t, "flaky").State != store.PluginInspecting && rs.version(t, "broken").State != store.PluginInspecting
	})
	if v := rs.version(t, "flaky"); v.State != store.PluginApproved {
		t.Fatalf("flaky: %s %q", v.State, v.Reason)
	}
	if v := rs.version(t, "broken"); v.State != store.PluginInvalid ||
		v.Reason != "refused by Claude Code: the plugin could not be checked: claude plugin validate printed no JSON: Error: boom" {
		t.Fatalf("broken: %s %q", v.State, v.Reason)
	}
}

// 009-AC13: a version announced while the sandbox image is not ready yet, as at the daemon's
// start, is inspected once the image is ready, without waiting for a retry.
func TestInspectionWaitsForImage(t *testing.T) {
	good := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"good"}`})
	f := &fakeRoom{snapshot: good.Tar, reportCode: http.StatusOK}
	r := f.serve(t)
	r.d.o.Docker = (&fakeInspector{validate: validOutput, details: "good 1\n"}).run
	r.d.o.MaxBackoff = time.Hour
	core, logs := observer.New(zap.DebugLevel)
	r.d.o.Log = zap.New(core)
	r.d.update(func(s *Status) { s.Image = Image{State: "pending"} })
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() { r.inspect(ctx, "tok", 7); close(done) }()
	waitFor(t, "the inspection waiting", func() bool { return logs.FilterMessage("plugin_inspect_waits_for_image").Len() > 0 })
	r.d.update(func(s *Status) { s.Image = Image{State: "ready", Ref: "test-image"} })
	<-done
	if len(f.reports) != 1 || !f.reports[0].OK {
		t.Fatalf("reports %+v", f.reports)
	}
}

// 009-AC17 009-AC18: a turn hands the sandbox the approved plugins its persona has on, by name,
// as the room's snapshots, kept in the plugin cache by sha256. A cached copy is used without
// asking the room; a corrupt one is fetched again and replaced. A plugin turned off, or another
// persona's, never reaches the turn, nor does a version waiting for the host.
func TestDaemonTurnLoadsPlugins(t *testing.T) {
	var fetches atomic.Int64
	rs := newRoomServerWrapped(t, web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20, MaxAttachmentBytes: 1 << 20}, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/daemon/turns/") && strings.Contains(r.URL.Path, "/plugins/") {
				fetches.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	ag := &fakeAgent{}
	ag.run = func(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		var got []string
		for _, p := range in.Plugins {
			tr := tar.NewReader(p.Tar)
			for h, err := tr.Next(); err == nil; h, err = tr.Next() {
				b, _ := io.ReadAll(tr)
				got = append(got, p.Name+":"+h.Name+"="+string(b))
			}
		}
		if len(got) == 0 {
			got = []string{"none"}
		}
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: strings.Join(got, ","), SessionID: sid}, SessionOffset: in.SessionSize, Session: []byte("{}\n")}, nil
	}
	fi := &fakeInspector{validate: validOutput, details: "x 1\n"}
	cache := t.TempDir()
	alice, _, _ := pairedDaemon(t, rs, "Alice", ag, func(d *Daemon) { d.o.Docker = fi.run; d.o.PluginCacheDir = cache })
	grill := rs.grill(t).ID
	snap := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"zed"}`, "skills/z/SKILL.md": "z"})
	rs.addPlugin(t, alice, "zed", snap, grill)
	rs.addPlugin(t, alice, "alpha", snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"alpha"}`}), grill)
	waitFor(t, "approved", func() bool {
		return rs.version(t, "zed").State == store.PluginApproved && rs.version(t, "alpha").State == store.PluginApproved
	})

	const want = `alpha:.claude-plugin/plugin.json={"name":"alpha"},zed:.claude-plugin/plugin.json={"name":"zed"},zed:skills/z/SKILL.md=z`
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" || rs.lastMessage(t) != want || fetches.Load() != 2 {
		t.Fatalf("first turn: %+v %q, %d fetches", v.Turn, rs.lastMessage(t), fetches.Load())
	}
	cached := filepath.Join(cache, snap.SHA256+".tar")
	if b, err := os.ReadFile(cached); err != nil || string(b) != string(snap.Tar) { //nolint:gosec // test temp dir
		t.Fatalf("cache: %v", err)
	}
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" || rs.lastMessage(t) != want || fetches.Load() != 2 {
		t.Fatalf("cached turn: %+v %q, %d fetches", v.Turn, rs.lastMessage(t), fetches.Load())
	}
	if err := os.WriteFile(cached, append(snap.Tar[:len(snap.Tar)-1:len(snap.Tar)-1], 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" || rs.lastMessage(t) != want || fetches.Load() != 3 {
		t.Fatalf("corrupt cache: %+v %q, %d fetches", v.Turn, rs.lastMessage(t), fetches.Load())
	}
	if b, _ := os.ReadFile(cached); string(b) != string(snap.Tar) { //nolint:gosec // test temp dir
		t.Fatal("the corrupt cached copy was not replaced")
	}

	ps, _ := rs.st.RoomPlugins(t.Context(), rs.room)
	for _, p := range ps {
		if err := rs.st.SetPersonaPlugin(t.Context(), rs.room, grill, p.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" || rs.lastMessage(t) != "none" {
		t.Fatalf("plugins off: %+v %q", v.Turn, rs.lastMessage(t))
	}
}

// A cached snapshot unused for longer than the TTL goes on the next download; others stay.
func TestPrunePluginCache(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"old.tar", "new.tar", "old.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := filepath.Join(dir, "old.tar")
	info, _ := os.Stat(past)
	// Whole seconds: NTFS keeps mtimes in 100 ns steps, so a 1 ns offset vanishes on Windows.
	cutoff := info.ModTime().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "new.tar"), cutoff.Add(time.Second), cutoff.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "old.txt"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	prunePluginCache(dir, cutoff, zap.NewNop())
	es, _ := os.ReadDir(dir)
	var left []string
	for _, e := range es {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"new.tar", "old.txt"}) {
		t.Fatalf("left %q", left)
	}
}

// fakeRoom serves the daemon routes a plugin inspection and a turn's plugins use, and records the
// reports posted.
type fakeRoom struct {
	snapshot []byte
	// tars are the turn's plugin snapshots by sha256.
	tars        map[string][]byte
	reportCode  int
	mu          sync.Mutex
	reports     []pluginReport
	tarRequests int
}

func (f *fakeRoom) serve(t *testing.T) *roomRun {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/rooms/r1/daemon/plugins/{v}/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		if f.snapshot == nil {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		_, _ = w.Write(f.snapshot)
	})
	mux.HandleFunc("POST /api/rooms/r1/daemon/plugins/{v}/report", func(w http.ResponseWriter, r *http.Request) {
		var rep pluginReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		f.mu.Lock()
		f.reports = append(f.reports, rep)
		f.mu.Unlock()
		w.WriteHeader(f.reportCode)
		_, _ = w.Write([]byte(`{"state":"approved"}`))
	})
	mux.HandleFunc("GET /api/rooms/r1/daemon/turns/1/plugins/{sum}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tarRequests++
		f.mu.Unlock()
		b, ok := f.tars[r.PathValue("sum")]
		if !ok {
			http.Error(w, "no such plugin", http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	d := newTestDaemon(t, dockerUp)
	r := &roomRun{d: d, room: Room{Base: ts.URL, ID: "r1"}, ctx: t.Context()}
	// Registered, so that an image change wakes what waits on it.
	d.rooms[r.room.String()] = r
	return r
}

// 009-AC13: what an inspection reports when its inputs go wrong. A snapshot the daemon refuses is
// reported invalid at once, and a validation that never finishes like any plugin failure, after a
// retry. A failed image, no snapshot, Docker refusing a step, or the room refusing the report are
// the host's or the room's: tried again and again while the stream lives, they report nothing. A
// 409 counts as reported; a details command that fails leaves the cost unknown but the
// validation stands.
func TestInspectionEdges(t *testing.T) {
	good := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"good"}`})
	fail := func(step string) sandbox.Runner {
		fi := &fakeInspector{validate: validOutput, details: "good 1\n"}
		return func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
			if slices.Contains(args, step) {
				return nil, errors.New("docker refused " + step)
			}
			return fi.run(ctx, stdin, args...)
		}
	}
	hang := func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if slices.Contains(args, "validate") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&fakeInspector{}).run(ctx, stdin, args...)
	}
	for _, tc := range []struct {
		name       string
		snapshot   []byte
		image      string
		docker     sandbox.Runner
		reportCode int
		want       string // the last report's error prefix, "ok", or "" for none
		marked     bool
	}{
		{name: "invalid snapshot", snapshot: []byte("not a tar"), reportCode: 200, want: "the snapshot is not a valid plugin: invalid plugin: not a tar", marked: true},
		{name: "validation never ends", snapshot: good.Tar, docker: hang, reportCode: 200, want: "the plugin could not be checked: claude plugin validate did not finish within", marked: true},
		{name: "image failed", snapshot: good.Tar, image: "failed", reportCode: 200},
		{name: "no snapshot", reportCode: 200},
		{name: "docker refuses the container", snapshot: good.Tar, docker: fail("run"), reportCode: 200},
		{name: "docker refuses the unpack", snapshot: good.Tar, docker: fail("mkdir"), reportCode: 200},
		{name: "docker prints nothing", snapshot: good.Tar, docker: fail("validate"), reportCode: 200},
		{name: "report refused", snapshot: good.Tar, reportCode: http.StatusForbidden, want: "ok"},
		{name: "details fail", snapshot: good.Tar, docker: fail("details"), reportCode: 200, want: "ok", marked: true},
		{name: "inspected already", snapshot: good.Tar, reportCode: http.StatusConflict, want: "ok", marked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRoom{snapshot: tc.snapshot, reportCode: tc.reportCode}
			r := f.serve(t)
			r.d.o.MaxBackoff = 10 * time.Millisecond
			if tc.docker == nil {
				tc.docker = (&fakeInspector{validate: validOutput, details: "good 1\n  Source: good@inline\n"}).run
			}
			r.d.o.Docker = tc.docker
			if tc.image != "" {
				r.d.st.Image = Image{State: tc.image, Error: "boom"}
			}
			ctx := t.Context()
			if !tc.marked {
				// A failure that is not the plugin's retries until the stream ends.
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			}
			if tc.name == "validation never ends" {
				defer func(old time.Duration) { inspectTimeout = old }(inspectTimeout)
				inspectTimeout = 20 * time.Millisecond
			}
			r.inspect(ctx, "tok", 7)
			f.mu.Lock()
			defer f.mu.Unlock()
			got := ""
			for _, rep := range f.reports {
				if got = rep.Error; rep.OK {
					got = "ok"
				}
				if tc.want == "" || rep.Error != "" && !rep.OK && !strings.HasPrefix(rep.Error, tc.want) {
					t.Fatalf("report %+v", rep)
				}
			}
			if tc.name == "details fail" && (f.reports[0].Details == nil || f.reports[0].Details.CostKnown) {
				t.Fatalf("details %+v", f.reports[0].Details)
			}
			if !strings.HasPrefix(got, tc.want) || r.d.inspected[r.room.String()+"#7"] != tc.marked {
				t.Fatalf("report %q, marked %v; want %q, %v", got, r.d.inspected[r.room.String()+"#7"], tc.want, tc.marked)
			}
		})
	}
}

// 009-AC13: a sandbox image that failed, then got ready, leaves the version waiting, never
// refused: once the image is ready the inspection approves it.
func TestInspectionOutlivesAFailedImage(t *testing.T) {
	good := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"good"}`})
	f := &fakeRoom{snapshot: good.Tar, reportCode: http.StatusOK}
	r := f.serve(t)
	r.d.o.Docker = (&fakeInspector{validate: validOutput, details: "good 1\n"}).run
	r.d.o.MaxBackoff = 10 * time.Millisecond
	core, logs := observer.New(zap.WarnLevel)
	r.d.o.Log = zap.New(core)
	r.d.update(func(s *Status) { s.Image = Image{State: "failed", Error: "pull refused"} })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { r.inspect(ctx, "tok", 7); close(done) }()
	// Several failed tries pass, more than the plugin's own allowance, before the image is fixed.
	waitFor(t, "a few tries", func() bool { return logs.FilterMessage("plugin_inspect_failed").Len() >= 3 })
	f.mu.Lock()
	n := len(f.reports)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d reports while the image failed", n)
	}
	r.d.update(func(s *Status) { s.Image = Image{State: "ready", Ref: "test-image"} })
	<-done
	if len(f.reports) != 1 || !f.reports[0].OK {
		t.Fatalf("reports %+v", f.reports)
	}
}

// 009-AC17: the daemon refuses, and the turn fails on, a plugin the room names by something other
// than a sha256, a snapshot that is not a valid plugin tar, or one whose sha256 differs; a cache
// it cannot write still delivers the snapshot.
func TestPluginTarEdges(t *testing.T) {
	good := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"good"}`})
	other := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"other"}`})
	bad := strings.Repeat("b", 64)
	f := &fakeRoom{tars: map[string][]byte{good.SHA256: good.Tar, other.SHA256: good.Tar, bad: []byte("junk")}}
	r := f.serve(t)
	for sum, want := range map[string]string{
		"../x":        `"../x" is not a sha256`,
		bad:           "not a tar",
		other.SHA256:  "the room sent a snapshot whose sha256 is " + good.SHA256,
		"c" + bad[1:]: "404",
		good.SHA256:   "",
	} {
		_, closePlugins, err := r.turnPlugins(t.Context(), "tok", "1", []turn.Plugin{{Name: "p", SHA256: sum}})
		if err == nil {
			closePlugins()
		}
		if (err == nil) != (want == "") || err != nil && !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v, want %q", sum, err, want)
		}
	}
	// A cache dir that is a file: nothing is cached, the snapshot still arrives, every time.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.d.o.PluginCacheDir = blocker
	before := f.tarRequests
	for range 2 {
		ps, closePlugins, err := r.turnPlugins(t.Context(), "tok", "1", []turn.Plugin{{Name: "good", SHA256: good.SHA256}})
		if err != nil || len(ps) != 1 {
			t.Fatalf("uncachable: %v", err)
		}
		closePlugins()
	}
	if f.tarRequests-before != 2 {
		t.Fatalf("%d fetches, want 2", f.tarRequests-before)
	}
}

// 009-AC25: the report lists the domains of the version's MCP servers that the egress allowlist
// leaves out, as it stands at the inspection.
func TestInspectionAllowlistMissing(t *testing.T) {
	snap := snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"good"}`,
		".mcp.json": `{"mcpServers":{"a":{"url":"https://mcp.example.com/x"},"b":{"url":"https://api.github.com/y"},"c":{"url":"http://10.1.2.3/z"}}}`})
	f := &fakeRoom{snapshot: snap.Tar, reportCode: http.StatusOK}
	r := f.serve(t)
	r.d.o.Docker = (&fakeInspector{validate: validOutput, details: "good 1\n"}).run
	r.d.st.Egress.Server = entriesFor([]string{".github.com"}, nil)
	tok := "t"
	r.inspect(t.Context(), tok, 7)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) != 1 || !slices.Equal(f.reports[0].AllowlistMissing, []string{"10.1.2.3", "mcp.example.com"}) {
		t.Fatalf("reports %+v", f.reports)
	}
}
