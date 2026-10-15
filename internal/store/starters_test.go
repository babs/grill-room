package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testManifest(title string) *StarterManifest {
	return &StarterManifest{Title: title, Description: "d", RoomName: "R", Agenda: "- [ ] one",
		Personas:  []StarterPersona{{PersonaFields: PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#112233", Text: "t", Model: "sonnet", Effort: "high"}, EveryVoice: 5}},
		Documents: []StarterDocument{{Name: "a.pdf", Kind: DocUpload, Format: "pdf", ContentType: "application/pdf", File: "doc/0", Size: 3}},
		Skills:    []StarterSkill{}, Fields: []StarterField{}, Chain: StarterChain{Budget: 25}}
}

func titles(ss []Starter) string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Library+":"+s.Title)
	}
	return strings.Join(out, ",")
}

// 006-AC1 006-AC3 006-AC6: a personal starter is its owner's alone, each save is an immutable
// version signed by its author with the full copy and its blobs; publishing moves it to everyone.
func TestStarterLibraries(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	bob := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	files := map[string][]byte{"doc/0": []byte("pdf")}
	st, err := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Mine"), files)
	if err != nil || st.Version != 1 || st.Author != "Alice" || st.Library != LibraryPersonal || !st.Mine || st.Size == 0 {
		t.Fatalf("save: %+v %v", st, err)
	}
	if ls, _ := s.Starters(ctx, bob.ProfileID); titles(ls) != "builtin:Spec grill,builtin:Retro,builtin:Doc review" {
		t.Fatalf("bob sees %s", titles(ls))
	}
	if _, _, err := s.StarterVersion(ctx, st.ID, 0, bob.ProfileID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob reads alice's personal starter: %v", err)
	}
	if _, err := s.SaveStarter(ctx, bob.ProfileID, st.ID, testManifest("Stolen"), nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob adds a version: %v", err)
	}
	// The profile's chosen name signs the next version; v1 keeps its own author and title.
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET name = 'Alicia' WHERE id = ?`, alice.ProfileID); err != nil {
		t.Fatal(err)
	}
	m2 := testManifest("Mine v2")
	if st, err = s.SaveStarter(ctx, alice.ProfileID, st.ID, m2, nil); err != nil || st.Version != 2 || st.Versions != 2 || st.Title != "Mine v2" {
		t.Fatalf("v2: %+v %v", st, err)
	}
	if st.Author != "Alicia" {
		t.Fatalf("v2 author %q", st.Author)
	}
	sv1, v1, err := s.StarterVersion(ctx, st.ID, 1, alice.ProfileID)
	if err != nil || v1.Title != "Mine" || v1.Personas[0].EveryVoice != 5 || v1.Personas[0].Effort != "high" ||
		sv1.Version != 1 || sv1.Author != "Alice" || sv1.Title != "Mine" || sv1.Versions != 2 {
		t.Fatalf("v1 unchanged: %+v %+v %v", sv1, v1, err)
	}
	if f, _ := s.StarterFiles(ctx, st.ID, 1); string(f["doc/0"]) != "pdf" {
		t.Fatalf("v1 files %v", f)
	}
	if f, _ := s.StarterFiles(ctx, st.ID, 2); len(f) != 0 {
		t.Fatalf("v2 files %v", f)
	}
	if err := s.PublishStarter(ctx, bob.ProfileID, st.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob publishes: %v", err)
	}
	if err := s.PublishStarter(ctx, alice.ProfileID, st.ID); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.Starters(ctx, bob.ProfileID); !strings.HasSuffix(titles(ls), ",instance:Mine v2") || ls[len(ls)-1].Versions != 2 || ls[len(ls)-1].Mine {
		t.Fatalf("after publish bob sees %s", titles(ls))
	}
	if err := s.PublishStarter(ctx, alice.ProfileID, st.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("publish twice: %v", err)
	}
	// Anyone duplicates what they see into their own library, blobs included.
	dup, err := s.DuplicateStarter(ctx, bob.ProfileID, st.ID)
	if err != nil || dup.Library != LibraryPersonal || dup.Author != "Bob" || dup.Version != 1 || !dup.Mine {
		t.Fatalf("duplicate: %+v %v", dup, err)
	}
	if err := s.DeleteStarter(ctx, bob.ProfileID, st.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob deletes alice's instance starter: %v", err)
	}
	if err := s.DeleteStarter(ctx, 0, dup.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the super-admin deletes a personal starter: %v", err)
	}
	if err := s.DeleteStarter(ctx, 0, st.ID); err != nil {
		t.Fatalf("the super-admin deletes an instance starter: %v", err)
	}
	if err := s.DeleteStarter(ctx, bob.ProfileID, dup.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM starter_files`); n != 0 {
		t.Fatalf("%d files left", n)
	}
}

// 006-AC2: built-ins ship read-only, with no owner; anyone duplicates one; a changed built-in gets
// a new version at start, and the copies keep theirs.
func TestBuiltinStarters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	ls, err := s.Starters(ctx, 0)
	if err != nil || len(ls) != 3 || ls[0].Builtin != "spec-grill" || ls[0].Version != 1 || ls[0].Author != presetAuthor {
		t.Fatalf("built-ins: %+v %v", ls, err)
	}
	grill := ls[0].ID
	for name, err := range map[string]error{
		"version": func() error { _, err := s.SaveStarter(ctx, alice.ProfileID, grill, testManifest("x"), nil); return err }(),
		"publish": s.PublishStarter(ctx, alice.ProfileID, grill),
		"delete":  s.DeleteStarter(ctx, 0, grill),
	} {
		if !errors.Is(err, ErrBuiltin) {
			t.Errorf("%s a built-in: %v", name, err)
		}
	}
	dup, err := s.DuplicateStarter(ctx, alice.ProfileID, grill)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	old := builtinStarters[0].m.Agenda
	t.Cleanup(func() { builtinStarters[0].m.Agenda = old })
	builtinStarters[0].m.Agenda = old + "\n- [ ] Next steps"
	for range 2 {
		if s, err = Open(path); err != nil {
			t.Fatal(err)
		}
		st, m, err := s.StarterVersion(ctx, grill, 0, 0)
		if err != nil || st.Version != 2 || m.Agenda != builtinStarters[0].m.Agenda {
			t.Fatalf("after a change: %+v %v", st, err)
		}
		if _, m, _ := s.StarterVersion(ctx, dup.ID, 0, alice.ProfileID); m.Agenda != old {
			t.Fatalf("the copy changed: %q", m.Agenda)
		}
		_ = s.Close()
	}
}

// 006-AC7: the built-ins as decided: their personas, models and triggers, no opus, no skills.
func TestBuiltinStarterContent(t *testing.T) {
	want := map[string]string{
		"spec-grill": "Grill sonnet/high on demand, Secretary sonnet/medium every 20 voice lines",
		"retro":      "Facilitator sonnet/medium on demand, Secretary sonnet/medium every 20 voice lines",
		"doc-review": "Reviewer sonnet/high on demand, Secretary sonnet/medium every 20 voice lines",
	}
	for _, b := range builtinStarters {
		var got []string
		for _, p := range b.m.Personas {
			trig := "on demand"
			if p.EveryVoice > 0 {
				trig = "every 20 voice lines"
			}
			got = append(got, p.Name+" "+p.Model+"/"+p.Effort+" "+trig)
			if strings.Contains(p.Model, "opus") || len(p.Skills) > 0 || !strings.Contains(p.Text, "language the room speaks") {
				t.Errorf("%s %s: opus, skills or no room language", b.key, p.Name)
			}
		}
		if g := strings.Join(got, ", "); g != want[b.key] {
			t.Errorf("%s: %s", b.key, g)
		}
		if len(b.m.Skills) > 0 || !strings.Contains(b.m.Agenda, "- [ ]") {
			t.Errorf("%s: skills or no agenda items", b.key)
		}
	}
}

// 006-AC4: a save past the instance storage quota or the profile's limits is refused with the
// reason, and writes nothing.
func TestStarterLimits(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	used, _ := s.UsedBytes(ctx)
	if err := putSettings(ctx, s, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used + 64<<10}); err != nil {
		t.Fatal(err)
	}
	big := map[string][]byte{"doc/0": make([]byte, 128<<10)}
	if _, err := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Big"), big); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "storage quota") {
		t.Fatalf("past the quota: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM starters WHERE profile_id = ?`, alice.ProfileID); n != 0 {
		t.Fatalf("%d starters written", n)
	}
	if err := putSettings(ctx, s, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone}); err != nil {
		t.Fatal(err)
	}
	var first *Starter
	for i := range MaxProfileStarters {
		st, err := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("S"), nil)
		if err != nil {
			t.Fatalf("starter %d: %v", i, err)
		}
		if first == nil {
			first = st
		}
	}
	if _, err := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("S"), nil); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "50 starters") {
		t.Fatalf("starter 51: %v", err)
	}
	for n := 2; n <= MaxStarterVersions; n++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO starter_versions(starter_id, n, author, manifest, size, created_at) VALUES(?, ?, 'Alice', '{}', 2, '')`,
			first.ID, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveStarter(ctx, alice.ProfileID, first.ID, testManifest("S"), nil); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "100 versions") {
		t.Fatalf("version 101: %v", err)
	}
}

// 006-AC5: publishing and deleting an instance starter are audited with who, which starter and
// which version; a personal delete is not an admin action.
func TestStarterAudit(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	st, err := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Weekly"), nil)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Draft"), nil)
	own, _ := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Own"), nil)
	if err := s.PublishStarter(ctx, alice.ProfileID, own.ID); err != nil {
		t.Fatal(err)
	}
	audit := func(action, actor string) context.Context {
		return WithAudit(ctx, AuditEntry{Actor: actor, Action: action, TargetKind: TargetStarter, TargetID: "x"}, nil)
	}
	if err := s.PublishStarter(audit("starter.publish", "profile:a"), alice.ProfileID, st.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStarter(audit("starter.delete", ActorSuperAdmin), 0, st.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStarter(audit("starter.delete", "profile:a"), alice.ProfileID, other.ID); err != nil {
		t.Fatal(err)
	}
	// Its author deletes a shared starter too, under their own name.
	if err := s.DeleteStarter(audit("starter.delete", "profile:a"), alice.ProfileID, own.ID); err != nil {
		t.Fatal(err)
	}
	log, err := s.AuditLog(ctx, AuditQuery{Limit: 10})
	if err != nil || len(log) != 3 {
		t.Fatalf("audit: %+v %v", log, err)
	}
	for i, want := range []string{"starter.delete profile:a Own", "starter.delete super-admin Weekly", "starter.publish profile:a Weekly"} {
		if got := log[i].Action + " " + log[i].Actor + " " + log[i].TargetName; got != want || !strings.Contains(string(log[i].Details), `"version":1`) {
			t.Errorf("entry %d: %s %s", i, got, log[i].Details)
		}
	}
}

// Starters cascade from their owner's profile, like persona templates; built-ins stay.
func TestDeleteProfileDropsItsStarters(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	st, _ := s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Mine"), map[string][]byte{"doc/0": []byte("x")})
	if err := s.PublishStarter(ctx, alice.ProfileID, st.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SaveStarter(ctx, alice.ProfileID, 0, testManifest("Private"), nil)
	if err := s.DeleteProfile(ctx, alice.ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM starters`); n != 3 {
		t.Fatalf("%d starters left, want the 3 built-ins", n)
	}
	if n := count(t, s, `SELECT count(*) FROM starter_files`); n != 0 {
		t.Fatalf("%d files left", n)
	}
}

// Every starter call reports a failing database instead of answering empty.
func TestStarterCallsFailOnAClosedStore(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_ = s.Close()
	for name, f := range map[string]func() error{
		"list":      func() error { _, err := s.Starters(ctx, 1); return err },
		"version":   func() error { _, _, err := s.StarterVersion(ctx, 1, 0, 1); return err },
		"files":     func() error { _, err := s.StarterFiles(ctx, 1, 1); return err },
		"save":      func() error { _, err := s.SaveStarter(ctx, 1, 0, testManifest("x"), nil); return err },
		"duplicate": func() error { _, err := s.DuplicateStarter(ctx, 1, 1); return err },
		"publish":   func() error { return s.PublishStarter(ctx, 1, 1) },
		"delete":    func() error { return s.DeleteStarter(ctx, 1, 1) },
		"blank":     func() error { _, err := s.BlankManifest(ctx); return err },
		"voice":     func() error { _, err := s.DueVoicePersonas(ctx, "r"); return err },
		"timer":     func() error { _, err := s.DueTimerPersonas(ctx, time.Now(), ""); return err },
		"release":   func() error { _, err := s.ReleaseStarterSkills(ctx, "r"); return err },
	} {
		if err := f(); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The blank starter is the Default template's persona alone.
func TestBlankManifest(t *testing.T) {
	s := openTest(t)
	m, err := s.BlankManifest(t.Context())
	if err != nil || m.Title != "Blank" || len(m.Personas) != 1 || m.Personas[0].Name != "Grill" || m.Agenda != "" || m.Chain.Budget != DefaultChainBudget {
		t.Fatalf("blank: %+v %v", m, err)
	}
}
