package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/store"
)

type pairingResp struct {
	ID         string `json:"id"`
	Secret     string `json:"secret"`
	Code       string `json:"code"`
	ApproveURL string `json:"approve_url"`
}

type pollResp struct {
	Status string `json:"status"`
	Token  string `json:"token"`
}

func (e *env) requestPairing(t *testing.T, room, name string) pairingResp {
	t.Helper()
	code, b := e.do(t, "POST", "/api/rooms/"+room+"/pairings", "", map[string]string{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("create pairing: %d %s", code, b)
	}
	return decodeJSON[pairingResp](t, b)
}

func (e *env) poll(t *testing.T, p pairingResp) (int, pollResp) {
	t.Helper()
	code, b := e.do(t, "GET", "/api/pairings/"+p.ID, p.Secret, nil)
	if code != http.StatusOK {
		return code, pollResp{}
	}
	return code, decodeJSON[pollResp](t, b)
}

// pairedDaemon pairs a daemon to a new participant of room and returns its token.
func (e *env) pairedDaemon(t *testing.T, room, name string) (daemonToken string, participant joined) {
	t.Helper()
	inv := e.createInvite(t, room)
	participant = e.join(t, inv.Token, name)
	p := e.requestPairing(t, room, name+"-laptop")
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/pairings/"+p.ID, participant.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	_, r := e.poll(t, p)
	if r.Status != "approved" || r.Token == "" {
		t.Fatalf("poll after approve = %+v", r)
	}
	return r.Token, participant
}

// 001-AC22: a daemon asks to pair, a participant of the room approves in their session, and the
// daemon receives its token once, without anyone seeing or typing it.
func TestPairingFlow(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	other := e.createRoom(t, "Other")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	mallory := e.join(t, e.createInvite(t, other.ID).Token, "Mallory")

	p := e.requestPairing(t, room.ID, "alice-laptop")
	if p.ApproveURL != baseURL+"/r/"+room.ID+"?pair="+p.ID || len(p.Code) != 7 || p.Secret == "" {
		t.Fatalf("pairing = %+v", p)
	}
	if _, r := e.poll(t, p); r.Status != "pending" {
		t.Fatalf("poll = %+v", r)
	}
	if code, _ := e.poll(t, pairingResp{ID: p.ID, Secret: "wrong"}); code != http.StatusNotFound {
		t.Fatalf("wrong secret: %d", code)
	}

	path := "/api/rooms/" + room.ID + "/pairings/" + p.ID
	if code, _ := e.do(t, "GET", path, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous view: %d", code)
	}
	// A participant of another room cannot see or answer it, even through their own room's path.
	if code, _ := e.do(t, "GET", path, mallory.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("other room's participant: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+other.ID+"/pairings/"+p.ID, mallory.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("answer through another room: %d", code)
	}
	code, b := e.do(t, "GET", path, alice.Token, nil)
	// 003-AC17: the machine name a daemon sends never reaches the room.
	if code != http.StatusOK || strings.Contains(string(b), "alice-laptop") || !strings.Contains(string(b), p.Code) {
		t.Fatalf("view: %d %s", code, b)
	}
	// The approval page's POST is CSRF-guarded like every participant write.
	if code, _ := e.raw(t, "POST", path, alice.Token, "application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}, `{"approve":true}`); code != http.StatusForbidden {
		t.Fatalf("cross-site approve: %d", code)
	}
	if code, b := e.do(t, "POST", path, alice.Token, map[string]bool{"approve": true}); code != http.StatusOK || strings.Contains(string(b), "token") {
		t.Fatalf("approve: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", path, alice.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("second answer: %d", code)
	}
	_, r := e.poll(t, p)
	if r.Status != "approved" || r.Token == "" {
		t.Fatalf("poll = %+v", r)
	}
	if code, _ := e.poll(t, p); code != http.StatusNotFound {
		t.Fatalf("token handed twice: %d", code)
	}
	d, err := e.st.DaemonByToken(t.Context(), HashToken(r.Token))
	if err != nil || d.ParticipantID != alice.Participant.ID {
		t.Fatalf("daemon = %+v, %v", d, err)
	}
}

func TestPairingDenyExpiryAndLimits(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")

	p := e.requestPairing(t, room.ID, "x")
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": false}); code != http.StatusOK {
		t.Fatalf("deny: %d", code)
	}
	if _, r := e.poll(t, p); r.Status != "denied" || r.Token != "" {
		t.Fatalf("poll = %+v", r)
	}

	p = e.requestPairing(t, room.ID, "x")
	e.srv.lease = func() time.Time { return time.Now().Add(pairingTTL + time.Second) }
	if code, _ := e.poll(t, p); code != http.StatusNotFound {
		t.Fatalf("expired poll: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("expired approve: %d", code)
	}

	for _, tc := range []struct {
		name, room, body string
		want             int
	}{
		{"unknown room", "nope", `{"name":"x"}`, http.StatusNotFound},
		{"bad json", room.ID, `{"name":`, http.StatusBadRequest},
	} {
		if code, _ := e.raw(t, "POST", "/api/rooms/"+tc.room+"/pairings", "", "application/json", nil, tc.body); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
	if code, _ := e.raw(t, "POST", "/api/rooms/"+room.ID+"/pairings", "", "text/plain", nil, `{"name":"x"}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", code)
	}
	for range maxPairingsPerRoom {
		e.requestPairing(t, room.ID, "spam")
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings", "", map[string]string{"name": "x"}); code != http.StatusTooManyRequests {
		t.Fatalf("over the room cap: %d", code)
	}
}

// 001-AC28: the daemon's heartbeat says whether it can host and why not; the room sees it while
// the daemon's stream is open.
func TestDaemonConnection(t *testing.T) {
	old := heartbeat
	heartbeat = 50 * time.Millisecond
	t.Cleanup(func() { heartbeat = old })
	e := newEnv(t)
	room := e.createRoom(t, "R")
	tok, alice := e.pairedDaemon(t, room.ID, "Alice")
	events := "/api/rooms/" + room.ID + "/daemon/events"
	status := "/api/rooms/" + room.ID + "/daemon/status"

	for _, bad := range []string{"", alice.Token, "nope"} {
		if code, _ := e.do(t, "GET", events, bad, nil); code != http.StatusUnauthorized {
			t.Fatalf("events with %q: %d", bad, code)
		}
	}
	other := e.createRoom(t, "Other")
	if code, _ := e.do(t, "GET", "/api/rooms/"+other.ID+"/daemon/events", tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("events for another room: %d", code)
	}

	ch, drop := e.openStream(t, events, tok, "")
	// 005-AC42: the hello names the room, for the daemon's page.
	if ev := next(t, ch); ev.event != "hello" || !strings.Contains(ev.data, `"heartbeat_seconds"`) || !strings.Contains(ev.data, `"room_name":"R"`) ||
		strings.Contains(ev.data, "Alice-laptop") {
		t.Fatalf("hello = %+v", ev)
	}
	if code, b := e.do(t, "POST", status, tok, map[string]any{"can_host": false, "reason": "Docker is not running", "version": "v1"}); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, b)
	}
	roomDaemons := func() []liveDaemon {
		_, b := e.do(t, "GET", "/api/rooms/"+room.ID, alice.Token, nil)
		return decodeJSON[roomResp](t, b).Daemons
	}
	ds := roomDaemons()
	if len(ds) != 1 || ds[0].CanHost || ds[0].Reason != "Docker is not running" || ds[0].ParticipantID != alice.Participant.ID {
		t.Fatalf("daemons = %+v", ds)
	}
	drop()
	deadline := time.Now().Add(5 * time.Second)
	for len(roomDaemons()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon still listed after its stream closed")
		}
		time.Sleep(20 * time.Millisecond) // polls the list, bounded by the deadline
	}

	// Re-pairing hands out a new token and ends the old daemon's stream.
	ch, _ = e.openStream(t, events, tok, "")
	next(t, ch)
	p := e.requestPairing(t, room.ID, "new")
	e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": true})
	_, r := e.poll(t, p)
	expectRevokedThenClosed(t, ch)

	// Revoking the participant's invite ends the new one too.
	ch, _ = e.openStream(t, events, r.Token, "")
	next(t, ch)
	if ds := roomDaemons(); len(ds) != 1 {
		t.Fatalf("after re-pairing, daemons = %+v", ds)
	}
	ap, err := e.st.ParticipantByToken(t.Context(), HashToken(alice.Token))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(ap.InviteID, 10)+"?cut=1", e.admin, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	expectRevokedThenClosed(t, ch)
}

// 017-AC12: each heartbeat renews the stream's lease, so a daemon connected past its first lease
// stays in the room.
func TestDaemonLeaseRenewed(t *testing.T) {
	oldBeat, oldLease := heartbeat, daemonLease
	heartbeat, daemonLease = 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { heartbeat, daemonLease = oldBeat, oldLease })
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	x := e.daemonFor(t, room, "Xena")
	e.connect(t, room, x, true)
	first := time.Now().Add(daemonLease)
	time.Sleep(time.Until(first))
	if st := e.agent(t, room, x.p.Token); !hostIs(x.p.Participant.ID, true)(st) {
		t.Fatalf("past the first lease: %+v", st)
	}
}

// 017-AC12: a pod killed under a daemon's stream leaves its connection row, live until its lease
// lapses. The daemon's last real stream ending then leaves the turn running; once the lease lapses,
// the janitor fails the turn and the room elects another host.
func TestStaleDaemonRowLapses(t *testing.T) {
	e := newEnv(t)
	clk := &clock{t: time.Now()}
	e.srv.lease = clk.now
	r := newTurnRoom(t, e)
	alice := r.alice
	bob := e.daemonFor(t, r.room, "Bob")
	e.connect(t, r.room, bob, true)
	tn := e.press(t, r, alice, alice.p.Token, false)
	d, err := e.st.DaemonByToken(t.Context(), HashToken(alice.tok))
	if err != nil {
		t.Fatal(err)
	}
	now := clk.now()
	// Renewed last a second before the pod died; Bob's lease runs a minute.
	if err := e.st.DaemonConnected(t.Context(), "killed-pod", d, "gone", now.UnixMilli(), now.Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	alice.drop()
	db := e.sqlDB(t)
	waitFor(t, func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM daemon_connections WHERE daemon_id = $1`, d.ID).Scan(&n); err != nil {
			t.Error(err)
		}
		return n == 1
	})
	if st := e.agent(t, r.room, bob.p.Token); st.Turn == nil || st.Turn.ID != tn.ID || st.Turn.Status != store.TurnRunning || st.HostID != alice.p.Participant.ID {
		t.Fatalf("with the stale row live: %+v", st)
	}
	clk.add(time.Second)
	e.srv.sweepShared(t.Context())
	st := e.waitAgent(t, r.room, bob.p.Token, hostIs(bob.p.Participant.ID, true))
	if st.Turn == nil || st.Turn.ID != tn.ID || st.Turn.Status != store.TurnFailed || st.Turn.Error != "the host's daemon disconnected" {
		t.Fatalf("after the sweep: %+v", st.Turn)
	}
}

// 017-AC13: the poll mints the daemon's token: it differs from the pairing secret, which never
// authenticates as the daemon, so a leaked secret is no token.
func TestPairingMintsToken(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	p := e.requestPairing(t, room.ID, "x")
	if code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	status := func(tok string) int {
		code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/daemon/status", tok, map[string]any{"can_host": true})
		return code
	}
	if code := status(p.Secret); code != http.StatusUnauthorized {
		t.Fatalf("the secret as a token before the poll: %d", code)
	}
	_, r := e.poll(t, p)
	if r.Status != "approved" || r.Token == "" || r.Token == p.Secret {
		t.Fatalf("poll = %+v, secret %q", r, p.Secret)
	}
	if code := status(p.Secret); code != http.StatusUnauthorized {
		t.Fatalf("the secret as a token after the poll: %d", code)
	}
	if code := status(r.Token); code != http.StatusNoContent {
		t.Fatalf("the minted token: %d", code)
	}
}

// The replica identity the bus carries names the pod in daemon_connections: one identity per
// process, the one its notifications carry.
func TestDaemonConnectionNamesThePod(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	e.connect(t, room, e.daemonFor(t, room, "Xena"), true)
	var pod string
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT pod FROM daemon_connections`).Scan(&pod); err != nil || pod != e.bus.Pod() {
		t.Fatalf("pod %q %v, want %q", pod, err, e.bus.Pod())
	}
}

// Leases and the janitor run on one clock, the lease clock: business time moving ahead sweeps no
// live daemon, presence or worker.
func TestJanitorOnLeaseClock(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	x := e.daemonFor(t, room, "Xena")
	e.connect(t, room, x, true)
	e.srv.now = func() time.Time { return time.Now().Add(time.Hour) }
	e.srv.sweepShared(t.Context())
	if st := e.agent(t, room, x.p.Token); !hostIs(x.p.Participant.ID, true)(st) {
		t.Fatalf("after a sweep an hour ahead in business time: %+v", st)
	}
}

// SweepShared sweeps on its tick until ctx ends: an expired presence row goes, and a daemon whose
// only connection lapsed takes the disconnect path (017-AC12).
func TestSweepSharedLoop(t *testing.T) {
	old := sharedSweepEvery
	sharedSweepEvery = 10 * time.Millisecond
	t.Cleanup(func() { sharedSweepEvery = old })
	core, logs := observer.New(zapcore.InfoLevel)
	e := newEnvLog(t, testOptions, zap.New(core))
	room := e.createRoom(t, "R").ID
	x := e.daemonFor(t, room, "Xena")
	d, err := e.st.DaemonByToken(t.Context(), HashToken(x.tok))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.st.DaemonConnected(t.Context(), "killed-pod", d, "gone", now.Add(-time.Minute).UnixMilli(), now.Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	old1 := store.PresenceTimes{Now: now.Add(-time.Hour).UnixMilli(), Grace: 1000, Forget: 1000}
	if err := e.st.PresenceOpen(t.Context(), room, x.p.Participant.ID, "old-tab", old1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.srv.SweepShared(ctx); close(done) }()
	db := e.sqlDB(t)
	waitFor(t, func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM presence) + (SELECT count(*) FROM daemon_connections)`).Scan(&n); err != nil {
			t.Error(err)
		}
		return n == 0
	})
	waitFor(t, func() bool { return logs.FilterMessage("daemon_lease_lapsed").Len() == 1 })
	cancel()
	<-done
}

// /metrics reads gauges SweepShared computes: a refresh slower than the streams' poll would let the
// device STT and TTS gauges trail the shared state by a scrape interval (e2e device-voice polls 10 s).
func TestGaugesRefreshEveryPoll(t *testing.T) {
	if gaugeEvery > pollEvery {
		t.Fatalf("gaugeEvery %s, want at most pollEvery %s", gaugeEvery, pollEvery)
	}
}
