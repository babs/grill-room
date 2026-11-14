package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 023-AC16 023-AC18 023-AC19 023-AC20: a turn's proposal becomes a card; creating it seats the
// members through the creator's shareable invite and links only for an admin of the first room;
// a proposal is acted on once; limits leave it open; "proposed in" outlives the first room.
func TestRoomProposals(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	join := func(name string) *Participant {
		p, _, err := s.Join(ctx, "inv-a", name, "tok-"+name, JoinProfile{RecoveryHash: "rec-" + name, SessionHash: "sess-" + name})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ana, bob := join("Ana"), join("Bob")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES('a', ?, ?)`, ana.ProfileID, now()); err != nil {
		t.Fatal(err)
	}
	n := 0
	propose := func(name string) (int64, int64) {
		tn := r.start(t, s, "a")
		n++
		if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{Synthesis: "See the card.", SessionID: fmt.Sprintf("s%d", n),
			Proposal: &NewProposal{Name: name, Brief: "# " + name + "\n", HTML: "<h1>" + name + "</h1>"}}); err != nil {
			t.Fatal(err)
		}
		msgs, err := s.Messages(ctx, "a", 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		m := msgs[len(msgs)-1]
		if m.Proposal == nil || m.Body != "Grill proposes a room: "+name || m.Proposal.Trigger != r.p.ID || !strings.Contains(m.HTML, "<h1>"+name+"</h1>") {
			t.Fatalf("card %+v", m)
		}
		return m.Proposal.ID, m.ID
	}
	in := func(id, name string, by *Participant, members ...int64) ProposedRoom {
		return ProposedRoom{ID: id, Name: name, By: by, InviteHash: "i-" + id, TokenHash: "t-" + id, Link: "l-" + id, LinkHash: "lh-" + id,
			Members: members, Reads: true, ReadBy: true, SeatHash: func() string { n++; return fmt.Sprintf("seat-%d", n) }}
	}

	// A real seat of another room: never seated through this room's proposal (023-AC18).
	if _, err := s.CreateRoom(ctx, "x", "X"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "x", "inv-x", 0); err != nil {
		t.Fatal(err)
	}
	carl, _, err := s.Join(ctx, "inv-x", "Carl", "tok-carl", JoinProfile{RecoveryHash: "rec-carl", SessionHash: "sess-carl"})
	if err != nil {
		t.Fatal(err)
	}
	p1, card := propose("Legal")
	audited := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorProfile + "ana", Action: "room.create", TargetKind: TargetRoom, TargetID: "legal"}, nil)
	room, seats, links, err := s.CreateProposedRoom(audited, "a", p1, in("legal", "Legal", ana, bob.ID, ana.ID, carl.ID, bob.ID))
	if err != nil || len(seats) != 1 || seats[0].From != bob.ID || len(links) != 2 || room.ProposedIn == nil || room.ProposedIn.Name != "R" {
		t.Fatalf("create: %+v %+v %+v %v", room, seats, links, err)
	}
	var via string
	if err := s.db.QueryRowContext(ctx, `SELECT i.link FROM participants p JOIN invites i ON i.id = p.invite_id WHERE p.id = (SELECT id FROM participants WHERE room_id = 'legal' AND name = 'Bob')`).Scan(&via); err != nil || via != "l-legal" {
		t.Fatalf("Bob's seat came through %q: %v", via, err)
	}
	if n := count(t, s, `SELECT count(*) FROM participants WHERE room_id = 'legal' AND name = 'Carl'`); n != 0 {
		t.Fatalf("a seat of another room got %d seats", n)
	}
	// 023-AC17: a closed card drops its brief.
	if m, _ := s.Message(ctx, "a", card); m.Body != "Created by Ana: Legal" || m.Proposal.Status != ProposalCreated || m.Proposal.Room != "legal" || m.HTML != "" {
		t.Fatalf("card %+v", m)
	}
	// One entry for the room, one per link, each under its own action.
	es, err := s.AuditLog(ctx, AuditQuery{Limit: 10})
	if err != nil || len(es) != 3 || es[0].Action != ActionRoomLinkCreate || es[1].Action != ActionRoomLinkCreate || es[2].Action != "room.create" || es[0].TargetID != "legal" {
		t.Fatalf("audit %+v %v", es, err)
	}
	if _, _, _, err := s.CreateProposedRoom(ctx, "a", p1, in("again", "Again", ana)); !errors.Is(err, ErrHandled) {
		t.Fatalf("second create: %v", err)
	}
	if err := s.DismissProposal(ctx, "a", p1, ana, true); !errors.Is(err, ErrHandled) {
		t.Fatalf("dismiss after create: %v", err)
	}
	// A proposal is its room's only: acted on through another room, it is unknown.
	p0, _ := propose("Other")
	if err := s.DismissProposal(ctx, "x", p0, ana, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("through another room: %v", err)
	}
	if _, _, _, err := s.CreateProposedRoom(ctx, "x", p0, in("x2", "X2", ana)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create through another room: %v", err)
	}
	if err := s.DismissProposal(ctx, "a", 9999, ana, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	// Limits: Ana has created one room, her limit; nothing is created and the card stays open.
	p2, card2 := propose("Tax")
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET max_rooms = 1 WHERE id = ?`, ana.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CreateProposedRoom(ctx, "a", p2, in("tax", "Tax", ana, bob.ID)); !errors.Is(err, ErrCreationRefused) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := s.Room(ctx, "tax"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused room exists: %v", err)
	}
	// Dismiss: Bob neither pressed nor administers; Alice pressed.
	if err := s.DismissProposal(ctx, "a", p2, bob, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Bob dismisses: %v", err)
	}
	// Alice holds no profile: her seat is the audit actor.
	if err := s.DismissProposal(WithAudit(ctx, AuditEntry{At: time.Now(), Action: "room_proposal.dismiss", TargetKind: TargetRoom, TargetID: "a"}, nil), "a", p2, r.p, false); err != nil {
		t.Fatal(err)
	}
	if es, err := s.AuditLog(ctx, AuditQuery{Action: "room_proposal.dismiss", Limit: 1}); err != nil || len(es) != 1 || es[0].Actor != fmt.Sprintf("%s%d", ActorParticipant, r.p.ID) {
		t.Fatalf("dismiss audit %+v %v", es, err)
	}
	if m, _ := s.Message(ctx, "a", card2); m.Body != "Dismissed by Alice: Tax" || m.Proposal.Status != ProposalDismissed || m.HTML != "" {
		t.Fatalf("dismissed card %+v", m)
	}

	// Bob administers no room here: his room gets no link whatever he asks.
	p3, _ := propose("Ops")
	if _, _, links, err := s.CreateProposedRoom(ctx, "a", p3, in("ops", "Ops", bob)); err != nil || len(links) != 0 {
		t.Fatalf("Bob creates: %+v %v", links, err)
	}

	// 023-AC20: the first room's current name, then the one it had once gone.
	if _, err := s.RenameRoom(ctx, "a", "Renamed", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Room(ctx, "legal"); err != nil || got.ProposedIn.Name != "Renamed" {
		t.Fatalf("after a rename: %+v %v", got.ProposedIn, err)
	}
	if o, err := s.RoomOverview(ctx, "legal", 0); err != nil || o.ProposedIn.Name != "Renamed" {
		t.Fatalf("overview after a rename: %+v %v", o.ProposedIn, err)
	}
	if _, err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Room(ctx, "legal"); err != nil || got.ProposedIn == nil || got.ProposedIn.Room != "" || got.ProposedIn.Name != "R" {
		t.Fatalf("after the first room went: %+v %v", got, err)
	}
	if o, err := s.RoomOverview(ctx, "legal", 0); err != nil || o.ProposedIn == nil || o.ProposedIn.Name != "R" {
		t.Fatalf("overview: %+v %v", o, err)
	}
}

// 023-AC16 023-AC18 023-AC19: a store failure at any step of a proposal leaves nothing behind and
// the proposal open.
func TestRoomProposalsFailWithTheStore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	ana, _, err := s.Join(ctx, "inv-a", "Ana", "tok-ana", JoinProfile{RecoveryHash: "rec-ana", SessionHash: "sess-ana"})
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{RecoveryHash: "rec-bob", SessionHash: "sess-bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES('a', ?, ?)`, ana.ProfileID, now()); err != nil {
		t.Fatal(err)
	}
	turnWith := func(name string) error {
		tn := r.start(t, s, "a")
		_, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: name, Proposal: &NewProposal{Name: name, Brief: "# " + name, HTML: "<h1>" + name + "</h1>"}})
		if err != nil {
			if _, ferr := s.db.ExecContext(ctx, `UPDATE turns SET status = 'failed' WHERE id = ?`, tn.ID); ferr != nil {
				t.Fatal(ferr)
			}
		}
		return err
	}
	for _, f := range [][2]string{{"messages", "NEW.body LIKE '%proposes a room%'"}, {"room_proposals", ""}} {
		undo := failOn(t, s, "INSERT", f[0], f[1], "boom")
		if err := turnWith("x"); err == nil {
			t.Errorf("%v: the turn completed", f)
		}
		undo()
	}
	if err := turnWith("Legal"); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM room_proposals`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	in := ProposedRoom{ID: "n", Name: "N", By: ana, InviteHash: "i", TokenHash: "t", Link: "l", LinkHash: "lh", Members: []int64{bob.ID},
		Reads: true, SeatHash: func() string { return "seat" }}
	for _, c := range []struct {
		event, table, when string
		dismiss            bool
	}{
		{"UPDATE", "room_proposals", "", true},
		{"UPDATE", "room_proposals", "", false},
		{"UPDATE", "messages", "NEW.body IS DISTINCT FROM OLD.body", true},
		{"UPDATE", "messages", "NEW.html IS NULL AND OLD.html IS NOT NULL", false},
		{"INSERT", "documents", "", false},
		{"UPDATE", "rooms", "NEW.proposed_in_name IS NOT NULL", false},
		{"INSERT", "participants", "NEW.name = 'Bob'", false},
		{"INSERT", "room_links", "", false},
	} {
		undo := failOn(t, s, c.event, c.table, c.when, "boom")
		if c.dismiss {
			err = s.DismissProposal(ctx, "a", id, ana, true)
		} else {
			_, _, _, err = s.CreateProposedRoom(ctx, "a", id, in)
		}
		undo()
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if m := count(t, s, `SELECT count(*) FROM room_proposals WHERE status = 'open'`); m != 1 {
		t.Fatalf("open proposals after the failures: %d", m)
	}
	if _, err := s.Room(ctx, "n"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed creation left its room: %v", err)
	}
}
