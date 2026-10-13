package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// stop stops turn id as tok and returns the status.
func (e *env) stop(t *testing.T, r *turnRoom, tok string, id int64) int {
	t.Helper()
	code, _ := e.do(t, "POST", r.path(fmt.Sprintf("/turns/%d/stop", id)), tok, struct{}{})
	return code
}

// expectStop reads d's events up to the next "stop" and checks it names turn id.
func expectStop(t *testing.T, d *testDaemon, id int64) {
	t.Helper()
	if ev := nextEvent(t, d.events, "stop"); ev.data != fmt.Sprintf(`{"id":%d}`, id) {
		t.Fatalf("stop event %+v, want turn %d", ev, id)
	}
}

// 005-AC5: whoever started a running turn, the host or a room admin stops it; nobody else. The
// turn ends as stopped with a line in the room, the host's daemon gets a "stop", and its result is
// refused: no revision, no synthesis, no reply, no session bytes. The queue moves on, and the
// persona's next turn resumes the session as the last done turn left it.
func TestStopTurn(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	dave := e.join(t, e.createInvite(t, r.room).Token, "Dave")
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/participants/%d", bob.Participant.ID)), alice.p.Token, map[string]bool{"can_trigger": true}); code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", code, b)
	}
	carolDev := e.device(t)
	carol := carolDev.join(t, r.room, "Carol").Participant
	if code, b := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", r.room, carol.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("appoint Carol: %d %s", code, b)
	}

	done := e.press(t, r, alice, alice.p.Token, false)
	e.finish(t, r, alice, done, "First pass.")
	stored, err := e.st.AgentSession(t.Context(), r.room, r.grill.ID, sid1)
	if err != nil {
		t.Fatal(err)
	}

	tn := e.press(t, r, alice, bob.Token, false)
	e.job(t, r, alice, tn.ID)
	// Alice's press waits behind the running turn.
	if code, b := e.do(t, "POST", r.path("/turns"), alice.p.Token, map[string]bool{}); code != http.StatusAccepted {
		t.Fatalf("queue: %d %s", code, b)
	}
	// A cross-site form sends no JSON: refused before anything else.
	if code, _ := e.do(t, "POST", r.path(fmt.Sprintf("/turns/%d/stop", tn.ID)), bob.Token, nil); code != http.StatusUnsupportedMediaType {
		t.Fatalf("stop without JSON: %d", code)
	}
	if code := e.stop(t, r, dave.Token, tn.ID); code != http.StatusForbidden {
		t.Fatalf("Dave stops: %d", code)
	}
	for _, id := range []string{"999", "x"} {
		if code, _ := e.do(t, "POST", r.path("/turns/"+id+"/stop"), bob.Token, struct{}{}); code != http.StatusNotFound {
			t.Fatalf("stop turn %s: %d", id, code)
		}
	}
	if code := e.stop(t, r, bob.Token, tn.ID); code != http.StatusNoContent {
		t.Fatalf("Bob stops his turn: %d", code)
	}
	expectStop(t, alice, tn.ID)
	if code := e.stop(t, r, bob.Token, tn.ID); code != http.StatusConflict {
		t.Fatalf("second stop: %d", code)
	}
	res := turn.Result{Synthesis: "too late", SessionID: sid1, SessionOffset: int64(len(stored)), Session: []byte("late\n"),
		Changes: []turn.Change{{Path: turn.DocsDir + "/synthesis.md", Content: []byte("# Synthesis\n\nchanged\n")}}}
	if code, b := e.result(t, r, alice, tn.ID, res); code != http.StatusConflict {
		t.Fatalf("result after stop: %d %s", code, b)
	}
	got, err := e.st.Turn(t.Context(), r.room, tn.ID)
	if err != nil || got.Status != store.TurnStopped {
		t.Fatalf("stopped turn: %+v, %v", got, err)
	}
	if _, err := e.st.Revision(t.Context(), r.doc, 2); err == nil {
		t.Fatal("the stopped turn's revision was applied")
	}
	if data, err := e.st.AgentSession(t.Context(), r.room, r.grill.ID, sid1); err != nil || string(data) != string(stored) {
		t.Fatalf("session after stop: %q, %v", data, err)
	}
	msgs, err := e.st.Messages(t.Context(), r.room, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, m := range msgs {
		if m.Kind == store.KindAgent || m.Kind == store.KindSystem && strings.Contains(m.Body, "turn") {
			lines = append(lines, m.Body)
		}
	}
	if strings.Join(lines, "|") != "First pass.|Grill turn stopped by Bob." {
		t.Fatalf("turn lines: %q", lines)
	}

	// Alice's queued press runs next and resumes the stored session; Alice, the host, stops it.
	queued := e.nextTurn(t, r, alice)
	if job := e.job(t, r, alice, queued.ID); job.Resume != sid1 || job.SessionSize != int64(len(stored)) {
		t.Fatalf("next job: %+v", job)
	}
	if code := e.stop(t, r, alice.p.Token, queued.ID); code != http.StatusNoContent {
		t.Fatalf("the host stops: %d", code)
	}
	expectStop(t, alice, queued.ID)
	// Bob's next turn is stopped by Carol, a room admin.
	third := e.press(t, r, alice, bob.Token, false)
	if code, _ := carolDev.do(t, "POST", r.path(fmt.Sprintf("/turns/%d/stop", third.ID)), struct{}{}); code != http.StatusNoContent {
		t.Fatalf("the room admin stops: %d", code)
	}
	expectStop(t, alice, third.ID)
	if st := e.agent(t, r.room, alice.p.Token); st.Turn.ID != third.ID || st.Turn.Status != store.TurnStopped || len(st.Running) != 0 {
		t.Fatalf("agent state: %+v", st)
	}
	// A done turn gets no stop: the next event after its result is the next turn.
	last := e.press(t, r, alice, alice.p.Token, false)
	e.finish(t, r, alice, last, "Done.")
	if code, b := e.do(t, "POST", r.path("/turns"), alice.p.Token, map[string]bool{}); code != http.StatusCreated {
		t.Fatalf("press: %d %s", code, b)
	}
	for ev := next(t, alice.events); ev.event != "turn"; ev = next(t, alice.events) {
		if ev.event == "stop" {
			t.Fatalf("stop for a done turn: %+v", ev)
		}
	}
}
