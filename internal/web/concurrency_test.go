package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

func (e *env) turnState(t *testing.T, r *turnRoom, d *testDaemon, id int64, queued bool) int {
	t.Helper()
	code, _ := e.do(t, "POST", r.daemonPath("turns/"+strconv.FormatInt(id, 10)+"/state"), d.tok, map[string]bool{"host_queued": queued})
	return code
}

// 003-AC18: a turn the host's daemon holds in its queue shows as such in the room, and the
// server's backstop leaves it alone until it runs: its clock starts then. Only the turn's own
// daemon tells.
func TestHostQueuedTurn(t *testing.T) {
	old := turnGrace
	turnGrace = 0
	t.Cleanup(func() { turnGrace = old })
	e := newEnvOpts(t, Options{TurnTimeout: 150 * time.Millisecond})
	// The backstops run when the test fires them, on a clock the test moves.
	backstops := make(chan func(), 8)
	e.srv.afterFunc = func(_ time.Duration, f func()) { backstops <- f }
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	e.srv.now = func() time.Time { return time.Unix(0, clock.Load()) }
	fire := func() {
		t.Helper()
		select {
		case f := <-backstops:
			f()
		case <-time.After(5 * time.Second):
			t.Fatal("no backstop armed")
		}
	}
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	tn := e.press(t, r, r.alice, tok, false)
	if code := e.turnState(t, r, r.alice, tn.ID, true); code != http.StatusNoContent {
		t.Fatalf("queued: %d", code)
	}
	st := e.waitAgent(t, r.room, tok, func(st agentState) bool { return len(st.Running) == 1 && st.Running[0].HostQueued })
	if st.Running[0].ID != tn.ID {
		t.Fatalf("running %+v", st.Running)
	}
	other := e.daemonFor(t, r.room, "Bob")
	if code := e.turnState(t, r, other, tn.ID, false); code != http.StatusConflict {
		t.Fatalf("another daemon's state: %d", code)
	}
	if code := e.turnState(t, r, r.alice, tn.ID+100, false); code != http.StatusNotFound {
		t.Fatalf("unknown turn: %d", code)
	}
	// The backstop armed at the start goes off while the turn waits in the host's queue: well past
	// the timeout, it leaves the turn alone.
	clock.Add(int64(time.Hour))
	fire()
	if got, _ := e.st.Turn(t.Context(), r.room, tn.ID); got.Status != store.TurnRunning || !got.HostQueued {
		t.Fatalf("the backstop failed a queued turn: %+v", got)
	}
	asked := time.Now()
	if code := e.turnState(t, r, r.alice, tn.ID, false); code != http.StatusNoContent {
		t.Fatalf("running: %d", code)
	}
	if got, _ := e.st.Turn(t.Context(), r.room, tn.ID); got.HostQueued || got.StartedAt.Before(asked) {
		t.Fatalf("left the queue: %+v", got)
	}
	// Leaving the queue armed a new backstop; once the timeout is over it fails the turn.
	clock.Store(time.Now().Add(time.Hour).UnixNano())
	fire()
	st = e.waitAgent(t, r.room, tok, func(st agentState) bool { return st.Turn.Status == store.TurnFailed })
	if st.Turn.Error != "timed out after 150ms" || len(st.Running) != 0 {
		t.Fatalf("after the run: %+v", st)
	}
}

// 003-AC20: only the host switches parallel mode, and the room is told.
func TestParallelModeIsTheHosts(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	bob := e.daemonFor(t, r.room, "Bob")
	path := "/api/rooms/" + r.room + "/agent"
	if code, _ := e.do(t, "PATCH", path, bob.p.Token, map[string]bool{"parallel_turns": true}); code != http.StatusForbidden {
		t.Fatalf("a guest switched it: %d", code)
	}
	if code, _ := e.do(t, "PATCH", path, r.alice.p.Token, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("no field: %d", code)
	}
	for range 2 {
		if code, b := e.do(t, "PATCH", path, r.alice.p.Token, map[string]bool{"parallel_turns": true}); code != http.StatusNoContent {
			t.Fatalf("host: %d %s", code, b)
		}
	}
	if st := e.agent(t, r.room, bob.p.Token); !st.ParallelTurns {
		t.Fatal("parallel mode not on")
	}
	msgs, _ := e.st.Messages(t.Context(), r.room, 0, 100)
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "let the personas run at the same time") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d messages about the switch, want 1", n)
	}
}

// 003-AC21: the workspace pins the agent documents' revisions at its first fetch: a revision or a
// document another turn writes afterwards stays out of it. An edit of a document revised since
// then is a conflict: the turn fails and runs again at once, on the new version.
func TestWorkspacePinsAndConflicts(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	tn := e.press(t, r, r.alice, tok, false)
	if ws := e.workspace(t, r, r.alice, tn.ID); ws["docs/synthesis.md"].data != "# Synthesis\n\nThe quick brown fox.\n" {
		t.Fatalf("workspace %+v", ws)
	}
	ctx := t.Context()
	if _, err := e.st.AddRevision(ctx, r.room, r.doc, "# Synthesis\n\nA lazy dog.\n", "Secretary"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateAgentDocument(ctx, r.room, "later.md", "md", "# Later\n", "Secretary"); err != nil {
		t.Fatal(err)
	}
	ws := e.workspace(t, r, r.alice, tn.ID)
	if ws["docs/synthesis.md"].data != "# Synthesis\n\nThe quick brown fox.\n" || ws["docs/later.md"] != (tarFile{}) {
		t.Fatalf("the workspace moved after its first fetch: %+v", ws)
	}
	code, b := e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, Synthesis: "edited",
		Changes: []turn.Change{{Path: "docs/synthesis.md", Content: []byte("# Synthesis\n\nThe quick red fox.\n")}}})
	if code != http.StatusConflict || !strings.Contains(string(b), "conflict on synthesis.md") {
		t.Fatalf("result: %d %s", code, b)
	}
	if got, _ := e.st.Turn(ctx, r.room, tn.ID); got.Status != store.TurnFailed || got.Error != "conflict on synthesis.md" {
		t.Fatalf("rejected turn %+v", got)
	}
	ev := nextEvent(t, r.alice.events, "turn")
	st := e.agent(t, r.room, tok)
	if len(st.Running) != 1 || fmt.Sprintf(`{"id":%d}`, st.Running[0].ID) != ev.data || st.Running[0].RerunOf != tn.ID ||
		st.Running[0].Cause != "re-run after a conflict on synthesis.md" {
		t.Fatalf("re-run %+v, event %+v", st.Running, ev)
	}
	if ws := e.workspace(t, r, r.alice, st.Running[0].ID); ws["docs/synthesis.md"].data != "# Synthesis\n\nA lazy dog.\n" || ws["docs/later.md"].data != "# Later\n" {
		t.Fatalf("re-run workspace %+v", ws)
	}
	if e.srv.TurnConflicts("rerun") != 1 || e.srv.TurnConflicts("failed") != 0 {
		t.Fatalf("conflict counters %d %d", e.srv.TurnConflicts("rerun"), e.srv.TurnConflicts("failed"))
	}
}

// 003-AC20: in parallel mode a press waits only for its own persona: another persona starts at
// once, the same one queues, and a second press of it is refused.
func TestParallelPresses(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.addPersona(t, r, tok, e.templates(t, r, tok)[1].ID)
	if code, b := e.do(t, "PATCH", "/api/rooms/"+r.room+"/agent", tok, map[string]bool{"parallel_turns": true}); code != http.StatusNoContent {
		t.Fatalf("parallel: %d %s", code, b)
	}
	e.press(t, r, r.alice, tok, false)
	press := func(persona int64, want int) {
		t.Helper()
		if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/turns", tok, map[string]int64{"persona_id": persona}); code != want {
			t.Fatalf("press %d: %d %s", persona, code, b)
		}
	}
	press(sec.ID, http.StatusCreated)
	press(r.grill.ID, http.StatusAccepted)
	press(r.grill.ID, http.StatusConflict)
	if st := e.agent(t, r.room, tok); len(st.Running) != 2 || len(st.Queue) != 1 {
		t.Fatalf("running %+v, queue %+v", st.Running, st.Queue)
	}
}

// 003-AC18: a daemon fetches the workspace once its turn runs: a turn still shown queued, its
// state change lost, runs from then on, under the backstop.
func TestWorkspaceFetchEndsHostQueue(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	if code := e.turnState(t, r, r.alice, tn.ID, true); code != http.StatusNoContent {
		t.Fatalf("queued: %d", code)
	}
	e.workspace(t, r, r.alice, tn.ID)
	if got, _ := e.st.Turn(t.Context(), r.room, tn.ID); got.HostQueued {
		t.Fatalf("still queued after its workspace fetch: %+v", got)
	}
}

// 003-AC26: each daemon's stream says whether it hosts the room's agent and, when that changes,
// the room's parallel mode; the job names why the turn runs, for the host's monitoring (003-AC27).
func TestDaemonRoomEvent(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	bob := e.daemonFor(t, r.room, "Bob")
	e.connect(t, r.room, bob, true)
	want := func(d *testDaemon, data string) {
		t.Helper()
		for {
			if ev := nextEvent(t, d.events, "room"); ev.data == data {
				return
			}
		}
	}
	want(r.alice, `{"host":true,"parallel_turns":false}`)
	want(bob, `{"host":false,"parallel_turns":false}`)
	if code, b := e.do(t, "PATCH", "/api/rooms/"+r.room+"/agent", r.alice.p.Token, map[string]bool{"parallel_turns": true}); code != http.StatusNoContent {
		t.Fatalf("parallel: %d %s", code, b)
	}
	want(r.alice, `{"host":true,"parallel_turns":true}`)
	want(bob, `{"host":false,"parallel_turns":true}`)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	if job := e.job(t, r, r.alice, tn.ID); job.Cause != "pressed by Alice" {
		t.Fatalf("job cause %q", job.Cause)
	}
}

// 003-AC22: a persona's trigger reads "every round" for one round, "every N rounds" otherwise.
func TestEveryRounds(t *testing.T) {
	for n, want := range map[int]string{1: "every round", 2: "every 2 rounds", 10: "every 10 rounds"} {
		if got := everyRounds(n); got != want {
			t.Errorf("everyRounds(%d) = %q", n, got)
		}
		if got := triggerText(&store.RoomPersona{EveryN: n}); got != "on demand and "+want {
			t.Errorf("triggerText(%d) = %q", n, got)
		}
	}
	if got := triggerText(&store.RoomPersona{}); got != "on demand" {
		t.Errorf("triggerText(0) = %q", got)
	}
}
