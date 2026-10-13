package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// states renders the room's mention states oldest first as "message:persona=status[>line]".
func states(t *testing.T, s *Store, room string) string {
	t.Helper()
	ms, err := s.MentionStates(context.Background(), room, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		e := fmt.Sprintf("%d:%d=%s", m.MessageID, m.PersonaID, m.Status)
		if m.LineID != 0 {
			e += fmt.Sprintf(">%d", m.LineID)
		}
		out = append(out, e)
	}
	return strings.Join(out, " ")
}

// 005-AC4: a mention's link follows its queue entry into the turn and shows that turn's state:
// queued, running (queued again while the host's daemon holds it), done with its synthesis,
// stopped or failed with the line saying so; a dropped entry is cancelled. A second mention of a
// persona waiting already shares its turn; a conflict's re-run takes over the rejected turn's.
func TestMentionStates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	g := r.grill.ID
	post := func(body string) int64 {
		m := &Message{RoomID: "a", Kind: KindText, ParticipantID: r.p.ID, Sender: "Alice", Body: body}
		if err := s.InsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	mention := func(msg int64) *QueuedTurn {
		q := &QueuedTurn{RoomID: "a", PersonaID: g, ParticipantID: r.p.ID}
		if err := s.EnqueueMention(ctx, msg, q); err != nil {
			t.Fatal(err)
		}
		return q
	}
	start := func(q *QueuedTurn) *Turn {
		if _, err := s.PopQueued(ctx, "a", false); err != nil {
			t.Fatal(err)
		}
		tn := &Turn{RoomID: "a", ParticipantID: r.p.ID, DaemonID: r.daemon, PersonaID: g, PersonaName: "Grill", QueueID: q.ID}
		if err := s.StartTurn(ctx, tn, false); err != nil {
			t.Fatal(err)
		}
		return tn
	}
	want := func(w string) {
		t.Helper()
		if got := states(t, s, "a"); got != w {
			t.Fatalf("states\n got %s\nwant %s", got, w)
		}
	}

	m1 := post("@grill one")
	q1 := mention(m1)
	m2 := post("@grill again")
	if q2 := mention(m2); q2.ID != q1.ID {
		t.Fatalf("second mention queued %d, want the waiting %d", q2.ID, q1.ID)
	}
	want(fmt.Sprintf("%d:%d=queued %d:%d=queued", m1, g, m2, g))
	t1 := start(q1)
	want(fmt.Sprintf("%d:%d=running %d:%d=running", m1, g, m2, g))
	if _, err := s.SetHostQueued(ctx, "a", t1.ID, r.daemon, true); err != nil {
		t.Fatal(err)
	}
	want(fmt.Sprintf("%d:%d=queued %d:%d=queued", m1, g, m2, g))
	if _, err := s.CompleteTurn(ctx, "a", t1.ID, r.daemon, &TurnOutcome{Synthesis: "Answer.", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	last, err := s.Messages(ctx, "a", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	synth := last[len(last)-1]
	if synth.Kind != KindAgent {
		t.Fatalf("last %+v", synth)
	}
	want(fmt.Sprintf("%d:%d=done>%d %d:%d=done>%d", m1, g, synth.ID, m2, g, synth.ID))

	m3 := post("@grill stop this")
	t3 := start(mention(m3))
	line, err := s.StopTurn(ctx, "a", t3.ID, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	m4 := post("@grill fail this")
	t4 := start(mention(m4))
	fail, err := s.FailTurn(ctx, "a", t4.ID, "boom")
	if err != nil {
		t.Fatal(err)
	}
	m5 := post("@grill never mind")
	q5 := mention(m5)
	if err := s.CancelQueued(ctx, "a", q5.ID, r.p.ID, true); err != nil {
		t.Fatal(err)
	}
	head := fmt.Sprintf("%d:%d=done>%d %d:%d=done>%d ", m1, g, synth.ID, m2, g, synth.ID)
	want(head + fmt.Sprintf("%d:%d=stopped>%d %d:%d=failed>%d %d:%d=cancelled", m3, g, line.ID, m4, g, fail.ID, m5, g))

	// A conflict re-runs the turn: its mention, and the one waiting behind it, follow the re-run.
	m6 := post("@grill conflict")
	t6 := start(mention(m6))
	m7 := post("@grill behind")
	mention(m7)
	if _, rerun, err := s.RejectTurn(ctx, "a", t6.ID, r.daemon, "plan.md"); err != nil || !rerun {
		t.Fatalf("reject: %v %v", rerun, err)
	}
	rerunQ, err := s.Queue(ctx, "a")
	if err != nil || len(rerunQ) != 1 {
		t.Fatalf("queue %+v %v", rerunQ, err)
	}
	head += fmt.Sprintf("%d:%d=stopped>%d %d:%d=failed>%d %d:%d=cancelled ", m3, g, line.ID, m4, g, fail.ID, m5, g)
	want(head + fmt.Sprintf("%d:%d=queued %d:%d=queued", m6, g, m7, g))
	start(&rerunQ[0])
	want(head + fmt.Sprintf("%d:%d=running %d:%d=running", m6, g, m7, g))

	// A pending persona queues nothing and links nothing.
	if err := s.EnqueueMention(ctx, post("@nobody"), &QueuedTurn{RoomID: "a", PersonaID: 999, ParticipantID: r.p.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown persona: %v", err)
	}
	want(head + fmt.Sprintf("%d:%d=running %d:%d=running", m6, g, m7, g))
}
