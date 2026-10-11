package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/turn"
)

// approvingRoom is personaRoom with Alice hosting on a daemon that approves everything.
func approvingRoom(t *testing.T, s *Store, room string) (alice, bob *Participant, grill RoomPersona, d *Daemon) {
	t.Helper()
	ctx := context.Background()
	alice, bob, grill = personaRoom(t, s, room)
	d, err := s.PairDaemon(ctx, room, alice.ID, "laptop", "dmn-"+room)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, room, alice.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetDaemonSettings(ctx, d.ID, DaemonSettings{MaxModel: "sonnet", ApproveAll: true}); err != nil || !changed {
		t.Fatalf("settings: %v %v", changed, err)
	}
	if changed, err := s.SetDaemonSettings(ctx, d.ID, DaemonSettings{MaxModel: "sonnet", ApproveAll: true}); err != nil || changed {
		t.Fatalf("same settings again: %v %v", changed, err)
	}
	if h, err := s.HostDaemon(ctx, room); err != nil || h.ID != d.ID || !h.Settings.ApproveAll || h.Settings.MaxModel != "sonnet" {
		t.Fatalf("host daemon: %+v %v", h, err)
	}
	return alice, bob, grill, d
}

func str(s string) *string { return &s }

func (s *Store) onlyChange(t *testing.T, room string) PersonaChange {
	t.Helper()
	cs, err := s.PersonaChanges(context.Background(), room)
	if err != nil || len(cs) != 1 {
		t.Fatalf("changes: %+v %v", cs, err)
	}
	return cs[0]
}

// 002-AC21: with the host approving everything, a member's new persona waits, unable to run, and
// each of their changes waits too; later changes add to the one request; the host's own changes
// apply at once; approving applies, refusing drops, and each outcome is in the chat.
func TestHeldChanges(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, grill, _ := approvingRoom(t, s, "a")

	sec, m, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, bob)
	if err != nil || !sec.Pending || m.Body != `Bob added the persona 📝 Secretary, from the template "Secretary" v1. It waits for the host's approval.` {
		t.Fatalf("held add: %+v %+v %v", sec, m, err)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "a", PersonaID: sec.ID, ParticipantID: bob.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("queued a pending persona: %v", err)
	}
	if err := s.StartTurn(ctx, &Turn{RoomID: "a", PersonaID: sec.ID, Prompt: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("started a pending persona: %v", err)
	}
	if _, m, held, err := s.EditPersona(ctx, "a", sec.ID, PersonaPatch{Name: str("Scribe")}, bob); err != nil || !held ||
		m.Body != "Bob asked to change 📝 Secretary: name Secretary → Scribe. It waits for the host's approval." {
		t.Fatalf("edit of a pending persona: %+v %v %v", m, held, err)
	}
	if c := s.onlyChange(t, "a"); c.Kind != ChangeAdd || c.Summary != "new persona, name Secretary → Scribe" || c.RequestedBy != "Bob" {
		t.Fatalf("held add with its edit: %+v", c)
	}
	m, err = s.DecideChange(ctx, "a", s.onlyChange(t, "a").ID, true, alice)
	if err != nil || m.Body != "Alice approved the persona 📝 Scribe, added by Bob." {
		t.Fatalf("approve add: %+v %v", m, err)
	}
	if p, _ := s.RoomPersona(ctx, "a", sec.ID); p.Pending || p.Name != "Scribe" {
		t.Fatalf("approved persona: %+v", p)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "a", PersonaID: sec.ID, ParticipantID: bob.ID}); err != nil {
		t.Fatalf("approved persona cannot queue: %v", err)
	}

	// The host's own change applies at once.
	if p, _, held, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Badge: str("🌶")}, alice); err != nil || held || p.Badge != "🌶" {
		t.Fatalf("host edit: %+v %v %v", p, held, err)
	}
	p, m, held, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Model: str("opus")}, bob)
	if err != nil || !held || p.Model != "" || m.Body != "Bob asked to change 🌶 Grill: model default → opus. It waits for the host's approval." {
		t.Fatalf("held edit: %+v %+v %v %v", p, m, held, err)
	}
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Effort: str("max")}, bob); err != nil {
		t.Fatal(err)
	}
	c := s.onlyChange(t, "a")
	if c.Kind != ChangeEdit || c.Summary != "model default → opus, effort default → max" {
		t.Fatalf("merged change: %+v", c)
	}
	if got, _ := s.RoomPersona(ctx, "a", grill.ID); got.Model != "" || got.Effort != "" {
		t.Fatalf("a held change applied: %+v", got)
	}
	m, err = s.DecideChange(ctx, "a", c.ID, true, alice)
	if err != nil || m.Body != "Alice approved Bob's change to 🌶 Grill: model default → opus, effort default → max." {
		t.Fatalf("approve: %+v %v", m, err)
	}
	if got, _ := s.RoomPersona(ctx, "a", grill.ID); got.Model != "opus" || got.Effort != "max" {
		t.Fatalf("approved change: %+v", got)
	}

	// Refused: nothing changes. A change bringing a name taken since stays held.
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Text: str("Be soft.")}, bob); err != nil {
		t.Fatal(err)
	}
	m, err = s.DecideChange(ctx, "a", s.onlyChange(t, "a").ID, false, alice)
	if err != nil || m.Body != "Alice refused Bob's change to 🌶 Grill: persona text." {
		t.Fatalf("refuse: %+v %v", m, err)
	}
	if got, _ := s.RoomPersona(ctx, "a", grill.ID); strings.HasPrefix(got.Text, "Be soft") {
		t.Fatalf("refused change applied: %+v", got)
	}
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Name: str("Judge")}, bob); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.EditPersona(ctx, "a", sec.ID, PersonaPatch{Name: str("judge")}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideChange(ctx, "a", s.onlyChange(t, "a").ID, true, alice); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("approve a taken name: %v", err)
	}
	s.onlyChange(t, "a")

	// A refused new persona is removed; its request goes with it.
	extra, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, PresetDefault).ID, bob)
	if err != nil || !extra.Pending {
		t.Fatalf("second held add: %+v %v", extra, err)
	}
	cs, _ := s.PersonaChanges(ctx, "a")
	m, err = s.DecideChange(ctx, "a", cs[1].ID, false, alice)
	if err != nil || m.Body != "Alice refused the persona 🔥 Grill-2, added by Bob: it is removed." {
		t.Fatalf("refuse add: %+v %v", m, err)
	}
	if _, err := s.RoomPersona(ctx, "a", extra.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused persona kept: %v", err)
	}
	if _, err := s.DecideChange(ctx, "a", cs[1].ID, true, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("decide twice: %v", err)
	}
	if _, err := s.DecideChange(ctx, "b", cs[0].ID, true, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("decide through another room: %v", err)
	}

	// A template update is a change too.
	tpl, err := s.PublishTemplate(ctx, bob.ProfileID, "Bob", "Critic", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideChange(ctx, "a", s.onlyChange(t, "a").ID, false, alice); err != nil {
		t.Fatal(err)
	}
	crit, _, err := s.AddPersona(ctx, "a", tpl.ID, alice)
	if err != nil || crit.Pending {
		t.Fatalf("host's add: %+v %v", crit, err)
	}
	if _, err := s.PublishVersion(ctx, bob.ProfileID, tpl.ID, "Bob", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v2", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	p, m, held, err = s.UpdatePersona(ctx, "a", crit.ID, bob)
	if err != nil || !held || p.Text != "v1" || p.UpdateVersion != 2 ||
		m.Body != "Bob asked to change 🧐 Critic: model default → opus, persona text, template v1 → v2. It waits for the host's approval." {
		t.Fatalf("held update: %+v %+v %v %v", p, m, held, err)
	}
	if _, err := s.DecideChange(ctx, "a", s.onlyChange(t, "a").ID, true, alice); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RoomPersona(ctx, "a", crit.ID); got.Text != "v2" || got.Model != "opus" || got.TemplateVersion != 2 || got.UpdateVersion != 0 {
		t.Fatalf("approved update: %+v", got)
	}
}

// 002-AC21: pending requests survive a host hand-over and the new host decides; a host whose daemon
// does not approve everything lets changes apply at once, and the approval mode of an offline host
// still holds.
func TestHeldChangesSurviveAHandOver(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, grill, d := approvingRoom(t, s, "a")
	if _, _, held, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Model: str("haiku")}, bob); err != nil || !held {
		t.Fatalf("held: %v %v", held, err)
	}
	carol, _, err := s.Join(ctx, "inv-a", "Carol", "c-a", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PairDaemon(ctx, "a", carol.ID, "desk", "dmn-carol"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "a", carol.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideCap(ctx, "a", grill.ID, d, turn.Level{Model: "haiku"}, turn.Level{}, true, alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the former host decided a cap: %v", err)
	}
	if _, _, held, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Effort: str("low")}, bob); err != nil || held {
		t.Fatalf("Carol approves nothing ahead, yet the change was held: %v", err)
	}
	c := s.onlyChange(t, "a")
	// The former host, past a handler's check before the hand-over, cannot decide any more.
	if _, err := s.DecideChange(ctx, "a", c.ID, true, alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the former host decided: %v", err)
	}
	if m, err := s.DecideChange(ctx, "a", c.ID, true, carol); err != nil || !strings.HasPrefix(m.Body, "Carol approved Bob's change") {
		t.Fatalf("the new host decides: %+v %v", m, err)
	}
	if got, _ := s.RoomPersona(ctx, "a", grill.ID); got.Model != "haiku" || got.Effort != "low" {
		t.Fatalf("after the decision: %+v", got)
	}
}

// 002-AC20: the host's answer on a persona above the cap is kept per persona and host daemon, with
// what was asked; it is refused when the persona asks for something else by now.
func TestDecideCap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, grill, d := approvingRoom(t, s, "a")
	opusMax := turn.Level{Model: "opus", Effort: "max"}
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Model: str("opus"), Effort: str("max")}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideCap(ctx, "a", grill.ID, d, turn.Level{Model: "opus"}, turn.Level{}, true, alice); !errors.Is(err, ErrStale) {
		t.Fatalf("decide on a stale request: %v", err)
	}
	m, err := s.DecideCap(ctx, "a", grill.ID, d, opusMax, turn.Level{Model: "sonnet", Effort: "max"}, false, alice)
	if err != nil || m.Body != "Alice refused opus · effort max for 🔥 Grill: it keeps running at the host cap, sonnet · effort max." {
		t.Fatalf("refuse: %+v %v", m, err)
	}
	p, _ := s.RoomPersona(ctx, "a", grill.ID)
	if !p.Cap.Settles(d.ID, opusMax) || p.Cap.Settles(d.ID+1, opusMax) || p.Cap.Settles(d.ID, turn.Level{Model: "fable"}) || p.Cap.Approved(d.ID) != (turn.Level{}) {
		t.Fatalf("refusal: %+v", p.Cap)
	}
	m, err = s.DecideCap(ctx, "a", grill.ID, d, opusMax, turn.Level{}, true, alice)
	if err != nil || m.Body != "Alice approved opus · effort max for 🔥 Grill on laptop: it runs as asked from its next turn." {
		t.Fatalf("approve: %+v %v", m, err)
	}
	p, _ = s.RoomPersona(ctx, "a", grill.ID)
	if p.Cap.Approved(d.ID) != opusMax || p.Cap.Approved(d.ID+1) != (turn.Level{}) {
		t.Fatalf("approval: %+v", p.Cap)
	}
	if _, err := s.DecideCap(ctx, "a", grill.ID, d, opusMax, turn.Level{}, true, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member decided: %v", err)
	}
	other := *d
	other.ParticipantID = bob.ID
	if _, err := s.DecideCap(ctx, "a", grill.ID, &other, opusMax, turn.Level{}, true, alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("decided on a daemon other than the host's: %v", err)
	}
	if _, err := s.DecideCap(ctx, "a", 999, d, opusMax, turn.Level{}, true, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown persona: %v", err)
	}
}
