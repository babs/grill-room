package web

import (
	"net/http"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// 003b-AC9 003b-AC10: the host removes the room from their computer. The token stops working, the
// stream ends with "revoked", the running turn fails, and the room says why while no one hosts;
// another daemon able to host takes over.
func TestRemoveDaemon(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tn := e.press(t, r, alice, alice.p.Token, false)
	// A press queued behind the running turn waits for the next host: the removed daemon never gets it.
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/turns", alice.p.Token, map[string]bool{}); code != http.StatusAccepted {
		t.Fatalf("queued press: %d %s", code, b)
	}
	bob := e.daemonFor(t, r.room, "Bob")

	if code, b := e.do(t, "DELETE", "/api/rooms/"+r.room+"/daemon", "nope", nil); code != http.StatusUnauthorized {
		t.Fatalf("remove with a bad token: %d %s", code, b)
	}
	if code, b := e.do(t, "DELETE", "/api/rooms/"+r.room+"/daemon", alice.tok, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", code, b)
	}
	// The failed turn's "stop" may come first: the daemon kills its claude.
	if ev := nextEvent(t, alice.events, "revoked"); ev.event != "revoked" {
		t.Fatalf("got %+v", ev)
	}
	select {
	case ev, ok := <-alice.events:
		if ok {
			t.Fatalf("event after revoked: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after remove")
	}
	if code, _ := e.do(t, "POST", r.daemonPath("status"), alice.tok, map[string]any{"can_host": true}); code != http.StatusUnauthorized {
		t.Fatalf("status after remove: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+r.room+"/daemon", alice.tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("second remove: %d", code)
	}
	got, err := e.st.Turn(t.Context(), r.room, tn.ID)
	if err != nil || got.Status != store.TurnFailed || got.Error != "the AI host removed this room from their computer" {
		t.Fatalf("turn after remove: %+v %v", got, err)
	}
	gone := e.waitAgent(t, r.room, alice.p.Token, func(st agentState) bool { return len(st.Daemons) == 0 })
	if gone.Online || gone.HostID != alice.p.Participant.ID || gone.Reason != "The AI host removed this room from their computer." {
		t.Fatalf("room after remove: %+v", gone)
	}

	if q, err := e.st.Queue(t.Context(), r.room); err != nil || len(q) != 1 {
		t.Fatalf("queue after remove: %+v %v", q, err)
	}
	if running, err := e.st.RunningTurns(t.Context(), r.room); err != nil || len(running) != 0 {
		t.Fatalf("running after remove: %+v %v", running, err)
	}

	e.connect(t, r.room, bob, true)
	if st := e.waitAgent(t, r.room, alice.p.Token, hostIs(bob.p.Participant.ID, true)); st.Reason != "" {
		t.Fatalf("bob hosting: %+v", st)
	}
	if ev := nextEvent(t, bob.events, "turn"); ev.event != "turn" {
		t.Fatalf("bob got %+v, want the queued turn", ev)
	}
}

// 003b-AC11 003b-AC5: a paused host reports can_host=false with its reason. With a second daemon
// able to host, the room moves the AI host role to it; with none, it shows the reason. Resume makes
// the paused daemon eligible again, without taking the role back.
func TestPausedHostFailsOver(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	alice, bob := e.daemonFor(t, room, "Alice"), e.daemonFor(t, room, "Bob")
	e.connect(t, room, alice, true)
	e.waitAgent(t, room, alice.p.Token, hostIs(alice.p.Participant.ID, true))
	pause := func(d *testDaemon, paused bool) {
		t.Helper()
		body := map[string]any{"can_host": !paused}
		if paused {
			body["reason"] = "paused on the host's machine"
		}
		if code, b := e.do(t, "POST", "/api/rooms/"+room+"/daemon/status", d.tok, body); code != http.StatusNoContent {
			t.Fatalf("status: %d %s", code, b)
		}
	}

	pause(alice, true)
	st := e.waitAgent(t, room, alice.p.Token, hostIs(alice.p.Participant.ID, false))
	if st.Reason != "paused on the host's machine" {
		t.Fatalf("paused, alone: %+v", st)
	}
	e.connect(t, room, bob, true)
	e.waitAgent(t, room, alice.p.Token, hostIs(bob.p.Participant.ID, true))
	pause(alice, false)
	if st := e.agent(t, room, alice.p.Token); !hostIs(bob.p.Participant.ID, true)(st) {
		t.Fatalf("resume took the role back: %+v", st)
	}
	// Bob pauses in turn: the role goes back to Alice, eligible again.
	pause(bob, true)
	e.waitAgent(t, room, alice.p.Token, hostIs(alice.p.Participant.ID, true))
}
