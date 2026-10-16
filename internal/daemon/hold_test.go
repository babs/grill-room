package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/sandbox"
)

// holdRoom is a room run whose server answers holds with decisions, by domain, once release
// closes; asked counts its questions.
type holdRoom struct {
	r       *roomRun
	asked   atomic.Int64
	release chan struct{}
	mu      sync.Mutex
	seen    []map[string]any
}

func newHoldRoom(t *testing.T, decisions map[string]string) *holdRoom {
	t.Helper()
	h := &holdRoom{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rooms/abc/daemon/egress-hold" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.asked.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.seen = append(h.seen, body)
		h.mu.Unlock()
		<-h.release
		_ = json.NewEncoder(w).Encode(map[string]string{"decision": decisions[body["domain"].(string)]})
	}))
	t.Cleanup(func() { srv.Close() })
	d := newTestDaemon(t, nil)
	room := Room{Base: srv.URL, ID: "abc"}
	if err := d.o.Tokens.Set(room, "tok"); err != nil {
		t.Fatal(err)
	}
	h.r = &roomRun{d: d, room: room, ctx: t.Context()}
	h.r.st.Connection = "connected"
	return h
}

// 014-AC29: a question per domain, its answer for every request that joined it: allowed is OK,
// dismissed, timeout or nobody refuse; a listed domain and a room not connected are answered
// without asking; the turn's end report leaves out what a hold put before the room.
func TestHoldDecision(t *testing.T) {
	h := newHoldRoom(t, map[string]string{"example.com": "allowed", "docs.rs": "dismissed", "slow.example": "timeout", "lone.example": "nobody"})
	ctx := t.Context()
	var wg sync.WaitGroup
	got := make([]bool, 3)
	for i := range 3 {
		wg.Go(func() { got[i] = h.r.holdDecision(ctx, "example.com", "Grill") })
	}
	for h.asked.Load() == 0 {
		time.Sleep(5 * time.Millisecond) // waits for the first question, bounded by the test's timeout
	}
	close(h.release)
	wg.Wait()
	if !slices.Equal(got, []bool{true, true, true}) || h.asked.Load() != 1 {
		t.Fatalf("joined requests: %v, %d questions", got, h.asked.Load())
	}
	h.mu.Lock()
	first := h.seen[0]
	h.mu.Unlock()
	if first["persona"] != "Grill" || first["seconds"] != float64(HoldDefault) {
		t.Fatalf("question %v", first)
	}
	for domain, want := range map[string]bool{"docs.rs": false, "slow.example": false, "lone.example": false} {
		if h.r.holdDecision(ctx, domain, "Grill") != want {
			t.Errorf("%s: want %v", domain, want)
		}
	}
	asked := h.asked.Load()
	if !h.r.holdDecision(ctx, "pypi.org", "Grill") || h.r.holdDecision(ctx, "api.anthropic.com", "Grill") || h.asked.Load() != asked {
		t.Fatalf("listed or Anthropic: %d questions", h.asked.Load()-asked)
	}
	h.r.st.Connection = "offline"
	if h.r.holdDecision(ctx, "other.example", "Grill") || h.asked.Load() != asked {
		t.Fatal("a room offline was asked")
	}
	since := time.Now().Add(-time.Minute)
	if got := h.r.reportedSince([]string{"example.com", "docs.rs", "never.example"}, since); !slices.Equal(got, []string{"never.example"}) {
		t.Fatalf("turn report %v", got)
	}
}

// 014-AC29: the hold socket speaks squid's concurrent helper protocol: each "<id> <host> -" line is
// answered "<id> OK" or "<id> ERR", out of order; the hold seconds are the host's, within bounds.
func TestHoldSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the hold runs with a native Docker on Linux only")
	}
	h := newHoldRoom(t, map[string]string{"example.com": "allowed"})
	close(h.release)
	// Relative, as a worker's state dir may be: the folder comes back absolute.
	t.Chdir(t.TempDir())
	h.r.d.o.HoldRoot = "hold"
	h.r.d.st.Egress.HoldSeconds = 99
	sl := &sandboxSlot{persona: "Scribe", busy: true}
	dir := h.r.startHold(0, sl)
	if !filepath.IsAbs(dir) {
		t.Fatalf("hold folder %q", dir)
	}
	t.Cleanup(func() { _ = sl.hold.Close() })
	c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", filepath.Join(dir, sandbox.HoldSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "7 example.com -\n8 refused.example -\n9 api.anthropic.com -\n")
	sc := bufio.NewScanner(c)
	answers := map[string]string{}
	for len(answers) < 3 && sc.Scan() {
		id, a, _ := strings.Cut(sc.Text(), " ")
		answers[id] = a
	}
	if answers["7"] != "OK" || answers["8"] != "ERR" || answers["9"] != "ERR" {
		t.Fatalf("answers %v", answers)
	}
	// Between turns, refused without a question.
	h.r.agentMu.Lock()
	sl.busy = false
	h.r.agentMu.Unlock()
	fmt.Fprint(c, "10 idle.example -\n")
	if !sc.Scan() || sc.Text() != "10 ERR" {
		t.Fatalf("idle answer %q", sc.Text())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 2 || h.seen[0]["seconds"] != float64(HoldMax) || h.seen[0]["persona"] != "Scribe" {
		t.Fatalf("questions %v", h.seen)
	}
	h.r.d.st.Docker.Desktop = true
	if dir := h.r.startHold(1, &sandboxSlot{}); dir != "" {
		t.Fatal("a hold under Docker Desktop")
	}
}

// 014-AC29: the seconds stay within bounds.
func TestHoldSeconds(t *testing.T) {
	for in, want := range map[int]int{0: HoldDefault, 10: HoldMin, 45: 45, 99: HoldMax} {
		if got := holdSeconds(in); got != want {
			t.Errorf("%d: %d, want %d", in, got, want)
		}
	}
}

// 014-AC29: the sockets' root is short whatever the state dir, private to the user, and one per
// state dir.
func TestHoldRootFor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	deep := filepath.Join(t.TempDir(), strings.Repeat("deep/", 30))
	root := HoldRootFor(deep)
	if p := filepath.Join(HoldDir(root, Room{Base: "https://h", ID: "a"}, 9), sandbox.HoldSocket); len(p) > maxSocketPath {
		t.Fatalf("socket path %d long: %s", len(p), p)
	}
	if HoldRootFor(deep) != root || HoldRootFor(deep+"x") == root {
		t.Fatal("roots not one per state dir")
	}
	if _, err := listenHold(HoldDir(root, Room{Base: "https://h", ID: "a"}, 0), filepath.Join(HoldDir(root, Room{Base: "https://h", ID: "a"}, 0), sandbox.HoldSocket)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %v %v", fi.Mode(), err)
	}
}

// 014-AC29: a policy change waits while a held request is being answered, then applies: squid's
// reconfigure would drop the lookup the approval just answered.
func TestHoldDefersPolicy(t *testing.T) {
	d := newTestDaemon(t, nil)
	ag := &fakeAgent{}
	r := &roomRun{d: d, room: Room{Base: "https://h", ID: "a"}, ctx: t.Context(), sandboxes: []*sandboxSlot{{ag: ag}}}
	d.mu.Lock()
	d.rooms[r.room.String()] = r
	d.mu.Unlock()
	r.holdBegin()
	if err := d.SetAllowlist(t.Context(), []string{"example.com"}); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	early := ag.allow
	ag.mu.Unlock()
	if early != nil {
		t.Fatalf("applied while answering: %v", early)
	}
	r.holdEnd()
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if !slices.Equal(ag.allow, []string{"example.com"}) {
		t.Fatalf("not applied after: %v", ag.allow)
	}
}
