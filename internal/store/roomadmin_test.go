package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// createFor creates room id for the profile of p, joining as name.
func createFor(t *testing.T, s *Store, id string, p *Participant, name string) (*Room, error) {
	t.Helper()
	return s.CreateProfileRoom(t.Context(), id, "Room "+id, p.ProfileID, name, "inv-"+id, "tok-"+id)
}

// 004-AC7 004-AC8: a profile's room has it as a participant and its first room admin, under an
// invite listed as its own; the room counts against the profile's limit, and goes when deleted.
func TestCreateProfileRoom(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	r, err := createFor(t, s, "mine", alice, "Ali")
	if err != nil || r.Name != "Room mine" {
		t.Fatalf("create: %+v %v", r, err)
	}
	ps, err := s.Participants(ctx, "mine")
	if err != nil || len(ps) != 1 || ps[0].Name != "Ali" || ps[0].ProfileID != alice.ProfileID || !ps[0].Admin {
		t.Fatalf("participants %+v %v", ps, err)
	}
	if invs, _ := s.Invites(ctx, "mine", ps[0].ID); len(invs) != 1 {
		t.Fatalf("own invite %+v", invs)
	}
	if uuid, err := s.RoomAdmin(ctx, "mine", alice.ProfileID); err != nil || !uuidRe.MatchString(uuid) {
		t.Fatalf("room admin %q %v", uuid, err)
	}
	if _, err := s.ProfileParticipant(ctx, "sess", "mine"); err != nil {
		t.Fatalf("the profile cookie opens the room: %v", err)
	}
	if ms, _ := s.Messages(ctx, "mine", 0, 10); len(ms) != 1 || ms[0].Body != "Ali joined" {
		t.Fatalf("messages %+v", ms)
	}
	if ps, _ := s.Participants(ctx, "lobby"); ps[0].Admin {
		t.Fatal("admin of a room the profile only joined")
	}
}

// 004-AC7: each guardrail refuses with its reason and creates nothing; the limit counts the rooms
// a profile created and still has, and holds under concurrent requests.
func TestCreateProfileRoomGuardrails(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if v, err := s.InstanceSettings(ctx); err != nil || v != (InstanceSettings{RoomsPerProfile: DefaultRoomsPerProfile, RoomCreation: CreationAnyone}) {
		t.Fatalf("defaults %+v %v", v, err)
	}
	set := func(v InstanceSettings) {
		t.Helper()
		if err := s.SetInstanceSettings(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(id, reason string) {
		t.Helper()
		_, err := createFor(t, s, id, alice, "Alice")
		if !errors.Is(err, ErrCreationRefused) || !strings.Contains(err.Error(), reason) {
			t.Fatalf("%s: %v, want a refusal saying %q", id, err, reason)
		}
		if n := count(t, s, `SELECT count(*) FROM rooms WHERE id = ?`, id); n != 0 {
			t.Fatalf("%s: a refused room was created", id)
		}
	}
	set(InstanceSettings{RoomsPerProfile: 2, RoomCreation: CreationAnyone})
	for _, id := range []string{"r1", "r2"} {
		if _, err := createFor(t, s, id, alice, "Alice"); err != nil {
			t.Fatal(err)
		}
	}
	refused("r3", "you have created 2 rooms, the most this instance allows per profile")
	// Another profile has its own count; a deleted room frees its place.
	bob := joinWithProfile(t, s, "lobby2", "Bob", JoinProfile{RecoveryHash: "rec2", SessionHash: "sess2"})
	if _, err := createFor(t, s, "b1", bob, "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRoom(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := createFor(t, s, "r3", alice, "Alice"); err != nil {
		t.Fatal(err)
	}

	set(InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationSuperAdmin})
	refused("r4", "only the super-admin creates rooms on this instance")
	used, err := s.UsedBytes(ctx)
	if err != nil || used <= 0 {
		t.Fatalf("used %d %v", used, err)
	}
	set(InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used})
	refused("r5", "at its "+mb(used)+" storage quota")
	set(InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used * 4})
	if _, err := createFor(t, s, "r6", alice, "Alice"); err != nil {
		t.Fatal(err)
	}

	// Concurrent creations cannot overshoot the limit: alice has r2, r3 and r6.
	set(InstanceSettings{RoomsPerProfile: 5, RoomCreation: CreationAnyone})
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() { _, _ = createFor(t, s, "c"+strconv.Itoa(i), alice, "Alice") })
	}
	wg.Wait()
	if n := count(t, s, `SELECT count(*) FROM rooms WHERE creator_profile_id = ?`, alice.ProfileID); n != 5 {
		t.Fatalf("rooms after concurrent creations: %d, want 5", n)
	}
}

// 004-AC8: only a room admin appoints and removes; the last one cannot step down; a target must be
// a current participant with a profile. Each change is a chat line.
func TestSetRoomAdmin(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if _, err := createFor(t, s, "r", alice, "Alice"); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "r")
	me := &ps[0]
	join := func(name string, prof JoinProfile) *Participant {
		t.Helper()
		if _, err := s.CreateInvite(ctx, "r", "inv-"+name, 0); err != nil {
			t.Fatal(err)
		}
		p, _, err := s.Join(ctx, "inv-"+name, name, "tok-"+name, prof)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	bob := join("Bob", JoinProfile{RecoveryHash: "rb", SessionHash: "sb"})
	guest := join("Guest", JoinProfile{})
	other := joinWithProfile(t, s, "elsewhere", "Olga", JoinProfile{RecoveryHash: "ro", SessionHash: "so"})

	for name, c := range map[string]struct {
		target int64
		on     bool
		by     *Participant
		want   error
	}{
		"plain member appoints":  {bob.ID, true, bob, ErrForbidden},
		"plain member removes":   {me.ID, false, bob, ErrForbidden},
		"last admin steps down":  {me.ID, false, me, ErrLastAdmin},
		"no profile":             {guest.ID, true, me, ErrNoProfile},
		"not in the room":        {other.ID, true, me, ErrNotFound},
		"unknown participant":    {999, true, me, ErrNotFound},
		"by someone without one": {bob.ID, true, guest, ErrForbidden},
	} {
		if _, err := s.SetRoomAdmin(ctx, "r", c.target, c.on, c.by); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	m, err := s.SetRoomAdmin(ctx, "r", bob.ID, true, me)
	if err != nil || m == nil || m.Body != "Alice made Bob a room admin." {
		t.Fatalf("appoint: %+v %v", m, err)
	}
	if m, err := s.SetRoomAdmin(ctx, "r", bob.ID, true, me); err != nil || m != nil {
		t.Fatalf("appoint again: %+v %v", m, err)
	}
	// Two admins now: either may step down, and the other stays.
	if m, err := s.SetRoomAdmin(ctx, "r", me.ID, false, me); err != nil || m.Body != "Alice stepped down as a room admin." {
		t.Fatalf("step down: %+v %v", m, err)
	}
	if _, err := s.SetRoomAdmin(ctx, "r", bob.ID, false, bob); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("bob, now the last: %v", err)
	}
	if _, err := s.SetRoomAdmin(ctx, "r", me.ID, true, me); !errors.Is(err, ErrForbidden) {
		t.Fatalf("former admin appoints: %v", err)
	}
	if m, err := s.SetRoomAdmin(ctx, "r", me.ID, true, bob); err != nil || m.Body != "Bob made Alice a room admin." {
		t.Fatalf("bob appoints alice: %+v %v", m, err)
	}
	if m, err := s.SetRoomAdmin(ctx, "r", bob.ID, false, me); err != nil || m.Body != "Alice removed Bob from the room admins." {
		t.Fatalf("remove bob: %+v %v", m, err)
	}
	// A revoked participant is no longer in the room.
	invs, _ := s.Invites(ctx, "r", 0)
	for _, inv := range invs {
		if inv.ID != 0 && inv.CreatedBy == 0 {
			if _, err := s.RevokeInvite(ctx, "r", inv.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.SetRoomAdmin(ctx, "r", bob.ID, true, me); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked bob: %v", err)
	}
}

// 004-AC8: a deleted profile loses its admin rights with it; the rooms it created stay, without a
// creator.
func TestDeleteProfileDropsRoomAdmin(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if _, err := createFor(t, s, "r", alice, "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(ctx, alice.ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM room_admins`); n != 0 {
		t.Fatalf("room admins left: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM rooms WHERE id = 'r' AND creator_profile_id IS NULL`); n != 1 {
		t.Fatal("room lost or still tied to the deleted profile")
	}
}

// Every room admin and settings call reports a failed database.
func TestRoomAdminCallsFailWithTheStore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.Close()
	p := &Participant{ID: 1, ProfileID: 1}
	for name, call := range map[string]func() error{
		"settings":     func() error { _, err := s.InstanceSettings(ctx); return err },
		"set settings": func() error { return s.SetInstanceSettings(ctx, InstanceSettings{}) },
		"used":         func() error { _, err := s.UsedBytes(ctx); return err },
		"create":       func() error { _, err := createFor(t, s, "x", p, "A"); return err },
		"room admin":   func() error { _, err := s.RoomAdmin(ctx, "x", 1); return err },
		"set admin":    func() error { _, err := s.SetRoomAdmin(ctx, "x", 1, true, p); return err },
	} {
		if err := call(); err == nil || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
