package web

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// 002-AC23 002-AC24: @name at a word start, any case, each persona once in order of first mention;
// never in code spans, code blocks, raw HTML or an email address.
func TestMentioned(t *testing.T) {
	ps := []store.RoomPersona{{ID: 1, PersonaFields: store.PersonaFields{Name: "Grill", Model: "sonnet", Effort: "medium"}}, {ID: 2, PersonaFields: store.PersonaFields{Name: "Secretary", Model: "sonnet", Effort: "medium"}},
		{ID: 3, PersonaFields: store.PersonaFields{Name: "Grill-2", Model: "sonnet", Effort: "medium"}}}
	for _, c := range []struct {
		body string
		want []int64
	}{
		{"@grill what now?", []int64{1}},
		{"Over to @SECRETARY, then @Grill.", []int64{2, 1}},
		{"@secretary @Secretary and again @secretary", []int64{2}},
		{"(@grill) *@secretary* [ask @Grill-2](https://x.example)", []int64{1, 2, 3}},
		{"first line\n\n- item @secretary\n> quote @grill", []int64{2, 1}},
		{"@grill-2 is not @grill", []int64{3, 1}},
		{"mail bob@grill or @@grill or @grillroom or @grill_x", nil},
		{"`@grill` in code", nil},
		{"```\n@grill\n```\nafter", nil},
		{"~~~md\n@secretary\n~~~", nil},
		{"    @grill indented code", nil},
		{"<span>@grill</span>", []int64{1}},
		{"<div>\n@grill\n</div>", nil},
		{"<x@grill.example>", nil},
		{"no mention at all", nil},
		{"@nobody here", nil},
	} {
		var got []int64
		for _, p := range mentioned(c.body, ps) {
			got = append(got, p.ID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.body, got, c.want)
		}
	}
}

// 002-AC28: the preamble template renders every field, says when there is no other agent, and
// cuts each line at presenceLineMax runes.
func TestRenderPresence(t *testing.T) {
	got, err := renderPresence(presenceView{Room: "R", Self: presencePersona{Name: "Solo", Level: "model default, effort default", Trigger: "on demand"},
		Chain: "unlimited", Humans: []string{strings.Repeat("é", 200)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No other agent is in the room.", "Host: none", "Speaking now: nobody", "Chain budget: unlimited", "Writing @Solo does nothing"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if n := len([]rune(l)); n > presenceLineMax {
			t.Errorf("line of %d runes: %q", n, l)
		}
	}
	if !strings.Contains(got, "Humans present: éé") || !strings.Contains(got, "é…\n") {
		t.Errorf("long line not cut with an ellipsis:\n%s", got)
	}
}

// 002-AC28: the preamble lists humans by the @token a mention of them takes, which the server's
// rule resolves to that very person, and tells the persona to address them that way. A human
// whose token a persona holds stays bare: @Grill would give Grill a turn (005-AC27).
func TestPresenceAddressesHumans(t *testing.T) {
	personas := []store.RoomPersona{{ID: 1, PersonaFields: store.PersonaFields{Name: "Grill"}}}
	people := []store.Participant{{ID: 3, Name: "Damien"}, {ID: 4, Name: "Ana María"}, {ID: 5, Name: "grill"}, {ID: 6, Name: "Jean-Luc O'Neil"}}
	var humans []string
	for _, p := range people {
		humans = append(humans, humanAt(p.Name, "#"+strconv.FormatInt(p.ID, 10), personas))
	}
	got, err := renderPresence(presenceView{Room: "R", Self: presencePersona{Name: "Grill", Level: "model default, effort default", Trigger: "on demand"},
		Chain: "unlimited", Humans: humans})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nHumans present: @Damien ·#3, @Ana_María ·#4, grill ·#5, @Jean-Luc_O_Neil ·#6\n",
		"\nAddress a human by the @name listed above: it notifies them. @agent is for giving a turn.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if n := len([]rune(l)); n > presenceLineMax {
			t.Errorf("line of %d runes: %q", n, l)
		}
	}
	for i, p := range people {
		at, _, _ := strings.Cut(humans[i], " ·")
		if !strings.HasPrefix(at, "@") {
			continue
		}
		if n := named("ok "+at+", go", personas, people); len(n) != 1 || n[0].ParticipantID != p.ID {
			t.Errorf("%s names %+v, want participant %d", at, n, p.ID)
		}
	}
}

// 002-AC28: the chain line says what is left, never below zero, or that there is no limit; with
// no host daemon yet a persona's level is what it asks for.
func TestPresenceParts(t *testing.T) {
	for c, want := range map[store.Chain]string{
		{Budget: 25}:                           "25 of 25 agent turns left",
		{Budget: 3, Count: 5}:                  "0 of 3 agent turns left",
		{Budget: 3, Count: 5, Suspended: true}: "unlimited, the limit is suspended (5 agent turns in this chain)",
	} {
		if got := chainText(c); got != want {
			t.Errorf("%+v: %q, want %q", c, got, want)
		}
	}
	e := newEnv(t)
	room := e.createRoom(t, "Solo room")
	r, err := e.st.Room(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := e.st.RoomPersonas(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	ps[0].Model, ps[0].EveryN = "opus", 3
	got, err := e.srv.presencePreamble(t.Context(), r, &ps[0], "pressed by Alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You: @Grill, model opus, effort medium, on demand and every 3 rounds\n", "No other agent is in the room.\n", "Host: none\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
}

// 002-AC28: the current speaker is the author of a voice line from the last ten seconds.
func TestSpeakingAt(t *testing.T) {
	now := time.Now()
	for age, want := range map[time.Duration]bool{0: true, speakerWindow: true, speakerWindow + time.Millisecond: false, time.Hour: false} {
		if got := speakingAt(&store.Message{TS: now.Add(-age)}, now); got != want {
			t.Errorf("line %v old: %v, want %v", age, got, want)
		}
	}
}

// postBody posts body in the room as tok.
func (e *env) postBody(t *testing.T, r *turnRoom, tok, body string) {
	t.Helper()
	if code, b := e.do(t, "POST", r.path("/messages"), tok, map[string]string{"body": body}); code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
}

// nextTurn waits for d's next turn event and returns the turn.
func (e *env) nextTurn(t *testing.T, r *turnRoom, d *testDaemon) store.Turn {
	t.Helper()
	ev := nextEvent(t, d.events, "turn")
	id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(ev.data, `{"id":`), "}"), 10, 64)
	if err != nil {
		t.Fatalf("turn event %+v: %v", ev, err)
	}
	tn, err := e.st.Turn(t.Context(), r.room, id)
	if err != nil {
		t.Fatal(err)
	}
	return *tn
}

// finish completes tn with synthesis, continuing its persona's session.
func (e *env) finish(t *testing.T, r *turnRoom, d *testDaemon, tn store.Turn, synthesis string) {
	t.Helper()
	res := turn.Result{Synthesis: synthesis, SessionID: sid1, Session: []byte("x\n")}
	if data, err := e.st.AgentSession(t.Context(), r.room, tn.PersonaID, sid1); err == nil {
		res.SessionOffset = int64(len(data))
	}
	if code, b := e.result(t, r, d, tn.ID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
}

func (e *env) chain(t *testing.T, r *turnRoom, tok string, body any) (int, store.Chain) {
	t.Helper()
	code, b := e.do(t, "PATCH", r.path("/chain"), tok, body)
	if code != http.StatusOK {
		return code, store.Chain{}
	}
	return code, decodeJSON[store.Chain](t, b)
}

// secretary adds the Secretary persona to r's room as tok, with the patch applied.
func (e *env) secretary(t *testing.T, r *turnRoom, tok string, patch map[string]any) store.RoomPersona {
	t.Helper()
	sec := e.addPersona(t, r, tok, e.templates(t, r, tok)[1].ID)
	if patch == nil {
		return sec
	}
	code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), tok, patch)
	if code != http.StatusOK {
		t.Fatalf("patch: %d %s", code, b)
	}
	return decodeJSON[store.RoomPersona](t, b)
}

// 002-AC22 002-AC24 002-AC25 002-AC26: personas call each other with @name and "every N rounds"
// runs a persona after N completed turns since its own; each is a chain link through the queue. At
// the budget the next link is dropped, with a line in the chat; the re-arm button starts over. A
// persona naming itself runs again only with self-call on; several mentions queue each persona
// once. Voice lines and plain text leave the count alone; a press and a human @mention reset it.
func TestChainLinks(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	count := func() int { return e.agent(t, r.room, tok).Chain.Count }
	if code, c := e.chain(t, r, tok, map[string]int{"budget": 3}); code != http.StatusOK || c.Budget != 3 {
		t.Fatalf("budget: %d %+v", code, c)
	}

	// A ping-pong between the two personas: three links, then the fourth is dropped.
	tn := e.press(t, r, alice, tok, false)
	for i, next := range []store.RoomPersona{sec, r.grill, sec} {
		other := "@" + strings.ToLower(next.Name)
		e.finish(t, r, alice, tn, "Over to "+other+" and "+other+".")
		if got := count(); got != i+1 {
			t.Fatalf("link %d: count %d", i+1, got)
		}
		tn = e.nextTurn(t, r, alice)
		if tn.PersonaID != next.ID || tn.ParticipantID != 0 {
			t.Fatalf("link %d ran %+v", i+1, tn)
		}
	}
	e.finish(t, r, alice, tn, "Back to @grill.")
	st := e.agent(t, r.room, tok)
	if !st.Chain.Paused() || len(st.Queue) != 0 || st.Turn.Status != store.TurnDone {
		t.Fatalf("at the budget: %+v", st)
	}
	if m := e.lastLine(t, r.room); m.Kind != store.KindSystem || m.Sender != "Secretary" ||
		m.Body != "The agent chain reached its budget of 3 turns: Grill did not run. Re-arm the chain to let agents call each other again." {
		t.Fatalf("pause line: %+v", m)
	}

	// Neither a voice line (the STT's lines go through bus.Post) nor plain text resets it; Bob, who
	// may not press, cannot re-arm it.
	e.postBody(t, r, tok, "plain text, no mention")
	if err := e.bus.Post(t.Context(), &store.Message{RoomID: r.room, Kind: store.KindVoice, Source: store.VoiceSourceServer, ParticipantID: alice.p.Participant.ID, Sender: "Alice", Body: "at grill"}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 3 {
		t.Fatalf("voice or text reset the chain: %d", got)
	}
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob").Token
	for _, body := range []any{map[string]bool{"rearm": true}, map[string]bool{"suspended": true}, map[string]int{"budget": 9}} {
		if code, _ := e.chain(t, r, bob, body); code != http.StatusForbidden {
			t.Fatalf("Bob steers the chain %v: %d", body, code)
		}
	}
	for _, bad := range []any{map[string]int{"budget": 0}, map[string]int{"budget": store.MaxChainBudget + 1}, "rearm"} {
		if code, _ := e.chain(t, r, tok, bad); code != http.StatusBadRequest {
			t.Fatalf("%v: %d", bad, code)
		}
	}
	if code, c := e.chain(t, r, tok, map[string]bool{"rearm": true}); code != http.StatusOK || c.Count != 0 {
		t.Fatalf("re-arm: %d %+v", code, c)
	}
	if m := e.lastLine(t, r.room); m.Body != "Alice re-armed the agent chain." {
		t.Fatalf("re-arm line: %+v", m)
	}

	// Self-call: off, naming itself does nothing; on, it queues itself once.
	tn = e.press(t, r, alice, tok, false)
	e.finish(t, r, alice, tn, "@Grill again.")
	if st := e.agent(t, r.room, tok); st.Turn.Status != store.TurnDone || len(st.Queue) != 0 || st.Chain.Count != 0 {
		t.Fatalf("self-call off: %+v", st)
	}
	on := true
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), tok, map[string]any{"self_call": on}); code != http.StatusOK {
		t.Fatalf("self-call on: %d %s", code, b)
	}
	tn = e.press(t, r, alice, tok, false)
	e.finish(t, r, alice, tn, "@grill once more, @Grill.")
	if again := e.nextTurn(t, r, alice); again.PersonaID != r.grill.ID || count() != 1 {
		t.Fatalf("self-call on: %+v, count %d", again, count())
	} else {
		e.finish(t, r, alice, again, "Done.")
	}

	// A press resets the count.
	if code, c := e.chain(t, r, tok, map[string]int{"budget": 1}); code != http.StatusOK || c.Count != 1 || !c.Paused() {
		t.Fatalf("budget 1: %+v", c)
	}
	tn = e.press(t, r, alice, tok, false)
	if got := count(); got != 0 {
		t.Fatalf("press: count %d", got)
	}
	e.finish(t, r, alice, tn, "Done.")
}

// 002-AC22: "every N rounds" runs the persona after N completed turns of any persona since its own
// last turn, through the queue, as a chain link.
func TestEveryNRounds(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"every_n": 2})
	if sec.EveryN != 2 {
		t.Fatalf("every_n: %+v", sec)
	}
	for round := 1; round <= 2; round++ {
		tn := e.press(t, r, alice, tok, false)
		e.finish(t, r, alice, tn, "Round.")
	}
	auto := e.nextTurn(t, r, alice)
	if auto.PersonaID != sec.ID || auto.ParticipantID != 0 || e.agent(t, r.room, tok).Chain.Count != 1 {
		t.Fatalf("after two rounds: %+v", auto)
	}
	e.finish(t, r, alice, auto, "Minutes.")
	// Its own turn starts the count over: one more round is not enough.
	tn := e.press(t, r, alice, tok, false)
	e.finish(t, r, alice, tn, "Round.")
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 || st.Turn.PersonaID != r.grill.ID {
		t.Fatalf("one round after its turn: %+v", st)
	}
}

// 002-AC23 002-AC26: a human's @mention queues each persona named once, as that human's press, and
// resets the chain; only for someone allowed to press, and while the agent is online.
func TestHumanMention(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	if code, c := e.chain(t, r, tok, map[string]int{"budget": 1}); code != http.StatusOK || c.Budget != 1 {
		t.Fatalf("budget: %+v", c)
	}
	tn := e.press(t, r, alice, tok, false)
	e.finish(t, r, alice, tn, "Ask @secretary.")
	link := e.nextTurn(t, r, alice)
	if e.agent(t, r.room, tok).Chain.Count != 1 {
		t.Fatal("no link")
	}

	e.postBody(t, r, bob.Token, "@secretary and @grill, from Bob who may not press")
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 || st.Chain.Count != 1 {
		t.Fatalf("Bob's mention: %+v", st)
	}
	e.postBody(t, r, tok, "`@grill` in code only")
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 {
		t.Fatalf("mention in code: %+v", st)
	}
	e.postBody(t, r, tok, "@Grill then @secretary, then @GRILL again")
	st := e.agent(t, r.room, tok)
	if st.Chain.Count != 0 || len(st.Queue) != 2 || st.Queue[0].PersonaID != r.grill.ID || st.Queue[1].PersonaID != sec.ID ||
		st.Queue[0].Source != store.SourceMention || st.Queue[0].ParticipantID != alice.p.Participant.ID {
		t.Fatalf("Alice's mention: %+v", st)
	}
	e.finish(t, r, alice, link, "Noted.")
	if next := e.nextTurn(t, r, alice); next.PersonaID != r.grill.ID || next.ParticipantID != alice.p.Participant.ID {
		t.Fatalf("queued mention ran %+v", next)
	} else {
		e.finish(t, r, alice, next, "Grill speaks.")
	}
	e.finish(t, r, alice, e.nextTurn(t, r, alice), "Secretary speaks.")

	// Offline host: a mention queues nothing.
	alice.drop()
	e.waitAgent(t, r.room, tok, hostIs(alice.p.Participant.ID, false))
	e.postBody(t, r, tok, "@grill are you there?")
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 {
		t.Fatalf("mention while offline: %+v", st.Queue)
	}
}

// 002-AC24: a turn an agent queued has no participant: anyone allowed to press may cancel it,
// others may not.
func TestCancelAgentTurn(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), tok, map[string]bool{"self_call": true}); code != http.StatusOK {
		t.Fatalf("self-call: %d %s", code, b)
	}
	tn := e.press(t, r, alice, tok, false)
	if code, b := e.do(t, "POST", r.path("/turns"), tok, map[string]int64{"persona_id": sec.ID}); code != http.StatusAccepted {
		t.Fatalf("queue Secretary: %d %s", code, b)
	}
	e.finish(t, r, alice, tn, "@grill, go on after the Secretary.")
	running := e.nextTurn(t, r, alice)
	q := e.agent(t, r.room, tok).Queue
	if running.PersonaID != sec.ID || len(q) != 1 || q[0].PersonaID != r.grill.ID || q[0].Source != string(store.LinkCall) || q[0].ParticipantID != 0 {
		t.Fatalf("queue: %+v, running %+v", q, running)
	}
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	cancel := fmt.Sprintf("/queue/%d", q[0].ID)
	if code, _ := e.do(t, "DELETE", r.path(cancel), bob.Token, nil); code != http.StatusForbidden {
		t.Fatalf("Bob cancels without the button: %d", code)
	}
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/participants/%d", bob.Participant.ID)), tok, map[string]bool{"can_trigger": true}); code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", code, b)
	}
	if code, _ := e.do(t, "DELETE", r.path(cancel), bob.Token, nil); code != http.StatusNoContent {
		t.Fatalf("Bob cancels with the button: %d", code)
	}
	e.finish(t, r, alice, running, "Minutes.")
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 || st.Turn.PersonaID != sec.ID {
		t.Fatalf("after cancel: %+v", st)
	}
}

// 002-AC28: the preamble opening a prompt, golden. Secretary's turn starts after Grill's: it sees
// its own effective level under the host cap and its triggers; Grill idle with its turn count and
// the first line of its synthesis; Critic queued by Grill's call; Alice present with her room
// open, Bob speaking a moment ago, Alice hosting; the chain budget left.
func TestPresencePreamble(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	e.settings(t, r.room, alice, store.DaemonSettings{MaxModel: "sonnet", MaxEffort: "high"})
	sec := e.secretary(t, r, tok, map[string]any{"every_n": 2, "self_call": true, "model": "opus", "effort": "max"})
	critic := e.addPersona(t, r, tok, e.templates(t, r, tok)[0].ID)
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", critic.ID)), tok, map[string]string{"name": "Critic", "model": "haiku"}); code != http.StatusOK {
		t.Fatalf("critic: %d %s", code, b)
	}
	e.openEvents(t, r.room, tok, "")
	e.waitPresent(t, alice.p.Participant.ID)
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	if err := e.bus.Post(t.Context(), &store.Message{RoomID: r.room, Kind: store.KindVoice, Source: store.VoiceSourceServer, ParticipantID: bob.Participant.ID, Sender: "Bob", Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	if code, c := e.chain(t, r, tok, map[string]int{"budget": 10}); code != http.StatusOK || c.Budget != 10 {
		t.Fatalf("budget: %+v", c)
	}
	tn := e.press(t, r, alice, tok, false)
	if code, b := e.do(t, "POST", r.path("/turns"), tok, map[string]int64{"persona_id": sec.ID}); code != http.StatusAccepted {
		t.Fatalf("queue Secretary: %d %s", code, b)
	}
	e.finish(t, r, alice, tn, "Over to @critic, and a very long first line that goes on and on until the preamble has to cut it short.\nSecond line.")
	next := e.nextTurn(t, r, alice)
	if next.PersonaID != sec.ID {
		t.Fatalf("ran %+v", next)
	}
	prompt := e.job(t, r, alice, next.ID).Prompt
	preamble, _, ok := strings.Cut(prompt, "\nYou are Secretary, an agent")
	if !ok {
		t.Fatalf("prompt:\n%s", prompt)
	}
	// Profile uuids are random: TestHumanTags pins their rendering.
	golden(t, "testdata/presence.golden", regexp.MustCompile(`·[0-9a-f]{4}\b`).ReplaceAllString(preamble, "·<uuid>"))
}

// waitPresent waits until participant pid has a room stream open.
func (e *env) waitPresent(t *testing.T, pid int64) {
	t.Helper()
	db := e.sqlDB(t)
	streaming := func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM presence WHERE participant_id = $1 AND tab <> ''`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	for i := 0; !streaming(); i++ {
		if i > 500 {
			t.Fatal("never present")
		}
		time.Sleep(10 * time.Millisecond) // polls presence, bounded by 500 rounds
	}
}
