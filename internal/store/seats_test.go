package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// endSeat ends seat id as reason, the way 016's removal and leave do.
func endSeat(t *testing.T, s *Store, id int64, reason string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE participants SET removed_at = ?, end_reason = ? WHERE id = ?`, now(), reason, id); err != nil {
		t.Fatal(err)
	}
}

// 016-AC2: a seat that ended opens nothing, whatever its invite: room cookie, profile session, API
// token, daemon token, invite creation and sharing, push, the room's lists, profile rooms and the
// overview.
func TestEndedSeatOpensNothing(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := seed(t, s, "a")
	if err := s.SavePushSubscription(ctx, PushSubscription{ParticipantID: p.ID, Endpoint: "e", P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(ctx, p.ProfileID, "t", "api-a", ScopeFull, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Valid first: each door opens, so a closed one below proves the seat closed it.
	open := func() []error {
		_, e1 := s.ParticipantByToken(ctx, "tok-a")
		_, e2 := s.ProfileParticipant(ctx, "sess-a", "a")
		_, e3 := s.APITokenParticipant(ctx, "api-a", "a", time.Now())
		_, e4 := s.DaemonByToken(ctx, "dmn-a")
		_, e5 := s.RoomParticipant(ctx, "a", p.ID)
		_, e6 := s.CreateInvite(ctx, "a", "by-alice-"+fmt.Sprint(time.Now().UnixNano()), p.ID)
		_, _, _, e7 := s.ShareInvite(ctx, "a", p.ID, "lnk", "lnk-h")
		return []error{e1, e2, e3, e4, e5, e6, e7}
	}
	for i, err := range open() {
		if err != nil {
			t.Fatalf("door %d before the end: %v", i+1, err)
		}
	}
	endSeat(t, s, p.ID, EndRemoved)
	for i, err := range open() {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("door %d after the end: %v, want ErrNotFound", i+1, err)
		}
	}
	if ps, err := s.Participants(ctx, "a"); err != nil || len(ps) != 0 {
		t.Errorf("participants %+v %v", ps, err)
	}
	if subs, err := s.PushSubscriptions(ctx, "a"); err != nil || len(subs) != 0 {
		t.Errorf("push %+v %v", subs, err)
	}
	if rooms, err := s.profileRooms(ctx, p.ProfileID); err != nil || len(rooms) != 0 {
		t.Errorf("profile rooms %+v %v", rooms, err)
	}
	if ov, err := s.RoomOverviews(ctx, 1); err != nil || ov[0].Participants != 0 {
		t.Errorf("overview %+v %v", ov, err)
	}
	// The invite stays valid: the seat alone ended.
	if inv, err := s.Invite(ctx, "a", p.InviteID); err != nil || inv.RevokedAt != nil {
		t.Errorf("invite %+v %v", inv, err)
	}
}

// 016-AC2: no store query reads seat validity off the invite any more. The access checks join
// participants to invites only through a seat's removed_at; the joins left are the invite chain's.
func TestNoAccessReadsTheInvite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "JOIN invites") && strings.Contains(line, "invite_id") {
				t.Errorf("%s:%d joins a seat to its invite: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// 016-AC3: an unticked revoke revokes that invite alone: no one joins through it, its joiners keep
// their seats and their own invites stay valid. 016-AC4 016-AC5: ticked, the chain is revoked, its
// valid seats end as removed, the room reads who did it and who left, and the audit entry says so.
// A seat whose invite an unticked revoke left behind counts as in the chain.
func TestRevokeCut(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := seed(t, s, "a")
	if _, err := s.CreateInvite(ctx, "a", "inv-b", alice.ID); err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.Join(ctx, "inv-b", "Bob", "tok-b", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	bobInv, err := s.CreateInvite(ctx, "a", "inv-c", bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	cy, _, err := s.Join(ctx, "inv-c", "Cy", "tok-c", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := s.RevokeInvite(ctx, "a", bobInv.ID, false, "Alice")
	if err != nil || !slices.Equal(plan.Invites, []int64{bobInv.ID}) || len(plan.Lose) != 0 || len(plan.Removed) != 0 {
		t.Fatalf("unticked: %+v %v", plan, err)
	}
	if _, _, err := s.Join(ctx, "inv-c", "Mallory", "tok-m", JoinProfile{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("join through the revoked invite: %v", err)
	}
	if _, err := s.ParticipantByToken(ctx, "tok-c"); err != nil {
		t.Fatalf("cy after the unticked revoke: %v", err)
	}

	// Bob hosts, with his app and a queued turn: the cut drops the turn, clears the host and
	// returns the app (016-AC8, 016-AC9).
	bobApp, err := s.PairDaemon(ctx, "a", bob.ID, "dmn-bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "a", bob.ID); err != nil {
		t.Fatal(err)
	}
	// The seeded queued turn becomes his: a persona queues once.
	if _, err := s.db.ExecContext(ctx, `UPDATE turn_queue SET participant_id = ? WHERE room_id = 'a'`, bob.ID); err != nil {
		t.Fatal(err)
	}
	if q, err := s.Queue(ctx, "a"); err != nil || len(q) != 1 || q[0].ParticipantID != bob.ID {
		t.Fatalf("queue before the cut: %+v %v", q, err)
	}

	bobsInvite := bob.InviteID
	entries := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE action = 'invite.revoke'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	audited := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: "admin", Action: "invite.revoke", TargetKind: TargetRoom, TargetID: "a"}, nil)
	plan, err = s.RevokeInvite(audited, "a", bobsInvite, true, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Invites, []int64{bobsInvite}) || !slices.Equal(plan.Removed, []int64{bob.ID, cy.ID}) || len(plan.Lose) != 2 {
		t.Fatalf("ticked: %+v", plan)
	}
	if !slices.Equal(plan.Daemons, []int64{bobApp.ID}) {
		t.Fatalf("cut apps %v, want [%d]", plan.Daemons, bobApp.ID)
	}
	if r, err := s.Room(ctx, "a"); err != nil || r.HostParticipantID != 0 {
		t.Fatalf("host after the cut: %+v %v", r, err)
	}
	if q, err := s.Queue(ctx, "a"); err != nil || len(q) != 0 {
		t.Fatalf("queue after the cut: %+v %v", q, err)
	}
	var reason string
	if err := s.db.QueryRowContext(ctx, `SELECT end_reason FROM participants WHERE id = ?`, cy.ID).Scan(&reason); err != nil || reason != EndRemoved {
		t.Fatalf("cy's seat: %q %v", reason, err)
	}
	msgs, err := s.Messages(ctx, "a", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if last := msgs[len(msgs)-1]; last.Kind != KindSystem || last.Body != "Alice revoked an invite; Bob, Cy were removed" {
		t.Fatalf("line %+v", last)
	}
	if entries() != 1 {
		t.Fatal("no audit entry")
	}
	var details string
	if err := s.db.QueryRowContext(ctx, `SELECT details FROM audit_log WHERE action = 'invite.revoke'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`"cut":true,"invite":%d,"removed":[%d,%d],"revoked":[%d]`, bobsInvite, bob.ID, cy.ID, bobsInvite); !strings.Contains(details, want) {
		t.Fatalf("details %s, want %s", details, want)
	}
	// Again: nothing left, no line.
	if plan, err := s.RevokeInvite(ctx, "a", bobsInvite, true, "Alice"); err != nil || len(plan.Removed) != 0 || len(plan.Invites) != 0 {
		t.Fatalf("again: %+v %v", plan, err)
	}
	if again, _ := s.Messages(ctx, "a", 0, 1000); len(again) != len(msgs) {
		t.Fatal("a second line")
	}
}

// 016-AC18: export writes an ended seat inactive; import ends it as revoked.
func TestExportImportEndedSeat(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if _, err := s.CreateRoom(ctx, "a", "Room a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "a", "inv-a", 0); err != nil {
		t.Fatal(err)
	}
	var bob *Participant
	for _, name := range []string{"Alice", "Bob"} {
		p, _, err := s.Join(ctx, "inv-a", name, "tok-"+name, JoinProfile{})
		if err != nil {
			t.Fatal(err)
		}
		bob = p
	}
	endSeat(t, s, bob.ID, EndLeft)
	dir := export(t, s, "a")
	raw, err := os.ReadFile(filepath.Join(dir, fileParticipants))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"Bob","joined_at"`) || strings.Count(string(raw), `"active":false`) != 1 {
		t.Fatalf("participants:\n%s", raw)
	}
	if _, err := s.ImportRoom(ctx, dir, importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	var name, reason string
	if err := s.db.QueryRowContext(ctx, `SELECT name, end_reason FROM participants WHERE room_id = 'copy' AND removed_at IS NOT NULL`).Scan(&name, &reason); err != nil || name != "Bob" || reason != EndRevoked {
		t.Fatalf("imported ended seat: %s %s %v", name, reason, err)
	}
	if ps, err := s.Participants(ctx, "copy"); err != nil || len(ps) != 1 || ps[0].Name != "Alice" {
		t.Fatalf("imported seats %+v %v", ps, err)
	}
}
