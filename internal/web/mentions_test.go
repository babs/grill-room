package web

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// say posts body as tok and returns the notices its author alone gets.
func (e *env) say(t *testing.T, r *turnRoom, tok, body string) []mentionNotice {
	t.Helper()
	code, b := e.do(t, "POST", r.path("/messages"), tok, map[string]string{"body": body})
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	return decodeJSON[struct {
		Notices []mentionNotice `json:"mention_notices"`
	}](t, b).Notices
}

func noticesAre(t *testing.T, got []mentionNotice, want ...mentionNotice) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("notices %v, want %v", got, want)
	}
}

func TestMentionToken(t *testing.T) {
	for name, want := range map[string]string{"Bob": "Bob", "Ana María": "Ana_María", " J.R. Ewing ": "J_R_Ewing", "x--y": "x--y", "@@": ""} {
		if got := mentionToken(name); got != want {
			t.Errorf("mentionToken(%q) = %q, want %q", name, got, want)
		}
	}
}

// 005-AC3: a mention that cannot run tells its author why: no right to trigger, no host online,
// a persona waiting for the host's approval, a name matching nobody. A participant's @name is no
// unknown name, and a mention that queues says nothing. 005-AC4: the agent state links the
// message to the turn it queued.
func TestMentionNotices(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob Smith")

	noticesAre(t, e.say(t, r, bob.Token, "@grill @Grill, what now?"), mentionNotice{"Grill", whyNoRight})
	noticesAre(t, e.say(t, r, bob.Token, "@Alice and `@nobody` in code, and bob@example.com"))
	noticesAre(t, e.say(t, r, tok, "@Bob_Smith ask @grill, not @nobody or @NOBODY"), mentionNotice{"nobody", whyUnknown})
	running := e.nextTurn(t, r, alice)
	if st := e.agent(t, r.room, tok); len(st.Mentions) != 1 || st.Mentions[0].PersonaID != r.grill.ID ||
		st.Mentions[0].Status != store.TurnRunning || st.Mentions[0].TurnID != running.ID {
		t.Fatalf("running mention: %+v", st.Mentions)
	}
	e.finish(t, r, alice, running, "Here.")
	if st := e.agent(t, r.room, tok); st.Mentions[0].Status != store.TurnDone || st.Mentions[0].LineID == 0 {
		t.Fatalf("done mention: %+v", st.Mentions)
	}

	e.settings(t, r.room, alice, store.DaemonSettings{ApproveAll: true})
	sec := e.addPersona(t, r, bob.Token, e.templates(t, r, bob.Token)[1].ID)
	noticesAre(t, e.say(t, r, tok, "@"+sec.Name+" please"), mentionNotice{sec.Name, whyPending})

	alice.drop()
	e.waitAgent(t, r.room, tok, hostIs(alice.p.Participant.ID, false))
	noticesAre(t, e.say(t, r, tok, "@grill are you there?"), mentionNotice{"Grill", whyOffline})
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 || len(st.Mentions) != 1 {
		t.Fatalf("offline mention queued: %+v", st)
	}
}

// 005-AC4: a line's chip comes with it in the stream's replay, however many mentions came after:
// the agent state carries only the latest ones.
func TestMentionChipsReplay(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	e.say(t, r, tok, "@Grill have a look")
	first := e.lastLine(t, r.room)
	tn := e.nextTurn(t, r, r.alice)
	e.finish(t, r, r.alice, tn, "looked")
	synthesis := e.lastLine(t, r.room)
	// More mentions than the agent state carries, all waiting on one queued turn.
	for i := range mentionStates + 1 {
		m := &store.Message{RoomID: r.room, Kind: store.KindText, ParticipantID: r.alice.p.Participant.ID, Sender: "Alice", Body: fmt.Sprintf("@Grill %d", i)}
		if err := e.bus.Post(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if err := e.st.EnqueueMention(t.Context(), m.ID, &store.QueuedTurn{RoomID: r.room, PersonaID: r.grill.ID, ParticipantID: r.alice.p.Participant.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.agent(t, r.room, tok); slices.ContainsFunc(st.Mentions, func(m store.MentionState) bool { return m.MessageID == first.ID }) {
		t.Fatal("the agent state still carries the first mention: the test proves nothing")
	}
	ch := e.chatEvents(t, r.room, tok, "")
	for {
		m := decodeJSON[store.Message](t, []byte(next(t, ch).data))
		if m.ID != first.ID {
			continue
		}
		if len(m.Mentions) != 1 || m.Mentions[0].Status != "done" || m.Mentions[0].LineID != synthesis.ID {
			t.Fatalf("replayed chip %+v, want done linking %d", m.Mentions, synthesis.ID)
		}
		return
	}
}

// edit rewrites line id as tok and returns the notices its author alone gets.
func (e *env) edit(t *testing.T, r *turnRoom, tok string, id int64, body string) []mentionNotice {
	t.Helper()
	code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/messages/%d", id)), tok, map[string]string{"body": body})
	if code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, b)
	}
	return decodeJSON[struct {
		Notices []mentionNotice `json:"mention_notices"`
	}](t, b).Notices
}

// linkedTo returns the personas line id queued, by id.
func (e *env) linkedTo(t *testing.T, room string, id int64) []int64 {
	t.Helper()
	states, err := e.st.MessageMentions(t.Context(), room, id-1, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, s := range states[id] {
		out = append(out, s.PersonaID)
	}
	slices.Sort(out)
	return out
}

// 016-AC19 016-AC20: an edit that names a new persona queues it as a send would, with the send's
// notices; a persona the previous body named, or the line queued before, never runs again, and
// unknown-name notices cover the new names only. Q3: a voice line's edit runs too.
func TestEditMentions(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	idle := func(step string) {
		t.Helper()
		if st := e.agent(t, r.room, tok); len(st.Queue) != 0 || len(st.Running) != 0 {
			t.Fatalf("%s queued: %+v %+v", step, st.Queue, st.Running)
		}
	}

	e.postBody(t, r, tok, "hello @nobody")
	line := e.lastLine(t, r.room).ID
	noticesAre(t, e.edit(t, r, tok, line, "hello again"))
	idle("no persona")

	// Add: queued, linked to the line, the chain reset, and @nobody, held before, says nothing.
	if code, c := e.chain(t, r, tok, map[string]int{"budget": 1}); code != http.StatusOK || c.Budget != 1 {
		t.Fatalf("budget: %+v", c)
	}
	e.finish(t, r, alice, e.press(t, r, alice, tok, false), "Ask @secretary.")
	e.finish(t, r, alice, e.nextTurn(t, r, alice), "Done.")
	if c := e.agent(t, r.room, tok).Chain; c.Count != 1 {
		t.Fatalf("chain before the edit: %+v", c)
	}
	noticesAre(t, e.edit(t, r, tok, line, "hello again @Grill"))
	tn := e.nextTurn(t, r, alice)
	if st := e.agent(t, r.room, tok); tn.PersonaID != r.grill.ID || tn.ParticipantID != alice.p.Participant.ID || st.Chain.Count != 0 {
		t.Fatalf("edit mention ran %+v, chain %+v", tn, st.Chain)
	}
	e.finish(t, r, alice, tn, "Here.")
	if got := e.linkedTo(t, r.room, line); fmt.Sprint(got) != fmt.Sprint([]int64{r.grill.ID}) {
		t.Fatalf("links %v", got)
	}

	// Keep, remove, re-add: nothing runs again.
	for _, body := range []string{"hello again @Grill, kept", "hello again", "hello again @GRILL, back"} {
		noticesAre(t, e.edit(t, r, tok, line, body))
		idle(body)
	}

	// Two new personas at once, one new unknown name: both run, only the new name is told.
	e.postBody(t, r, tok, "plain")
	two := e.lastLine(t, r.room).ID
	noticesAre(t, e.edit(t, r, tok, two, "@grill @secretary @ghost"), mentionNotice{"ghost", whyUnknown})
	e.finish(t, r, alice, e.nextTurn(t, r, alice), "One.")
	e.finish(t, r, alice, e.nextTurn(t, r, alice), "Two.")
	if got := e.linkedTo(t, r.room, two); fmt.Sprint(got) != fmt.Sprint([]int64{r.grill.ID, sec.ID}) {
		t.Fatalf("links %v", got)
	}
	noticesAre(t, e.edit(t, r, tok, two, "@grill @secretary @ghost @phantom"), mentionNotice{"phantom", whyUnknown})
	idle("unknown name added")

	// No right: told why, nothing runs.
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	e.postBody(t, r, bob.Token, "hi")
	hi := e.lastLine(t, r.room).ID
	noticesAre(t, e.edit(t, r, bob.Token, hi, "hi @grill"), mentionNotice{"Grill", whyNoRight})
	idle("no right")
	// Granted the right since, Bob keeps the name his line held: no turn, as the line never queued it.
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/participants/%d", bob.Participant.ID)), tok, map[string]bool{"can_trigger": true}); code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", code, b)
	}
	noticesAre(t, e.edit(t, r, bob.Token, hi, "hi @grill, allowed now"))
	idle("name kept")

	// A voice line edited to name a persona runs it.
	voice := &store.Message{RoomID: r.room, Kind: store.KindVoice, Source: store.VoiceSourceServer, ParticipantID: alice.p.Participant.ID, Sender: "Alice", Body: "ask grill"}
	if err := e.bus.Post(t.Context(), voice); err != nil {
		t.Fatal(err)
	}
	noticesAre(t, e.edit(t, r, tok, voice.ID, "ask @grill"))
	if tn := e.nextTurn(t, r, alice); tn.PersonaID != r.grill.ID {
		t.Fatalf("voice edit ran %+v", tn)
	}
}
