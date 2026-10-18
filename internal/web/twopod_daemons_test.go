package web

import (
	"net/http"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// 017-AC12: 20 rounds of two apps connecting and dropping, spread over both pods, the drop of one
// racing the other becoming able to host, post exactly one "holds the agent(s)" line per host change.
func TestTwoPodsHostElection(t *testing.T) {
	a, b := newPair(t, testOptions)
	room := a.createRoom(t, "Hosts").ID
	x, y := a.daemonFor(t, room, "Xena"), a.daemonFor(t, room, "Yann")
	pods := []*env{a, b}
	const rounds = 20
	for i := range rounds {
		p, q := pods[i%2], pods[(i+1)%2]
		p.connect(t, room, x, true)
		q.waitAgent(t, room, x.p.Token, hostIs(x.p.Participant.ID, true))
		q.connect(t, room, y, false)
		// Both pods elect at once: Xena's on her drop, Yann's as he becomes able to host.
		go x.drop()
		q.status(t, room, y, true)
		p.waitAgent(t, room, y.p.Token, hostIs(y.p.Participant.ID, true))
		y.drop()
		q.waitAgent(t, room, y.p.Token, hostIs(y.p.Participant.ID, false))
	}
	msgs, err := a.st.Messages(t.Context(), room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, m := range msgs {
		if m.Body == m.Sender+" holds the agent(s)." {
			hosts = append(hosts, m.Sender)
		}
	}
	if len(hosts) != 2*rounds {
		t.Fatalf("%d host lines, want %d: %v", len(hosts), 2*rounds, hosts)
	}
	for i, h := range hosts {
		if want := []string{"Xena", "Yann"}[i%2]; h != want {
			t.Fatalf("line %d names %s, want %s: %v", i, h, want, hosts)
		}
	}
}

// 017-AC12: an app that reconnected through pod B while its old stream on pod A ends stays
// connected: A's cleanup does not take it for gone.
func TestTwoPodsDaemonReconnect(t *testing.T) {
	a, b := newPair(t, testOptions)
	room := a.createRoom(t, "Reconnect").ID
	x := a.daemonFor(t, room, "Xena")
	a.connect(t, room, x, true)
	a.waitAgent(t, room, x.p.Token, hostIs(x.p.Participant.ID, true))
	onA := x.drop
	b.connect(t, room, x, true)
	onA()
	db := a.sqlDB(t)
	conns := func() int {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM daemon_connections`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitFor(t, func() bool { return conns() == 1 })
	for _, e := range []*env{a, b} {
		if st := e.agent(t, room, x.p.Token); !hostIs(x.p.Participant.ID, true)(st) || len(st.Daemons) != 1 {
			t.Fatalf("after A's stream ended: %+v", st)
		}
	}
}

// 017-AC11: the app's stream on pod A, the press on pod B: the turn starts, reaches the app, and
// its result posted through B ends it. No press answers "agent is offline".
func TestTwoPodsTurn(t *testing.T) {
	a, b := newPair(t, testOptions)
	r := newTurnRoom(t, a)
	for i := range 3 {
		tn := b.press(t, r, r.alice, r.alice.p.Token, false)
		res := turn.Result{SessionID: sid1, SessionOffset: int64(i), Session: []byte("x")}
		if code, out := b.result(t, r, r.alice, tn.ID, res); code != http.StatusOK {
			t.Fatalf("result %d: %d %s", i, code, out)
		}
		if got, err := a.st.Turn(t.Context(), r.room, tn.ID); err != nil || got.Status != store.TurnDone {
			t.Fatalf("turn %d: %+v %v", i, got, err)
		}
	}
}

// 017-AC13: the app's pairing request on one pod, its polls and the participant's answer on the
// others; the caps count every pod's requests.
func TestTwoPodsPairing(t *testing.T) {
	a, b := newPair(t, testOptions)
	room := a.createRoom(t, "Pair")
	alice := a.join(t, a.createInvite(t, room.ID).Token, "Alice")
	path := func(p pairingResp) string { return "/api/rooms/" + room.ID + "/pairings/" + p.ID }

	p := a.requestPairing(t, room.ID, "x")
	if _, r := b.poll(t, p); r.Status != "pending" {
		t.Fatalf("poll on B: %+v", r)
	}
	if code, out := b.do(t, "GET", path(p), alice.Token, nil); code != http.StatusOK {
		t.Fatalf("view on B: %d %s", code, out)
	}
	if code, out := b.do(t, "POST", path(p), alice.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve on B: %d %s", code, out)
	}
	if code, _ := a.do(t, "POST", path(p), alice.Token, map[string]bool{"approve": false}); code != http.StatusNotFound {
		t.Fatalf("second answer on A: %d", code)
	}
	_, r := a.poll(t, p)
	if r.Status != "approved" || r.Token == "" || r.Token == p.Secret {
		t.Fatalf("poll on A: %+v", r)
	}
	if code, _ := b.poll(t, p); code != http.StatusNotFound {
		t.Fatalf("token handed twice: %d", code)
	}
	d := &testDaemon{tok: r.Token, p: alice}
	b.connect(t, room.ID, d, true)
	a.waitAgent(t, room.ID, alice.Token, hostIs(alice.Participant.ID, true))

	p = b.requestPairing(t, room.ID, "y")
	if code, out := a.do(t, "POST", path(p), alice.Token, map[string]bool{"approve": false}); code != http.StatusOK {
		t.Fatalf("deny on A: %d %s", code, out)
	}
	if _, r := b.poll(t, p); r.Status != "denied" || r.Token != "" {
		t.Fatalf("poll on B: %+v", r)
	}

	for i := range maxPairingsPerRoom {
		[]*env{a, b}[i%2].requestPairing(t, room.ID, "spam")
	}
	for _, e := range []*env{a, b} {
		if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings", "", map[string]string{"name": "x"}); code != http.StatusTooManyRequests {
			t.Fatalf("over the room cap: %d", code)
		}
	}
}
