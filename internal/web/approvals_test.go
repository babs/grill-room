package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// settings is d's heartbeat reporting set, able to host.
func (e *env) settings(t *testing.T, room string, d *testDaemon, set store.DaemonSettings) {
	t.Helper()
	body := map[string]any{"can_host": true, "max_model": set.MaxModel, "max_effort": set.MaxEffort, "approve_all": set.ApproveAll}
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/daemon/status", d.tok, body); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, b)
	}
}

// runTurn presses persona as tok on d, checks the job, and posts res as its result.
func (e *env) runTurn(t *testing.T, r *turnRoom, d *testDaemon, tok string, persona int64, res turn.Result) turn.Job {
	t.Helper()
	code, b := e.do(t, "POST", r.path("/turns"), tok, map[string]int64{"persona_id": persona})
	if code != http.StatusCreated {
		t.Fatalf("press: %d %s", code, b)
	}
	tn := decodeJSON[store.Turn](t, b)
	nextEvent(t, d.events, "turn")
	job := e.job(t, r, d, tn.ID)
	if res.SessionID == "" {
		res.SessionID, res.Session = sid1, []byte("x\n")
		if data, err := e.st.AgentSession(t.Context(), r.room, persona, sid1); err == nil {
			res.SessionOffset = int64(len(data))
		}
	}
	if code, b := e.result(t, r, d, tn.ID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	return job
}

func (e *env) decideCap(t *testing.T, r *turnRoom, tok string, persona int64, asks turn.Level, approve bool) (int, []byte) {
	t.Helper()
	return e.do(t, "POST", r.path(fmt.Sprintf("/personas/%d/cap", persona)), tok, map[string]any{"approve": approve, "model": asks.Model, "effort": asks.Effort})
}

// 002-AC19 002-AC20: a persona asking above the host cap shows in the banner with what it runs
// at; its synthesis says it was capped; only the host answers; a refusal settles that request and
// a later, higher ask opens a new one; an approval reaches the job of the next turn and leaves the
// cap as it is. The answer belongs to the host daemon that gave it: after a hand-over the new
// host's cap applies and the banner comes back, and the old host's answer applies again once it
// hosts again.
func TestHostCap(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob").Token
	sonnetHigh := store.DaemonSettings{MaxModel: "sonnet", MaxEffort: "high"}
	e.settings(t, r.room, alice, sonnetHigh)
	opusMax := turn.Level{Model: "opus", Effort: "max"}
	edit := func(body map[string]string) {
		t.Helper()
		if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), bob, body); code != http.StatusOK {
			t.Fatalf("edit: %d %s", code, b)
		}
	}
	edit(map[string]string{"model": "opus", "effort": "max"})
	st := e.agent(t, r.room, bob)
	if st.Cap == nil || *st.Cap != sonnetHigh || len(st.CapRequests) != 1 ||
		st.CapRequests[0] != (capRequest{PersonaID: r.grill.ID, Asks: opusMax, Runs: turn.Level{Model: "sonnet", Effort: "high"}}) {
		t.Fatalf("banner: %+v %+v", st.Cap, st.CapRequests)
	}

	job := e.runTurn(t, r, alice, alice.p.Token, r.grill.ID, turn.Result{Synthesis: "Capped.", Model: "claude-sonnet-x", Effort: "high", Capped: true})
	if job.Model != "opus" || job.Effort != "max" || job.Approved != (turn.Level{}) {
		t.Fatalf("job: %+v", job)
	}
	if m := e.lastLine(t, r.room); m.Kind != store.KindAgent || m.ReqModel != "opus" || m.ReqEffort != "max" || m.Model != "claude-sonnet-x" {
		t.Fatalf("capped synthesis: %+v", m)
	}

	if code, _ := e.decideCap(t, r, bob, r.grill.ID, opusMax, true); code != http.StatusForbidden {
		t.Fatalf("Bob decided: %d", code)
	}
	if code, b := e.decideCap(t, r, alice.p.Token, r.grill.ID, turn.Level{Model: "opus"}, true); code != http.StatusConflict {
		t.Fatalf("stale decision: %d %s", code, b)
	}
	if code, b := e.decideCap(t, r, alice.p.Token, 999, opusMax, true); code != http.StatusNotFound {
		t.Fatalf("unknown persona: %d %s", code, b)
	}
	if code, b := e.decideCap(t, r, alice.p.Token, r.grill.ID, opusMax, false); code != http.StatusOK {
		t.Fatalf("refuse: %d %s", code, b)
	}
	if m := e.lastLine(t, r.room); m.Body != "Alice refused opus · effort max for 🔥 Grill: it keeps running at the host cap, sonnet · effort high." {
		t.Fatalf("refusal line: %+v", m)
	}
	if st := e.agent(t, r.room, bob); len(st.CapRequests) != 0 {
		t.Fatalf("a refused request is still asked: %+v", st.CapRequests)
	}
	if job := e.runTurn(t, r, alice, alice.p.Token, r.grill.ID, turn.Result{Synthesis: "Still capped.", Capped: true}); job.Approved != (turn.Level{}) {
		t.Fatalf("job after a refusal: %+v", job)
	}

	edit(map[string]string{"effort": "xhigh"})
	opusX := turn.Level{Model: "opus", Effort: "xhigh"}
	if st := e.agent(t, r.room, bob); len(st.CapRequests) != 1 || st.CapRequests[0].Asks != opusX {
		t.Fatalf("a new ask after a refusal: %+v", st.CapRequests)
	}
	if code, b := e.decideCap(t, r, alice.p.Token, r.grill.ID, opusX, true); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	if code, b := e.decideCap(t, r, alice.p.Token, r.grill.ID, opusX, true); code != http.StatusConflict {
		t.Fatalf("approve twice: %d %s", code, b)
	}
	st = e.agent(t, r.room, bob)
	if len(st.CapRequests) != 0 || *st.Cap != sonnetHigh {
		t.Fatalf("after the approval: %+v %+v", st.Cap, st.CapRequests)
	}
	if job := e.runTurn(t, r, alice, alice.p.Token, r.grill.ID, turn.Result{Synthesis: "As asked.", Model: "claude-opus-x", Effort: "xhigh"}); job.Approved != opusX {
		t.Fatalf("job after the approval: %+v", job)
	}
	if m := e.lastLine(t, r.room); m.ReqModel != "" || m.ReqEffort != "" || m.Effort != "xhigh" {
		t.Fatalf("uncapped synthesis: %+v", m)
	}
	// Asking more than approved opens a new request; the approval still raises the cap meanwhile.
	edit(map[string]string{"model": "fable"})
	if st := e.agent(t, r.room, bob); len(st.CapRequests) != 1 || st.CapRequests[0].Runs != opusX {
		t.Fatalf("an ask above the approval: %+v", st.CapRequests)
	}
	edit(map[string]string{"model": "opus"})

	// Carol hosts on her daemon, with its own lower cap: Alice's answer does not follow the role.
	carol := e.daemonFor(t, r.room, "Carol")
	e.connect(t, r.room, carol, true)
	e.settings(t, r.room, carol, store.DaemonSettings{MaxModel: "haiku"})
	if code, b := e.do(t, "PUT", r.path("/host"), alice.p.Token, map[string]int64{"participant_id": carol.p.Participant.ID}); code != http.StatusNoContent {
		t.Fatalf("hand over: %d %s", code, b)
	}
	st = e.agent(t, r.room, bob)
	if st.Cap == nil || st.Cap.MaxModel != "haiku" || len(st.CapRequests) != 1 || st.CapRequests[0].Runs != (turn.Level{Model: "haiku", Effort: "xhigh"}) {
		t.Fatalf("after the hand-over: %+v %+v", st.Cap, st.CapRequests)
	}
	if code, _ := e.decideCap(t, r, alice.p.Token, r.grill.ID, opusX, true); code != http.StatusForbidden {
		t.Fatalf("the former host decided: %d", code)
	}
	if job := e.runTurn(t, r, carol, carol.p.Token, r.grill.ID, turn.Result{Synthesis: "On Carol's.", Capped: true}); job.Approved != (turn.Level{}) {
		t.Fatalf("job on the new host: %+v", job)
	}
	if code, b := e.do(t, "PUT", r.path("/host"), carol.p.Token, map[string]int64{"participant_id": alice.p.Participant.ID}); code != http.StatusNoContent {
		t.Fatalf("hand back: %d %s", code, b)
	}
	if st := e.agent(t, r.room, bob); len(st.CapRequests) != 0 {
		t.Fatalf("Alice's answer is back with her: %+v", st.CapRequests)
	}
	edit(map[string]string{"model": "haiku", "effort": "low"})
	if code, b := e.decideCap(t, r, alice.p.Token, r.grill.ID, turn.Level{Model: "haiku", Effort: "low"}, true); code != http.StatusConflict {
		t.Fatalf("a persona under the cap has nothing to decide: %d %s", code, b)
	}
}

// 002-AC21: with the host's daemon approving everything, a member's new persona waits and cannot
// run, and a member's change answers 202 and waits; the banner lists both; only the host decides,
// and the decision applies.
func TestApproveEverything(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob").Token
	e.settings(t, r.room, alice, store.DaemonSettings{ApproveAll: true})
	sec := e.addPersona(t, r, bob, e.templates(t, r, bob)[1].ID)
	if !sec.Pending {
		t.Fatalf("not pending: %+v", sec)
	}
	if code, b := e.do(t, "POST", r.path("/turns"), alice.p.Token, map[string]int64{"persona_id": sec.ID}); code != http.StatusConflict ||
		!strings.Contains(string(b), "waits for the host's approval") {
		t.Fatalf("a pending persona ran: %d %s", code, b)
	}
	code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), bob, map[string]string{"model": "haiku"})
	if p := decodeJSON[store.RoomPersona](t, b); code != http.StatusAccepted || p.Model != "" {
		t.Fatalf("held edit: %d %s", code, b)
	}
	st := e.agent(t, r.room, bob)
	if len(st.Changes) != 2 || st.Changes[0].Kind != store.ChangeAdd || st.Changes[1].Summary != "model default → haiku" || !st.Cap.ApproveAll {
		t.Fatalf("held: %+v", st.Changes)
	}
	path := func(id int64) string { return r.path(fmt.Sprintf("/changes/%d", id)) }
	if code, _ := e.do(t, "POST", path(st.Changes[0].ID), bob, map[string]bool{"approve": true}); code != http.StatusForbidden {
		t.Fatalf("Bob decided: %d", code)
	}
	for _, c := range st.Changes {
		if code, b := e.do(t, "POST", path(c.ID), alice.p.Token, map[string]bool{"approve": true}); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
	}
	if code, _ := e.do(t, "POST", path(st.Changes[0].ID), alice.p.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("decided twice: %d", code)
	}
	if code, _ := e.do(t, "POST", r.path("/changes/x"), alice.p.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("bad id: %d", code)
	}
	st = e.agent(t, r.room, bob)
	if len(st.Changes) != 0 || st.Personas[0].Model != "haiku" || st.Personas[1].Pending {
		t.Fatalf("after the approvals: %+v", st)
	}
	// The host's own changes apply at once; turning the mode off lets members' changes through too.
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), alice.p.Token, map[string]string{"effort": "low"}); code != http.StatusOK {
		t.Fatalf("host edit: %d %s", code, b)
	}
	e.settings(t, r.room, alice, store.DaemonSettings{})
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), bob, map[string]string{"effort": "high"}); code != http.StatusOK {
		t.Fatalf("edit with the mode off: %d %s", code, b)
	}
}
