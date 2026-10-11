package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

func (r *turnRoom) path(s string) string { return "/api/rooms/" + r.room + s }

// templates returns the library as tok sees it, presets first.
func (e *env) templates(t *testing.T, r *turnRoom, tok string) []store.Template {
	t.Helper()
	code, b := e.do(t, "GET", r.path("/templates"), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("templates: %d %s", code, b)
	}
	return decodeJSON[[]store.Template](t, b)
}

func (e *env) addPersona(t *testing.T, r *turnRoom, tok string, template int64) store.RoomPersona {
	t.Helper()
	code, b := e.do(t, "POST", r.path("/personas"), tok, map[string]int64{"template_id": template})
	if code != http.StatusCreated {
		t.Fatalf("add persona: %d %s", code, b)
	}
	return decodeJSON[store.RoomPersona](t, b)
}

func (e *env) lastLine(t *testing.T, room string) store.Message {
	t.Helper()
	msgs, err := e.st.Messages(t.Context(), room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return msgs[len(msgs)-1]
}

// 002-AC13 002-AC14 002-AC15: any member adds a persona from the library and edits it, each change
// in the chat; fields are checked; a profile publishes templates and versions of its own; the
// room is offered a newer version and takes it on request; the admin removes published templates.
func TestPersonaAPI(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice.p.Token
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob").Token
	ts := e.templates(t, r, bob)
	if len(ts) != 2 || ts[0].Preset != store.PresetDefault || ts[1].Name != "Secretary" {
		t.Fatalf("library: %+v", ts)
	}
	// Bob has no button right: adding and editing personas need none.
	sec := e.addPersona(t, r, bob, ts[1].ID)
	if m := e.lastLine(t, r.room); m.Kind != store.KindSystem || !strings.HasPrefix(m.Body, "Bob added the persona 📝 Secretary") {
		t.Fatalf("add line: %+v", m)
	}
	if code, _ := e.do(t, "POST", r.path("/personas"), bob, map[string]int64{"template_id": 999}); code != http.StatusNotFound {
		t.Fatalf("unknown template: %d", code)
	}
	for _, bad := range []map[string]any{
		{"name": "two words"}, {"name": ""}, {"name": strings.Repeat("n", 31)}, {"badge": ""}, {"badge": "123456789"},
		{"colour": "red"}, {"colour": "#12345"}, {"model": "gpt-5"}, {"effort": "turbo"}, {"text": " "},
		{"text": strings.Repeat("x", maxPersonaText+1)}, {"every_n": -1}, {"every_n": maxEveryN + 1},
	} {
		if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), bob, bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, code, b)
		}
	}
	code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), bob, map[string]any{"effort": "xhigh", "model": "opus", "badge": " 🗒 "})
	if p := decodeJSON[store.RoomPersona](t, b); code != http.StatusOK || p.Effort != "xhigh" || p.Model != "opus" || p.Badge != "🗒" {
		t.Fatalf("edit: %d %s", code, b)
	}
	if m := e.lastLine(t, r.room); m.Body != "Bob changed 📝 Secretary: badge 📝 → 🗒, model default → opus, effort default → xhigh." {
		t.Fatalf("edit line: %+v", m)
	}
	if code, _ := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), bob, map[string]string{"name": "grill"}); code != http.StatusConflict {
		t.Fatalf("name taken: %d", code)
	}
	if code, _ := e.do(t, "PATCH", r.path("/personas/999"), bob, map[string]string{"name": "X"}); code != http.StatusNotFound {
		t.Fatalf("unknown persona: %d", code)
	}

	// Publishing: a template of Alice's own, then a version only she may add.
	critic := map[string]any{"name": "Critic", "badge": "🧐", "colour": "#112233", "text": "Find the flaw.", "effort": "low", "title": "Critic"}
	code, b = e.do(t, "POST", r.path("/templates"), alice, critic)
	tp := decodeJSON[store.Template](t, b)
	if code != http.StatusCreated || tp.Version != 1 || tp.Author != "Alice" || !tp.Mine || tp.Effort != "low" {
		t.Fatalf("publish: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", r.path("/templates"), alice, map[string]any{"name": "Critic"}); code != http.StatusBadRequest {
		t.Fatalf("publish without text: %d", code)
	}
	if code, _ := e.do(t, "POST", r.path("/templates"), alice, map[string]any{"name": "Critic", "badge": "🧐", "colour": "#112233", "text": "x", "effort": "turbo"}); code != http.StatusBadRequest {
		t.Fatalf("publish with a bad effort: %d", code)
	}
	critic["template_id"], critic["text"] = tp.ID, "Find the flaw, then the fix."
	if code, _ := e.do(t, "POST", r.path("/templates"), bob, critic); code != http.StatusForbidden {
		t.Fatalf("Bob versions Alice's template: %d", code)
	}
	if code, _ := e.do(t, "POST", r.path("/templates"), bob, map[string]any{"template_id": ts[0].ID, "name": "Grill", "badge": "🔥", "colour": "#112233", "text": "x"}); code != http.StatusForbidden {
		t.Fatalf("Bob versions a preset: %d", code)
	}
	cp := e.addPersona(t, r, bob, tp.ID)
	code, b = e.do(t, "POST", r.path("/templates"), alice, critic)
	if v2 := decodeJSON[store.Template](t, b); code != http.StatusCreated || v2.Version != 2 {
		t.Fatalf("version 2: %d %s", code, b)
	}
	st := e.agent(t, r.room, bob)
	var offered store.RoomPersona
	for _, p := range st.Personas {
		if p.ID == cp.ID {
			offered = p
		}
	}
	if offered.Text != "Find the flaw." || offered.UpdateVersion != 2 {
		t.Fatalf("offer: %+v", offered)
	}
	code, b = e.do(t, "POST", r.path(fmt.Sprintf("/personas/%d/update", cp.ID)), bob, nil)
	if p := decodeJSON[store.RoomPersona](t, b); code != http.StatusOK || p.Text != "Find the flaw, then the fix." || p.TemplateVersion != 2 {
		t.Fatalf("update: %d %s", code, b)
	}

	// The admin's library.
	if code, _ := e.do(t, "GET", "/api/templates", bob, nil); code != http.StatusUnauthorized {
		t.Fatalf("library without the admin token: %d", code)
	}
	if code, b := e.do(t, "GET", "/api/templates", e.admin, nil); code != http.StatusOK || len(decodeJSON[[]store.Template](t, b)) != 3 {
		t.Fatalf("admin library: %d %s", code, b)
	}
	for path, want := range map[string]int{
		"/api/templates/" + strconv.FormatInt(ts[0].ID, 10): http.StatusConflict,
		"/api/templates/999": http.StatusNotFound,
		"/api/templates/x":   http.StatusNotFound,
		"/api/templates/" + strconv.FormatInt(tp.ID, 10): http.StatusNoContent,
	} {
		if code, b := e.do(t, "DELETE", path, e.admin, nil); code != want {
			t.Errorf("DELETE %s: %d %s, want %d", path, code, b, want)
		}
	}
	if code, _ := e.do(t, "DELETE", r.path(fmt.Sprintf("/personas/%d", cp.ID)), bob, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if m := e.lastLine(t, r.room); m.Body != "Bob removed the persona 🧐 Critic." {
		t.Fatalf("remove line: %+v", m)
	}
	if code, _ := e.do(t, "DELETE", r.path(fmt.Sprintf("/personas/%d", cp.ID)), bob, nil); code != http.StatusNotFound {
		t.Fatalf("remove twice: %d", code)
	}
}

// 002-AC15 002-AC18: a persona changed during its turn runs that turn as it started; the next turn
// gets the change. The job carries the persona's text, model and effort, and the synthesis shows
// the model and effort the daemon reports; an effort claude does not know is not shown.
func TestPersonaEditDuringTurn(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tn := e.press(t, r, alice, alice.p.Token, false)
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), alice.p.Token,
		map[string]string{"text": "Be kind.", "model": "sonnet", "effort": "high"}); code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, b)
	}
	if code, _ := e.do(t, "DELETE", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), alice.p.Token, nil); code != http.StatusConflict {
		t.Fatalf("remove while running: %d", code)
	}
	job := e.job(t, r, alice, tn.ID)
	if job.Persona != "Grill" || job.System != r.grill.Text || job.Model != "" || job.Effort != "" || !strings.Contains(job.Prompt, "\nYou are Grill, an agent") {
		t.Fatalf("job of the running turn: %+v", job)
	}
	if code, b := e.result(t, r, alice, tn.ID, turn.Result{Synthesis: "One.", Model: "claude-haiku-x", Effort: "turbo", SessionID: sid1, Session: []byte("a\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if m := e.lastLine(t, r.room); m.Kind != store.KindAgent || m.Sender != "Grill" || m.Badge != "🔥" || m.Model != "claude-haiku-x" || m.Effort != "" {
		t.Fatalf("synthesis: %+v", m)
	}
	tn2 := e.press(t, r, alice, alice.p.Token, false)
	job = e.job(t, r, alice, tn2.ID)
	if job.System != "Be kind." || job.Model != "sonnet" || job.Effort != "high" || job.Resume != sid1 {
		t.Fatalf("job of the next turn: %+v", job)
	}
	if code, b := e.result(t, r, alice, tn2.ID, turn.Result{Synthesis: "Two.", Model: "claude-sonnet-x", Effort: "high", SessionID: sid1, SessionOffset: 2, Session: []byte("b\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if m := e.lastLine(t, r.room); m.Model != "claude-sonnet-x" || m.Effort != "high" {
		t.Fatalf("synthesis: %+v", m)
	}
	if last, _ := e.st.LastTurn(t.Context(), r.room); last.Effort != "high" || last.PersonaID != r.grill.ID {
		t.Fatalf("turn: %+v", last)
	}
}

// 002-AC17: one turn at a time per room. Presses during a turn queue in order, one per persona,
// shown in the agent state; only who queued one cancels it; when the turn ends the next starts by
// itself on the host, for its own persona.
func TestTurnQueue(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/participants/%d", bob.Participant.ID)), alice.p.Token, map[string]bool{"can_trigger": true}); code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", code, b)
	}
	sec := e.addPersona(t, r, alice.p.Token, e.templates(t, r, alice.p.Token)[1].ID)
	first := e.press(t, r, alice, alice.p.Token, false)
	queue := func(tok string, persona int64) (int, store.QueuedTurn) {
		code, b := e.do(t, "POST", r.path("/turns"), tok, map[string]int64{"persona_id": persona})
		if code != http.StatusAccepted {
			return code, store.QueuedTurn{}
		}
		return code, decodeJSON[store.QueuedTurn](t, b)
	}
	_, q1 := queue(bob.Token, sec.ID)
	if code, _ := queue(alice.p.Token, sec.ID); code != http.StatusConflict {
		t.Fatalf("persona queued twice: %d", code)
	}
	_, q2 := queue(alice.p.Token, r.grill.ID)
	if code, _ := queue(alice.p.Token, 999); code != http.StatusNotFound {
		t.Fatalf("unknown persona: %d", code)
	}
	st := e.agent(t, r.room, bob.Token)
	if len(st.Queue) != 2 || st.Queue[0].ID != q1.ID || st.Queue[0].PersonaID != sec.ID || st.Queue[0].ParticipantID != bob.Participant.ID || st.Queue[1].ID != q2.ID {
		t.Fatalf("queue: %+v", st.Queue)
	}
	if code, _ := e.do(t, "DELETE", r.path(fmt.Sprintf("/queue/%d", q1.ID)), alice.p.Token, nil); code != http.StatusForbidden {
		t.Fatalf("cancel someone else's: %d", code)
	}
	if code, _ := e.do(t, "DELETE", r.path("/queue/x"), alice.p.Token, nil); code != http.StatusNotFound {
		t.Fatalf("cancel a bad id: %d", code)
	}

	if code, b := e.result(t, r, alice, first.ID, turn.Result{Synthesis: "Grill speaks.", SessionID: sid1, Session: []byte("g\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	ev := nextEvent(t, alice.events, "turn")
	st = e.agent(t, r.room, bob.Token)
	if st.Turn.Status != store.TurnRunning || st.Turn.PersonaID != sec.ID || st.Turn.ParticipantID != bob.Participant.ID ||
		ev.data != fmt.Sprintf(`{"id":%d}`, st.Turn.ID) || len(st.Queue) != 1 {
		t.Fatalf("after the first turn: %+v, event %+v", st, ev)
	}
	// The Secretary reads Grill's synthesis as news, tagged as another agent's.
	job := e.job(t, r, alice, st.Turn.ID)
	if job.Persona != "Secretary" || job.Resume != "" || !strings.Contains(job.Prompt, "**Grill** (agent): Grill speaks.") {
		t.Fatalf("secretary's job: %+v", job)
	}
	if code, _ := e.do(t, "DELETE", r.path(fmt.Sprintf("/queue/%d", q2.ID)), alice.p.Token, nil); code != http.StatusNoContent {
		t.Fatalf("cancel: %d", code)
	}
	if code, b := e.result(t, r, alice, st.Turn.ID, turn.Result{Synthesis: "Noted.", SessionID: "22222222-2222-4333-8444-555555555555", Session: []byte("s\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if st := e.agent(t, r.room, bob.Token); st.Turn.Status != store.TurnDone || len(st.Queue) != 0 {
		t.Fatalf("after cancel: %+v", st)
	}
	// Grill's next turn: the Secretary's line is news, its own synthesis is not.
	tn := e.press(t, r, alice, alice.p.Token, false)
	job = e.job(t, r, alice, tn.ID)
	if job.Resume != sid1 || !strings.Contains(job.Prompt, "**Secretary** (agent): Noted.") || strings.Contains(job.Prompt, "Grill speaks.") {
		t.Fatalf("grill's next job: %+v", job)
	}
}

// 002-AC17: a queue left by a dropped host waits, and starts once a host's daemon is back.
func TestQueueWaitsForHost(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	first := e.press(t, r, alice, alice.p.Token, false)
	if code, b := e.do(t, "POST", r.path("/turns"), alice.p.Token, map[string]any{}); code != http.StatusAccepted {
		t.Fatalf("queue: %d %s", code, b)
	}
	alice.drop()
	e.waitAgent(t, r.room, alice.p.Token, func(st agentState) bool {
		return !st.Online && st.Turn.ID == first.ID && st.Turn.Status == store.TurnFailed
	})
	if st := e.agent(t, r.room, alice.p.Token); len(st.Queue) != 1 {
		t.Fatalf("queue after the drop: %+v", st.Queue)
	}
	if code, _ := e.do(t, "POST", r.path("/turns"), alice.p.Token, map[string]any{}); code != http.StatusConflict {
		t.Fatalf("press with the host away: %d", code)
	}
	e.connect(t, r.room, alice, true)
	ev := nextEvent(t, alice.events, "turn")
	st := e.waitAgent(t, r.room, alice.p.Token, func(st agentState) bool { return st.Turn.ID != first.ID })
	if st.Turn.Status != store.TurnRunning || len(st.Queue) != 0 || ev.data != fmt.Sprintf(`{"id":%d}`, st.Turn.ID) {
		t.Fatalf("after reconnect: %+v %+v", st, ev)
	}
}

// 002-AC16: a thread reply by one persona is news to another, flagged (new), and not to itself.
func TestOtherPersonaRepliesAreNews(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	sec := e.addPersona(t, r, alice.p.Token, e.templates(t, r, alice.p.Token)[1].ID)
	const sid2 = "22222222-2222-4333-8444-555555555555"
	run := func(persona int64, res turn.Result) turn.Job {
		t.Helper()
		code, b := e.do(t, "POST", r.path("/turns"), alice.p.Token, map[string]int64{"persona_id": persona})
		if code != http.StatusCreated {
			t.Fatalf("press: %d %s", code, b)
		}
		nextEvent(t, alice.events, "turn")
		id := decodeJSON[store.Turn](t, b).ID
		job := e.job(t, r, alice, id)
		if code, b := e.result(t, r, alice, id, res); code != http.StatusOK {
			t.Fatalf("result: %d %s", code, b)
		}
		return job
	}
	run(sec.ID, turn.Result{SessionID: sid2, Session: []byte("s\n")})
	run(r.grill.ID, turn.Result{SessionID: sid1, Session: []byte("g\n"),
		Changes: []turn.Change{{Path: turn.RepliesFile, Content: fmt.Appendf(nil, `[{"annotation": %d, "body": "the grill reply"}]`, r.ann)}}})
	if job := run(sec.ID, turn.Result{SessionID: sid2, SessionOffset: 2, Session: []byte("s\n")}); !strings.Contains(job.Prompt, "- **Grill** (new): the grill reply") {
		t.Fatalf("the secretary's prompt lacks Grill's reply as news:\n%s", job.Prompt)
	}
	if job := run(r.grill.ID, turn.Result{SessionID: sid1, SessionOffset: 2, Session: []byte("g\n")}); strings.Contains(job.Prompt, "the grill reply") {
		t.Fatalf("Grill's own reply came back as news:\n%s", job.Prompt)
	}
}

// Persona texts from API clients may use CRLF; the room stores LF. Limits answer 409.
func TestPersonaTextNewlinesAndLimits(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), tok, map[string]string{"text": "one\r\ntwo"})
	if p := decodeJSON[store.RoomPersona](t, b); code != http.StatusOK || p.Text != "one\ntwo" {
		t.Fatalf("CRLF text: %d %s", code, b)
	}
	sec := e.templates(t, r, tok)[1].ID
	for range store.MaxRoomPersonas - 1 {
		e.addPersona(t, r, tok, sec)
	}
	if code, b := e.do(t, "POST", r.path("/personas"), tok, map[string]int64{"template_id": sec}); code != http.StatusConflict || !strings.Contains(string(b), "at most 12 personas per room") {
		t.Fatalf("persona past the limit: %d %s", code, b)
	}
}
