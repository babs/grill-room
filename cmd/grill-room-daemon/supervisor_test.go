package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/web"
)

// TestMain lets the process tests run this test binary as the daemon, which runs it again as its
// workers: the whole supervisor, worker and protocol path, with no build step.
func TestMain(m *testing.M) {
	if os.Getenv("GRILL_ROOM_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type roomServer struct {
	ts    *httptest.Server
	st    *store.Store
	room  string
	admin string
}

func newRoomServer(t *testing.T) *roomServer {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "room.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rs := &roomServer{admin: web.NewToken(), st: st}
	rs.ts = httptest.NewUnstartedServer(nil)
	rs.ts.Start()
	t.Cleanup(rs.ts.Close)
	srv := web.New(bus.New(st), web.HashToken(rs.admin), rs.ts.URL, web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20, MaxAttachmentBytes: 1 << 20}, zap.NewNop())
	rs.ts.Config.Handler = srv.Handler()
	var room store.Room
	rs.call(t, "POST", "/api/rooms", rs.admin, map[string]string{"name": "R"}, http.StatusCreated, &room)
	rs.room = room.ID
	return rs
}

func (rs *roomServer) url() string { return rs.ts.URL + "/r/" + rs.room }

func (rs *roomServer) call(t *testing.T, method, path, token string, body any, want int, out any) {
	t.Helper()
	code, b := do(t, method, rs.ts.URL+path, token, "", body)
	if code != want {
		t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
}

func (rs *roomServer) join(t *testing.T, name string) string {
	t.Helper()
	var inv struct{ Token string }
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/invites", rs.admin, nil, http.StatusCreated, &inv)
	var j struct{ Token string }
	rs.call(t, "POST", "/api/join", "", map[string]string{"invite": inv.Token, "name": name}, http.StatusCreated, &j)
	return j.Token
}

// do sends a JSON request; origin, when set, is the admin page's own.
func do(t *testing.T, method, url, token, origin string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// proc is the daemon binary running as a child process.
type proc struct {
	cmd  *exec.Cmd
	out  *syncBuf
	done chan struct{}
	err  error
}

// daemonEnv is a throwaway state dir and a fake Claude login: no Claude request.
func daemonEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"proc-test-fake","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"GRILL_ROOM_DAEMON_STATE_DIR=" + filepath.Join(dir, "state"), "CLAUDE_CONFIG_DIR=" + dir, "GRILL_ROOM_SANDBOX_IMAGE=test-image"}
}

func startProc(t *testing.T, env []string, args ...string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...) //nolint:gosec,noctx // this test binary, run as the daemon
	cmd.Env = append(append(os.Environ(), "GRILL_ROOM_TEST_MAIN=1", "GRILL_ROOM_DAEMON_BROWSER=none", "LOG_LEVEL=info"), env...)
	p := &proc{cmd: cmd, out: &syncBuf{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
		if t.Failed() {
			t.Logf("daemon output:\n%s", p.out.String())
		}
	})
	return p
}

// exit waits for the process to end and returns its exit code.
func (p *proc) exit(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("still running:\n%s", p.out.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

var linkRe = regexp.MustCompile(`Admin page: (http://127\.0\.0\.1:\d+)/s/(\S+)`)

// page is an admin page session.
type page struct{ base, token string }

// open trades the latest link p printed for a page session.
func (p *proc) open(t *testing.T) page {
	t.Helper()
	var m [][]string
	waitFor(t, "an admin link", func() bool { m = linkRe.FindAllStringSubmatch(p.out.String(), -1); return len(m) > 0 })
	last := m[len(m)-1]
	return session(t, last[1], last[2])
}

func session(t *testing.T, base, code string) page {
	t.Helper()
	status, b := do(t, "POST", base+"/api/session", "", base, map[string]string{"code": code})
	var s struct{ Token string }
	_ = json.Unmarshal(b, &s)
	if status != http.StatusOK || s.Token == "" {
		t.Fatalf("session: %d %s", status, b)
	}
	return page{base: base, token: s.Token}
}

type pageStatus struct {
	Status    daemon.Status         `json:"status"`
	Instances []daemon.InstanceView `json:"instances"`
	Instance  string                `json:"instance"`
}

func (pg page) status(t *testing.T) pageStatus {
	t.Helper()
	code, b := do(t, "GET", pg.base+"/api/status", pg.token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, b)
	}
	var s pageStatus
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (pg page) post(t *testing.T, path string, body any) int {
	t.Helper()
	code, _ := do(t, "POST", pg.base+path, pg.token, pg.base, body)
	return code
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond) // polls cond, bounded by the deadline
	}
}

// pair approves the pending pairing the page shows, as a participant in the room would.
func (rs *roomServer) pair(t *testing.T, pg page, participant string) {
	t.Helper()
	var approve string
	waitFor(t, "a pending pairing", func() bool { approve = pg.status(t).Status.ApproveURL; return approve != "" })
	id := approve[strings.Index(approve, "=")+1:]
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+id, participant, map[string]bool{"approve": true}, http.StatusOK, nil)
	waitFor(t, "connected", func() bool {
		s := pg.status(t).Status
		return s.Pairing == "paired" && s.Connection == "connected"
	})
}

func onlyInstance(t *testing.T, s pageStatus) daemon.InstanceView {
	t.Helper()
	if len(s.Instances) != 1 {
		t.Fatalf("instances: %+v", s.Instances)
	}
	return s.Instances[0]
}

// 003-AC6, 005-AC46: with a supervisor running, a launch with a room link starts no second
// supervisor: it hands the room over through the local socket, prints the running supervisor's
// admin page and exits. A launch with no room, or `grill-room-daemon admin`, only gives a fresh
// link to that page; with no supervisor running, `admin` says how to start one.
func TestSecondLaunchHandsOver(t *testing.T) {
	rs := newRoomServer(t)
	env := daemonEnv(t)
	none := startProc(t, env, "admin")
	if code := none.exit(t); code != 1 || !strings.Contains(none.out.String(), "no AI host app runs for") ||
		!strings.Contains(none.out.String(), "grill-room-daemon <room-url>") {
		t.Fatalf("admin with no supervisor: exit %d:\n%s", code, none.out.String())
	}
	first := startProc(t, env)
	pg := first.open(t)
	if s := pg.status(t); s.Instance != "" || len(s.Instances) != 0 {
		t.Fatalf("instances before any room: %+v", s)
	}

	second := startProc(t, env, rs.url())
	if code := second.exit(t); code != 0 {
		t.Fatalf("second launch exit %d:\n%s", code, second.out.String())
	}
	if !strings.Contains(second.out.String(), "handed "+rs.url()+" to the running supervisor") || strings.Contains(second.out.String(), `"startup"`) {
		t.Fatalf("second launch output:\n%s", second.out.String())
	}
	// The link it printed opens the first supervisor's page, which now pairs with the room.
	handed := second.open(t)
	if handed.base != pg.base {
		t.Fatalf("handed link %s, page %s", handed.base, pg.base)
	}
	waitFor(t, "the room pairing on the first supervisor", func() bool {
		s := handed.status(t)
		return s.Status.Room == rs.url() && s.Status.Pairing == "pending" && len(s.Instances) == 1
	})

	for _, args := range [][]string{nil, {"admin"}} {
		again := startProc(t, env, args...)
		if code := again.exit(t); code != 0 || !strings.Contains(again.out.String(), "the AI host app is already running") ||
			strings.Contains(again.out.String(), `"startup"`) {
			t.Fatalf("launch %v: exit %d:\n%s", args, code, again.out.String())
		}
		if p := again.open(t); p.base != pg.base {
			t.Fatalf("launch %v: link %s, page %s", args, p.base, pg.base)
		}
	}
	if pg.post(t, "/api/stop", nil) != http.StatusNoContent {
		t.Fatal("stop refused")
	}
	if code := first.exit(t); code != 0 {
		t.Fatalf("supervisor exit %d", code)
	}
}

// 003-AC8: a worker that crashes is restarted, and its room reconnects with its stored token.
// Stopping the supervisor stops the worker, and the supervisor exits cleanly.
func TestWorkerCrashRestartsAndRoomReconnects(t *testing.T) {
	// Each phase's time goes to the test log, shown on failure: a Windows run once ran out of time
	// waiting for the reconnect, with no trace of which phase took it.
	start := time.Now()
	phase := func(what string) { t.Logf("%7.3fs %s", time.Since(start).Seconds(), what) }
	rs := newRoomServer(t)
	alice := rs.join(t, "Alice")
	sup := startProc(t, daemonEnv(t), rs.url())
	pg := sup.open(t)
	phase("page open")
	rs.pair(t, pg, alice)
	phase("paired")
	before := onlyInstance(t, pg.status(t))
	if before.State != "running" || before.PID == 0 || before.Proto != daemon.Proto || before.Room != rs.url() {
		t.Fatalf("instance: %+v", before)
	}
	worker, err := os.FindProcess(before.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Kill(); err != nil {
		t.Fatal(err)
	}
	phase("worker killed")
	seen := map[string]bool{}
	once := func(what string, ok bool) {
		if ok && !seen[what] {
			seen[what] = true
			phase(what)
		}
	}
	waitFor(t, "the restarted worker connected again", func() bool {
		s := pg.status(t)
		in := onlyInstance(t, s)
		once("restart counted", in.Restarts == 1)
		once("new worker running", in.PID != 0 && in.PID != before.PID)
		once("new worker paired", in.PID != before.PID && s.Status.Pairing == "paired")
		return in.Restarts == 1 && in.PID != 0 && in.PID != before.PID && s.Status.Pairing == "paired" && s.Status.Connection == "connected"
	})
	phase("reconnected")
	after := onlyInstance(t, pg.status(t))
	if pg.post(t, "/api/stop", nil) != http.StatusNoContent {
		t.Fatal("stop refused")
	}
	if code := sup.exit(t); code != 0 {
		t.Fatalf("supervisor exit %d:\n%s", code, sup.out.String())
	}
	if alive(after.PID) {
		t.Fatalf("worker %d still runs after the supervisor stopped", after.PID)
	}
	for _, want := range []string{`"worker_exited"`, `"worker_stopping"`, `"worker_shutdown"`} {
		if !strings.Contains(sup.out.String(), want) {
			t.Errorf("no %s in the logs", want)
		}
	}
	if strings.Contains(sup.out.String(), "proc-test-fake") {
		t.Fatal("the Claude token reached the logs")
	}
}

// sandboxObjects counts the Docker objects of sandbox name.
func sandboxObjects(t *testing.T, name string) int {
	t.Helper()
	n := 0
	for _, args := range [][]string{{"ps", "-aq"}, {"network", "ls", "-q"}, {"volume", "ls", "-q"}} {
		out, err := exec.Command("docker", append(args, "--filter", "label="+sandbox.Label+"="+name)...).Output() //nolint:gosec,noctx // fixed test arguments
		if err != nil {
			t.Fatal(err)
		}
		n += len(strings.Fields(string(out)))
	}
	return n
}

// online waits for the room to hear its host can run turns: the supervisor's page shows the worker
// able to host before the worker's first heartbeat reaches the room, and a press before that is
// refused.
func (rs *roomServer) online(t *testing.T, tok string) {
	t.Helper()
	waitFor(t, "the room sees its host online", func() bool {
		var v struct{ Online bool }
		rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", tok, nil, http.StatusOK, &v)
		return v.Online
	})
}

// turn posts a message, presses the agent button and waits for the turn to end.
func (rs *roomServer) turn(t *testing.T, tok string) {
	t.Helper()
	rs.online(t, tok)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", tok, map[string]string{"body": "hello FAKE_MARK"}, http.StatusCreated, nil)
	var started struct{ ID int64 }
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", tok, map[string]bool{}, http.StatusCreated, &started)
	waitFor(t, "the turn to end", func() bool {
		var v struct {
			Turn *struct {
				ID     int64
				Status string
			}
		}
		rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", tok, nil, http.StatusOK, &v)
		return v.Turn != nil && v.Turn.ID == started.ID && v.Turn.Status != "running"
	})
}

// 003-AC8 in real Docker with the fake claude: a crashed worker's sandbox is removed before it
// restarts, and stopping the supervisor removes the sandbox of the worker that replaced it.
func TestDockerSupervisorRemovesSandboxes(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	rs := newRoomServer(t)
	alice := rs.join(t, "Alice")
	env := append(daemonEnv(t), "GRILL_ROOM_SANDBOX_IMAGE="+image)
	stateDir := strings.TrimPrefix(env[0], "GRILL_ROOM_DAEMON_STATE_DIR=")
	room, _ := daemon.ParseRoomURL(rs.url())
	name := daemon.SandboxName(stateDir, room)
	sup := startProc(t, env, rs.url())
	pg := sup.open(t)
	rs.pair(t, pg, alice)
	waitFor(t, "can host", func() bool { return pg.status(t).Status.CanHost })

	rs.turn(t, alice)
	if sandboxObjects(t, name) == 0 {
		t.Fatal("no sandbox after a turn")
	}
	pid := onlyInstance(t, pg.status(t)).PID
	worker, _ := os.FindProcess(pid)
	if err := worker.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the crashed worker's sandbox removed", func() bool { return sandboxObjects(t, name) == 0 })
	waitFor(t, "the restarted worker connected", func() bool {
		s := pg.status(t)
		return onlyInstance(t, s).PID != pid && s.Status.Connection == "connected" && s.Status.CanHost
	})

	rs.turn(t, alice)
	if sandboxObjects(t, name) == 0 {
		t.Fatal("no sandbox after the second turn")
	}
	if pg.post(t, "/api/stop", nil) != http.StatusNoContent {
		t.Fatal("stop refused")
	}
	if code := sup.exit(t); code != 0 {
		t.Fatalf("supervisor exit %d", code)
	}
	if n := sandboxObjects(t, name); n != 0 {
		t.Fatalf("%d sandbox objects left after the supervisor stopped", n)
	}
}
