package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// lastLine returns room's newest message.
func lastLine(t *testing.T, s *Store, room string) Message {
	t.Helper()
	msgs, err := s.Messages(t.Context(), room, 0, 10000)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v", err)
	}
	return msgs[len(msgs)-1]
}

// auditDetails returns the details of the newest audit entry of action, "" for none.
func auditDetails(t *testing.T, s *Store, action string) string {
	t.Helper()
	log, err := s.AuditLog(t.Context(), AuditQuery{Action: action, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) == 0 {
		return ""
	}
	return string(log[0].Details)
}

// 016-AC7 016-AC10 016-AC16 016-AC8 016-AC9: removal ends every valid seat of the person's profile
// in the room, posts and audits it in one commit, revokes the invites they made while their joiners
// stay, drops the turns they queued, and leaves the room hostless when one of the seats hosted. A
// room admin cannot be removed; an unknown or ended seat is not found.
func TestEndSeatsRemove(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := seed(t, s, "a") // the AI host, with a queued turn
	// Alice's second device, and Bob, who joined through Alice's shareable link.
	second, _, err := s.Join(ctx, "inv-a", "Alice", "tok-a2", JoinProfile{ID: alice.ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	link, _, _, err := s.ShareInvite(ctx, "a", alice.ID, "lnk", "lnk-h")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Join(ctx, "lnk-h", "Bob", "tok-b", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if q, err := s.Queue(ctx, "a"); err != nil || len(q) == 0 {
		t.Fatalf("queue before: %+v %v", q, err)
	}
	carol, _, err := s.Join(ctx, "inv-a", "Carol", "tok-c", JoinProfile{RecoveryHash: "rec-c", SessionHash: "sess-c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoomAdmin(ctx, "a", carol.ID, true, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := s.EndSeats(ctx, "a", carol.ID, Ending{By: alice}); !errors.Is(err, ErrIsRoomAdmin) {
		t.Fatalf("remove a room admin: %v", err)
	}
	if _, err := s.EndSeats(ctx, "a", 9999, Ending{By: carol}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown seat: %v", err)
	}
	audited := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: "profile:x", Action: "participant.remove", TargetKind: TargetRoom, TargetID: "a"}, nil)
	ended, err := s.EndSeats(audited, "a", second.ID, Ending{By: carol, Block: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ended.Seats, []int64{alice.ID, second.ID}) || !ended.Host || !ended.Blocked || ended.Name != "Alice" {
		t.Fatalf("ended %+v", ended)
	}
	for _, tok := range []string{"tok-a", "tok-a2"} {
		if _, err := s.ParticipantByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s after removal: %v", tok, err)
		}
	}
	if _, err := s.ParticipantByToken(ctx, "tok-b"); err != nil {
		t.Errorf("bob after alice's removal: %v", err)
	}
	if inv, err := s.Invite(ctx, "a", link.ID); err != nil || inv.RevokedAt == nil {
		t.Errorf("alice's link: %+v %v", inv, err)
	}
	if q, err := s.Queue(ctx, "a"); err != nil || len(q) != 0 {
		t.Errorf("queue after: %+v %v", q, err)
	}
	if r, err := s.Room(ctx, "a"); err != nil || r.HostParticipantID != 0 {
		t.Errorf("host after: %+v %v", r, err)
	}
	if m := lastLine(t, s, "a"); m.Body != "Carol removed Alice from the room and blocked them from rejoining" || m.ParticipantID != carol.ID {
		t.Errorf("line %+v", m)
	}
	if d := auditDetails(t, s, "participant.remove"); !strings.Contains(d, `"blocked":true`) || !strings.Contains(d, `"name":"Alice"`) ||
		!strings.Contains(d, `"seats":[`) {
		t.Errorf("audit %s", d)
	}
	if _, err := s.EndSeats(ctx, "a", alice.ID, Ending{By: carol}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
}

// 016-AC12: a leave ends your seats as left, posts the leaving line and no audit entry; a room
// admin must step down first. 016-AC11: a departed person's seats read "left" or "removed" until
// they hold a seat again.
func TestEndSeatsLeave(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-b", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	if err != nil {
		t.Fatal(err)
	}
	gus, _, err := s.Join(ctx, "inv-a", "Gus", "tok-g", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoomAdmin(ctx, "a", alice.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSeats(ctx, "a", alice.ID, Ending{By: alice, Leave: true}); !errors.Is(err, ErrIsRoomAdmin) {
		t.Fatalf("admin leaves: %v", err)
	}
	audited := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: "profile:x", Action: "participant.remove", TargetKind: TargetRoom, TargetID: "a"}, nil)
	if _, err := s.EndSeats(audited, "a", bob.ID, Ending{By: bob, Leave: true}); err != nil {
		t.Fatal(err)
	}
	if m := lastLine(t, s, "a"); m.Body != "Bob left the room" {
		t.Errorf("line %+v", m)
	}
	if d := auditDetails(t, s, "participant.remove"); d != "" {
		t.Errorf("a leave audited: %s", d)
	}
	if _, err := s.EndSeats(ctx, "a", gus.ID, Ending{}); err != nil {
		t.Fatal(err)
	}
	if m := lastLine(t, s, "a"); m.Body != "The admin removed Gus from the room" || m.ParticipantID != 0 {
		t.Errorf("super-admin line %+v", m)
	}
	departed, err := s.Departed(ctx, "a")
	if err != nil || departed[bob.ID] != EndLeft || departed[gus.ID] != EndRemoved || len(departed) != 2 {
		t.Fatalf("departed %v %v", departed, err)
	}
	// Bob comes back on a new seat: his old lines lose the mark.
	if _, _, err := s.Join(ctx, "inv-a", "Bob", "tok-b2", JoinProfile{ID: bob.ProfileID}); err != nil {
		t.Fatal(err)
	}
	if departed, _ := s.Departed(ctx, "a"); departed[bob.ID] != "" {
		t.Fatalf("bob back, still %q", departed[bob.ID])
	}
}

// 016-AC13 016-AC14: a blocked profile's join is refused; an unblocked one gets a new seat. Unblock
// posts and audits; a second unblock finds nothing. A block without a profile does nothing.
func TestBlocks(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-b", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	if err != nil {
		t.Fatal(err)
	}
	gus, _, err := s.Join(ctx, "inv-a", "Gus", "tok-g", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if e, err := s.EndSeats(ctx, "a", gus.ID, Ending{By: alice, Block: true}); err != nil || e.Blocked {
		t.Fatalf("block without a profile: %+v %v", e, err)
	}
	if _, err := s.EndSeats(ctx, "a", bob.ID, Ending{By: alice, Block: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Join(ctx, "inv-a", "Bob again", "tok-b2", JoinProfile{ID: bob.ProfileID}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked join: %v", err)
	}
	bs, err := s.Blocks(ctx, "a")
	if err != nil || len(bs) != 1 || bs[0].ProfileID != bob.ProfileID || bs[0].Name != "Bob" || bs[0].By != "Alice" {
		t.Fatalf("blocks %+v %v", bs, err)
	}
	audited := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: "profile:x", Action: "participant.unblock", TargetKind: TargetRoom, TargetID: "a"}, nil)
	if err := s.Unblock(audited, "a", bob.ProfileID, alice); err != nil {
		t.Fatal(err)
	}
	if m := lastLine(t, s, "a"); m.Body != "Alice unblocked Bob" {
		t.Errorf("line %+v", m)
	}
	if d := auditDetails(t, s, "participant.unblock"); !strings.Contains(d, `"name":"Bob"`) {
		t.Errorf("audit %s", d)
	}
	if err := s.Unblock(ctx, "a", bob.ProfileID, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unblock twice: %v", err)
	}
	back, _, err := s.Join(ctx, "inv-a", "Bob again", "tok-b2", JoinProfile{ID: bob.ProfileID})
	if err != nil || back.ID == bob.ID {
		t.Fatalf("rejoin %+v %v", back, err)
	}
}

// 016-AC15: blocks go with their room and their profile, and a merge moves B's to A, one per room.
func TestBlocksFollowRoomProfileAndMerge(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	aliceB := joinWithProfile(t, s, "b", "Alice", JoinProfile{ID: alice.ProfileID})
	var bobs [3]*Participant
	for i, room := range []string{"a", "a", "b"} {
		p, _, err := s.Join(ctx, "inv-"+room, "Bob", "tok-bob-"+string(rune('0'+i)), JoinProfile{RecoveryHash: "rec-bob-" + string(rune('0'+i)), SessionHash: "sess-bob-" + string(rune('0'+i))})
		if err != nil {
			t.Fatal(err)
		}
		bobs[i] = p
	}
	// Bob's two profiles are blocked from room a.
	for _, p := range bobs[:2] {
		if _, err := s.EndSeats(ctx, "a", p.ID, Ending{By: alice, Block: true}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, _, err := s.MergeProfiles(ctx, bobs[0].ProfileID, bobs[1].ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM room_blocks WHERE profile_id = ?`, bobs[0].ProfileID); n != 1 {
		t.Fatalf("A's blocks after the merge: %d", n)
	}
	if n := count(`SELECT count(*) FROM room_blocks WHERE profile_id = ?`, bobs[1].ProfileID); n != 0 {
		t.Fatalf("B's blocks after the merge: %d", n)
	}
	if err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM room_blocks`); n != 0 {
		t.Fatalf("blocks after the room's delete: %d", n)
	}
	if _, err := s.EndSeats(ctx, "b", bobs[2].ID, Ending{By: aliceB, Block: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(ctx, bobs[2].ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM room_blocks`); n != 0 {
		t.Fatalf("blocks after the profile's delete: %d", n)
	}
}
