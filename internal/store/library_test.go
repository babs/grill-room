package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	voicecat "github.com/babs/grill-room/internal/voices"
)

// 035-AC18 035-AC21: migration 65 puts a database at version 64 in sections: a profile's template
// becomes personal with its versions and their texts, read with the default settings; presets
// become built-in and take their description and settings in place, without a new version. Room
// copies keep their link.
func TestMigrateLibrary(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dsn := testDSN(t)
	s := openAt(t, dsn)
	alice := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	f := PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1", Model: "haiku", Effort: "low"}
	tp, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "Critic", f)
	if err != nil {
		t.Fatal(err)
	}
	f.Text = "v2"
	if _, err := s.PublishVersion(ctx, alice.ProfileID, tp.ID, "Alice", f); err != nil {
		t.Fatal(err)
	}
	cp, _, err := s.AddPersona(ctx, "a", tp.ID, alice, true)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]int{}
	for _, p := range presets {
		before[p.key] = templateByPreset(t, s, p.key).Version
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE persona_template_files;
ALTER TABLE persona_template_versions DROP COLUMN description, DROP COLUMN manifest, DROP COLUMN size;
ALTER TABLE persona_templates DROP CONSTRAINT persona_templates_builtin;
ALTER TABLE persona_templates DROP COLUMN section;
ALTER TABLE persona_templates ADD CHECK ((preset IS NULL) = (profile_id IS NOT NULL));
UPDATE schema_version SET version = 64`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openAt(t, dsn)
	got, err := s.Template(ctx, tp.ID)
	if err != nil || got.Section != SectionPersonal || got.Version != 2 || got.Text != "v2" || got.Description != "" ||
		!got.SpeaksInCall() || got.VoiceRate != 1 || got.EveryN+got.EveryVoice+got.EveryMinutes != 0 || got.TimeKeeper || len(got.FrozenSkills) != 0 {
		t.Fatalf("migrated template: %+v %v", got, err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_template_versions WHERE template_id = ? AND (n = 1 AND text = 'v1' OR n = 2 AND text = 'v2')`, tp.ID); n != 2 {
		t.Fatalf("%d versions kept, want 2", n)
	}
	for _, p := range presets {
		pt := templateByPreset(t, s, p.key)
		if pt.Section != SectionBuiltin || pt.Version != before[p.key] || pt.Description != p.description || pt.TimeKeeper != p.f.TimeKeeper || pt.EveryMinutes != p.f.EveryMinutes {
			t.Fatalf("migrated preset %s: %+v", p.key, pt)
		}
	}
	if pc, err := s.RoomPersona(ctx, "a", cp.ID); err != nil || pc.TemplateID != tp.ID || pc.TemplateSection != SectionPersonal || pc.UpdateVersion != 0 {
		t.Fatalf("room copy: %+v %v", pc, err)
	}
	if n := count(t, s, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'persona_templates'::regclass AND conname = 'persona_templates_check'`); n != 0 {
		t.Fatal("the preset/profile check survived the migration")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO persona_templates(profile_id, section, title, created_at) VALUES(?, 'builtin', 'x', ?)`, alice.ProfileID, now()); err == nil {
		t.Fatal("a profile's built-in template stored")
	}
}

// personaSettings is every setting a library persona carries (035-AC8).
type personaSettings struct {
	PersonaFields
	EveryN, EveryVoice, EveryMinutes, Autocompact, CutInS int
	SelfCall, WatchesNotes, Speaks, TimeKeeper            bool
	Voice                                                 string
	VoiceRate                                             float64
}

func settingsOf(p *RoomPersona) personaSettings {
	return personaSettings{p.PersonaFields, p.EveryN, p.EveryVoice, p.EveryMinutes, p.Autocompact, p.CutInS, p.SelfCall, p.WatchesNotes, p.Speaks, p.TimeKeeper, p.Voice, p.VoiceRate}
}

// 035-AC8: every setting goes room persona → library → another room unchanged, for each trigger.
func TestSavePersonaRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	voice := voicecat.All()[0].ID
	for i, tr := range []struct {
		name                 string
		every, voice, every2 int
	}{{"every_n", 3, 0, 0}, {"every_voice", 0, 7, 0}, {"every_minutes", 0, 0, 15}} {
		t.Run(tr.name, func(t *testing.T) {
			src, dst := fmt.Sprintf("src%d", i), fmt.Sprintf("dst%d", i)
			alice := joinWithProfile(t, s, src, "Alice", JoinProfile{RecoveryHash: "r" + src, SessionHash: "s" + src})
			ps, err := s.RoomPersonas(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			c := PersonaPatch{Name: ptr("Critic"), Badge: ptr("🧐"), Colour: ptr("#123456"), Text: ptr("Find the flaw."), Model: ptr("opus"), Effort: ptr("high"),
				Autocompact: ptr(200000), EveryN: &tr.every, EveryVoice: &tr.voice, EveryMinutes: &tr.every2, SelfCall: yes(), WatchesNotes: yes(),
				Speaks: no(), Voice: &voice, VoiceRate: ptr(1.2), TimeKeeper: yes(), CutInS: ptr(30)}
			want, _, _, err := s.EditPersona(ctx, src, ps[0].ID, c, alice)
			if err != nil || want.Speaks || !want.TimeKeeper || want.Voice != voice {
				t.Fatalf("edit: %+v %v", want, err)
			}
			tp, err := s.SavePersonaToLibrary(ctx, src, want.ID, alice.ProfileID)
			if err != nil {
				t.Fatal(err)
			}
			ali := joinWithProfile(t, s, dst, "Ali", JoinProfile{ID: alice.ProfileID})
			got, _, err := s.AddPersona(ctx, dst, tp.ID, ali, true)
			if err != nil {
				t.Fatal(err)
			}
			if settingsOf(got) != settingsOf(want) {
				t.Fatalf("copy %+v, want %+v", settingsOf(got), settingsOf(want))
			}
		})
	}
}

// 035-AC13 035-AC16: a first save makes a personal template and links the room persona to it, a
// second save its version 2 with the description kept; a persona linked to another profile's
// template saves to a new one of the saver's. The room persona names its source.
func TestSavePersonaTwice(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	if p, err := s.RoomPersona(ctx, "a", grill.ID); err != nil || p.TemplateSection != SectionBuiltin || p.TemplateTitle != "Grill" || p.TemplateAuthor != presetAuthor {
		t.Fatalf("preset copy source: %+v %v", p, err)
	}
	t1, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil || t1.Version != 1 || t1.Section != SectionPersonal || t1.Title != "Grill" || t1.Author != "Alice" || !t1.Mine || t1.Text != grill.Text {
		t.Fatalf("first save: %+v %v", t1, err)
	}
	p, err := s.RoomPersona(ctx, "a", grill.ID)
	if err != nil || p.TemplateID != t1.ID || p.TemplateVersion != 1 || p.UpdateVersion != 0 || p.TemplateTitle != "Grill" ||
		p.TemplateSection != SectionPersonal || p.TemplateAuthor != "Alice" {
		t.Fatalf("linked: %+v %v", p, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE persona_template_versions SET description = 'One line.' WHERE template_id = ?`, t1.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Text: ptr("Sharper.")}, alice); err != nil {
		t.Fatal(err)
	}
	t2, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil || t2.ID != t1.ID || t2.Version != 2 || t2.Text != "Sharper." || t2.Description != "One line." {
		t.Fatalf("second save: %+v %v", t2, err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, alice.ProfileID); n != 1 {
		t.Fatalf("%d library personas, want 1", n)
	}
	if p, _ := s.RoomPersona(ctx, "a", grill.ID); p.TemplateID != t1.ID || p.TemplateVersion != 2 {
		t.Fatalf("linked after the second save: %+v", p)
	}
	t3, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, bob.ProfileID)
	if err != nil || t3.ID == t1.ID || t3.Version != 1 || t3.Author != "Bob" {
		t.Fatalf("Bob's save: %+v %v", t3, err)
	}
	if p, _ := s.RoomPersona(ctx, "a", grill.ID); p.TemplateID != t3.ID || p.TemplateAuthor != "Bob" {
		t.Fatalf("linked to Bob's: %+v", p)
	}
	if _, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no profile: %v", err)
	}
	for room, id := range map[string]int64{"a": 999, "b": grill.ID} {
		if _, err := s.SavePersonaToLibrary(ctx, room, id, alice.ProfileID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("persona %d of room %s: %v", id, room, err)
		}
	}
}

// 035-AC16 035-AC18: a room persona names its library persona's author as profileNameSQL does: the
// profile's chosen name, else its latest name in a room.
func TestRoomPersonaAuthor(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "a")
	if _, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		chosen any
		name   string
	}{{nil, "Alice"}, {"Chosen", "Chosen"}} {
		if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET name = ? WHERE id = ?`, c.chosen, alice.ProfileID); err != nil {
			t.Fatal(err)
		}
		var want string
		if err := s.db.QueryRowContext(ctx, `SELECT `+profileNameSQL, alice.ProfileID, alice.ProfileID).Scan(&want); err != nil {
			t.Fatal(err)
		}
		p, err := s.RoomPersona(ctx, "a", grill.ID)
		if err != nil || want != c.name || p.TemplateAuthor != want {
			t.Fatalf("chosen name %v: author %q, profileNameSQL %q, %v", c.chosen, p.TemplateAuthor, want, err)
		}
	}
}

// 035-AC8 035-AC9: a version published the 002 way, six fields only, keeps the latest version's
// description, settings and frozen files; a save after it keeps the description.
func TestPublishVersionKeepsWholePersona(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "a")
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{EveryN: ptr(3), Speaks: no()}, alice); err != nil {
		t.Fatal(err)
	}
	var sid int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES('stamp', ?, 'a', 0) RETURNING id`, now()).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at) VALUES(?,1,'Bob','d',1,6,'x','tar v1',?)`,
		sid, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?, ?, 1)`, grill.ID, sid); err != nil {
		t.Fatal(err)
	}
	t1, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE persona_template_versions SET description = 'One line.' WHERE template_id = ?`, t1.ID); err != nil {
		t.Fatal(err)
	}
	f := t1.PersonaFields
	f.Text = "Published."
	t2, err := s.PublishVersion(ctx, alice.ProfileID, t1.ID, "Alice", f)
	if err != nil || t2.Version != 2 || t2.Text != "Published." || t2.Description != "One line." || t2.EveryN != 3 || t2.SpeaksInCall() ||
		len(t2.FrozenSkills) != 1 || t2.FrozenSkills[0].Name != "stamp" {
		t.Fatalf("published version: %+v %v", t2, err)
	}
	var data string
	if err := s.db.QueryRowContext(ctx, `SELECT f.data FROM persona_template_files f JOIN persona_template_versions v ON v.id = f.version_id
		WHERE v.template_id = ? AND v.n = 2 AND f.name = ?`, t1.ID, t2.FrozenSkills[0].File).Scan(&data); err != nil || data != "tar v1" {
		t.Fatalf("v2 frozen skill %q %v", data, err)
	}
	t3, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil || t3.ID != t1.ID || t3.Version != 3 || t3.Description != "One line." {
		t.Fatalf("save after publish: %+v %v", t3, err)
	}
}

// 035-AC9: a save freezes the persona's skill at its pin, not the latest, and its plugin at the
// running version, with their bytes, whoever saves; a skill or plugin still waiting for the host is
// left out.
func TestSavePersonaFreezesKit(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	r, vs := pluginRoom(t, s)
	var sid int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES('stamp', ?, 'a', 0) RETURNING id`, now()).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at) VALUES(?,?,'Bob','d',1,6,'x',?,?)`,
			sid, n, fmt.Sprintf("tar v%d", n), now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?, ?, 1)`, r.grill.ID, sid); err != nil {
		t.Fatal(err)
	}
	// L3: a skill and a plugin the host has not approved are not what the persona runs with.
	var held int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES('held', ?, 'a', 1) RETURNING id`, now()).Scan(&held); err != nil {
		t.Fatal(err)
	}
	for q, args := range map[string][]any{
		`INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at) VALUES(?,1,'Bob','d',1,4,'x','held',?)`: {held, now()},
		`INSERT INTO room_persona_plugins(persona_id, plugin_id) SELECT ?, id FROM room_plugins WHERE room_id = 'a' AND name = 'priv'`:               {r.grill.ID},
	} {
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?, ?, 1)`, r.grill.ID, held); err != nil {
		t.Fatal(err)
	}
	tp, err := s.SavePersonaToLibrary(ctx, "a", r.grill.ID, newProfile(t, s, "saver"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.FrozenSkills) != 1 || tp.FrozenSkills[0].Name != "stamp" || tp.FrozenSkills[0].Version != 1 || strings.Join(tp.Skills, ",") != "stamp" ||
		len(tp.FrozenPlugins) != 1 || tp.FrozenPlugins[0].Name != "babs" || tp.FrozenPlugins[0].Commit != vs[2].Commit || strings.Join(tp.Plugins, ",") != "babs" {
		t.Fatalf("frozen kit: %+v", tp.TemplateManifest)
	}
	files := map[string][]byte{}
	rows, err := s.db.QueryContext(ctx, `SELECT f.name, f.data FROM persona_template_files f JOIN persona_template_versions v ON v.id = f.version_id
		WHERE v.template_id = ? AND v.n = 1`, tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	if len(files) != 2 || string(files[tp.FrozenSkills[0].File]) != "tar v1" || string(files[tp.FrozenPlugins[0].File]) != string(vs[2].Data) {
		t.Fatalf("frozen files: %d, skill %q", len(files), files[tp.FrozenSkills[0].File])
	}
	if n := count(t, s, `SELECT count(*) FROM persona_template_versions WHERE template_id = ? AND size > ?`, tp.ID, len(vs[2].Data)+6); n != 1 {
		t.Fatal("the version's size leaves out its files")
	}
}

// 035-AC7: past 100 versions or 50 library personas, or past the instance storage quota, a save
// or a publish answers with the limit and changes nothing.
func TestSavePersonaLimits(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	tp, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= MaxTemplateVersions; n++ {
		if err := insertVersion(ctx, s.db, tp.ID, n, "Alice", grill.PersonaFields, "", []byte("{}"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID); !errors.Is(err, ErrLimit) || err.Error() != "limit reached: at most 100 versions per persona" {
		t.Fatalf("version 101: %v", err)
	}
	if p, _ := s.RoomPersona(ctx, "a", grill.ID); p.TemplateVersion != 1 || count(t, s, `SELECT count(*) FROM persona_template_versions WHERE template_id = ?`, tp.ID) != MaxTemplateVersions {
		t.Fatalf("a refused version changed something: %+v", p)
	}
	var last *Template
	for i := 1; i < MaxProfileTemplates; i++ {
		if last, err = s.PublishTemplate(ctx, alice.ProfileID, "Alice", "T", grill.PersonaFields); err != nil {
			t.Fatal(err)
		}
	}
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, alice, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePersonaToLibrary(ctx, "a", sec.ID, alice.ProfileID); !errors.Is(err, ErrLimit) || err.Error() != "limit reached: at most 50 personas per profile" {
		t.Fatalf("persona 51: %v", err)
	}
	if p, _ := s.RoomPersona(ctx, "a", sec.ID); p.TemplateID != sec.TemplateID || count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, alice.ProfileID) != MaxProfileTemplates {
		t.Fatalf("a refused save changed something: %+v", p)
	}
	used, err := s.UsedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := putSettings(ctx, s, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePersonaToLibrary(ctx, "a", sec.ID, bob.ProfileID); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "storage quota") {
		t.Fatalf("past the quota: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, bob.ProfileID); n != 0 {
		t.Fatalf("a save past the quota left %d templates", n)
	}
	// The 002 route is held to the same quota.
	if _, err := s.PublishTemplate(ctx, bob.ProfileID, "Bob", "T", grill.PersonaFields); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "storage quota") {
		t.Fatalf("publish past the quota: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, bob.ProfileID); n != 0 {
		t.Fatalf("a publish past the quota left %d templates", n)
	}
	if _, err := s.PublishVersion(ctx, alice.ProfileID, last.ID, "Alice", grill.PersonaFields); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "storage quota") {
		t.Fatalf("version past the quota: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_template_versions WHERE template_id = ?`, last.ID); n != 1 {
		t.Fatalf("a version past the quota left %d versions", n)
	}
}

// 035-AC13: saves of one room persona by Alice, Bob, then Alice give Alice one library persona at
// v2 and Bob one at v1.
func TestSavePersonaAlternating(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	for _, by := range []*Participant{alice, bob, alice} {
		if _, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, by.ProfileID); err != nil {
			t.Fatal(err)
		}
	}
	for by, want := range map[*Participant]int{alice: 2, bob: 1} {
		ts, err := s.Templates(ctx, by.ProfileID)
		var own []Template
		for _, x := range ts {
			if x.Mine {
				own = append(own, x)
			}
		}
		if err != nil || len(own) != 1 || own[0].Version != want {
			t.Fatalf("%s's library personas: %+v %v", by.Name, own, err)
		}
	}
	if p, _ := s.RoomPersona(ctx, "a", grill.ID); p.TemplateAuthor != "Alice" || p.TemplateVersion != 2 {
		t.Fatalf("linked: %+v", p)
	}
}

// 035-AC12: a persona the host has yet to approve, or with a change the host has yet to approve,
// is not saved: the library would hold a state the room never ran.
func TestSavePersonaAwaitingHost(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, bob, grill, _ := approvingRoom(t, s, "h")
	sec, _, err := s.AddPersona(ctx, "h", templateByPreset(t, s, "secretary").ID, bob, false)
	if err != nil || !sec.Pending {
		t.Fatalf("held add: %+v %v", sec, err)
	}
	if _, _, held, err := s.EditPersona(ctx, "h", grill.ID, PersonaPatch{Text: ptr("Unapproved.")}, bob); err != nil || !held {
		t.Fatalf("held edit: %v %v", held, err)
	}
	for id, name := range map[int64]string{sec.ID: "Secretary", grill.ID: "Grill"} {
		_, err := s.SavePersonaToLibrary(ctx, "h", id, bob.ProfileID)
		if !errors.Is(err, ErrAwaitingHost) || err.Error() != name+" waits for the AI host's approval: save once it is approved." {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, bob.ProfileID); n != 0 {
		t.Fatalf("%d library personas saved while waiting", n)
	}
}

// 035-AC11: a copy watches the notes only in a room without a watcher, and never while held for the
// host; otherwise the add line says it does not.
func TestAddWatcherTemplate(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "w0", "Alice", JoinProfile{RecoveryHash: "rw", SessionHash: "sw"})
	src, _ := s.RoomPersonas(ctx, "w0")
	if _, _, _, err := s.EditPersona(ctx, "w0", src[0].ID, PersonaPatch{Name: ptr("Watcher"), WatchesNotes: yes()}, alice); err != nil {
		t.Fatal(err)
	}
	tp, err := s.SavePersonaToLibrary(ctx, "w0", src[0].ID, alice.ProfileID)
	if err != nil || !tp.WatchesNotes {
		t.Fatalf("save: %+v %v", tp, err)
	}
	// Shared, so that a member of another profile may add it.
	if _, err := s.db.ExecContext(ctx, `UPDATE persona_templates SET section = 'instance' WHERE id = ?`, tp.ID); err != nil {
		t.Fatal(err)
	}
	watchers := func(room string) string {
		t.Helper()
		var out []string
		ps, _ := s.RoomPersonas(ctx, room)
		for _, p := range ps {
			if p.WatchesNotes {
				out = append(out, p.Name)
			}
		}
		return strings.Join(out, ",")
	}
	ali := joinWithProfile(t, s, "w1", "Ali", JoinProfile{ID: alice.ProfileID})
	if p, _, err := s.AddPersona(ctx, "w1", tp.ID, ali, false); err != nil || !p.WatchesNotes || watchers("w1") != "Watcher" {
		t.Fatalf("into a room without a watcher: %+v %v", p, err)
	}
	wa, _, wg := personaRoom(t, s, "w2")
	if _, _, _, err := s.EditPersona(ctx, "w2", wg.ID, PersonaPatch{WatchesNotes: yes()}, wa); err != nil {
		t.Fatal(err)
	}
	p, m, err := s.AddPersona(ctx, "w2", tp.ID, wa, true)
	if err != nil || p.WatchesNotes || watchers("w2") != "Grill" || !strings.HasSuffix(m.Body, ", not the notes watcher (🔥 Grill watches the notes).") {
		t.Fatalf("into a watched room: %+v %+v %v", p, m, err)
	}
	host, bob, hg, _ := approvingRoom(t, s, "w3")
	if _, _, held, err := s.EditPersona(ctx, "w3", hg.ID, PersonaPatch{WatchesNotes: yes()}, host); err != nil || held {
		t.Fatalf("host's watcher: %v %v", held, err)
	}
	if p, _, err := s.AddPersona(ctx, "w3", tp.ID, bob, false); err != nil || !p.Pending || p.WatchesNotes || watchers("w3") != "Grill" {
		t.Fatalf("held add: %+v %v", p, err)
	}
	// Held into a room without a watcher: no role, before the host's approval or after it.
	host, bob, _, _ = approvingRoom(t, s, "w4")
	p, m, err = s.AddPersona(ctx, "w4", tp.ID, bob, false)
	if err != nil || !p.Pending || p.WatchesNotes || watchers("w4") != "" ||
		!strings.Contains(m.Body, ", not the notes watcher: a room member gives it the role once the host approves it.") {
		t.Fatalf("held add into an unwatched room: %+v %+v %v", p, m, err)
	}
	if _, err := s.DecideChange(ctx, "w4", s.onlyChange(t, "w4").ID, true, host); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RoomPersona(ctx, "w4", p.ID); err != nil || got.Pending || got.WatchesNotes || watchers("w4") != "" {
		t.Fatalf("after approval: %+v %v", got, err)
	}
}

// 035-AC21: a merge moves the merged profile's library personas, shared ones too, into the kept
// profile's own with their versions and files; a deletion removes them, and a room copy keeps every
// setting without its link.
func TestLibraryMergeAndDelete(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	files := map[string][]byte{"skill/1": []byte("tar")}
	raw, err := TemplateManifest{StarterPersona: StarterPersona{EveryMinutes: 5, Speaks: no()}}.marshal()
	if err != nil {
		t.Fatal(err)
	}
	f := PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "t", Model: "sonnet", Effort: "medium"}
	library := func(owner int64) (personal, shared int64) {
		t.Helper()
		for i, id := range []*int64{&personal, &shared} {
			tp, err := s.PublishTemplate(ctx, owner, "x", fmt.Sprint("T", i), f)
			if err != nil {
				t.Fatal(err)
			}
			if err := insertVersion(ctx, s.db, tp.ID, 2, "x", f, "", raw, files); err != nil {
				t.Fatal(err)
			}
			*id = tp.ID
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE persona_templates SET section = 'instance' WHERE id = ?`, shared); err != nil {
			t.Fatal(err)
		}
		return personal, shared
	}
	a := joinWithProfile(t, s, "m1", "Alice", JoinProfile{RecoveryHash: "ra", SessionHash: "sa"})
	b := joinWithProfile(t, s, "m2", "Bob", JoinProfile{RecoveryHash: "rb", SessionHash: "sb"})
	bp, bs := library(b.ProfileID)
	if _, _, _, err := s.MergeProfiles(ctx, a.ProfileID, b.ProfileID, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{bp, bs} {
		if n := count(t, s, `SELECT count(*) FROM persona_templates t WHERE t.id = ? AND t.profile_id = ? AND t.section = 'personal'
			AND (SELECT count(*) FROM persona_template_versions WHERE template_id = t.id) = 2
			AND (SELECT count(*) FROM persona_template_files f JOIN persona_template_versions v ON v.id = f.version_id WHERE v.template_id = t.id) = 1`, id, a.ProfileID); n != 1 {
			t.Fatalf("template %d after the merge", id)
		}
	}
	cp, _, err := s.AddPersona(ctx, "m1", bp, a, true)
	if err != nil || cp.EveryMinutes != 5 || cp.Speaks || cp.TemplateVersion != 2 {
		t.Fatalf("copy: %+v %v", cp, err)
	}
	if err := s.DeleteProfile(ctx, a.ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id IS NOT NULL`) + count(t, s, `SELECT count(*) FROM persona_template_files`); n != 0 {
		t.Fatalf("%d library rows left after the deletion", n)
	}
	got, err := s.RoomPersona(ctx, "m1", cp.ID)
	if err != nil || got.TemplateID != 0 || got.TemplateVersion != 2 || got.TemplateSection != "" || settingsOf(got) != settingsOf(cp) {
		t.Fatalf("room copy after the deletion: %+v %v", got, err)
	}
}

// 035-AC18: a same-instance import links a persona to its template only when the importing profile
// sees it; another profile's personal template stays private, the copy keeping its settings.
func TestImportLinksVisibleTemplatesOnly(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, _ := personaRoom(t, s, "orig")
	tp, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "Critic", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "t", Model: "sonnet", Effort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddPersona(ctx, "orig", tp.ID, alice, true); err != nil {
		t.Fatal(err)
	}
	dir := export(t, s, "orig")
	for _, c := range []struct {
		room     string
		importer *Participant
		want     int64
	}{{"by-bob", bob, 0}, {"by-alice", alice, tp.ID}, {"by-admin", nil, tp.ID}} {
		o := importOpts(c.room)
		if c.importer != nil {
			o.Importer, o.DisplayName, o.SourceRoom = c.importer.ProfileID, c.importer.Name, "orig"
		}
		if _, err := s.ImportRoom(ctx, dir, o); err != nil {
			t.Fatalf("%s: %v", c.room, err)
		}
		ps, _ := s.RoomPersonas(ctx, c.room)
		var got *RoomPersona
		for i := range ps {
			if ps[i].Name == "Critic" {
				got = &ps[i]
			}
		}
		// 035-AC16: unlinked, the copy keeps no version either.
		if got == nil || got.TemplateID != c.want || got.Text != "t" || (c.want == 0) != (got.TemplateVersion == 0) {
			t.Errorf("%s: imported copy %+v, want template %d", c.room, got, c.want)
		}
	}
	// 035-AC16: a library persona deleted before the export stays deleted: the copy keeps its version.
	gone, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "Gone", PersonaFields{Name: "Gone", Badge: "👻", Colour: "#112233", Text: "g", Model: "sonnet", Effort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddPersona(ctx, "orig", gone.ID, alice, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTemplate(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportRoom(ctx, export(t, s, "orig"), importOpts("after-delete")); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.RoomPersonas(ctx, "after-delete")
	if i := slices.IndexFunc(ps, func(p RoomPersona) bool { return p.Name == "Gone" }); i < 0 || ps[i].TemplateID != 0 || ps[i].TemplateVersion != 1 {
		t.Fatalf("copy of a deleted library persona: %+v", ps)
	}
}
