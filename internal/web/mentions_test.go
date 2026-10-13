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
