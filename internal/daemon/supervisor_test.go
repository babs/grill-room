package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	_ "github.com/babs/grill-room/internal/sqlitetest"
)

// TestMain lets the supervisor tests run this test binary as a fake worker speaking the protocol
// version in GRILL_ROOM_TEST_WORKER, or as a worker that refuses the supervisor or crashes.
func TestMain(m *testing.M) {
	if mode := os.Getenv("GRILL_ROOM_TEST_WORKER"); mode != "" {
		os.Exit(fakeWorker(mode))
	}
	os.Exit(m.Run())
}

// fakeWorker is a worker from another release: it only says hello at its version, reports a
// status, answers requests and stops when told. "crash" exits right after its config, "mute"
// never answers, "silent" never says hello, "early" talks before it, "served" reports that its
// instance serves v9.9.9, "slots" asks for two slots and frees the first once granted, "turns"
// reports two ended turns.
func fakeWorker(mode string) int {
	proto, err := strconv.Atoi(mode)
	if err != nil {
		proto = Proto
	}
	// A copy named worker-next is a release two protocol versions ahead (protowindow_test.go).
	if strings.HasPrefix(filepath.Base(os.Args[0]), "worker-next") {
		proto = Proto + 2
	}
	conn := NewConn(os.Stdin, os.Stdout)
	switch mode {
	case "silent":
		time.Sleep(time.Hour) // a worker that never says hello: the supervisor's own timeout ends it
	case "early":
		_ = conn.Send(Msg{Type: "status"})
		_, _ = conn.Recv()
		return 0
	}
	_ = conn.Send(Msg{Type: "hello", Proto: proto, Version: "fake-" + mode, Instance: os.Args[len(os.Args)-1]})
	m, err := conn.Recv()
	if err != nil || m.Type != "config" {
		return 3
	}
	if mode == "refuse" {
		_ = conn.Send(Msg{Type: "refused", Error: "fake worker: supervisor too old"})
		return 3
	}
	if mode == "crash" {
		return 1
	}
	served := ""
	if mode == "served" {
		served = "v9.9.9"
	}
	_ = conn.Send(Msg{Type: "status", Status: &Status{Host: "fake-worker", Room: m.Config.Pair, Pairing: "paired", Connection: "connected",
		Update: Update{Available: "v9.9.9", Served: served}}})
	_ = conn.Send(Msg{Type: "log", Log: []byte(`{"msg":"fake_worker_ready"}`)})
	logAccess := func(a *Access) {
		if a != nil {
			b, _ := json.Marshal(map[string]any{"msg": "fake_access", "access": a})
			_ = conn.Send(Msg{Type: "log", Log: b})
		}
	}
	logAccess(m.Config.Access)
	b, _ := json.Marshal(map[string]any{"msg": "fake_limits", "limits": m.Config.Limits})
	_ = conn.Send(Msg{Type: "log", Log: b})
	if mode == "turns" {
		for _, e := range []TurnEvent{
			{Room: m.Config.Pair, ID: 1, Phase: "start"},
			{Room: m.Config.Pair, ID: 1, Phase: "end", Persona: "Grill", Cause: "pressed by Alice", AskedModel: "opus", Model: "claude-sonnet-fake",
				Effort: "high", DurationMS: 1500, QueueMS: 200, TokensIn: 5, TokensOut: 6, TokensCacheRead: 7, TokensCacheWrite: 8,
				CostUSD: new(0.25), ContextTokens: 90, ContextWindow: 100, WorkspaceBytes: 4096},
			{Room: m.Config.Pair, ID: 2, Phase: "end", Persona: "Scribe", Error: errCutOff.Error()},
		} {
			_ = conn.Send(Msg{Type: "turn", Turn: &e})
		}
	}
	if mode == "slots" {
		for i := range int64(2) {
			_ = conn.Send(Msg{Type: "acquire", Slot: i + 1, Turn: &TurnEvent{Room: m.Config.Pair, ID: 10 + i}})
		}
	}
	for {
		r, err := conn.Recv()
		if err != nil || r.Type == "stop" {
			return 0
		}
		if r.Type == "grant" || r.Type == "wait" {
			_ = conn.Send(Msg{Type: "log", Log: []byte(fmt.Sprintf(`{"msg":"fake_slot %s %d"}`, r.Type, r.Slot))})
			if r.Type == "grant" && r.Slot == 1 {
				_ = conn.Send(Msg{Type: "release", Slot: 1})
			}
			continue
		}
		logAccess(r.Access)
		if mode != "mute" {
			_ = conn.Send(Msg{Type: "reply", ID: r.ID})
		}
	}
}

func ownBinary(context.Context, Instance) (Resolved, error) {
	return Resolved{Path: os.Args[0], Version: "v-test", Trust: "the supervisor's own binary"}, nil
}

func testSupervisor(t *testing.T, mode string, a *Autostart, set ...func(*SupervisorOptions)) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	o := SupervisorOptions{
		Paths: Paths{Config: dir, State: dir, Cache: dir}, Version: "v-test", Host: "laptop", Worker: WorkerConfig{SandboxRoot: dir},
		Binary: ownBinary, Env: append(os.Environ(), "GRILL_ROOM_TEST_WORKER="+mode),
		Stderr: io.Discard, Log: zap.NewNop(), MaxBackoff: 100 * time.Millisecond, StopTimeout: 5 * time.Second, Autostart: a,
	}
	for _, f := range set {
		f(&o)
	}
	s := NewSupervisor(o)
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// waitEnded waits until the only worker's run loop returned.
func waitEnded(t *testing.T, s *Supervisor) {
	t.Helper()
	s.mu.Lock()
	var ended chan struct{}
	for _, w := range s.workers {
		ended = w.ended
	}
	s.mu.Unlock()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's run loop is still going")
	}
}

func instance(t *testing.T, s *Supervisor) InstanceView {
	t.Helper()
	list, _ := s.Instances()
	if len(list) != 1 {
		t.Fatalf("instances %+v", list)
	}
	return list[0]
}

// 003-AC9: a worker one protocol version older or newer than the supervisor runs, at the lower
// version. Two versions apart, it is refused with a message naming the supported range, and it is
// not restarted.
func TestProtocolSkew(t *testing.T) {
	room := Room{Base: "https://rooms.example", ID: "abc"}
	for _, tc := range []struct {
		proto int
		ok    bool
	}{{Proto - 1, true}, {Proto + 1, true}, {Proto - 2, false}, {Proto + 2, false}} {
		t.Run(strconv.Itoa(tc.proto), func(t *testing.T) {
			s := testSupervisor(t, strconv.Itoa(tc.proto), nil)
			s.SetRoom(context.Background(), room)
			if tc.ok {
				waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
				if in := instance(t, s); in.State != "running" || in.Proto != min(Proto, tc.proto) || in.Restarts != 0 {
					t.Fatalf("instance %+v", in)
				}
				if err := s.SetSettings(Settings{MaxModel: "sonnet"}); err != nil {
					t.Fatalf("request to the worker: %v", err)
				}
				return
			}
			waitFor(t, "the worker refused", func() bool { return instance(t, s).State == "refused" })
			// Not restarted: its run loop is over, so no backoff can bring it back.
			waitEnded(t, s)
			in := instance(t, s)
			want := "worker fake-" + strconv.Itoa(tc.proto) + " refused: protocol version " + strconv.Itoa(tc.proto) + " is outside the supported range " + strconv.Itoa(Proto-ProtoSkew) + " to " + strconv.Itoa(Proto+ProtoSkew)
			if in.Restarts != 0 || !strings.HasPrefix(in.Error, want) {
				t.Fatalf("instance %+v, want error %q", in, want)
			}
			if st := s.Status(); !strings.HasPrefix(st.Error, want) || st.Room != room.String() {
				t.Fatalf("page status %+v", st)
			}
			if err := s.SetSettings(Settings{}); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("request to a refused worker: %v", err)
			}
		})
	}
	t.Run("worker refuses", func(t *testing.T) {
		s := testSupervisor(t, "refuse", nil)
		// With no login item to manage, the page offers none.
		if _, err := s.Autostart(); err == nil {
			t.Fatal("autostart state with no autostart")
		}
		if err := s.SetAutostart(true); err == nil {
			t.Fatal("autostart set with no autostart")
		}
		s.SetRoom(context.Background(), room)
		waitFor(t, "the worker refusing", func() bool { return instance(t, s).State == "refused" })
		if in := instance(t, s); in.Error != "the worker refused this supervisor: fake worker: supervisor too old" {
			t.Fatalf("instance %+v", in)
		}
	})
}

// 003-AC9: the worker side refuses a supervisor two versions apart, and tells it why.
func TestWorkerRefusesSupervisor(t *testing.T) {
	for _, tc := range []struct {
		proto int
		ok    bool
	}{{Proto + 1, true}, {Proto - 1, true}, {Proto + 2, false}, {Proto - 2, false}} {
		supR, workerW := io.Pipe()
		workerR, supW := io.Pipe()
		sup := NewConn(supR, supW)
		got := make(chan Msg, 2)
		go func() {
			hello, _ := sup.Recv()
			got <- hello
			_ = sup.Send(Msg{Type: "config", Proto: tc.proto, Version: "v-other", Config: &WorkerConfig{Instance: "abc"}})
			m, err := sup.Recv()
			if err != nil {
				m = Msg{Type: "none"}
			}
			got <- m
		}()
		cfg, err := WorkerHandshake(NewConn(workerR, workerW), "abc", "v-test")
		if hello := <-got; hello.Type != "hello" || hello.Proto != Proto || hello.Instance != "abc" {
			t.Fatalf("hello %+v", hello)
		}
		if tc.ok {
			if err != nil || cfg.Instance != "abc" {
				t.Fatalf("proto %d: %+v %v", tc.proto, cfg, err)
			}
			_ = workerW.Close()
			<-got
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "refuses the supervisor v-other: protocol version "+strconv.Itoa(tc.proto)+" is outside") {
			t.Fatalf("proto %d: %v", tc.proto, err)
		}
		if m := <-got; m.Type != "refused" || !strings.Contains(m.Error, "outside the supported range "+strconv.Itoa(Proto-ProtoSkew)+" to "+strconv.Itoa(Proto+ProtoSkew)) {
			t.Fatalf("refusal %+v", m)
		}
	}
	// A supervisor that sends something else, or another instance's config, is refused too.
	for _, reply := range []Msg{{Type: "status"}, {Type: "refused", Error: "too old"}, {Type: "config", Proto: Proto, Config: &WorkerConfig{Instance: "other"}}} {
		supR, workerW := io.Pipe()
		workerR, supW := io.Pipe()
		sup := NewConn(supR, supW)
		go func() { _, _ = sup.Recv(); _ = sup.Send(reply) }()
		if _, err := WorkerHandshake(NewConn(workerR, workerW), "abc", "v-test"); err == nil {
			t.Fatalf("handshake passed on %+v", reply)
		}
	}
}

func TestNegotiate(t *testing.T) {
	for _, tc := range []struct{ own, peer, want int }{{1, 1, 1}, {1, 0, 0}, {1, 2, 1}, {5, 4, 4}} {
		if got, err := Negotiate(tc.own, tc.peer); err != nil || got != tc.want {
			t.Errorf("Negotiate(%d, %d) = %d %v", tc.own, tc.peer, got, err)
		}
	}
	for _, peer := range []int{-1, 3, 100} {
		if _, err := Negotiate(1, peer); err == nil {
			t.Errorf("Negotiate(1, %d) passed", peer)
		}
	}
}

// The page drives the supervisor's current instance, lists the instances, and turns autostart on
// and off; requests reach the worker over the protocol.
func TestSupervisorPage(t *testing.T) {
	a, r := newAutostart(t, "linux")
	var restarted atomic.Bool
	stderr := &lockedBuf{}
	s := testSupervisor(t, strconv.Itoa(Proto), &a, func(o *SupervisorOptions) { o.Restart = func() { restarted.Store(true) }; o.Stderr = stderr })
	e := newAdminEnv(t, s)
	tok := e.login(t)
	status := func() map[string]any {
		_, body := e.req(t, "GET", "/api/status", tok, "", nil)
		var v map[string]any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := status(); v["instance"] != "" || v["autostart"] != false || len(v["instances"].([]any)) != 0 {
		t.Fatalf("before any room: %v", v)
	}
	if code, body := e.req(t, "POST", "/api/settings", tok, `{"max_model":"sonnet"}`, nil); code != http.StatusBadRequest || !strings.Contains(body, "no room yet") {
		t.Fatalf("settings with no room: %d %s", code, body)
	}
	if code, body := e.req(t, "GET", "/api/denied", tok, "", nil); code != http.StatusOK || !strings.Contains(body, `"denied":[]`) {
		t.Fatalf("denied with no room: %d %s", code, body)
	}
	if code, _ := e.req(t, "POST", "/api/room", tok, `{"url":"https://rooms.example/r/abc"}`, nil); code != http.StatusNoContent {
		t.Fatalf("room: %d", code)
	}
	waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
	id, _ := InstanceID("https://rooms.example")
	if v := status(); v["instance"] != id {
		t.Fatalf("current instance: %v", v)
	}
	for path, body := range map[string]string{"/api/settings": `{"max_model":"sonnet"}`, "/api/allowlist": `{"allowlist":[]}`, "/api/recheck": ""} {
		if code, b := e.req(t, "POST", path, tok, body, nil); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", path, code, b)
		}
	}
	if code, _ := e.req(t, "POST", "/api/settings", tok, `{"max_model":"gpt"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad settings: %d", code)
	}
	if code, body := e.req(t, "GET", "/api/denied", tok, "", nil); code != http.StatusOK || !strings.Contains(body, `"denied":[]`) {
		t.Fatalf("denied: %d %s", code, body)
	}
	// The worker's update offer is not the supervisor's: with no binary store, there is none
	// (selfupdate_test.go covers the supervisor's own update).
	if code, _ := e.req(t, "POST", "/api/update", tok, "", nil); code != http.StatusBadGateway || restarted.Load() {
		t.Fatalf("update: %d, restarted %v", code, restarted.Load())
	}
	if st := s.Status(); st.Update.Available != "" || st.Version != "v-test" {
		t.Fatalf("status shows the worker's update or version: %+v", st)
	}
	waitFor(t, "the worker's log line", func() bool {
		return strings.Contains(stderr.String(), `{"instance":"`+id+`","msg":"fake_worker_ready"}`) && strings.Contains(strings.Join(s.o.Ring.Lines(), "\n"), "fake_worker_ready")
	})
	if code, _ := e.req(t, "POST", "/api/autostart", tok, `{"enabled":true}`, nil); code != http.StatusNoContent || status()["autostart"] != true {
		t.Fatalf("autostart on: %d", code)
	}
	r.fail = "systemctl --user disable"
	if code, body := e.req(t, "POST", "/api/autostart", tok, `{"enabled":false}`, nil); code != http.StatusBadGateway || !strings.Contains(body, "failed") || status()["autostart"] != true {
		t.Fatalf("autostart off failing: %d %s", code, body)
	}
	// A second room on the same server goes to the same worker; the instance keeps both.
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "def"})
	if in := instance(t, s); len(in.Rooms) != 2 || in.Rooms[1] != "https://rooms.example/r/def" {
		t.Fatalf("instance %+v", in)
	}
	insts, errs := LoadInstances(s.o.Paths)
	if len(errs) != 0 || len(insts) != 1 || len(insts[0].Rooms) != 2 {
		t.Fatalf("saved %+v %v", insts, errs)
	}
}

// A supervisor started again runs a worker for every instance it saved, and shows the newest.
func TestSupervisorRestoresInstances(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Config: dir, State: dir, Cache: dir}
	// A copied directory whose instance.json names another instance is skipped, not run twice.
	ida, _ := InstanceID("https://a.example")
	if err := saveInstance(p, Instance{ID: ida, URL: "https://a.example"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.InstanceConfig(ida), p.InstanceConfig("copy")); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"https://a.example", "https://b.example"} {
		id, _ := InstanceID(base)
		if err := saveInstance(p, Instance{ID: id, URL: base, Rooms: []string{base + "/r/abc"}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond) // distinct modification times
	}
	if err := os.MkdirAll(p.InstanceConfig("broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.InstanceConfig("broken")+"/instance.json", []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs := LoadInstances(p); len(errs) != 2 {
		t.Fatalf("errors %v", errs)
	}
	s := NewSupervisor(SupervisorOptions{Paths: p, Version: "v-test", Binary: ownBinary,
		Env: append(os.Environ(), "GRILL_ROOM_TEST_WORKER="+strconv.Itoa(Proto)), Stderr: io.Discard, Log: zap.NewNop()})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	defer func() { cancel(); s.Wait() }()
	idb, _ := InstanceID("https://b.example")
	waitFor(t, "both workers running", func() bool {
		list, current := s.Instances()
		return len(list) == 2 && list[0].State == "running" && list[1].State == "running" && current == idb
	})
}

// A worker binary that cannot be found or started is retried with backoff, and the page says why.
func TestSupervisorWorkerStartFails(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	s := NewSupervisor(SupervisorOptions{Paths: Paths{Config: dir, State: dir, Cache: dir}, Version: "v-test", Stderr: io.Discard, Log: zap.NewNop(),
		MaxBackoff: 50 * time.Millisecond,
		Binary: func(context.Context, Instance) (Resolved, error) {
			calls++
			if calls == 1 {
				return Resolved{}, errors.New("no release for this version")
			}
			return Resolved{Path: dir + "/missing-binary"}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	defer func() { cancel(); s.Wait() }()
	s.SetRoom(ctx, Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "a retried start", func() bool {
		in := instance(t, s)
		return in.Restarts >= 2 && strings.Contains(in.Error, "start worker")
	})
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// 003-AC8: a worker that crashes is restarted with backoff, and the sandboxes it left are removed
// first; a request to it fails until it runs again.
func TestSupervisorCleansUpAfterCrash(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	docker := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "container" && args[1] == "ls" {
			return []byte("c1\n"), nil
		}
		return nil, nil
	}
	s := testSupervisor(t, "crash", nil, func(o *SupervisorOptions) { o.Docker = docker })
	room := Room{Base: "https://rooms.example", ID: "abc"}
	s.SetRoom(context.Background(), room)
	waitFor(t, "two restarts", func() bool { return instance(t, s).Restarts >= 2 })
	id, _ := InstanceID(room.Base)
	filter := " --filter label=" + RootLabel(s.o.Worker.SandboxRoot) + " --filter label=" + InstanceLabel(id)
	name := "grill-room-" + SandboxName(s.o.Worker.SandboxRoot, room)
	want := []string{
		// Every object of the instance's sandboxes, its parallel ones included, by label.
		"container ls -aq" + filter, "container rm -f -v c1", "network ls -q" + filter, "volume ls -q" + filter,
		// The room's first sandbox by name, as workers before the labels left it.
		"rm -f -v " + name + " " + name + "-relay " + name + "-egress", "network rm " + name + " " + name + "-egress",
		"volume rm -f " + name + "-home " + name + "-workspace",
	}
	mu.Lock()
	defer mu.Unlock()
	// The first call is the start's Docker check, which finds no Docker here.
	if len(calls) < 1+len(want) || !slices.Equal(calls[1:1+len(want)], want) {
		t.Fatalf("docker calls %q", calls)
	}
	if _, err := (workerOps{s: s}).request(context.Background(), Msg{Type: "recheck"}); err == nil {
		t.Fatal("a request reached a crashed worker")
	}
}

// 003-AC6: the lock admits one supervisor per state dir, the socket hands a later launch's room
// over, and the launch only opens a link to a local page.
func TestLockAndHandoff(t *testing.T) {
	dir := t.TempDir()
	release, err := LockSupervisor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockSupervisor(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if _, err := Handoff(ctx, dir, ""); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("no socket yet: %v", err)
	}
	cancel()
	ln, err := ListenHandoff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	var mu sync.Mutex
	link := "http://127.0.0.1:1234/s/CODE"
	go ServeHandoff(ln, func(room string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, room)
		switch room {
		case "bad":
			return "", errors.New("room URL must end in /r/<room>")
		case "evil":
			return "https://evil.example/", nil
		}
		return link, nil
	}, zap.NewNop())
	for room, want := range map[string]string{"https://rooms.example/r/abc": link, "": link} {
		if l, err := Handoff(context.Background(), dir, room); err != nil || l != want {
			t.Fatalf("handoff %q: %q %v", room, l, err)
		}
	}
	if _, err := Handoff(context.Background(), dir, "bad"); err == nil || !strings.Contains(err.Error(), "must end in /r/<room>") {
		t.Fatalf("bad room: %v", err)
	}
	if _, err := Handoff(context.Background(), dir, "evil"); err == nil || !strings.Contains(err.Error(), "unexpected link") {
		t.Fatalf("foreign link: %v", err)
	}
	_ = ln.Close()
	release()
	// A released lock is free for the next supervisor, which opens the socket again.
	release, err = LockSupervisor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ln, err = ListenHandoff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("handed %q", got)
	}
}

// A worker that never answers fails the page's request after the timeout instead of hanging it.
func TestSupervisorRequestTimeout(t *testing.T) {
	s := testSupervisor(t, "mute", nil, func(o *SupervisorOptions) { o.RequestTimeout = 200 * time.Millisecond })
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
	start := time.Now()
	err := s.SetSettings(Settings{})
	if err == nil || !strings.Contains(err.Error(), "did not answer: context deadline exceeded") || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
	w := s.currentWorker()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(w.pending) != 0 {
		t.Fatalf("pending requests left: %v", w.pending)
	}
}

// A worker dying with a request in flight fails it at once, and the page says it restarts. A room
// handed over meanwhile is paired by the next worker.
func TestSupervisorWorkerDiesMidRequest(t *testing.T) {
	s := testSupervisor(t, "mute", nil, func(o *SupervisorOptions) { o.MaxBackoff = 2 * time.Second })
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
	errc := make(chan error, 1)
	go func() { errc <- s.SetSettings(Settings{}) }()
	w := s.currentWorker()
	waitFor(t, "the request in flight", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(w.pending) == 1 })
	p, err := os.FindProcess(instance(t, s).PID)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Kill()
	select {
	case err := <-errc:
		if err == nil || err.Error() != "the worker stopped" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request outlived its worker")
	}
	waitFor(t, "restarting", func() bool { return strings.Contains(s.Status().Error, "restarting in") })
	other := Room{Base: "https://rooms.example", ID: "def"}
	s.SetRoom(context.Background(), other)
	waitFor(t, "the next worker pairing the new room", func() bool { st := s.Status(); return st.Connection == "connected" && st.Room == other.String() })
}

func TestLockAndHandoffFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LockSupervisor(filepath.Join(file, "state")); err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("state dir under a file: %v", err)
	}
	// Windows hands off through a named pipe, which never lives under the state dir.
	if runtime.GOOS != "windows" {
		if _, err := ListenHandoff(context.Background(), filepath.Join(file, "state")); err == nil {
			t.Fatal("a socket under a file")
		}
	}
	// A hello that cannot be sent fails the handshake.
	r, w := io.Pipe()
	_ = r.Close()
	if _, err := WorkerHandshake(NewConn(strings.NewReader(""), w), "abc", "v-test"); err == nil || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("handshake on a closed pipe: %v", err)
	}
	if _, err := WorkerHandshake(NewConn(strings.NewReader(""), io.Discard), "abc", "v-test"); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("handshake with no answer: %v", err)
	}
}

// A room on a URL with no host, or an instance that cannot be saved, is logged; the page stays up.
func TestSupervisorSetRoomFailures(t *testing.T) {
	s := testSupervisor(t, strconv.Itoa(Proto), nil)
	s.SetRoom(context.Background(), Room{Base: "no host", ID: "abc"})
	if list, _ := s.Instances(); len(list) != 0 {
		t.Fatalf("instances %+v", list)
	}
	// The instance cannot be saved: its worker still runs for this session.
	if err := os.WriteFile(filepath.Join(s.o.Paths.Config, "instances"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
}

// A worker that never says hello is killed and restarted; one that talks before hello is cut off.
func TestSupervisorHandshakeFailures(t *testing.T) {
	for mode, c := range map[string]struct {
		want  string
		hello time.Duration
	}{
		"silent": {"no hello from the worker", 200 * time.Millisecond},
		// The default timeout: a short one killed this worker before it spoke on a loaded runner.
		"early": {`the worker sent "status" before hello`, 0},
	} {
		t.Run(mode, func(t *testing.T) {
			s := testSupervisor(t, mode, nil, func(o *SupervisorOptions) {
				if c.hello != 0 {
					o.HelloTimeout = c.hello
				}
			})
			s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
			waitFor(t, "a restart", func() bool { in := instance(t, s); return in.Restarts >= 1 && strings.Contains(in.Error, c.want) })
		})
	}
}

// lastAccess is the last access the fake worker logged it was given.
func lastAccess(s *Supervisor) *Access {
	lines := s.o.Ring.Lines()
	for i := len(lines) - 1; i >= 0; i-- {
		if j := strings.Index(lines[i], `{"access":`); j >= 0 && strings.Contains(lines[i], "fake_access") {
			var v struct{ Access *Access }
			if json.Unmarshal([]byte(lines[i][j:]), &v) == nil {
				return v.Access
			}
		}
	}
	return nil
}

// 003-AC13 003-AC14 003-AC15: the worker gets its rooms' proxies in its config and again whenever
// the mapping changes; a cut-off reaches it and is kept; accounts in use cannot be removed.
func TestSupervisorAccounts(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	var accounts *Accounts
	s := testSupervisor(t, strconv.Itoa(Proto), nil, func(o *SupervisorOptions) {
		up, _ := url.Parse("http://127.0.0.1:1")
		var err error
		accounts, err = LoadAccounts(AccountsOptions{Paths: o.Paths, ClaudeConfigDir: claude, Upstream: up, Docker: desktopDocker,
			Keyring: &memKeyring{m: map[string]string{}}, Log: zap.NewNop()})
		if err != nil {
			t.Fatal(err)
		}
		o.Accounts = accounts
	})
	room := Room{Base: "https://rooms.example", ID: "abc"}
	s.SetRoom(context.Background(), room)
	waitFor(t, "the room's proxy in the config", func() bool {
		a := lastAccess(s)
		return a != nil && strings.HasPrefix(a.Proxies[room.String()].Addr, "host-gateway:") && a.Proxies[room.String()].Bearer != ""
	})
	id := instance(t, s).ID
	if in := instance(t, s); in.Account != "default" || in.CutOff {
		t.Fatalf("instance %+v", in)
	}
	if err := s.AddAccount(context.Background(), Account{Name: "work", Kind: KindSetupToken}, setupToken); err != nil {
		t.Fatal(err)
	}
	first := lastAccess(s).Proxies[room.String()]
	if err := s.SetAccount(id, room.String(), "work"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the room moved to the work account's proxy", func() bool {
		a := lastAccess(s)
		return a != nil && a.Proxies[room.String()].Addr != "" && a.Proxies[room.String()].Addr != first.Addr
	})
	if in := instance(t, s); in.RoomAccounts[room.String()] != "work" || in.Account != "default" {
		t.Fatalf("instance %+v", in)
	}
	for _, tc := range []struct {
		room, account, want string
	}{
		{"", "nobody", "no such account"},
		{"https://rooms.example/r/other", "work", "no such room"},
	} {
		if err := s.SetAccount(id, tc.room, tc.account); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("SetAccount(%q, %q) = %v", tc.room, tc.account, err)
		}
	}
	for _, name := range []string{"work", "default"} {
		if err := s.RemoveAccount(name); err == nil || !strings.Contains(err.Error(), "uses this account") {
			t.Errorf("removed %s while in use: %v", name, err)
		}
	}
	if err := s.SetCutOff(id, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cut-off", func() bool { a := lastAccess(s); return a != nil && a.CutOff && len(a.Proxies) == 0 })
	if in := instance(t, s); !in.CutOff {
		t.Fatalf("instance %+v", in)
	}
	saved, err := loadInstance(s.o.Paths.InstanceConfig(id))
	if err != nil || !saved.CutOff || saved.RoomAccounts[room.String()] != "work" {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if err := s.SetCutOff(id, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "let back", func() bool { a := lastAccess(s); return a != nil && !a.CutOff && a.Proxies[room.String()].Bearer != "" })
	if err := s.SetAccount(id, room.String(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAccount("work"); err != nil {
		t.Fatal(err)
	}
}

// 003-AC12 003-AC13 003-AC15: the page adds and removes accounts, maps a room, and cuts a worker
// off. A setup token entered once never comes back from the page, nor reaches its logs.
func TestSupervisorPageAccounts(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	s := testSupervisor(t, strconv.Itoa(Proto), nil, func(o *SupervisorOptions) {
		up, _ := url.Parse("http://127.0.0.1:1")
		a, err := LoadAccounts(AccountsOptions{Paths: o.Paths, ClaudeConfigDir: claude, Upstream: up, Docker: desktopDocker,
			Keyring: &memKeyring{m: map[string]string{}}, Log: zap.NewNop()})
		if err != nil {
			t.Fatal(err)
		}
		o.Accounts = a
	})
	e := newAdminEnv(t, s)
	tok := e.login(t)
	post := func(path, body string, want int) {
		t.Helper()
		if code, b := e.req(t, "POST", path, tok, body, nil); code != want {
			t.Fatalf("%s %s: %d %s", path, body, code, b)
		}
	}
	post("/api/accounts", `{"name":"work","kind":"setup_token","token":"`+setupToken+`"}`, http.StatusNoContent)
	post("/api/accounts", `{"name":"half","kind":"setup_token","token":"`+setupToken[:60]+`"}`, http.StatusBadRequest)
	post("/api/accounts", `{"name":"other","kind":"config_dir","config_dir":"`+filepath.ToSlash(claude)+`"}`, http.StatusNoContent)
	post("/api/accounts/probe", `{"name":"default"}`, http.StatusBadRequest)
	post("/api/room", `{"url":"https://rooms.example/r/abc"}`, http.StatusNoContent)
	waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
	id, _ := InstanceID("https://rooms.example")
	post("/api/instance-account", `{"instance":"`+id+`","room":"https://rooms.example/r/abc","account":"work"}`, http.StatusNoContent)
	post("/api/instance-account", `{"instance":"`+id+`","account":"other"}`, http.StatusNoContent)
	post("/api/cut-off", `{"instance":"`+id+`","cut_off":true}`, http.StatusNoContent)
	_, body := e.req(t, "GET", "/api/status", tok, "", nil)
	var v struct {
		Accounts  []AccountView  `json:"accounts"`
		Instances []InstanceView `json:"instances"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Accounts) != 3 || len(v.Instances) != 1 || v.Instances[0].Account != "other" || !v.Instances[0].CutOff ||
		v.Instances[0].RoomAccounts["https://rooms.example/r/abc"] != "work" {
		t.Fatalf("status %s", body)
	}
	post("/api/accounts/remove", `{"name":"work"}`, http.StatusBadRequest)
	post("/api/instance-account", `{"instance":"`+id+`","room":"https://rooms.example/r/abc","account":""}`, http.StatusNoContent)
	post("/api/accounts/remove", `{"name":"work"}`, http.StatusNoContent)
	_, body = e.req(t, "GET", "/api/status", tok, "", nil)
	if strings.Contains(body, setupToken) || strings.Contains(body, "cfg-secret") || strings.Contains(strings.Join(s.o.Ring.Lines(), "\n"), setupToken) {
		t.Fatal("a token reached the page")
	}
}

// 003-AC18: the supervisor grants a worker's slots under the global cap the page saved, queues
// the one past it, and grants it once the first is released. The cap survives a restart, within
// its bounds.
func TestSupervisorSlots(t *testing.T) {
	s := testSupervisor(t, "slots", nil)
	for _, bad := range []int{0, MaxTurnsLimit + 1} {
		if err := s.SetMaxTurns(bad); err == nil {
			t.Fatalf("cap %d accepted", bad)
		}
	}
	if err := s.SetMaxTurns(1); err != nil {
		t.Fatal(err)
	}
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "the second slot granted", func() bool { v := s.Concurrency(); return v.Granted == 2 && v.Waited == 1 })
	v := s.Concurrency()
	if v.MaxTurns != 1 || len(v.Running) != 1 || v.Running[0].Turn != 11 || v.Running[0].Room != "https://rooms.example/r/abc" || len(v.Queued) != 0 {
		t.Fatalf("concurrency %+v", v)
	}
	slot := regexp.MustCompile(`fake_slot \w+ \d`)
	var lines []string
	// The worker logs each grant after it reads it, so the ring trails the supervisor's counters.
	waitFor(t, "the worker's three slot lines", func() bool {
		lines = nil
		for _, l := range s.o.Ring.Lines() {
			lines = append(lines, slot.FindAllString(l, -1)...)
		}
		return len(lines) >= 3
	})
	if !slices.Equal(lines, []string{"fake_slot grant 1", "fake_slot wait 2", "fake_slot grant 2"}) {
		t.Fatalf("the worker heard %q", lines)
	}
	if set, err := loadSupervisorSettings(s.o.Paths.supervisorSettings()); err != nil || set.maxTurns() != 1 || set.UpdateChannel != "github" {
		t.Fatalf("saved %+v %v", set, err)
	}
	if err := s.SetUpdateChannel("github", ""); err != nil {
		t.Fatal(err)
	}
	if set, _ := loadSupervisorSettings(s.o.Paths.supervisorSettings()); set.MaxTurns != 1 {
		t.Fatalf("the update channel dropped the cap: %+v", set)
	}
	if err := os.WriteFile(s.o.Paths.supervisorSettings(), []byte(`{"update_channel":"github","max_turns":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := loadSupervisorSettings(s.o.Paths.supervisorSettings()); err == nil || set.maxTurns() != DefaultMaxTurns {
		t.Fatalf("a cap out of bounds loaded: %+v %v", set, err)
	}
}

// A slow Docker never holds up the supervisor's start, and so its page: the orphan cleanup runs
// beside it, and the workers start once it is done.
func TestSupervisorStartsBeforeCleanup(t *testing.T) {
	release := make(chan struct{})
	cleaning := make(chan struct{}, 1)
	docker := func(ctx context.Context, _ io.Reader, args ...string) ([]byte, error) {
		select {
		case cleaning <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("no docker here")
	}
	// Start returns while Docker still blocks: it does not wait for the cleanup.
	s := testSupervisor(t, "ok", nil, func(o *SupervisorOptions) { o.Docker = docker })
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	select {
	case <-cleaning:
	case <-time.After(10 * time.Second):
		t.Fatal("the orphan cleanup never called Docker")
	}
	// The workers wait on s.cleaned, still shut while the cleanup blocks.
	select {
	case <-s.cleaned:
		t.Fatal("the cleanup counted as done while Docker blocks")
	default:
	}
	if st := instance(t, s).State; st != "starting" {
		t.Fatalf("a worker ran before the cleanup: %s", st)
	}
	close(release)
	waitFor(t, "the worker running", func() bool { return instance(t, s).State == "running" })
}

// 008-AC15: "Add a room" on the page hands any link inside a room to the supervisor, as a second
// launch does: before any room and while another is connected. A link that is no room's says why.
func TestAddRoomFromPage(t *testing.T) {
	s := testSupervisor(t, strconv.Itoa(Proto), nil)
	e := newAdminEnv(t, s)
	tok := e.login(t)
	for _, tc := range []struct{ body, want string }{
		{`{"url":"https://rooms.example/admin"}`, "is not a room link"},
		{`{"url":"https://rooms.example/elsewhere/x"}`, "is not a room link"},
		{`{"url":"https://rooms.example/r/"}`, "is not a room link"},
	} {
		if code, body := e.req(t, "POST", "/api/room", tok, tc.body, nil); code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Fatalf("%s: %d %s", tc.body, code, body)
		}
	}
	if v, _ := s.Instances(); len(v) != 0 {
		t.Fatalf("a refused link added %+v", v)
	}
	rooms := func() []string {
		v, _ := s.Instances()
		if len(v) != 1 {
			return nil
		}
		return v[0].Rooms
	}
	if code, body := e.req(t, "POST", "/api/room", tok, `{"url":"https://rooms.example/r/abc/docs"}`, nil); code != http.StatusNoContent {
		t.Fatalf("first room: %d %s", code, body)
	}
	waitFor(t, "the worker running", func() bool { return s.Status().Connection == "connected" })
	if code, body := e.req(t, "POST", "/api/room", tok, `{"url":"https://rooms.example/r/def/docs/7?x=1#m2"}`, nil); code != http.StatusNoContent {
		t.Fatalf("second room: %d %s", code, body)
	}
	waitFor(t, "both rooms on the instance", func() bool {
		return slices.Equal(rooms(), []string{"https://rooms.example/r/abc", "https://rooms.example/r/def"})
	})
}
