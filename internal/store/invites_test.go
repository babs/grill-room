package store

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// 011-AC26: one shareable link per member and room, the creator's made with the room, the same one
// on every ask, concurrent asks included, a new one after its revoke, none for the super-admin.
func TestShareInvite(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	if _, err := createFor(t, s, "r", alice, "Ali"); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "r")
	ali := ps[0]
	inv, tok, created, err := s.ShareInvite(ctx, "r", ali.ID, "unused", "unused-h")
	if err != nil || created || tok != "lnk-r" || !inv.Shareable || inv.CreatedBy != ali.ID {
		t.Fatalf("creator's link: %+v %q %v %v", inv, tok, created, err)
	}
	bob, _, err := s.Join(ctx, "lnkh-r", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatalf("join through the shareable link: %v", err)
	}

	var wg sync.WaitGroup
	type got struct {
		id      int64
		tok     string
		created bool
		err     error
	}
	res := make([]got, 8)
	for i := range res {
		wg.Go(func() {
			inv, tok, created, err := s.ShareInvite(ctx, "r", bob.ID, fmt.Sprint("b", i), fmt.Sprint("bh", i))
			res[i] = got{tok: tok, created: created, err: err}
			if inv != nil {
				res[i].id = inv.ID
			}
		})
	}
	wg.Wait()
	made := 0
	for _, g := range res {
		if g.err != nil || g.id != res[0].id || g.tok != res[0].tok {
			t.Fatalf("concurrent asks disagree: %+v", res)
		}
		if g.created {
			made++
		}
	}
	if made != 1 {
		t.Fatalf("concurrent asks made %d links, want 1: %+v", made, res)
	}
	first := res[0].id

	if _, err := s.RevokeInvite(ctx, "r", first); err != nil {
		t.Fatal(err)
	}
	inv, tok, created, err = s.ShareInvite(ctx, "r", bob.ID, "b-new", "bh-new")
	if err != nil || !created || tok != "b-new" || inv.ID == first {
		t.Fatalf("after its revoke: %+v %q %v %v", inv, tok, created, err)
	}
	// The unique index still holds when someone bypasses the lookup.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO invites(room_id, token_hash, created_at, created_by, link) VALUES('r', 'dup-h', '', ?, 'dup')`, bob.ID); err == nil {
		t.Fatal("a second valid shareable link for bob")
	}

	invs, _ := s.Invites(ctx, "r", 0)
	var shareable []int64
	for _, i := range invs {
		if i.Shareable {
			shareable = append(shareable, i.CreatedBy)
		}
	}
	// Alice's, bob's revoked one, bob's new one: never one for the super-admin (created_by 0).
	if want := []int64{ali.ID, bob.ID, bob.ID}; !slices.Equal(shareable, want) {
		t.Fatalf("shareable invites by %v, want %v", shareable, want)
	}
	if _, _, _, err := s.ShareInvite(ctx, "r", 0, "adm", "adm-h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("super-admin: %v, want ErrNotFound", err)
	}
	if adm, _ := s.Invites(ctx, "lobby", 0); len(adm) != 1 || adm[0].Shareable {
		t.Fatalf("a super-admin room got a shareable link: %+v", adm)
	}
	// A member cut off gets none.
	if _, err := s.RevokeInvite(ctx, "r", bob.InviteID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ShareInvite(ctx, "r", bob.ID, "late", "late-h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked member: %v, want ErrNotFound", err)
	}
}

// 011-AC19 011-AC20: on a three-level chain (alice's link, Bea joins, Bea's invite, Cy joins, Cy's
// invite, Pat joins, Pat holding another seat outside the chain), the preview names the invites and
// people the revoke then cuts, writes nothing, and the revoke counts a joiner who came in between.
func TestRevokePreviewMatchesRevoke(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	if _, err := createFor(t, s, "r", alice, "Ali"); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "r")
	ali := ps[0]
	a, _, _, err := s.ShareInvite(ctx, "r", ali.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	join := func(inviteHash, name, tok string, prof JoinProfile) *Participant {
		t.Helper()
		p, _, err := s.Join(ctx, inviteHash, name, tok, prof)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	invite := func(hash string, by int64) int64 {
		t.Helper()
		inv, err := s.CreateInvite(ctx, "r", hash, by)
		if err != nil {
			t.Fatal(err)
		}
		return inv.ID
	}
	bea := join("lnkh-r", "Bea", "tok-bea", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	if _, err := s.SetRoomAdmin(ctx, "r", bea.ID, true, &ali); err != nil {
		t.Fatal(err)
	}
	b := invite("inv-b", bea.ID)
	cy := join("inv-b", "Cy", "tok-cy", JoinProfile{})
	c := invite("inv-c", cy.ID)
	pat := joinWithProfile(t, s, "pat-lobby", "Pat", JoinProfile{RecoveryHash: "rec-p", SessionHash: "sess-p"})
	invite("inv-out", 0)
	patOut := join("inv-out", "Pat", "tok-pat-out", JoinProfile{ID: pat.ProfileID})
	patIn := join("inv-c", "Pat", "tok-pat-in", JoinProfile{ID: pat.ProfileID})
	if patOut.ProfileID != patIn.ProfileID {
		t.Fatal("pat's seats have two profiles")
	}

	ids := func(seats []RevokeSeat) []int64 {
		var out []int64
		for _, p := range seats {
			out = append(out, p.ParticipantID)
		}
		return out
	}
	plan, err := s.RevokePreview(ctx, "r", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{a.ID, b, c}; !slices.Equal(plan.Invites, want) {
		t.Errorf("preview invites %v, want %v", plan.Invites, want)
	}
	if want := []int64{bea.ID, cy.ID}; !slices.Equal(ids(plan.Lose), want) || !plan.Lose[0].Admin || plan.Lose[1].Admin {
		t.Errorf("preview lose %+v, want bea (admin) and cy", plan.Lose)
	}
	if want := []int64{patIn.ID}; !slices.Equal(ids(plan.Keep), want) {
		t.Errorf("preview keep %+v, want pat's seat in the chain", plan.Keep)
	}
	if _, err := s.ParticipantByToken(ctx, "tok-bea"); err != nil {
		t.Fatalf("the preview revoked: %v", err)
	}

	eve := join("inv-b", "Eve", "tok-eve", JoinProfile{})
	done, err := s.RevokeInvite(ctx, "r", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(done.Invites, plan.Invites) {
		t.Errorf("revoked %v, previewed %v", done.Invites, plan.Invites)
	}
	if want := []int64{bea.ID, cy.ID, eve.ID}; !slices.Equal(ids(done.Lose), want) || !slices.Equal(ids(done.Keep), ids(plan.Keep)) {
		t.Errorf("revoke lost %+v kept %+v, want bea, cy, eve and pat", done.Lose, done.Keep)
	}
	for _, tok := range []string{"tok-bea", "tok-cy", "tok-eve", "tok-pat-in"} {
		if _, err := s.ParticipantByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s kept its seat: %v", tok, err)
		}
	}
	for _, tok := range []string{"tok-pat-out", "tok-r"} {
		if _, err := s.ParticipantByToken(ctx, tok); err != nil {
			t.Errorf("%s lost its seat: %v", tok, err)
		}
	}
}

// 011-AC19: an invite nobody joined cuts itself and nobody; a person with two seats in the cut
// counts once; unknown invites are ErrNotFound.
func TestRevokePreviewEdges(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	seed(t, s, "a")
	lone, err := s.CreateInvite(ctx, "a", "lone", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.RevokePreview(ctx, "a", lone.ID)
	if err != nil || !slices.Equal(plan.Invites, []int64{lone.ID}) || len(plan.Lose) != 0 || len(plan.Keep) != 0 {
		t.Fatalf("nobody joined: %+v %v", plan, err)
	}
	pat := joinWithProfile(t, s, "lobby", "Pat", JoinProfile{RecoveryHash: "rec-p", SessionHash: "sess-p"})
	for _, tok := range []string{"tok-p1", "tok-p2"} {
		if _, _, err := s.Join(ctx, "lone", "Pat", tok, JoinProfile{ID: pat.ProfileID}); err != nil {
			t.Fatal(err)
		}
	}
	if plan, err = s.RevokePreview(ctx, "a", lone.ID); err != nil || len(plan.Lose) != 1 || len(plan.Keep) != 0 {
		t.Fatalf("two seats of one person: %+v %v", plan, err)
	}
	for _, c := range []struct {
		room string
		id   int64
	}{{"a", 9999}, {"lobby", lone.ID}} {
		if _, err := s.RevokePreview(ctx, c.room, c.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("preview %s/%d: %v, want ErrNotFound", c.room, c.id, err)
		}
		if _, err := s.RevokeInvite(ctx, c.room, c.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoke %s/%d: %v, want ErrNotFound", c.room, c.id, err)
		}
	}
}
