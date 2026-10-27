package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// joinWithProfile adds a room with an invite and joins it as name under prof.
func joinWithProfile(t *testing.T, s *Store, room, name string, prof JoinProfile) *Participant {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, room, "Room "+room); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, room, "inv-"+room, 0); err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Join(ctx, "inv-"+room, name, "tok-"+room, prof)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 002-AC1 002-AC2: the first join creates the profile, a later one attaches to it, and the profile
// lists the rooms it still has access to.
func TestJoinCreatesThenAttachesProfile(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if a.ProfileID == 0 {
		t.Fatal("first join created no profile")
	}
	b := joinWithProfile(t, s, "b", "Ali", JoinProfile{ID: a.ProfileID})
	if b.ProfileID != a.ProfileID {
		t.Fatalf("second join: profile %d, want %d", b.ProfileID, a.ProfileID)
	}
	joinWithProfile(t, s, "c", "Stranger", JoinProfile{})
	prof, err := s.ProfileBySession(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(prof.Rooms) != 2 || prof.Rooms[0].RoomID != "b" || prof.Rooms[0].Name != "Ali" || prof.Rooms[1].RoomID != "a" {
		t.Fatalf("rooms: %+v", prof.Rooms)
	}
	if _, err := s.RevokeInvite(ctx, "b", b.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if prof, _ = s.ProfileBySession(ctx, "sess"); len(prof.Rooms) != 1 || prof.Rooms[0].RoomID != "a" {
		t.Fatalf("rooms after a revoke: %+v", prof.Rooms)
	}
	if _, err := s.ProfileBySession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	// A failed join creates no profile.
	if _, _, err := s.Join(ctx, "nope", "X", "t", JoinProfile{RecoveryHash: "rec2", SessionHash: "sess2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad invite: %v", err)
	}
	if _, err := s.ProfileBySession(ctx, "sess2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile left by a failed join: %v", err)
	}
}

// 002-AC3
func TestSignInByRecovery(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	prof, err := s.SignInByRecovery(ctx, "rec", "other-device", "")
	if err != nil || prof.ID != a.ProfileID || len(prof.Rooms) != 1 {
		t.Fatalf("recover: %+v %v", prof, err)
	}
	if p, err := s.ProfileParticipant(ctx, "other-device", "a"); err != nil || p.ID != a.ID {
		t.Fatalf("participant through the new session: %+v %v", p, err)
	}
	if _, err := s.SignInByRecovery(ctx, "sess", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session hash is no recovery: %v", err)
	}
}

func TestProfileParticipant(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	if _, err := s.ProfileParticipant(ctx, "sess", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another profile's room: %v", err)
	}
	// Joining the same room again: the newest participant wins.
	again, _, err := s.Join(ctx, "inv-a", "Alice 2", "tok-a2", JoinProfile{ID: a.ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.ProfileParticipant(ctx, "sess", "a"); err != nil || p.ID != again.ID || p.ProfileID != a.ProfileID {
		t.Fatalf("newest: %+v %v", p, err)
	}
	if _, err := s.RevokeInvite(ctx, "a", a.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfileParticipant(ctx, "sess", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE profile_sessions SET expires_at_s = ?`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfileBySession(ctx, "sess-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: %v", err)
	}
}

func link(s *Store, t *testing.T, profileID int64, email, purpose, token string, ttl time.Duration) {
	t.Helper()
	if err := s.CreateMagicLink(context.Background(), MagicLink{ProfileID: profileID, Email: email, Purpose: purpose, TokenHash: token, ExpiresAt: time.Now().Add(ttl)}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// 002-AC4: single use, expiry, bind then sign in, and links gone stale or opened elsewhere.
func TestMagicLinks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})

	link(s, t, a.ProfileID, "alice@example.com", MagicBind, "bind-a", time.Minute)
	// A bind link opened outside Alice's profile, or with none, binds nothing and stays usable.
	for _, current := range []int64{0, b.ProfileID} {
		if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bind-a", Purpose: MagicBind, SessionHash: "dev2", Current: current, Now: time.Now()}); !errors.Is(err, ErrWrongProfile) {
			t.Fatalf("bind from profile %d: %v", current, err)
		}
	}
	// Offered as the other kind, a link is unknown and stays unused.
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bind-a", Purpose: MagicSignIn, SessionHash: "dev2", Current: a.ProfileID, Now: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bind link as a sign-in: %v", err)
	}
	prof, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bind-a", Purpose: MagicBind, SessionHash: "dev2", Current: a.ProfileID, Now: time.Now()})
	if err != nil || prof.Email != "alice@example.com" || prof.ID != a.ProfileID {
		t.Fatalf("bind: %+v %v", prof, err)
	}
	if _, err := s.ProfileBySession(ctx, "dev2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a bind link opened a session: %v", err)
	}
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bind-a", Purpose: MagicBind, SessionHash: "dev3", Current: a.ProfileID, Now: time.Now()}); !errors.Is(err, ErrLinkUsed) {
		t.Fatalf("second use: %v", err)
	}
	if id, err := s.ProfileIDByEmail(ctx, "alice@example.com", ""); err != nil || id != a.ProfileID {
		t.Fatalf("by email: %d %v", id, err)
	}
	if _, err := s.ProfileIDByEmail(ctx, "nobody@example.com", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown email: %v", err)
	}

	link(s, t, a.ProfileID, "alice@example.com", MagicSignIn, "expired", -time.Second)
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "expired", Purpose: MagicSignIn, SessionHash: "x", Current: 0, Now: time.Now()}); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("expired: %v", err)
	}
	link(s, t, a.ProfileID, "alice@example.com", MagicSignIn, "signin", time.Minute)
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "signin", Purpose: MagicBind, SessionHash: "dev4", Current: a.ProfileID, Now: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sign-in link as a bind: %v", err)
	}
	if prof, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "signin", Purpose: MagicSignIn, SessionHash: "dev4", Current: b.ProfileID, Now: time.Now()}); err != nil || prof.ID != a.ProfileID {
		t.Fatalf("sign in: %+v %v", prof, err)
	}
	if got, err := s.ProfileBySession(ctx, "dev4"); err != nil || got.ID != a.ProfileID {
		t.Fatalf("sign-in session: %+v %v", got, err)
	}

	// Bob asks to bind Alice's address: the link stays unused, and the address stays hers.
	link(s, t, b.ProfileID, "alice@example.com", MagicBind, "bind-b", time.Minute)
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bind-b", Purpose: MagicBind, SessionHash: "x", Current: b.ProfileID, Now: time.Now()}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("taken: %v", err)
	}
	var used sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT used_at FROM magic_links WHERE token_hash = 'bind-b'`).Scan(&used); err != nil || used.Valid {
		t.Fatalf("refused bind spent the link: %v %v", used, err)
	}

	// Alice moves to another address: a sign-in link for the old one is stale.
	link(s, t, a.ProfileID, "old@example.com", MagicSignIn, "stale", time.Minute)
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "stale", Purpose: MagicSignIn, SessionHash: "x", Current: 0, Now: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale: %v", err)
	}

	// Creating a link sweeps the ones used or expired over a day before (011-AC23).
	if err := s.CreateMagicLink(ctx, MagicLink{ProfileID: a.ProfileID, Email: "alice@example.com", Purpose: MagicSignIn, TokenHash: "fresh",
		ExpiresAt: time.Now().Add(25 * time.Hour)}, time.Now().Add(magicLinkKeep+time.Minute)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM magic_links WHERE token_hash IN ('bind-a', 'expired', 'signin')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("swept: %d left, %v", n, err)
	}
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 002-AC5: the purge invariant for profiles. Deleting one removes its sessions, links and email
// from every table, and detaches its participants, which keep their name and access.
func TestDeleteProfilePurges(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	joinWithProfile(t, s, "b", "Alice B", JoinProfile{ID: a.ProfileID})
	other := joinWithProfile(t, s, "c", "Carol", JoinProfile{RecoveryHash: "rec-c", SessionHash: "sess-c"})
	const email = "secret-address@example.com"
	link(s, t, a.ProfileID, email, MagicBind, "bind", time.Minute)
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bind", Purpose: MagicBind, SessionHash: "", Current: a.ProfileID, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	link(s, t, a.ProfileID, email, MagicSignIn, "pending", time.Minute)
	link(s, t, other.ProfileID, "carol@example.com", MagicBind, "carol", time.Minute)

	if err := s.DeleteProfile(ctx, a.ProfileID); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"profiles": 0, "profile_sessions": 0, "magic_links": 0} {
		if n := count(t, s, `SELECT count(*) FROM "`+table+`" WHERE `+map[bool]string{true: "id", false: "profile_id"}[table == "profiles"]+` = ?`, a.ProfileID); n != want {
			t.Errorf("%s: %d rows of the deleted profile left", table, n)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM participants WHERE name IN ('Alice', 'Alice B') AND profile_id IS NULL`); n != 2 {
		t.Errorf("detached participants: %d, want 2", n)
	}
	if p, err := s.ParticipantByToken(ctx, "tok-a"); err != nil || p.Name != "Alice" || p.ProfileID != 0 {
		t.Errorf("detached participant keeps its access and name: %+v %v", p, err)
	}
	if _, err := s.ProfileParticipant(ctx, "sess-a", "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted profile's session still opens the room: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM magic_links WHERE profile_id = ?`, other.ProfileID); n != 1 {
		t.Errorf("other profile's link: %d, want 1", n)
	}
	if prof, err := s.ProfileBySession(ctx, "sess-c"); err != nil || len(prof.Rooms) != 1 {
		t.Errorf("other profile: %+v %v", prof, err)
	}
	// The tables hold no copy of the email; on disk it lasts until autovacuum reuses the space (017 Q4).
	if n := count(t, s, `SELECT (SELECT count(*) FROM profiles WHERE email = ?) + (SELECT count(*) FROM magic_links WHERE email = ?)`, email, email); n != 0 {
		t.Errorf("%d rows still hold the deleted email", n)
	}
	if err := s.DeleteProfile(ctx, a.ProfileID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// 002-AC6
func TestRenameRoom(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	m, err := s.RenameRoom(ctx, "a", `New "plan"`, p)
	if err != nil || m.Kind != KindSystem || m.ParticipantID != p.ID || m.Body != `Alice renamed the room from "Room a" to "New \"plan\""` {
		t.Fatalf("rename: %+v %v", m, err)
	}
	if r, _ := s.Room(ctx, "a"); r.Name != `New "plan"` {
		t.Fatalf("name = %q", r.Name)
	}
	if m, err := s.RenameRoom(ctx, "a", `New "plan"`, p); err != nil || m != nil {
		t.Fatalf("same name: %+v %v", m, err)
	}
	m, err = s.RenameRoom(ctx, "a", "Final", nil)
	if err != nil || m.ParticipantID != 0 || m.Body != `The admin renamed the room from "New \"plan\"" to "Final"` {
		t.Fatalf("admin rename: %+v %v", m, err)
	}
	if _, err := s.RenameRoom(ctx, "zz", "x", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
}

// 002-AC7
func TestInvitesRecordTheirCreator(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := s.CreateInvite(ctx, "a", "mine", p.ID)
	if err != nil || mine.CreatedBy != p.ID {
		t.Fatalf("create: %+v %v", mine, err)
	}
	if _, err := s.CreateInvite(ctx, "a", "bobs", bob.ID); err != nil {
		t.Fatal(err)
	}
	if invs, err := s.Invites(ctx, "a", p.ID); err != nil || len(invs) != 1 || invs[0].ID != mine.ID {
		t.Fatalf("mine: %+v %v", invs, err)
	}
	if invs, err := s.Invites(ctx, "a", 0); err != nil || len(invs) != 3 {
		t.Fatalf("all: %+v %v", invs, err)
	}
	if _, err := s.RevokeInvite(ctx, "a", mine.ID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Invite(ctx, "a", mine.ID)
	if err != nil || got.CreatedBy != p.ID || got.RevokedAt == nil {
		t.Fatalf("revoked invite: %+v %v", got, err)
	}
	if _, err := s.Invite(ctx, "b", mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("through another room: %v", err)
	}
	// A participant joining through a member's invite: the chain invites -> participants -> invites
	// still purges with the room.
	if _, _, err := s.Join(ctx, "bobs", "Carol", "tok-carol", JoinProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM invites`); n != 0 {
		t.Fatalf("invites left: %d", n)
	}
}

// Storage failures surface as errors, never as a missing profile or link a caller would treat as
// "not signed in" or "invalid link".
func TestProfileCallsFailOnAClosedStore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	_ = s.Close()
	calls := map[string]func() error{
		"ProfileBySession":   func() error { _, err := s.ProfileBySession(ctx, "sess-a"); return err },
		"SignInByRecovery":   func() error { _, err := s.SignInByRecovery(ctx, "rec-a", "x", ""); return err },
		"ProfileIDByEmail":   func() error { _, err := s.ProfileIDByEmail(ctx, "a@example.com", ""); return err },
		"ProfileParticipant": func() error { _, err := s.ProfileParticipant(ctx, "sess-a", "a"); return err },
		"Profiles":           func() error { _, err := s.Profiles(ctx); return err },
		"DeleteProfile":      func() error { return s.DeleteProfile(ctx, p.ProfileID) },
		"CreateMagicLink": func() error {
			return s.CreateMagicLink(ctx, MagicLink{ProfileID: 1, Email: "a@example.com", Purpose: MagicBind, TokenHash: "t", ExpiresAt: time.Now().Add(time.Minute)}, time.Now())
		},
		"UseMagicLink": func() error {
			_, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "t", Purpose: MagicSignIn, SessionHash: "x", Current: 0, Now: time.Now()})
			return err
		},
		"PendingEmail":       func() error { _, err := s.PendingEmail(ctx, 1, time.Now()); return err },
		"DeleteMagicLink":    func() error { return s.DeleteMagicLink(ctx, "t") },
		"SnoozeEmailNudge":   func() error { return s.SnoozeEmailNudge(ctx, 1, time.Now()) },
		"CreateRecoveryLink": func() error { _, err := s.CreateRecoveryLink(ctx, 1, "t", time.Now()); return err },
		"UseRecoveryLink":    func() error { _, err := s.UseRecoveryLink(ctx, "t", "x", time.Now(), ""); return err },
		"RenameRoom":         func() error { _, err := s.RenameRoom(ctx, "a", "x", p); return err },
		"CreateInvite":       func() error { _, err := s.CreateInvite(ctx, "a", "h", p.ID); return err },
		"Invite":             func() error { _, err := s.Invite(ctx, "a", 1); return err },
		"Invites":            func() error { _, err := s.Invites(ctx, "a", 0); return err },
		"Join": func() error {
			_, _, err := s.Join(ctx, "inv-a", "B", "t", JoinProfile{RecoveryHash: "r", SessionHash: "s"})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrEmailTaken) {
			t.Errorf("%s: %v, want a storage error", name, err)
		}
	}
}

// 002-AC7: revoking an invite also revokes the invites its joiners created, down the chain, so a
// member cut off cannot walk back in through an invite they made. Other branches stay.
func TestRevokeInviteCascadesToInvitesItsJoinersMade(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice := seed(t, s, "a") // joined through inv-a, the admin's
	chain := []int64{alice.InviteID}
	by := alice
	for i, name := range []string{"Carol", "Dave"} {
		inv, err := s.CreateInvite(ctx, "a", fmt.Sprintf("chain-%d", i), by.ID)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, inv.ID)
		if by, _, err = s.Join(ctx, fmt.Sprintf("chain-%d", i), name, "tok-"+name, JoinProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := s.CreateInvite(ctx, "a", "other", 0)
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.Join(ctx, "other", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := s.CreateInvite(ctx, "a", "bobs", bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RevokeInvite(ctx, "a", alice.InviteID, true, "Admin")
	if err != nil || !slices.Equal(got.Invites, chain) {
		t.Fatalf("revoked %v %v, want %v", got, err, chain)
	}
	// Idempotent: the second revoke finds nothing left to cut (011-AC20).
	if got, err := s.RevokeInvite(ctx, "a", alice.InviteID, true, "Admin"); err != nil || len(got.Invites) != 0 || len(got.Lose) != 0 {
		t.Fatalf("second revoke: %+v %v, want nothing cut", got, err)
	}
	for _, tok := range []string{"tok-a", "tok-Carol", "tok-Dave"} {
		if _, err := s.ParticipantByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s kept access: %v", tok, err)
		}
	}
	if _, _, err := s.Join(ctx, "chain-0", "Alice again", "t2", JoinProfile{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("rejoin through her own invite: %v", err)
	}
	// Nor can a revoked member mint a new one, whatever the HTTP layer checked before.
	if _, err := s.CreateInvite(ctx, "a", "late", alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("invite by a revoked member: %v", err)
	}
	if _, err := s.CreateInvite(ctx, "b", "elsewhere", bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("invite into a room the member is not in: %v", err)
	}
	if _, err := s.ParticipantByToken(ctx, "tok-bob"); err != nil {
		t.Errorf("another branch lost access: %v", err)
	}
	if inv, err := s.Invite(ctx, "a", bobs.ID); err != nil || inv.RevokedAt != nil || other.ID == 0 {
		t.Errorf("another branch's invite: %+v %v", inv, err)
	}
}

// 005-AC41: the profile's room cards count the room's people, carry its last activity and say
// where the profile is a room admin.
func TestProfileRoomsCarryPeopleActivityAndAdmin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	joinWithProfile(t, s, "b", "Ali", JoinProfile{ID: a.ProfileID})
	if _, err := s.CreateInvite(ctx, "a", "inv-a2", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Join(ctx, "inv-a2", "Bob", "tok-a2", JoinProfile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoomAdmin(ctx, "a", a.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	prof, err := s.ProfileBySession(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ProfileRoom{}
	for _, r := range prof.Rooms {
		got[r.RoomID] = r
	}
	if got["a"].People != 2 || !got["a"].Admin || got["b"].People != 1 || got["b"].Admin {
		t.Fatalf("rooms: %+v", prof.Rooms)
	}
	// The last line is Alice's appointment, after both joins.
	if got["a"].LastActivity.Before(got["a"].JoinedAt) || got["b"].LastActivity.IsZero() {
		t.Fatalf("activity: %+v", prof.Rooms)
	}
}

// 011-AC23: a bind link opened outside its profile names the profile and waits for a confirm, which
// binds the address and leaves the opening browser as it was; the stored link tells expired from
// used on the injected clock.
func TestMagicLinkOutcomes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := s.CreateMagicLink(ctx, MagicLink{ProfileID: a.ProfileID, Email: "alice@example.com", Purpose: MagicBind, TokenHash: "bind", ExpiresAt: t0.Add(time.Minute)}, t0); err != nil {
		t.Fatal(err)
	}
	use := func(current int64, confirm bool, at time.Time) (*Profile, error) {
		return s.UseMagicLink(ctx, MagicUse{TokenHash: "bind", Purpose: MagicBind, SessionHash: "x", Current: current, Confirm: confirm, Now: at})
	}
	for _, current := range []int64{0, b.ProfileID} {
		p, err := use(current, false, t0)
		if !errors.Is(err, ErrWrongProfile) || p.Name != "Alice" || p.Email != "alice@example.com" || p.Rooms != nil {
			t.Fatalf("unconfirmed from %d: %+v %v", current, p, err)
		}
	}
	if _, err := use(b.ProfileID, true, t0.Add(time.Minute)); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("at its expiry: %v", err)
	}
	p, err := use(b.ProfileID, true, t0)
	if err != nil || p.ID != a.ProfileID || p.Email != "alice@example.com" || p.Rooms != nil {
		t.Fatalf("confirmed: %+v %v", p, err)
	}
	if got, _ := s.ProfileBySession(ctx, "sess-b"); got.Email != "" {
		t.Fatal("the confirming browser's own profile changed")
	}
	if got, _ := s.ProfileBySession(ctx, "sess-a"); got.Email != "alice@example.com" {
		t.Fatal("not bound to the profile that asked")
	}
	if _, err := s.ProfileBySession(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a confirmed bind opened a session")
	}
	if _, err := use(b.ProfileID, true, t0); !errors.Is(err, ErrLinkUsed) {
		t.Fatalf("again: %v", err)
	}
	// Bob asks for Alice's address: taken comes before any confirm, and names nobody.
	if err := s.CreateMagicLink(ctx, MagicLink{ProfileID: b.ProfileID, Email: "alice@example.com", Purpose: MagicBind, TokenHash: "bob", ExpiresAt: t0.Add(time.Minute)}, t0); err != nil {
		t.Fatal(err)
	}
	if p, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "bob", Purpose: MagicBind, Current: a.ProfileID, Now: t0}); !errors.Is(err, ErrEmailTaken) || p != nil {
		t.Fatalf("taken: %+v %v", p, err)
	}
}

// 011-AC21: the pending address is the newest bind link's, while it is unused and unexpired.
func TestPendingEmail(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	t0 := time.Now()
	add := func(id int64, email, purpose, tok string) {
		t.Helper()
		if err := s.CreateMagicLink(ctx, MagicLink{ProfileID: id, Email: email, Purpose: purpose, TokenHash: tok, ExpiresAt: t0.Add(time.Minute)}, t0); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(id int64, at time.Time) string {
		t.Helper()
		got, err := s.PendingEmail(ctx, id, at)
		if err != nil {
			t.Fatal(err)
		}
		return got.Email
	}
	if got := pending(a.ProfileID, t0); got != "" {
		t.Fatalf("none yet: %q", got)
	}
	add(a.ProfileID, "old@example.com", MagicBind, "1")
	add(a.ProfileID, "new@example.com", MagicBind, "2")
	add(a.ProfileID, "signin@example.com", MagicSignIn, "3")
	add(b.ProfileID, "bob@example.com", MagicBind, "4")
	cases := []struct {
		name string
		id   int64
		at   time.Time
		want string
	}{
		{"newest bind link", a.ProfileID, t0, "new@example.com"},
		{"another profile's", b.ProfileID, t0, "bob@example.com"},
		{"expired", a.ProfileID, t0.Add(time.Minute), ""},
	}
	for _, c := range cases {
		if got := pending(c.id, c.at); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if pb, err := s.PendingEmail(ctx, a.ProfileID, t0); err != nil || !pb.SentAt.Equal(t0) || !pb.ExpiresAt.Equal(t0.Add(time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("dates: %+v %v", pb, err)
	}
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "2", Purpose: MagicBind, Current: a.ProfileID, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if got := pending(a.ProfileID, t0); got != "" {
		t.Fatalf("after the newest is used: %q", got)
	}
}

// 011-AC21: after a resend, opening the first link binds its address and spends the newer link, so
// nothing stays pending and the newer link cannot rebind another address.
func TestBindSpendsTheOtherBindLinks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	t0 := time.Now()
	for _, l := range []struct{ email, tok string }{{"first@example.com", "first"}, {"second@example.com", "second"}} {
		if err := s.CreateMagicLink(ctx, MagicLink{ProfileID: a.ProfileID, Email: l.email, Purpose: MagicBind, TokenHash: l.tok, ExpiresAt: t0.Add(time.Minute)}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "first", Purpose: MagicBind, Current: a.ProfileID, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PendingEmail(ctx, a.ProfileID, t0); err != nil || got.Email != "" {
		t.Fatalf("pending after the first link: %q %v", got, err)
	}
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "second", Purpose: MagicBind, Current: a.ProfileID, Now: t0}); !errors.Is(err, ErrLinkUsed) {
		t.Fatalf("the newer link: %v", err)
	}
	if p, _ := s.ProfileBySession(ctx, "sess-a"); p.Email != "first@example.com" {
		t.Fatalf("bound %q", p.Email)
	}
}

// 011-AC23: another request's link creation keeps a used or expired link readable as such for a
// day on the injected clock, then sweeps it.
func TestMagicLinkStateOutlivesOtherRequests(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	create := func(id int64, email, tok string, at time.Time) {
		t.Helper()
		if err := s.CreateMagicLink(ctx, MagicLink{ProfileID: id, Email: email, Purpose: MagicBind, TokenHash: tok, ExpiresAt: at.Add(time.Minute)}, at); err != nil {
			t.Fatal(err)
		}
	}
	create(a.ProfileID, "a@example.com", "used", t0)
	create(b.ProfileID, "b@example.com", "expired", t0)
	if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "used", Purpose: MagicBind, Current: a.ProfileID, Now: t0}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		at   time.Time
		used error
		exp  error
	}{
		{"another request an hour later", t0.Add(time.Hour), ErrLinkUsed, ErrLinkExpired},
		{"another request past a day", t0.Add(magicLinkKeep + 2*time.Minute), ErrNotFound, ErrNotFound},
	}
	for i, c := range cases {
		create(b.ProfileID, "other@example.com", fmt.Sprint("other", i), c.at)
		if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "used", Purpose: MagicBind, Current: a.ProfileID, Now: c.at}); !errors.Is(err, c.used) {
			t.Errorf("%s, used link: %v", c.name, err)
		}
		if _, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "expired", Purpose: MagicBind, Current: b.ProfileID, Now: c.at}); !errors.Is(err, c.exp) {
			t.Errorf("%s, expired link: %v", c.name, err)
		}
	}
}

// 011-AC1: concurrent uses of one recovery link sign in exactly one browser.
func TestRecoveryLinkConcurrentUse(t *testing.T) {
	s := openTest(t)
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	t0 := time.Now()
	if _, err := s.CreateRecoveryLink(context.Background(), a.ProfileID, "tok", t0); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := s.UseRecoveryLink(context.Background(), "tok", fmt.Sprint("sess-", i), t0, "")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrNotFound):
			t.Errorf("use: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d uses succeeded, want 1", ok)
	}
}

// 011-AC8: the snooze is stored on the profile, so every device reads it.
func TestSnoozeEmailNudge(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	if p, _ := s.ProfileBySession(ctx, "sess-a"); p.EmailNudgeUntil != nil {
		t.Fatal("snoozed from the start")
	}
	until := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := s.SnoozeEmailNudge(ctx, a.ProfileID, until); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.SignInByRecovery(ctx, "rec-a", "other", ""); p.EmailNudgeUntil == nil || !p.EmailNudgeUntil.Equal(until) {
		t.Fatalf("another device: %+v", p.EmailNudgeUntil)
	}
	if err := s.SnoozeEmailNudge(ctx, 999, until); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no profile: %v", err)
	}
}

// 011-AC1 011-AC2 011-AC3: a recovery link works once within 24 h on the injected clock, a newer
// one voids the older, both uses are audited without the token, and a profile delete removes them.
func TestRecoveryLinks(t *testing.T) {
	s := openTest(t)
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	audited := func(action string, id int64) context.Context {
		return WithAudit(context.Background(), AuditEntry{At: t0, Actor: ActorSuperAdmin, Action: action, TargetKind: TargetProfile,
			TargetID: strconv.FormatInt(id, 10)}, nil)
	}
	create := func(id int64, tok string) time.Time {
		t.Helper()
		exp, err := s.CreateRecoveryLink(audited("gen", id), id, tok, t0)
		if err != nil {
			t.Fatal(err)
		}
		return exp
	}
	use := func(tok, sess string, at time.Time) error {
		_, err := s.UseRecoveryLink(audited("use", 0), tok, sess, at, "")
		return err
	}
	if exp := create(a.ProfileID, "old-token"); !exp.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("expiry: %v", exp)
	}
	create(a.ProfileID, "new-token")
	create(b.ProfileID, "bob-token")
	if _, err := s.CreateRecoveryLink(context.Background(), 999, "x", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown profile: %v", err)
	}
	cases := []struct {
		name, tok string
		at        time.Time
		ok        bool
	}{
		{"voided by the newer", "old-token", t0, false},
		{"unknown", "nope", t0, false},
		{"expired", "bob-token", t0.Add(24 * time.Hour), false},
		{"within 24 h", "new-token", t0.Add(24*time.Hour - time.Millisecond), true},
		{"second use", "new-token", t0, false},
	}
	for _, c := range cases {
		err := use(c.tok, "sess-"+c.name, c.at)
		if c.ok != (err == nil) || (!c.ok && !errors.Is(err, ErrNotFound)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if p, err := s.ProfileBySession(context.Background(), "sess-within 24 h"); err != nil || p.ID != a.ProfileID {
		t.Fatalf("session from the link: %+v %v", p, err)
	}
	var uuid string
	if err := s.db.QueryRowContext(context.Background(), `SELECT uuid FROM profiles WHERE id = ?`, a.ProfileID).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM audit_log WHERE action IN ('gen', 'use') AND target_id = ? AND target_name = 'Alice' AND details::jsonb->>'profile_uuid' = ?`,
		strconv.FormatInt(a.ProfileID, 10), uuid); n != 3 {
		t.Fatalf("audit entries for Alice: %d, want 2 generations and 1 use", n)
	}
	if n := count(t, s, `SELECT count(*) FROM audit_log WHERE action = 'use'`); n != 1 {
		t.Fatalf("failed uses audited: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM audit_log WHERE details LIKE '%token%' OR target_name LIKE '%token%'`); n != 0 {
		t.Fatal("the audit log holds a token")
	}
	if err := s.DeleteProfile(context.Background(), a.ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM recovery_links WHERE profile_id = ?`, a.ProfileID); n != 0 {
		t.Fatalf("links left after the profile delete: %d", n)
	}
}
