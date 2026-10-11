package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
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
// never answers, "silent" never says hello, "early" talks before it.
func fakeWorker(mode string) int {
	proto, err := strconv.Atoi(mode)
	if err != nil {
		proto = Proto
	}
	conn := NewConn(os.Stdin, os.Stdout)
	switch mode {
	case "silent":
		time.Sleep(time.Hour)
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
	_ = conn.Send(Msg{Type: "status", Status: &Status{Host: "fake-worker", Room: m.Config.Pair, Pairing: "paired", Connection: "connected",
		Update: Update{Available: "v9.9.9"}}})
	_ = conn.Send(Msg{Type: "log", Log: []byte(`{"msg":"fake_worker_ready"}`)})
	for {
		r, err := conn.Recv()
		if err != nil || r.Type == "stop" {
			return 0
		}
		if mode != "mute" {
			_ = conn.Send(Msg{Type: "reply", ID: r.ID})
		}
	}
}

func testSupervisor(t *testing.T, mode string, a *Autostart, set ...func(*SupervisorOptions)) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	o := SupervisorOptions{
		Paths: Paths{Config: dir, State: dir, Cache: dir}, Version: "v-test", Worker: WorkerConfig{Host: "laptop", SandboxRoot: dir},
		Binary: func(string) (string, error) { return os.Args[0], nil }, Env: append(os.Environ(), "GRILL_ROOM_TEST_WORKER="+mode),
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
				waitFor(t, "the worker running", func() bool { return s.Status().Host == "fake-worker" })
				if in := instance(t, s); in.State != "running" || in.Proto != min(Proto, tc.proto) || in.Restarts != 0 {
					t.Fatalf("instance %+v", in)
				}
				if err := s.SetSettings(Settings{MaxModel: "sonnet"}); err != nil {
					t.Fatalf("request to the worker: %v", err)
				}
				return
			}
			waitFor(t, "the worker refused", func() bool { return instance(t, s).State == "refused" })
			time.Sleep(300 * time.Millisecond) // three backoffs: a restart would show
			in := instance(t, s)
			want := "worker fake-" + strconv.Itoa(tc.proto) + " refused: protocol version " + strconv.Itoa(tc.proto) + " is outside the supported range 0 to 2"
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
		if m := <-got; m.Type != "refused" || !strings.Contains(m.Error, "outside the supported range 0 to 2") {
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
	waitFor(t, "the worker running", func() bool { return s.Status().Host == "fake-worker" })
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
	if code, _ := e.req(t, "POST", "/api/update", tok, "", nil); code != http.StatusNoContent || !restarted.Load() {
		t.Fatalf("update: %d, restarted %v", code, restarted.Load())
	}
	waitFor(t, "the worker's log line", func() bool {
		return strings.Contains(stderr.String(), `{"msg":"fake_worker_ready"}`) && strings.Contains(strings.Join(s.o.Ring.Lines(), "\n"), "fake_worker_ready")
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
	s := NewSupervisor(SupervisorOptions{Paths: p, Version: "v-test", Binary: func(string) (string, error) { return os.Args[0], nil },
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
		Binary: func(string) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("no release for this version")
			}
			return dir + "/missing-binary", nil
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
		return nil, nil
	}
	s := testSupervisor(t, "crash", nil, func(o *SupervisorOptions) { o.Docker = docker })
	room := Room{Base: "https://rooms.example", ID: "abc"}
	s.SetRoom(context.Background(), room)
	waitFor(t, "two restarts", func() bool { return instance(t, s).Restarts >= 2 })
	name := "grill-room-" + SandboxName(s.o.Worker.SandboxRoot, room)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 3 || calls[0] != "rm -f -v "+name+" "+name+"-relay "+name+"-egress" || calls[1] != "network rm "+name+" "+name+"-egress" ||
		calls[2] != "volume rm -f "+name+"-home "+name+"-workspace" {
		t.Fatalf("docker calls %q", calls)
	}
	if _, err := s.requestCurrent(context.Background(), Msg{Type: "recheck"}); err == nil {
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
	waitFor(t, "the worker running", func() bool { return s.Status().Host == "fake-worker" })
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
	waitFor(t, "the worker running", func() bool { return s.Status().Host == "fake-worker" })
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
	waitFor(t, "the next worker pairing the new room", func() bool { st := s.Status(); return st.Host == "fake-worker" && st.Room == other.String() })
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
	waitFor(t, "the worker running", func() bool { return s.Status().Host == "fake-worker" })
}

// A worker that never says hello is killed and restarted; one that talks before hello is cut off.
func TestSupervisorHandshakeFailures(t *testing.T) {
	for mode, want := range map[string]string{"silent": "no hello from the worker", "early": `the worker sent "status" before hello`} {
		t.Run(mode, func(t *testing.T) {
			s := testSupervisor(t, mode, nil, func(o *SupervisorOptions) { o.HelloTimeout = 200 * time.Millisecond })
			s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
			waitFor(t, "a restart", func() bool { in := instance(t, s); return in.Restarts >= 1 && strings.Contains(in.Error, want) })
		})
	}
}
