package store

import (
	"context"
	"errors"
	"testing"
)

// 002-AC25 002-AC26 002-AC27: the chain counter as a state machine. Each link source counts and is
// dropped at the budget; each reset source zeroes the count; suspending lets links run past the
// budget and still counts them; restoring the limit pauses a chain already past it.
func TestChainStateMachine(t *testing.T) {
	links := []ChainEvent{LinkEveryN, LinkCall}
	resets := []ChainEvent{ResetButton, ResetRearm, ResetMention}
	for _, link := range links {
		c := Chain{Budget: 2}
		for i := 1; i <= 2; i++ {
			var ok bool
			if c, ok = c.On(link); !ok || c.Count != i {
				t.Fatalf("%s link %d: %+v %v", link, i, c, ok)
			}
		}
		if !c.Paused() {
			t.Fatalf("%s: not paused at the budget: %+v", link, c)
		}
		for _, other := range links {
			if next, ok := c.On(other); ok || next != c {
				t.Fatalf("%s past the budget: %+v %v, want dropped and unchanged", other, next, ok)
			}
		}
		for _, reset := range resets {
			next, ok := c.On(reset)
			if !ok || next.Count != 0 || next.Paused() || next.Budget != 2 {
				t.Fatalf("%s after %s: %+v %v", reset, link, next, ok)
			}
			if again, ok := next.On(link); !ok || again.Count != 1 {
				t.Fatalf("%s after %s reset: %+v %v", link, reset, again, ok)
			}
		}
	}
	c, _ := Chain{Budget: 1, Count: 1}.On(Suspend)
	for i := 2; i <= 4; i++ {
		var ok bool
		if c, ok = c.On(LinkCall); !ok || c.Count != i || c.Paused() {
			t.Fatalf("suspended link %d: %+v %v", i, c, ok)
		}
	}
	if c, _ = c.On(Unsuspend); !c.Paused() || c.Count != 4 {
		t.Fatalf("restored limit past the budget: %+v", c)
	}
	if c, _ = c.On(ResetRearm); c.Paused() || c.Suspended {
		t.Fatalf("re-armed: %+v", c)
	}
}

// 002-AC25 002-AC26 002-AC27: a room starts with the default budget; links queue and count until
// the budget, where they are dropped with nothing queued or counted; a persona already queued is
// not counted twice; a reset and the member controls change the stored chain, the controls with a
// line in the chat. A turn an agent queued has no participant: any member allowed to press may
// cancel it.
func TestChainStore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, grill := personaRoom(t, s, "a")
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	chain := func() Chain {
		t.Helper()
		r, err := s.Room(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		return r.Chain
	}
	if c := chain(); c != (Chain{Budget: DefaultChainBudget}) {
		t.Fatalf("new room: %+v", c)
	}
	budget := 2
	if _, m, err := s.ControlChain(ctx, "a", ChainControl{Budget: &budget}, alice); err != nil || m.Body != "Alice set the chain budget to 2." {
		t.Fatalf("budget: %+v %v", m, err)
	}
	link := &QueuedTurn{RoomID: "a", PersonaID: sec.ID}
	if c, err := s.EnqueueLink(ctx, link, LinkCall); err != nil || c.Count != 1 || link.Source != "call" {
		t.Fatalf("first link: %+v %v", c, err)
	}
	if _, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: "a", PersonaID: sec.ID}, LinkEveryN); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("queued twice: %v", err)
	}
	if c := chain(); c.Count != 1 {
		t.Fatalf("a link already queued counted: %+v", c)
	}
	if c, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: "a", PersonaID: grill.ID}, LinkEveryN); err != nil || c.Count != 2 || !c.Paused() {
		t.Fatalf("second link: %+v %v", c, err)
	}
	if err := s.CancelQueued(ctx, "a", link.ID, bob.ID, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cancel an agent's turn without the button: %v", err)
	}
	if err := s.CancelQueued(ctx, "a", link.ID, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: "a", PersonaID: sec.ID}, LinkCall); !errors.Is(err, ErrChainPaused) {
		t.Fatalf("link at the budget: %v", err)
	}
	if q, _ := s.Queue(ctx, "a"); len(q) != 1 || q[0].PersonaID != grill.ID || q[0].ParticipantID != 0 || q[0].Source != "every_n" {
		t.Fatalf("queue after a dropped link: %+v", q)
	}
	if c := chain(); c.Count != 2 {
		t.Fatalf("a dropped link counted: %+v", c)
	}
	if err := s.ResetChain(ctx, "a", ResetMention); err != nil || chain().Count != 0 {
		t.Fatalf("reset: %+v %v", chain(), err)
	}
	if _, m, err := s.ControlChain(ctx, "a", ChainControl{Rearm: true}, alice); err != nil || m != nil {
		t.Fatalf("re-arm at 0 changes nothing: %+v %v", m, err)
	}
	if _, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: "a", PersonaID: sec.ID}, LinkCall); err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	c, m, err := s.ControlChain(ctx, "a", ChainControl{Rearm: true, Suspended: &on}, bob)
	if err != nil || c != (Chain{Budget: 2, Suspended: true}) || m.Body != "Bob re-armed the agent chain, suspended the chain limit: agents may call each other without end." {
		t.Fatalf("re-arm and suspend: %+v %+v %v", c, m, err)
	}
	if c, m, err = s.ControlChain(ctx, "a", ChainControl{Suspended: &off}, bob); err != nil || c.Suspended || m.Body != "Bob restored the chain limit of 2." {
		t.Fatalf("restore: %+v %+v %v", c, m, err)
	}
	if _, _, err := s.ControlChain(ctx, "nope", ChainControl{Rearm: true}, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
	if _, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: "nope", PersonaID: sec.ID}, LinkCall); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link in an unknown room: %v", err)
	}
}

// 002-AC22: a persona is due once N turns of any persona completed since its own last turn,
// whatever that turn's outcome; never while it waits for the host's approval.
func TestDuePersonas(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, r.p)
	if err != nil {
		t.Fatal(err)
	}
	two := 2
	if _, _, _, err := s.EditPersona(ctx, "a", sec.ID, PersonaPatch{EveryN: &two}, r.p); err != nil {
		t.Fatal(err)
	}
	due := func() []int64 {
		t.Helper()
		ps, err := s.DuePersonas(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		return ids
	}
	run := func(persona RoomPersona, fail bool) {
		t.Helper()
		tn := &Turn{RoomID: "a", DaemonID: r.daemon, Prompt: "go", PersonaID: persona.ID, PersonaName: persona.Name}
		if err := s.StartTurn(ctx, tn, false); err != nil {
			t.Fatal(err)
		}
		if fail {
			if _, err := s.FailTurn(ctx, "a", tn.ID, "boom"); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{Synthesis: persona.Name + " spoke\nsecond line", SessionID: "s-" + persona.Name}); err != nil {
			t.Fatal(err)
		}
	}
	run(r.grill, false)
	if got := due(); len(got) != 0 {
		t.Fatalf("after one round: %v", got)
	}
	run(r.grill, true)
	if got := due(); len(got) != 0 {
		t.Fatalf("a failed turn is no round: %v", got)
	}
	run(r.grill, false)
	if got := due(); len(got) != 1 || got[0] != sec.ID {
		t.Fatalf("after two rounds: %v", got)
	}
	run(*sec, true)
	if got := due(); len(got) != 0 {
		t.Fatalf("its own failed turn counts as its last: %v", got)
	}
	run(r.grill, false)
	run(r.grill, false)
	if got := due(); len(got) != 1 {
		t.Fatalf("two rounds after its failure: %v", got)
	}
	run(*sec, false)
	stats, err := s.PersonaStats(ctx, "a")
	if err != nil || stats[r.grill.ID] != (PersonaStat{Turns: 4, Last: "Grill spoke\nsecond line"}) || stats[sec.ID].Turns != 1 {
		t.Fatalf("stats: %+v %v", stats, err)
	}
}
