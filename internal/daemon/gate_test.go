package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// gateLog records what the gate tells each slot.
type gateLog struct {
	mu  sync.Mutex
	got []string
}

func (l *gateLog) send(name string) func(Msg) error {
	return func(m Msg) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.got = append(l.got, name+":"+m.Type)
		return nil
	}
}

func (l *gateLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.got
	l.got = nil
	return out
}

// 003-AC18: past the cap, turns wait in one FIFO across instances; a freed slot goes to the
// first waiting. A waiting turn that gives up leaves the queue, a worker that exits frees its
// slots, and a higher cap grants at once.
func TestGate(t *testing.T) {
	var l gateLog
	g := newGate(1)
	a1, a2, b1, b2 := slotKey{"a", 1}, slotKey{"a", 2}, slotKey{"b", 1}, slotKey{"b", 2}
	g.acquire(a1, "r1", 1, l.send("a1"))
	g.acquire(b1, "r3", 3, l.send("b1"))
	g.acquire(a2, "r2", 2, l.send("a2"))
	g.acquire(b2, "r4", 4, l.send("b2"))
	if got := l.take(); !slices.Equal(got, []string{"a1:grant", "b1:wait", "a2:wait", "b2:wait"}) {
		t.Fatalf("acquire %q", got)
	}
	if v := g.view(); len(v.Running) != 1 || len(v.Queued) != 3 || v.Queued[0].Room != "r3" || v.MaxTurns != 1 {
		t.Fatalf("view %+v", v)
	}
	// a2 gives up waiting; a1 ends: b1 was first in line.
	g.release(a2)
	g.release(a1)
	if got := l.take(); !slices.Equal(got, []string{"b1:grant"}) {
		t.Fatalf("after a1 %q", got)
	}
	// A release of a slot no one holds changes nothing.
	g.release(slotKey{"a", 9})
	if got := l.take(); len(got) != 0 {
		t.Fatalf("stray release %q", got)
	}
	// b's worker exits: its slot is freed and its queued turn forgotten.
	g.acquire(a2, "r2", 5, l.send("a2"))
	g.drop("b")
	if got := l.take(); !slices.Equal(got, []string{"a2:wait", "a2:grant"}) {
		t.Fatalf("after b exited %q", got)
	}
	g.acquire(b1, "r3", 6, l.send("b1"))
	g.acquire(b2, "r4", 7, l.send("b2"))
	g.setMax(3)
	if got := l.take(); !slices.Equal(got, []string{"b1:wait", "b2:wait", "b1:grant", "b2:grant"}) {
		t.Fatalf("a higher cap %q", got)
	}
	v := g.view()
	if len(v.Running) != 3 || len(v.Queued) != 0 || v.Granted != 5 || v.Waited != 4 || v.WaitMaxMS > v.WaitTotalMS {
		t.Fatalf("view %+v", v)
	}
}

// A worker whose pipe broke never holds a slot: the next in line gets it.
func TestGateSkipsAGoneWorker(t *testing.T) {
	var l gateLog
	g := newGate(1)
	g.acquire(slotKey{"a", 1}, "r1", 1, l.send("a1"))
	g.acquire(slotKey{"b", 1}, "r2", 2, func(m Msg) error {
		if m.Type == "grant" {
			return errors.New("broken pipe")
		}
		return nil
	})
	g.acquire(slotKey{"c", 1}, "r3", 3, l.send("c1"))
	g.release(slotKey{"a", 1})
	if got := l.take(); !slices.Equal(got, []string{"a1:grant", "c1:wait", "c1:grant"}) {
		t.Fatalf("calls %q", got)
	}
	if v := g.view(); len(v.Running) != 1 || v.Running[0].Room != "r3" {
		t.Fatalf("view %+v", v)
	}
}

// 003-AC18: the page sets the global cap on a supervisor and shows it with the queue waits; a
// daemon on its own has no such route.
func TestAdminMaxTurns(t *testing.T) {
	s := testSupervisor(t, "0", nil)
	e := newAdminEnv(t, s)
	tok := e.login(t)
	if code, body := e.req(t, "POST", "/api/max-turns", tok, `{"max_turns":0}`, nil); code != http.StatusBadRequest || !strings.Contains(body, "1 to 16") {
		t.Fatalf("cap 0: %d %s", code, body)
	}
	if code, body := e.req(t, "POST", "/api/max-turns", tok, `{"max_turns":3}`, nil); code != http.StatusNoContent {
		t.Fatalf("cap 3: %d %s", code, body)
	}
	_, body := e.req(t, "GET", "/api/status", tok, "", nil)
	var st struct {
		Concurrency ConcurrencyView `json:"concurrency"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || st.Concurrency.MaxTurns != 3 {
		t.Fatalf("status %s %v", body, err)
	}
	solo := newAdminEnv(t, newTestDaemon(t, nil))
	if code, _ := solo.req(t, "POST", "/api/max-turns", solo.login(t), `{"max_turns":3}`, nil); code != http.StatusNotFound {
		t.Fatalf("a daemon on its own took a cap: %d", code)
	}
}

// 003-AC20: a room's first sandbox keeps its pre-phase-5 name, the others get a suffix; the root
// label is stable per state root and differs between two.
func TestSandboxNamesAndLabels(t *testing.T) {
	r := Room{Base: "https://rooms.example", ID: "abc"}
	if SlotSandboxName("/a", r, 0) != SandboxName("/a", r) || SlotSandboxName("/a", r, 2) != SandboxName("/a", r)+"-2" {
		t.Fatalf("names %q %q", SlotSandboxName("/a", r, 0), SlotSandboxName("/a", r, 2))
	}
	if RootLabel("/a") != "grill-room-root=6a50dc858413" || RootLabel("/a") == RootLabel("/b") {
		t.Fatalf("labels %q %q", RootLabel("/a"), RootLabel("/b"))
	}
}
