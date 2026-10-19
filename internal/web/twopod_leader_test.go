package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// fastLeader makes followers try the leader lock, and leaders check it, every 20 ms.
func fastLeader(t *testing.T) {
	t.Helper()
	old := leaderEvery
	leaderEvery = 20 * time.Millisecond
	t.Cleanup(func() { leaderEvery = old })
}

// lead runs e's Lead until stop, which waits for it to end; t's end stops it too.
func lead(t *testing.T, e *env) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.srv.Lead(ctx); close(done) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

// terminateLeader kills e's leader lock session from outside, as a lost connection would.
func terminateLeader(t *testing.T, e *env) {
	t.Helper()
	db, err := sql.Open("pgx", e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = current_database()`, store.LeaderApplication+" "+e.bus.Pod()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminated %d sessions: %v", n, err)
	}
}

// A killed lock session: a follower takes the lock within one interval and the old leader stops its
// loops within one (017-AC17's 15 s). A leader cut off without a FIN keeps the lock until
// PostgreSQL drops its idle session, then a follower needs one interval more: 20 s at most.
func TestLeaderMoveBounds(t *testing.T) {
	if 2*leaderEvery > 15*time.Second {
		t.Fatalf("leaderEvery %s: a move after a killed session may take %s", leaderEvery, 2*leaderEvery)
	}
	if d := store.LeaderIdle(leaderEvery) + leaderEvery; d > 20*time.Second {
		t.Fatalf("a move after a leader lost without a FIN may take %s", d)
	}
}

// 017-AC17, 017-AC20: of two pods, one leads and runs every singleton loop, and only it exports the
// shared-state gauges. Killing its lock session moves the leadership and the loops to the other.
func TestTwoPodsLeader(t *testing.T) {
	fastLeader(t)
	a, b := newPair(t, voicePodOptions(t))
	// running counts each pod's singleton loops running now, the real ones under a wrapper.
	running := map[*env]*atomic.Int32{a: {}, b: {}}
	for _, e := range []*env{a, b} {
		n := running[e]
		for i, loop := range e.srv.singletons {
			e.srv.singletons[i] = func(ctx context.Context) {
				n.Add(1)
				defer n.Add(-1)
				loop(ctx)
			}
		}
	}
	all := int32(len(a.srv.singletons))
	if all != 5 {
		t.Fatalf("%d singletons, want PurgeAudit, PurgeProfiles, RunTimers, PushLoop, SweepTurns", all)
	}
	lead(t, a)
	lead(t, b)
	leads := func(l, f *env) func() bool {
		return func() bool {
			return l.srv.Leader() && !f.srv.Leader() && running[l].Load() == all && running[f].Load() == 0
		}
	}
	waitFor(t, func() bool { return leads(a, b)() || leads(b, a)() })
	l, f := a, b
	if b.srv.Leader() {
		l, f = b, a
	}
	if code, out := f.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "w1", "rooms": []string{}}); code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", code, out)
	}
	l.srv.refreshGauges()
	f.srv.refreshGauges()
	if nl, nf := l.srv.STTWorkers(), f.srv.STTWorkers(); nl != 1 || nf != 0 {
		t.Fatalf("STT workers: leader %d, follower %d; want 1, 0", nl, nf)
	}

	terminateLeader(t, l)
	waitFor(t, leads(f, l))
}

// 017-AC17: a leader change neither loses nor repeats a push. Pod A leads and pushes a line, then
// reads a second one and steps down before its batch goes; a third line comes while no pod leads.
// Pod B, leading from the cursor A left, pushes the second and third lines in one batch, and not
// the first again.
func TestTwoPodsPushAcrossLeaderChange(t *testing.T) {
	fastLeader(t)
	br := newPushBrowser(t)
	opts := testOptions
	opts.PushHosts, opts.PushClient = []string{"127.0.0.1"}, br.srv.Client()
	oldWindow, oldTick := pushWindow, pushTick
	pushWindow, pushTick = 150*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pushWindow, pushTick = oldWindow, oldTick })
	a, b := newPair(t, opts)
	br.clock = time.Now()
	var ticks atomic.Int64
	for _, e := range []*env{a, b} {
		e.srv.now, e.srv.pushRead = br.now, br.noteRead
		e.srv.pushTicked = func(now time.Time, sent int) { br.ticked(now, sent); ticks.Add(1) }
		e.srv.singletons = []func(context.Context){e.srv.PushLoop}
	}
	br.lastLine = func() int64 {
		id, err := a.st.LastMessageID(context.Background())
		if err != nil {
			t.Error(err)
		}
		return id
	}
	room := a.createRoom(t, "Pricing").ID
	alice := a.join(t, a.createInvite(t, room).Token, "Alice")
	bob := a.join(t, a.createInvite(t, room).Token, "Bob")
	if code, _ := a.do(t, "PUT", "/api/rooms/"+room+"/push", alice.Token, map[string]any{"subscription": br.subscription("/alice")}); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d", code)
	}
	say := func(e *env, body string) int64 {
		t.Helper()
		if code, out := e.do(t, "POST", "/api/rooms/"+room+"/messages", bob.Token, map[string]string{"body": body}); code != http.StatusCreated {
			t.Fatalf("post: %d %s", code, out)
		}
		return e.lastLine(t, room).ID
	}
	stopA := lead(t, a)
	waitFor(t, a.srv.Leader)
	say(b, "@Alice one")
	waitPushes(t, br, 1)
	second := say(b, "@Alice two")
	waitFor(t, func() bool { return br.readUpTo() >= second })
	// Two ticks after the read: the cursor A writes is the one it leaves.
	seen := ticks.Load()
	waitFor(t, func() bool { return ticks.Load() >= seen+2 })
	stopA()
	third := say(a, "@Alice three")
	lead(t, b)
	waitFor(t, b.srv.Leader)
	got := waitPushes(t, br, 2)
	if want := (pushPayload{Room: "Pricing", Kind: pushMention, Count: 2, URL: fmt.Sprintf("/r/%s#m%d", room, third)}); got[1] != want {
		t.Fatalf("push after the change %+v, want %+v", got[1], want)
	}
}

// 017-AC17: the push cursor stays before a batch in flight. Pod A's send hangs while its ticks
// save the cursor; A steps down, the send dies with it, and pod B, leading next, sends the batch.
func TestTwoPodsPushInFlightResent(t *testing.T) {
	fastLeader(t)
	br := newPushBrowser(t)
	br.hold = make(chan struct{})
	opts := testOptions
	opts.PushHosts, opts.PushClient = []string{"127.0.0.1"}, br.srv.Client()
	oldWindow, oldTick := pushWindow, pushTick
	pushWindow, pushTick = 150*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pushWindow, pushTick = oldWindow, oldTick })
	a, b := newPair(t, opts)
	br.clock = time.Now()
	var aTicks, bSent, bRead atomic.Int64
	for _, e := range []*env{a, b} {
		e.srv.now, e.srv.singletons = br.now, []func(context.Context){e.srv.PushLoop}
	}
	a.srv.pushRead = br.noteRead
	a.srv.pushTicked = func(now time.Time, sent int) { br.ticked(now, sent); aTicks.Add(1) }
	b.srv.pushRead = func(last int64) { bRead.Store(last) }
	b.srv.pushTicked = func(_ time.Time, sent int) { bSent.Add(int64(sent)) }
	room := a.createRoom(t, "Pricing").ID
	alice := a.join(t, a.createInvite(t, room).Token, "Alice")
	bob := a.join(t, a.createInvite(t, room).Token, "Bob")
	if code, _ := a.do(t, "PUT", "/api/rooms/"+room+"/push", alice.Token, map[string]any{"subscription": br.subscription("/alice")}); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d", code)
	}
	stopA := lead(t, a)
	waitFor(t, a.srv.Leader)
	if code, out := b.do(t, "POST", "/api/rooms/"+room+"/messages", bob.Token, map[string]string{"body": "@Alice one"}); code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, out)
	}
	line := a.lastLine(t, room).ID
	waitFor(t, func() bool { return br.readUpTo() >= line })
	br.advance(pushWindow)
	waitFor(t, func() bool { _, sent := br.flushed(); return sent == 1 })
	// Two ticks with the send in flight: the cursor A leaves is the one they saved.
	seen := aTicks.Load()
	waitFor(t, func() bool { return aTicks.Load() >= seen+2 })
	stopA()
	if n := len(br.pushes()); n != 0 {
		t.Fatalf("%d pushes arrived from the held send", n)
	}
	br.mu.Lock()
	br.hold = closedChan()
	br.mu.Unlock()
	lead(t, b)
	waitFor(t, func() bool { return b.srv.Leader() && bRead.Load() >= line })
	br.advance(pushWindow)
	waitFor(t, func() bool { return bSent.Load() == 1 && len(br.pushes()) == 1 })
	if want := (pushPayload{Room: "Pricing", Kind: pushMention, Count: 1, URL: fmt.Sprintf("/r/%s#m%d", room, line)}); br.pushes()[0] != want {
		t.Fatalf("push %+v, want %+v", br.pushes()[0], want)
	}
}

// 017-AC18: pod A restarting, as a new server on the same database that takes the leadership,
// fails no turn running on an app connected to pod B: its sweeper finds that app live.
func TestTwoPodsRestartKeepsOtherPodsTurn(t *testing.T) {
	fastLeader(t)
	_, b := newPair(t, testOptions)
	r := newTurnRoom(t, b)
	tn := b.press(t, r, r.alice, r.alice.p.Token, false)
	restarted := newPodEnv(t, b.dsn, b.admin, testOptions)
	restarted.srv.singletons = nil
	lead(t, restarted)
	waitFor(t, restarted.srv.Leader)
	restarted.srv.sweepTurns(t.Context())
	if got, err := b.st.Turn(t.Context(), r.room, tn.ID); err != nil || got.Status != store.TurnRunning {
		t.Fatalf("after the restart and a sweep: %+v %v", got, err)
	}
}

// 017-AC18: an app's turn fails at most a lease plus a sweep interval after the app's last
// renewal: that fits the shortest turn timeout plus 30 s.
func TestVanishedAppBound(t *testing.T) {
	if bound := daemonLease + turnSweepEvery; bound > MinTurnTimeout+30*time.Second {
		t.Fatalf("lease %s + sweep %s = %s, over the minimum turn timeout %s plus 30 s", daemonLease, turnSweepEvery, bound, MinTurnTimeout)
	}
}

// failureLines counts the lines that say turn id failed for reason.
func failureLines(t *testing.T, e *env, room string, id int64, reason string) int {
	t.Helper()
	msgs, err := e.st.Messages(t.Context(), room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.TurnID == id && strings.Contains(m.Body, "turn failed: "+reason) {
			n++
		}
	}
	return n
}

// lapses checks, on clock clk of leader l, that turn id of room still runs at the end of a lease
// renewed at renewed and fails at the first sweep past it: within daemonLease plus one sweep
// interval of the last renewal. Every pod's lease janitor then runs too: one failure line.
func lapses(t *testing.T, clk *clock, renewed time.Time, l *env, pods []*env, room string, id int64) {
	t.Helper()
	status := func() string {
		got, err := l.st.Turn(t.Context(), room, id)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	clk.set(renewed.Add(daemonLease - time.Millisecond))
	l.srv.sweepTurns(t.Context())
	if st := status(); st != store.TurnRunning {
		t.Fatalf("%s while its lease runs", st)
	}
	clk.set(renewed.Add(daemonLease))
	l.srv.sweepTurns(t.Context())
	if st := status(); st != store.TurnFailed {
		t.Fatalf("%s at the first sweep past its lease", st)
	}
	for _, e := range pods {
		e.srv.lease = clk.now
		e.srv.sweepShared(t.Context())
	}
	if n := failureLines(t, l, room, id, "the host's daemon disconnected"); n != 1 {
		t.Fatalf("%d failure lines, want 1", n)
	}
}

// 017-AC18: a turn whose app vanished with its pod, killed with no cleanup, fails at the leader's
// first sweep once the app's connection lease lapsed.
func TestTwoPodsVanishedAppTurnFails(t *testing.T) {
	a, b := newPair(t, testOptions)
	clk := &clock{t: time.Now()}
	a.srv.lease = clk.now
	r := newTurnRoom(t, b)
	tn := b.press(t, r, r.alice, r.alice.p.Token, false)
	d, err := b.st.DaemonByToken(t.Context(), HashToken(r.alice.tok))
	if err != nil {
		t.Fatal(err)
	}
	// Pod B dies: its stream's row stays, last renewed now; the app's stream ends.
	renewed := clk.now()
	if err := b.st.DaemonConnected(t.Context(), "killed-pod", d, "gone", renewed.UnixMilli(), renewed.Add(daemonLease).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r.alice.drop()
	waitFor(t, func() bool {
		ds, err := b.st.LiveDaemons(t.Context(), r.room, renewed.UnixMilli())
		return err == nil && len(ds) == 1
	})
	lapses(t, clk, renewed, a, []*env{a, b}, r.room, tn.ID)
}

// closed waits for stream ch to end.
func closed(t *testing.T, ch <-chan sseEvent) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream did not end within 10 s")
		}
	}
}

// 017-AC19: a stopping pod ends its app streams at once and runs their disconnect cleanup: the
// app drops its running turn with the stream, so the turn fails now, with one line.
func TestTwoPodsShutdownFailsTurn(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	tn := a.press(t, r, r.alice, r.alice.p.Token, false)
	a.shutdown()
	closed(t, r.alice.events)
	if got, err := b.st.Turn(t.Context(), r.room, tn.ID); err != nil || got.Status != store.TurnFailed {
		t.Fatalf("after pod A stopped: %+v %v", got, err)
	}
	b.srv.sweepTurns(t.Context())
	a.srv.sweepShared(t.Context())
	b.srv.sweepShared(t.Context())
	if n := failureLines(t, b, r.room, tn.ID, "the host's daemon disconnected"); n != 1 {
		t.Fatalf("%d failure lines, want 1", n)
	}
}

// 017-AC19: Serve hands each request the pod's context, which ends a stopping pod's app streams.
func TestServeHandsPodContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	srv := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	if err := Serve(ctx, srv, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(srv.BaseContext(nil), http.MethodGet, "/", nil)
	if stop := podStopping(r); stop != ctx.Done() {
		t.Fatal("the request does not carry Serve's context")
	}
}

// turnEvent waits for the next "turn" event on ch and checks it names turn id.
func turnEvent(t *testing.T, ch <-chan sseEvent, id int64) {
	t.Helper()
	if ev := nextEvent(t, ch, "turn"); ev.data != fmt.Sprintf(`{"id":%d}`, id) {
		t.Fatalf("turn event %+v, want turn %d", ev, id)
	}
}

// crashUnder leaves a dead pod's live connection row for r's app, then ends its stream: the
// stream's own cleanup takes the app for connected, as after a pod killed with no cleanup.
func crashUnder(t *testing.T, e *env, r *turnRoom) {
	t.Helper()
	d, err := e.st.DaemonByToken(t.Context(), HashToken(r.alice.tok))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.st.DaemonConnected(t.Context(), "killed-pod", d, "gone", now.UnixMilli(), now.Add(daemonLease).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r.alice.drop()
	closed(t, r.alice.events)
}

// 017-AC18: a turn pressed after pod A died under the app's stream, while the dead pod's row still
// counts the app connected, reaches no app. When the app reconnects through pod B, the turn is
// announced on the new stream and runs.
func TestTwoPodsUndeliveredTurnRunsOnReconnect(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	crashUnder(t, a, r)
	code, out := b.do(t, "POST", r.path("/turns"), r.alice.p.Token, map[string]bool{})
	if code != http.StatusCreated {
		t.Fatalf("press while the dead pod's row counts: %d %s", code, out)
	}
	tn := decodeJSON[store.Turn](t, out)
	b.connect(t, r.room, r.alice, true)
	turnEvent(t, r.alice.events, tn.ID)
	b.job(t, r, r.alice, tn.ID)
	if code, out := b.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, Session: []byte("one\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, out)
	}
	if n := failureLines(t, b, r.room, tn.ID, ""); n != 0 {
		t.Fatalf("%d failure lines", n)
	}
	if got, err := b.st.Turn(t.Context(), r.room, tn.ID); err != nil || got.Status != store.TurnDone {
		t.Fatalf("after the result: %+v %v", got, err)
	}
}

// 017-AC18: pod A dies under the app's stream with no cleanup, a turn delivered and running and
// another queued. The app, which dropped the running turn with its stream, reconnects through pod
// B: that turn fails at once, with one line, and the queued one starts on the new stream and runs.
func TestTwoPodsCrashedPodTurnFailsOnReconnect(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	tok := r.alice.p.Token
	first := a.press(t, r, r.alice, tok, false)
	if code, out := a.do(t, "POST", r.path("/turns"), tok, map[string]bool{}); code != http.StatusAccepted {
		t.Fatalf("second press: %d %s", code, out)
	}
	// The app took the event; its job fetch never lands before the stream drops.
	crashUnder(t, a, r)
	if got, _ := a.st.Turn(t.Context(), r.room, first.ID); got.Status != store.TurnRunning {
		t.Fatalf("before the reconnect: %+v", got)
	}
	b.connect(t, r.room, r.alice, true)
	if got, _ := b.st.Turn(t.Context(), r.room, first.ID); got.Status != store.TurnFailed {
		t.Fatalf("after the reconnect: %+v", got)
	}
	running, _ := turnsOf(t, b, r.room)
	if len(running) != 1 || running[0].ID == first.ID {
		t.Fatalf("running after the reconnect: %+v", running)
	}
	second := running[0]
	turnEvent(t, r.alice.events, second.ID)
	b.job(t, r, r.alice, second.ID)
	b.srv.sweepTurns(t.Context())
	if code, out := b.result(t, r, r.alice, second.ID, turn.Result{SessionID: sid1, Session: []byte("one\n")}); code != http.StatusOK {
		t.Fatalf("the queued turn's result: %d %s", code, out)
	}
	if n := failureLines(t, b, r.room, first.ID, "the host's daemon disconnected"); n != 1 {
		t.Fatalf("%d failure lines, want 1", n)
	}
}

// 017-AC18: a stream the app no longer reads (half-open) still takes the turn event, which marks
// the turn delivered. When the app opens its next stream, the turn fails there, with one line: the
// user presses again. A turn may fail; it must never hang.
func TestHalfOpenStreamTurnFailsOnReconnect(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	// The first stream stays open: the server goes on writing to it.
	e.connect(t, r.room, r.alice, true)
	if got, err := e.st.Turn(t.Context(), r.room, tn.ID); err != nil || got.Status != store.TurnFailed {
		t.Fatalf("after the reconnect: %+v %v", got, err)
	}
	if n := failureLines(t, e, r.room, tn.ID, "the host's daemon disconnected"); n != 1 {
		t.Fatalf("%d failure lines, want 1", n)
	}
}

// 017-AC18: a stream that cannot mark a turn delivered writes nothing and ends: the disconnect
// cleanup fails the turn, with one line, and the app's next stream finds nothing running.
func TestDeliveredMarkFailureEndsStream(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	db := e.sqlDB(t)
	if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION refuse_delivered() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'delivered refused'; END $$;
		CREATE TRIGGER refuse_delivered BEFORE UPDATE OF delivered ON turns FOR EACH ROW EXECUTE FUNCTION refuse_delivered()`); err != nil {
		t.Fatal(err)
	}
	code, out := e.do(t, "POST", r.path("/turns"), r.alice.p.Token, map[string]bool{})
	if code != http.StatusCreated {
		t.Fatalf("press: %d %s", code, out)
	}
	tn := decodeJSON[store.Turn](t, out)
	deadline := time.After(10 * time.Second)
	for ended := false; !ended; {
		select {
		case ev, ok := <-r.alice.events:
			if !ok {
				ended = true
			} else if ev.event == "turn" {
				t.Fatalf("an unmarked turn written: %+v", ev)
			}
		case <-deadline:
			t.Fatal("the stream did not end within 10 s")
		}
	}
	if got, err := e.st.Turn(t.Context(), r.room, tn.ID); err != nil || got.Status != store.TurnFailed {
		t.Fatalf("after the stream ended: %+v %v", got, err)
	}
	if n := failureLines(t, e, r.room, tn.ID, "the host's daemon disconnected"); n != 1 {
		t.Fatalf("%d failure lines, want 1", n)
	}
	e.connect(t, r.room, r.alice, true)
	if running, _ := turnsOf(t, e, r.room); len(running) != 0 {
		t.Fatalf("running after the reconnect: %+v", running)
	}
}
