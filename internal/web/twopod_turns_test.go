package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// both runs a on pod A and b on pod B at once and waits for both.
func both(a, b func()) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a() }()
	go func() { defer wg.Done(); b() }()
	wg.Wait()
}

// turnsOf returns the room's running turns and queue, read from the shared database.
func turnsOf(t *testing.T, e *env, room string) ([]store.Turn, []store.QueuedTurn) {
	t.Helper()
	running, err := e.st.RunningTurns(t.Context(), room)
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.st.Queue(t.Context(), room)
	if err != nil {
		t.Fatal(err)
	}
	return running, q
}

// failRunning ends the room's running turns in the store alone: no drain follows.
func failRunning(t *testing.T, e *env, room string) {
	t.Helper()
	running, _ := turnsOf(t, e, room)
	for _, tn := range running {
		if _, err := e.st.FailTurn(t.Context(), room, tn.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

// I7, 017-AC11: presses on pods A and B at once start one turn and queue the other, every round:
// without the room's turn lock both see an idle room and the second start answers 409.
func TestTwoPodsConcurrentPresses(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	tok := r.alice.p.Token
	for i := range 10 {
		var ca, cb int
		var ba, bb []byte
		press := func(e *env, code *int, body *[]byte) func() {
			return func() { *code, *body = e.do(t, "POST", r.path("/turns"), tok, map[string]bool{}) }
		}
		both(press(a, &ca, &ba), press(b, &cb, &bb))
		if min(ca, cb) != http.StatusCreated || max(ca, cb) != http.StatusAccepted {
			t.Fatalf("round %d: A %d %s, B %d %s; want one 201 and one 202", i, ca, ba, cb, bb)
		}
		running, q := turnsOf(t, a, r.room)
		if len(running) != 1 || len(q) != 1 {
			t.Fatalf("round %d: running %+v, queue %+v", i, running, q)
		}
		if err := a.st.CancelQueued(t.Context(), r.room, q[0].ID, r.alice.p.Participant.ID, true); err != nil {
			t.Fatal(err)
		}
		failRunning(t, a, r.room)
	}
}

// startedOnce fails unless the room ran want turns and lost no queued one to a failed start.
func startedOnce(t *testing.T, e *env, room string, want int) {
	t.Helper()
	msgs, err := e.st.Messages(t.Context(), room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Body, "could not start") {
			t.Fatalf("a queued turn was lost: %q", m.Body)
		}
	}
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM turns WHERE room_id = $1`, room).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%d turns, want %d", n, want)
	}
}

// I7: drains on pods A and B at once pop each queued turn exactly once: one starts, the other
// waits for the next drain. Without the lock each pod pops one, the second start fails and its
// press is lost with "could not start".
func TestTwoPodsConcurrentDrains(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	tok := r.alice.p.Token
	sec := a.addPersona(t, r, tok, a.templates(t, r, tok)[1].ID)
	ctx := t.Context()
	started := 0
	for i := range 5 {
		for j, p := range []int64{r.grill.ID, sec.ID, r.grill.ID} {
			want := http.StatusAccepted
			if j == 0 {
				want = http.StatusCreated
			}
			if code, body := b.do(t, "POST", r.path("/turns"), tok, map[string]int64{"persona_id": p}); code != want {
				t.Fatalf("round %d: press %d: %d %s", i, p, code, body)
			}
		}
		started++
		for left := 1; left >= 0; left-- {
			failRunning(t, a, r.room)
			both(func() { a.srv.drain(ctx, r.room) }, func() { b.srv.drain(ctx, r.room) })
			started++
			if running, q := turnsOf(t, a, r.room); len(running) != 1 || len(q) != left {
				t.Fatalf("round %d: running %+v, queue %+v; want 1 and %d", i, running, q, left)
			}
		}
		failRunning(t, a, r.room)
		both(func() { a.srv.drain(ctx, r.room) }, func() { b.srv.drain(ctx, r.room) })
		if running, q := turnsOf(t, a, r.room); len(running) != 0 || len(q) != 0 {
			t.Fatalf("round %d: drained an empty queue: running %+v, queue %+v", i, running, q)
		}
	}
	startedOnce(t, a, r.room, started)
}

// I7: an "every voice line" link triggered on pods A and B at once queues and starts its persona
// once. Without the lock, one pod finds it due between the other's pop and start, queues it again,
// and that entry is lost to a failed start.
func TestTwoPodsAutoLinkOnce(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	tok := r.alice.p.Token
	sec := a.secretary(t, r, tok, map[string]any{"every_voice": 1})
	ctx := t.Context()
	for i := range 10 {
		// The chain budget would pause the links after a few rounds.
		if err := a.st.ResetChain(ctx, r.room, store.ResetButton); err != nil {
			t.Fatal(err)
		}
		if err := a.bus.Post(ctx, &store.Message{RoomID: r.room, Kind: store.KindVoice, Source: store.VoiceSourceServer,
			ParticipantID: r.alice.p.Participant.ID, Sender: "Alice", Body: "a line"}); err != nil {
			t.Fatal(err)
		}
		// Several tries per pod widen the race: the persona is due again only after another line.
		trigger := func(e *env) func() {
			return func() {
				for range 10 {
					e.srv.voiceTriggers(ctx, r.room)
				}
			}
		}
		both(trigger(a), trigger(b))
		running, q := turnsOf(t, a, r.room)
		if len(running) != 1 || running[0].PersonaID != sec.ID || len(q) != 0 {
			t.Fatalf("round %d: running %+v, queue %+v; want one turn of %d", i, running, q, sec.ID)
		}
		failRunning(t, a, r.room)
	}
	startedOnce(t, a, r.room, 10)
}

// I7: a press whose wait for the room's turn lock, which pod B holds, fails server-side (as at
// lock_timeout) answers the usual 500 and starts or queues nothing.
func TestTwoPodsPressLockWaitFails(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	release, _, err := b.st.LockRoom(t.Context(), store.LockTurns, r.room, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan int)
	go func() {
		code, _ := a.do(t, "POST", r.path("/turns"), r.alice.p.Token, map[string]bool{})
		done <- code
	}()
	db := a.sqlDB(t)
	canceled := func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(pg_cancel_backend(pid)) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
			t.Error(err)
		}
		return n > 0
	}
	waitFor(t, canceled)
	if code := <-done; code != http.StatusInternalServerError {
		t.Fatalf("press: %d", code)
	}
	if running, q := turnsOf(t, a, r.room); len(running) != 0 || len(q) != 0 {
		t.Fatalf("running %+v, queue %+v", running, q)
	}
}

// I26: a plugin fetch in progress on pod A makes a fetch of the same room on pod B answer 409;
// once it ends, B fetches.
func TestTwoPodsOnePluginFetch(t *testing.T) {
	gs := newGitServer(t)
	a, b := newPair(t, pluginOpts(gs))
	r := newTurnRoom(t, a)
	a.appoint(t, r.room, r.alice.p.Participant.ID)
	_, url := marketRepo(t, gs).commit("skills.git")
	check := func() (int, []byte) {
		return b.do(t, "POST", r.path("/plugins/check"), r.alice.p.Token, map[string]any{"url": url})
	}
	w := httptest.NewRecorder()
	release, ok := a.srv.fetchSlot(context.Background(), w, r.room)
	if !ok {
		t.Fatalf("slot on A: %d %s", w.Code, w.Body)
	}
	if code, body := check(); code != http.StatusConflict {
		release()
		t.Fatalf("B during A's fetch: %d %s", code, body)
	}
	release()
	if code, body := check(); code != http.StatusOK {
		t.Fatalf("B after A's fetch: %d %s", code, body)
	}
}

// I7: while pod B holds room x's turn lock, pod A's drain of x waits for it outside turnMu: a drain
// of room y on A goes through meanwhile, and x's goes on once B lets go.
func TestTwoPodsLockWaitFreesOtherRooms(t *testing.T) {
	a, b := newPair(t, testOptions)
	release, _, err := b.st.LockRoom(t.Context(), store.LockTurns, "x", false)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	free := func() { once.Do(release) }
	defer free()
	x := make(chan struct{})
	go func() { a.srv.drain(context.Background(), "x"); close(x) }()
	db := a.sqlDB(t)
	waitFor(t, func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
			t.Error(err)
		}
		return n > 0
	})
	y := make(chan struct{})
	go func() { a.srv.drain(context.Background(), "y"); close(y) }()
	select {
	case <-y:
	case <-x:
		t.Fatal("drain of x did not wait for B's lock")
	case <-time.After(10 * time.Second):
		t.Fatal("drain of y waited behind x's lock")
	}
	free()
	<-x
}

// While pod B holds room x's turn lock, 20 drains of x on pod A pin one pool connection, not 20:
// the pod's other queries go on.
func TestTwoPodsLockWaitsPinOneConn(t *testing.T) {
	a, b := newPair(t, testOptions)
	release, _, err := b.st.LockRoom(t.Context(), store.LockTurns, "x", false)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	free := func() { once.Do(release) }
	defer free()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer free()
	for range 20 {
		wg.Go(func() { a.srv.drain(context.Background(), "x") })
	}
	db := a.sqlDB(t)
	// Every drain waits: behind x's guard, or, without one, each on a connection of its own until the
	// pool runs out.
	waitFor(t, func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
			t.Error(err)
		}
		a.srv.turnWaits.mu.Lock()
		defer a.srv.turnWaits.mu.Unlock()
		g := a.srv.turnWaits.m["x"]
		return n >= 16 || g != nil && g.n == 20
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := a.st.Room(ctx, "x"); err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a read on A while its drains wait: %v", err)
	}
	free()
	wg.Wait()
	if n := len(a.srv.turnWaits.m); n != 0 {
		t.Errorf("%d guards left once every drain ended", n)
	}
}

// While pod B holds room x's turn lock, waiters on pod A queued behind x's guard each give up
// within one bound of their start, not one bound per waiter ahead, and leave no guard behind.
func TestTwoPodsLockWaitsBounded(t *testing.T) {
	a, b := newPair(t, testOptions)
	release, _, err := b.st.LockRoom(t.Context(), store.LockTurns, "x", false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	const bound = time.Second
	a.srv.turnWait = bound
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			start := time.Now()
			unlock, err := a.srv.lockTurns(context.Background(), "x")
			if err == nil {
				unlock()
				t.Errorf("waiter %d got x's lock while B holds it", i)
				return
			}
			// The bound is the point here: a guard that ignores it waits a whole lock_timeout per waiter.
			if took := time.Since(start); took > 2*bound {
				t.Errorf("waiter %d gave up after %v, want about %v: %v", i, took, bound, err)
			}
		})
	}
	wg.Wait()
	if n := len(a.srv.turnWaits.m); n != 0 {
		t.Errorf("%d guards left once every waiter gave up", n)
	}
}
