package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/skill"
)

// skillUpload is a parsed skill named name whose files say body; another body is another version.
func skillUpload(name, body string) *skill.Skill {
	return &skill.Skill{Name: name, Description: "does " + name, Files: 1, Size: int64(len(body)), SHA256: name + ":" + body, Tar: []byte(body)}
}

func publish(t *testing.T, s *Store, profileID int64, shared bool, name, body string) *Skill {
	t.Helper()
	k, err := s.PublishSkill(context.Background(), profileID, shared, skillUpload(name, body))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func names(ks []Skill) []string {
	var out []string
	for _, k := range ks {
		out = append(out, k.Name)
	}
	return out
}

// 002-AC29 002-AC30: uploads become immutable versions signed by their author; a name is a new
// version only for its owner or publisher; the library a member sees is the shared skills and
// their own personal ones.
func TestPublishSkill(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	bob := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})

	k := publish(t, s, alice.ProfileID, false, "stamp", "v1")
	if k.Version != 1 || k.Shared || !k.Mine || k.Author != "Alice" || k.Description != "does stamp" {
		t.Fatalf("first upload: %+v", k)
	}
	if _, err := s.PublishSkill(ctx, alice.ProfileID, false, skillUpload("stamp", "v1")); !errors.Is(err, ErrSameVersion) {
		t.Fatalf("same files again: %v", err)
	}
	if k = publish(t, s, alice.ProfileID, false, "stamp", "v2"); k.Version != 2 {
		t.Fatalf("second version: %+v", k)
	}
	// Bob may use the same name for his own personal skill, and publish a shared one.
	publish(t, s, bob.ProfileID, false, "stamp", "bob")
	shared := publish(t, s, bob.ProfileID, true, "pdf", "s1")
	if !shared.Shared || !shared.Mine {
		t.Fatalf("shared: %+v", shared)
	}
	if _, err := s.PublishSkill(ctx, alice.ProfileID, true, skillUpload("pdf", "mine now")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a version of someone else's shared skill: %v", err)
	}
	var v1 []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM skill_versions WHERE skill_id = ? AND n = 1`, k.ID).Scan(&v1); err != nil || string(v1) != "v1" {
		t.Fatalf("version 1 changed: %q %v", v1, err)
	}

	lib, err := s.Skills(ctx, alice.ProfileID, "", SkillsLibrary)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(lib); !slices.Equal(got, []string{"pdf", "stamp"}) || lib[1].ProfileID != alice.ProfileID || lib[0].Mine || !lib[1].Mine {
		t.Fatalf("Alice's library: %+v", lib)
	}
	mine, err := s.Skills(ctx, bob.ProfileID, "", SkillsMine)
	if err != nil || !slices.Equal(names(mine), []string{"pdf", "stamp"}) || mine[1].ProfileID != bob.ProfileID {
		t.Fatalf("Bob's skills: %+v %v", mine, err)
	}
	all, err := s.Skills(ctx, 0, "", SkillsAll)
	if err != nil || len(all) != 3 || slices.ContainsFunc(all, func(k Skill) bool { return k.Mine }) {
		t.Fatalf("admin list: %+v %v", all, err)
	}
	if _, err := s.Skills(ctx, 0, "", "everything"); err == nil {
		t.Fatal("unknown listing accepted")
	}
}

func TestPublishSkillLimits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	for i := range MaxProfileSkills {
		publish(t, s, a.ProfileID, i%2 == 0, "s"+strings.Repeat("x", i), "v")
	}
	if _, err := s.PublishSkill(ctx, a.ProfileID, false, skillUpload("one-more", "v")); !errors.Is(err, ErrLimit) {
		t.Fatalf("past the skills limit: %v", err)
	}
	for i := 2; i <= MaxSkillVersions; i++ {
		publish(t, s, a.ProfileID, false, "sx", strings.Repeat("v", i))
	}
	if _, err := s.PublishSkill(ctx, a.ProfileID, false, skillUpload("sx", "last")); !errors.Is(err, ErrLimit) {
		t.Fatalf("past the versions limit: %v", err)
	}
}

// skillRoom is room a with Alice (profile), Bob (profile) and Carol (none), Alice's personal
// skill "stamp" and Bob's shared "pdf".
type skillRoom struct {
	s                  *Store
	alice, bob, carol  *Participant
	grill              RoomPersona
	stamp, pdf, bobOwn *Skill
}

func newSkillRoom(t *testing.T) *skillRoom {
	t.Helper()
	s := openTest(t)
	ctx := context.Background()
	r := &skillRoom{s: s, alice: joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})}
	var err error
	if r.bob, _, err = s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"}); err != nil {
		t.Fatal(err)
	}
	if r.carol, _, err = s.Join(ctx, "inv-a", "Carol", "tok-carol", JoinProfile{}); err != nil {
		t.Fatal(err)
	}
	ps, err := s.RoomPersonas(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	r.grill = ps[0]
	r.stamp = publish(t, s, r.alice.ProfileID, false, "stamp", "stamp v1")
	r.pdf = publish(t, s, r.bob.ProfileID, true, "pdf", "pdf v1")
	r.bobOwn = publish(t, s, r.bob.ProfileID, false, "bobs", "bobs v1")
	return r
}

func (r *skillRoom) set(skillID int64, on, accept bool, by *Participant) (*RoomPersona, *Message, bool, error) {
	return r.s.SetPersonaSkill(context.Background(), "a", r.grill.ID, skillID, on, accept, by)
}

// 002-AC31 002-AC32 002-AC30 002-AC15: activating a skill needs a profile and, the first time,
// the disclaimer; a member activates the shared library and their own personal skills only; each
// change is a chat line; two skills of one name cannot both be on.
func TestSetPersonaSkill(t *testing.T) {
	r := newSkillRoom(t)
	ctx := context.Background()
	if _, _, _, err := r.set(r.pdf.ID, true, true, r.carol); !errors.Is(err, ErrForbidden) {
		t.Fatalf("without a profile: %v", err)
	}
	if _, _, _, err := r.set(r.stamp.ID, true, false, r.alice); !errors.Is(err, ErrDisclaimer) {
		t.Fatalf("first activation without the disclaimer: %v", err)
	}
	if ok, err := r.s.SkillsDisclaimerAccepted(ctx, r.alice.ProfileID); err != nil || ok {
		t.Fatalf("a refused activation recorded the disclaimer: %v %v", ok, err)
	}
	p, m, held, err := r.set(r.stamp.ID, true, true, r.alice)
	if err != nil || held || m == nil || m.Body != "Alice changed 🔥 Grill: skill stamp v1 on." {
		t.Fatalf("activate: %+v %+v %v %v", p, m, held, err)
	}
	if len(p.Skills) != 1 || p.Skills[0] != (PersonaSkill{SkillID: r.stamp.ID, Name: "stamp", Scope: ScopePersonal, Version: 1}) {
		t.Fatalf("skills: %+v", p.Skills)
	}
	if ok, err := r.s.SkillsDisclaimerAccepted(ctx, r.alice.ProfileID); err != nil || !ok {
		t.Fatalf("disclaimer not recorded: %v %v", ok, err)
	}
	// Once per profile: Alice's next activation needs no acceptance.
	if _, _, _, err := r.set(r.pdf.ID, true, false, r.alice); err != nil {
		t.Fatalf("second activation: %v", err)
	}
	if _, _, _, err := r.set(r.bobOwn.ID, true, true, r.alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's personal skill: %v", err)
	}
	// Bob's own personal "stamp" clashes with Alice's on the same persona.
	bobStamp := publish(t, r.s, r.bob.ProfileID, false, "stamp", "bob stamp")
	if _, _, _, err := r.s.SetPersonaSkill(ctx, "a", r.grill.ID, bobStamp.ID, true, true, r.bob); !errors.Is(err, ErrSkillName) {
		t.Fatalf("name clash: %v", err)
	}
	if _, m, _, err := r.set(r.pdf.ID, true, false, r.alice); err != nil || m != nil {
		t.Fatalf("activating an active skill again changes nothing: %+v %v", m, err)
	}
	// Anyone may turn a skill off, Carol included.
	p, m, _, err = r.set(r.stamp.ID, false, false, r.carol)
	if err != nil || m.Body != "Carol changed 🔥 Grill: skill stamp off." || len(p.Skills) != 1 || p.Skills[0].Name != "pdf" {
		t.Fatalf("off: %+v %+v %v", p, m, err)
	}
	if _, m, _, err := r.set(r.stamp.ID, false, false, r.carol); err != nil || m != nil {
		t.Fatalf("off again: %+v %v", m, err)
	}
	if _, _, _, err := r.set(9999, true, true, r.alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown skill: %v", err)
	}
	if _, _, _, err := r.s.SetPersonaSkill(ctx, "a", 9999, r.pdf.ID, true, true, r.alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown persona: %v", err)
	}
}

func TestPersonaSkillsLimit(t *testing.T) {
	r := newSkillRoom(t)
	for i := range MaxPersonaSkills {
		k := publish(t, r.s, r.alice.ProfileID, false, "k"+strings.Repeat("k", i), "v")
		if _, _, _, err := r.set(k.ID, true, true, r.alice); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := r.set(r.pdf.ID, true, true, r.alice); !errors.Is(err, ErrLimit) {
		t.Fatalf("past the limit: %v", err)
	}
}

// 002-AC31: a pin holds across a library update, which is offered and applied only when asked; a
// turn loads the versions pinned when it started; a personal skill rides only while its owner is
// in the room.
func TestSkillPinning(t *testing.T) {
	r := newSkillRoom(t)
	ctx := context.Background()
	for _, k := range []*Skill{r.stamp, r.pdf} {
		if _, _, _, err := r.set(k.ID, true, true, r.alice); err != nil {
			t.Fatal(err)
		}
	}
	publish(t, r.s, r.alice.ProfileID, false, "stamp", "stamp v2")
	p, err := r.s.RoomPersona(ctx, "a", r.grill.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Skills[1] != (PersonaSkill{SkillID: r.stamp.ID, Name: "stamp", Scope: ScopePersonal, Version: 1, UpdateVersion: 2}) {
		t.Fatalf("after the library update: %+v", p.Skills)
	}
	turnSkills := func() []TurnSkill {
		t.Helper()
		tn := &Turn{RoomID: "a", ParticipantID: r.alice.ID, Prompt: "go", PersonaID: r.grill.ID, PersonaName: "Grill"}
		if err := r.s.StartTurn(ctx, tn, false); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.FailTurn(ctx, "a", tn.ID, "test"); err != nil {
			t.Fatal(err)
		}
		ks, err := r.s.TurnSkills(ctx, "a", tn.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		lean, err := r.s.TurnSkills(ctx, "a", tn.ID, false)
		if err != nil || len(lean) != len(ks) || (len(lean) > 0 && len(lean[0].Data) != 0) {
			t.Fatalf("without data: %+v %v", lean, err)
		}
		return ks
	}
	if ks := turnSkills(); len(ks) != 2 || ks[1].Name != "stamp" || ks[1].Version != 1 || string(ks[1].Data) != "stamp v1" || string(ks[0].Data) != "pdf v1" {
		t.Fatalf("turn loads: %+v", ks)
	}
	_, m, _, err := r.set(r.stamp.ID, true, false, r.alice)
	if err != nil || m.Body != "Alice changed 🔥 Grill: skill stamp v1 → v2." {
		t.Fatalf("update: %+v %v", m, err)
	}
	if ks := turnSkills(); string(ks[1].Data) != "stamp v2" {
		t.Fatalf("after the update: %+v", ks)
	}
	// Alice cut off: her personal skill stays pinned but no longer reaches the sandbox.
	if _, err := r.s.RevokeInvite(ctx, "a", r.alice.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.CreateInvite(ctx, "a", "inv-2", 0); err != nil {
		t.Fatal(err)
	}
	dave, _, err := r.s.Join(ctx, "inv-2", "Dave", "tok-dave", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	r.alice = dave
	if ks := turnSkills(); len(ks) != 1 || ks[0].Name != "pdf" {
		t.Fatalf("owner gone: %+v", ks)
	}
}

// 002-AC21: while the host approves everything, a member's skill change waits for the host, who
// sees it in the request's summary; approving applies the pinned version.
func TestSkillChangeHeld(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "a")
	seedHeld(t, s, "a")
	bob, _, err := s.Join(ctx, "inv-a", "Dan", "tok-dan", JoinProfile{RecoveryHash: "rec-dan", SessionHash: "sess-dan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishSkill(ctx, 0, true, skillUpload("anon", "v1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("publishing without a profile: %v", err)
	}
	k := publish(t, s, bob.ProfileID, true, "pdf", "v1")
	ps, err := s.RoomPersonas(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	p, m, held, err := s.SetPersonaSkill(ctx, "a", ps[0].ID, k.ID, true, true, bob)
	if err != nil || !held || len(p.Skills) != 0 || m.Body != "Dan asked to change 🔥 Grill: skill pdf v1 on."+waitsNote {
		t.Fatalf("held: %+v %+v %v %v", p, m, held, err)
	}
	// A second request adds to the held one: the host decides on both skills at once.
	k2 := publish(t, s, bob.ProfileID, true, "docx", "v1")
	if _, _, held, err := s.SetPersonaSkill(ctx, "a", ps[0].ID, k2.ID, true, false, bob); err != nil || !held {
		t.Fatalf("second held: %v %v", held, err)
	}
	changes, err := s.PersonaChanges(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(changes, func(c PersonaChange) bool { return c.PersonaID == ps[0].ID })
	if i < 0 || changes[i].Summary != "skill docx v1 on, skill pdf v1 on" {
		t.Fatalf("changes: %+v", changes)
	}
	host, err := s.RoomParticipant(ctx, "a", mustHost(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideChange(ctx, "a", changes[i].ID, true, host); err != nil {
		t.Fatal(err)
	}
	if p, err = s.RoomPersona(ctx, "a", ps[0].ID); err != nil || len(p.Skills) != 2 || p.Skills[1].Name != "pdf" || p.Skills[1].Version != 1 {
		t.Fatalf("approved: %+v %v", p, err)
	}
}

func mustHost(t *testing.T, s *Store) int64 {
	t.Helper()
	r, err := s.Room(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	return r.HostParticipantID
}

// 002-AC29 002-AC30: a profile removes its personal skills, never a shared one; the admin removes
// any; personas that had a removed skill lose it.
func TestRemoveSkill(t *testing.T) {
	r := newSkillRoom(t)
	ctx := context.Background()
	if _, _, _, err := r.set(r.pdf.ID, true, true, r.alice); err != nil {
		t.Fatal(err)
	}
	if err := r.s.RemoveSkill(ctx, r.pdf.ID, r.bob.ProfileID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publisher removing a shared skill: %v", err)
	}
	if err := r.s.RemoveSkill(ctx, r.stamp.ID, r.bob.ProfileID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's personal skill: %v", err)
	}
	if err := r.s.RemoveSkill(ctx, r.stamp.ID, r.alice.ProfileID); err != nil {
		t.Fatal(err)
	}
	if err := r.s.RemoveSkill(ctx, r.pdf.ID, 0); err != nil {
		t.Fatal(err)
	}
	if p, err := r.s.RoomPersona(ctx, "a", r.grill.ID); err != nil || len(p.Skills) != 0 {
		t.Fatalf("removed skill still active: %+v %v", p, err)
	}
	if n := count(t, r.s, `SELECT count(*) FROM skill_versions WHERE skill_id IN (?, ?)`, r.stamp.ID, r.pdf.ID); n != 0 {
		t.Fatalf("%d versions left", n)
	}
}

// 002-AC30 002-AC29: deleting a profile purges its personal skills and their activations; a shared
// skill it published stays in the library, its publisher and authors detached.
func TestDeleteProfileSkills(t *testing.T) {
	r := newSkillRoom(t)
	ctx := context.Background()
	for _, k := range []*Skill{r.pdf, r.bobOwn} {
		if _, _, _, err := r.s.SetPersonaSkill(ctx, "a", r.grill.ID, k.ID, true, true, r.bob); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.s.DeleteProfile(ctx, r.bob.ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, r.s, `SELECT count(*) FROM skills WHERE id = ?`, r.bobOwn.ID); n != 0 {
		t.Fatal("personal skill survived its profile")
	}
	if n := count(t, r.s, `SELECT count(*) FROM skills WHERE id = ? AND publisher_id IS NULL`, r.pdf.ID); n != 1 {
		t.Fatal("shared skill gone or still attached")
	}
	if n := count(t, r.s, `SELECT count(*) FROM skill_versions WHERE author_id IS NOT NULL AND author_id = ?`, r.bob.ProfileID); n != 0 {
		t.Fatal("versions still point at the deleted profile")
	}
	p, err := r.s.RoomPersona(ctx, "a", r.grill.ID)
	if err != nil || len(p.Skills) != 1 || p.Skills[0].Name != "pdf" {
		t.Fatalf("persona skills: %+v %v", p, err)
	}
}

// 004-AC10: a member copies a personal skill into the room, frozen at its latest version; every
// member may turn the copy on; it keeps reaching the sandbox after its author leaves, unlike the
// personal skill it came from; a room admin removes it.
func TestRoomSkill(t *testing.T) {
	r := newSkillRoom(t)
	ctx := context.Background()
	publish(t, r.s, r.bob.ProfileID, false, "bobs", "bobs v2")
	k, m, err := r.s.CopySkillToRoom(ctx, "a", r.bobOwn.ID, r.bob)
	if err != nil || k.Scope != ScopeRoom || k.Shared || k.Pending || k.Version != 2 || k.Author != "Bob" || k.Mine {
		t.Fatalf("copy: %+v %v", k, err)
	}
	if m.Body != "Bob copied the skill bobs v2 into the room." || m.Kind != KindSystem {
		t.Fatalf("chat line: %+v", m)
	}
	for name, c := range map[string]struct {
		skill int64
		by    *Participant
		want  error
	}{
		"the same name again":         {r.bobOwn.ID, r.bob, ErrRoomSkillName},
		"someone else's personal one": {r.stamp.ID, r.bob, ErrNotFound},
		"a shared one":                {r.pdf.ID, r.bob, ErrNotFound},
		"the room skill itself":       {k.ID, r.bob, ErrNotFound},
		"without a profile":           {r.stamp.ID, r.carol, ErrForbidden},
	} {
		if _, _, err := r.s.CopySkillToRoom(ctx, "a", c.skill, c.by); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// Alice sees it in the room's library, not in another room's, nor among her own skills.
	lib, err := r.s.Skills(ctx, r.alice.ProfileID, "a", SkillsLibrary)
	if err != nil || !slices.Equal(names(lib), []string{"bobs", "pdf", "stamp"}) {
		t.Fatalf("library in a: %+v %v", lib, err)
	}
	if lib, _ := r.s.Skills(ctx, r.alice.ProfileID, "elsewhere", SkillsLibrary); slices.Contains(names(lib), "bobs") {
		t.Fatal("a room skill offered in another room")
	}
	if mine, _ := r.s.Skills(ctx, r.bob.ProfileID, "", SkillsMine); len(mine) != 2 {
		t.Fatalf("a room skill listed among its author's: %+v", mine)
	}
	p, _, _, err := r.set(k.ID, true, true, r.alice)
	if err != nil || len(p.Skills) != 1 || p.Skills[0] != (PersonaSkill{SkillID: k.ID, Name: "bobs", Scope: ScopeRoom, Version: 2}) {
		t.Fatalf("Alice turns it on: %+v %v", p, err)
	}
	if _, _, _, err := r.set(r.bobOwn.ID, true, true, r.bob); !errors.Is(err, ErrSkillName) {
		t.Fatalf("the personal source beside its copy: %v", err)
	}
	if _, _, _, err := r.s.SetPersonaSkill(ctx, "elsewhere", r.grill.ID, k.ID, true, true, r.alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another room's persona: %v", err)
	}
	if v, err := r.s.RoomOverview(ctx, "a", 1<<20); err != nil || v.Volume.RoomSkills != int64(len("bobs v2")) {
		t.Fatalf("volume: %+v %v", v, err)
	}

	turnSkills := func() []string {
		t.Helper()
		tn := &Turn{RoomID: "a", ParticipantID: r.alice.ID, Prompt: "go", PersonaID: r.grill.ID, PersonaName: "Grill"}
		if err := r.s.StartTurn(ctx, tn, false); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.FailTurn(ctx, "a", tn.ID, "test"); err != nil {
			t.Fatal(err)
		}
		ks, err := r.s.TurnSkills(ctx, "a", tn.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, k := range ks {
			out = append(out, k.Name+"="+string(k.Data))
		}
		return out
	}
	if got := turnSkills(); !slices.Equal(got, []string{"bobs=bobs v2"}) {
		t.Fatalf("turn loads: %v", got)
	}
	// Bob leaves and removes his personal skill: the room's copy still rides.
	if _, err := r.s.RevokeInvite(ctx, "a", r.bob.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if err := r.s.RemoveSkill(ctx, r.bobOwn.ID, r.bob.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.CreateInvite(ctx, "a", "inv-2", 0); err != nil {
		t.Fatal(err)
	}
	if r.alice, _, err = r.s.Join(ctx, "inv-2", "Alice", "tok-alice-2", JoinProfile{ID: r.alice.ProfileID}); err != nil {
		t.Fatal(err)
	}
	if got := turnSkills(); !slices.Equal(got, []string{"bobs=bobs v2"}) {
		t.Fatalf("after its author left: %v", got)
	}

	if _, err := r.s.RemoveRoomSkill(ctx, "elsewhere", k.ID, r.alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove from another room: %v", err)
	}
	m, err = r.s.RemoveRoomSkill(ctx, "a", k.ID, r.alice)
	if err != nil || m.Body != "Alice removed the room skill bobs." {
		t.Fatalf("remove: %+v %v", m, err)
	}
	if p, _ := r.s.RoomPersona(ctx, "a", r.grill.ID); len(p.Skills) != 0 {
		t.Fatalf("persona kept a removed room skill: %+v", p.Skills)
	}
	if got := turnSkills(); len(got) != 0 {
		t.Fatalf("turn after the removal: %v", got)
	}
}

// 004-AC10: while the host approves everything, a member's copy waits for the host and cannot be
// turned on; the host approves or refuses it, and the chat says so. The host's own copy goes
// through.
func TestRoomSkillHeld(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	host := seed(t, s, "a")
	seedHeld(t, s, "a")
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-dan", JoinProfile{RecoveryHash: "rec-dan", SessionHash: "sess-dan"})
	if err != nil {
		t.Fatal(err)
	}
	k1 := publish(t, s, bob.ProfileID, false, "one", "v1")
	k2 := publish(t, s, bob.ProfileID, false, "two", "v1")
	held, m, err := s.CopySkillToRoom(ctx, "a", k1.ID, bob)
	if err != nil || !held.Pending || m.Body != "Bob copied the skill one v1 into the room."+waitsNote {
		t.Fatalf("held copy: %+v %+v %v", held, m, err)
	}
	refused, _, err := s.CopySkillToRoom(ctx, "a", k2.ID, bob)
	if err != nil || !refused.Pending {
		t.Fatalf("second copy: %+v %v", refused, err)
	}
	if lib, _ := s.Skills(ctx, bob.ProfileID, "a", SkillsLibrary); slices.Contains(names(lib), "one") && slices.ContainsFunc(lib, func(k Skill) bool { return k.Scope == ScopeRoom }) {
		t.Fatalf("a pending copy offered: %+v", lib)
	}
	ps, _ := s.RoomPersonas(ctx, "a")
	if _, _, _, err := s.SetPersonaSkill(ctx, "a", ps[0].ID, held.ID, true, true, host); !errors.Is(err, ErrNotFound) {
		t.Fatalf("turning on a pending copy: %v", err)
	}
	if _, err := s.DecideRoomSkill(ctx, "a", held.ID, true, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("decided by a member: %v", err)
	}
	if m, err := s.DecideRoomSkill(ctx, "a", held.ID, true, host); err != nil || m.Body != "Alice approved Bob's copy of the skill one v1." {
		t.Fatalf("approve: %+v %v", m, err)
	}
	if _, err := s.DecideRoomSkill(ctx, "a", held.ID, true, host); !errors.Is(err, ErrNotFound) {
		t.Fatalf("decided twice: %v", err)
	}
	if m, err := s.DecideRoomSkill(ctx, "a", refused.ID, false, host); err != nil || m.Body != "Alice refused Bob's copy of the skill two v1: it is removed." {
		t.Fatalf("refuse: %+v %v", m, err)
	}
	ks, err := s.RoomSkills(ctx, "a")
	if err != nil || len(ks) != 1 || ks[0].Name != "one" || ks[0].Pending {
		t.Fatalf("room skills: %+v %v", ks, err)
	}
	if _, _, _, err := s.SetPersonaSkill(ctx, "a", ps[0].ID, held.ID, true, true, host); err != nil {
		t.Fatalf("turning on the approved copy: %v", err)
	}
	mine := publish(t, s, host.ProfileID, false, "hosts", "v1")
	if k, _, err := s.CopySkillToRoom(ctx, "a", mine.ID, host); err != nil || k.Pending {
		t.Fatalf("the host's own copy: %+v %v", k, err)
	}
}

func TestRoomSkillLimit(t *testing.T) {
	r := newSkillRoom(t)
	ctx := context.Background()
	for i := range MaxRoomSkills {
		k := publish(t, r.s, r.alice.ProfileID, false, "s"+strings.Repeat("x", i), "v")
		if _, _, err := r.s.CopySkillToRoom(ctx, "a", k.ID, r.alice); err != nil {
			t.Fatal(err)
		}
		if err := r.s.RemoveSkill(ctx, k.ID, r.alice.ProfileID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.s.CopySkillToRoom(ctx, "a", r.stamp.ID, r.alice); !errors.Is(err, ErrLimit) {
		t.Fatalf("past the room's limit: %v", err)
	}
}
