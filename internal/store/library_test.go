package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	cp, _, err := s.AddPersona(ctx, "a", tp.ID, alice, true, 1<<20)
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
			got, _, err := s.AddPersona(ctx, dst, tp.ID, ali, true, 1<<20)
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
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, alice, true, 1<<20)
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
	sec, _, err := s.AddPersona(ctx, "h", templateByPreset(t, s, "secretary").ID, bob, false, 1<<20)
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
	if p, _, err := s.AddPersona(ctx, "w1", tp.ID, ali, false, 1<<20); err != nil || !p.WatchesNotes || watchers("w1") != "Watcher" {
		t.Fatalf("into a room without a watcher: %+v %v", p, err)
	}
	wa, _, wg := personaRoom(t, s, "w2")
	if _, _, _, err := s.EditPersona(ctx, "w2", wg.ID, PersonaPatch{WatchesNotes: yes()}, wa); err != nil {
		t.Fatal(err)
	}
	p, m, err := s.AddPersona(ctx, "w2", tp.ID, wa, true, 1<<20)
	if err != nil || p.WatchesNotes || watchers("w2") != "Grill" || !strings.HasSuffix(m.Body, ", not the notes watcher (🔥 Grill watches the notes).") {
		t.Fatalf("into a watched room: %+v %+v %v", p, m, err)
	}
	host, bob, hg, _ := approvingRoom(t, s, "w3")
	if _, _, held, err := s.EditPersona(ctx, "w3", hg.ID, PersonaPatch{WatchesNotes: yes()}, host); err != nil || held {
		t.Fatalf("host's watcher: %v %v", held, err)
	}
	if p, _, err := s.AddPersona(ctx, "w3", tp.ID, bob, false, 1<<20); err != nil || !p.Pending || p.WatchesNotes || watchers("w3") != "Grill" {
		t.Fatalf("held add: %+v %v", p, err)
	}
	// Held into a room without a watcher: no role, before the host's approval or after it.
	host, bob, _, _ = approvingRoom(t, s, "w4")
	p, m, err = s.AddPersona(ctx, "w4", tp.ID, bob, false, 1<<20)
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
	cp, _, err := s.AddPersona(ctx, "m1", bp, a, true, 1<<20)
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
	if _, _, err := s.AddPersona(ctx, "orig", tp.ID, alice, true, 1<<20); err != nil {
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
	if _, _, err := s.AddPersona(ctx, "orig", gone.ID, alice, true, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTemplate(ctx, alice.ProfileID, gone.ID); err != nil {
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

// librarySkill gives room persona grill of room "a" an approved skill, stamp v1, holding "tar v1".
func librarySkill(t *testing.T, s *Store, grill int64) {
	t.Helper()
	ctx := t.Context()
	var sid int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES('stamp', ?, 'a', 0) RETURNING id`, now()).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at) VALUES(?,1,'Bob','d',1,6,'x','tar v1',?)`,
		sid, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?, ?, 1)`, grill, sid); err != nil {
		t.Fatal(err)
	}
}

// frozenData is the bytes of file name in version n of template id, "" when it has none.
func frozenData(t *testing.T, s *Store, id int64, n int, name string) string {
	t.Helper()
	var data string
	err := s.db.QueryRowContext(t.Context(), `SELECT f.data FROM persona_template_files f JOIN persona_template_versions v ON v.id = f.version_id
		WHERE v.template_id = ? AND v.n = ? AND f.name = ?`, id, n, name).Scan(&data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return data
}

// 035-AC3 035-AC7: the author's edit is version N+1 with every setting and the description, its
// frozen skill carried over, the title following the new name, as a save from the room renames it
// too; rooms keep their copy and are offered the update. An edit made on an older version, someone
// else's edit and a built-in's are refused, and past 100 versions the edit is.
func TestEditTemplate(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	librarySkill(t, s, grill.ID)
	t1, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	e := TemplateEdit{StarterPersona: StarterPersona{PersonaFields: PersonaFields{Name: "Grilled", Badge: "🍖", Colour: "#123456", Text: "Edited.", Model: "opus", Effort: "high"},
		EveryMinutes: 5, SelfCall: true, Autocompact: 50_000, Speaks: no(), Voice: "fr-1", VoiceRate: 1.2, TimeKeeper: true, CutInS: 20,
		Skills: []string{"ignored"}}, Description: "One line.", Version: 1}
	t2, err := s.EditTemplate(ctx, alice.ProfileID, t1.ID, e)
	if err != nil || t2.Version != 2 || t2.Name != "Grilled" || t2.Text != "Edited." || t2.Model != "opus" || t2.Description != "One line." ||
		t2.EveryMinutes != 5 || !t2.SelfCall || t2.Autocompact != 50_000 || t2.SpeaksInCall() || t2.Voice != "fr-1" || t2.VoiceRate != 1.2 ||
		!t2.TimeKeeper || t2.CutInS != 20 || t2.Author != "Alice" || strings.Join(t2.Skills, ",") != "stamp" || len(t2.FrozenSkills) != 1 ||
		t2.Title != "Grilled" {
		t.Fatalf("edited: %+v %v", t2, err)
	}
	if got := frozenData(t, s, t1.ID, 2, t2.FrozenSkills[0].File); got != "tar v1" {
		t.Fatalf("v2 frozen skill %q", got)
	}
	p, err := s.RoomPersona(ctx, "a", grill.ID)
	if err != nil || p.Name != "Grill" || p.Text != grill.Text || p.TemplateVersion != 1 || p.UpdateVersion != 2 || p.EveryMinutes != 0 {
		t.Fatalf("room copy: %+v %v", p, err)
	}
	// A second tab's edit, made on v1 too, changes nothing.
	stale := e
	stale.Text = "Second tab."
	if _, err := s.EditTemplate(ctx, alice.ProfileID, t1.ID, stale); !errors.Is(err, ErrStaleVersion) || err.Error() != "version 2 exists: reopen the persona to edit it" {
		t.Fatalf("edit on v1 after v2: %v", err)
	}
	if got, err := s.Template(ctx, t1.ID); err != nil || got.Version != 2 || got.Text != "Edited." {
		t.Fatalf("after the stale edit: %+v %v", got, err)
	}
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{Name: ptr("Roaster")}, alice); err != nil {
		t.Fatal(err)
	}
	if t3, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID); err != nil || t3.Version != 3 || t3.Title != "Roaster" {
		t.Fatalf("save after a rename: %+v %v", t3, err)
	}
	def := templateByPreset(t, s, PresetDefault)
	for _, c := range []struct {
		who  int64
		id   int64
		want error
	}{{bob.ProfileID, t1.ID, ErrNotFound}, {alice.ProfileID, def.ID, ErrPreset}, {alice.ProfileID, 999, ErrNotFound}} {
		if _, err := s.EditTemplate(ctx, c.who, c.id, e); !errors.Is(err, c.want) {
			t.Errorf("edit %d by %d: %v, want %v", c.id, c.who, err, c.want)
		}
	}
	if err := s.ShareTemplate(ctx, alice.ProfileID, t1.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditTemplate(ctx, bob.ProfileID, t1.ID, e); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob edits the shared one: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE persona_template_versions SET n = n + 97 WHERE template_id = ?`, t1.ID); err != nil {
		t.Fatal(err)
	}
	e.Version = MaxTemplateVersions
	if _, err := s.EditTemplate(ctx, alice.ProfileID, t1.ID, e); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "at most 100 versions") {
		t.Fatalf("past 100 versions: %v", err)
	}
}

// 035-AC4 035-AC7: a duplicate is a personal library persona of the caller at version 1 with the
// whole persona and its own copy of the frozen files, titled "(copy)" when the title is taken in
// Yours, never linked to a room persona; another profile's private one is not found; past 50 the
// copy is refused.
func TestDuplicateTemplate(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	librarySkill(t, s, grill.ID)
	if _, _, _, err := s.EditPersona(ctx, "a", grill.ID, PersonaPatch{EveryN: ptr(3)}, alice); err != nil {
		t.Fatal(err)
	}
	t1, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DuplicateTemplate(ctx, bob.ProfileID, t1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob copies alice's private one: %v", err)
	}
	var copies []*Template
	for _, want := range []string{"Grill (copy)", "Grill (copy) 2"} {
		d, err := s.DuplicateTemplate(ctx, alice.ProfileID, t1.ID)
		if err != nil || d.Title != want || d.Version != 1 || d.Section != SectionPersonal || !d.Mine || d.SourcePersonaID != 0 || d.EveryN != 3 ||
			len(d.FrozenSkills) != 1 {
			t.Fatalf("duplicate: %+v %v, want %q", d, err, want)
		}
		copies = append(copies, d)
	}
	// The copy keeps its "(copy)" title through an edit; a rename retitles it; a copy of the copy
	// is named after the persona, not "(copy) (copy)".
	cp := copies[0]
	e := TemplateEdit{StarterPersona: cp.StarterPersona, Description: "Edited.", Version: 1}
	if got, err := s.EditTemplate(ctx, alice.ProfileID, cp.ID, e); err != nil || got.Title != "Grill (copy)" || got.Version != 2 || !got.Mine {
		t.Fatalf("edit without a rename: %+v %v", got, err)
	}
	if got, err := s.DuplicateTemplate(ctx, alice.ProfileID, cp.ID); err != nil || got.Title != "Grill (copy) 3" {
		t.Fatalf("copy of the copy: %+v %v", got, err)
	}
	e.Name, e.Version = "Toaster", 2
	if got, err := s.EditTemplate(ctx, alice.ProfileID, cp.ID, e); err != nil || got.Title != "Toaster" {
		t.Fatalf("edit with a rename: %+v %v", got, err)
	}
	if err := s.ShareTemplate(ctx, alice.ProfileID, t1.ID, true); err != nil {
		t.Fatal(err)
	}
	d, err := s.DuplicateTemplate(ctx, bob.ProfileID, t1.ID)
	if err != nil || d.Title != "Grill" || d.Author != "Bob" || !d.Mine || d.ProfileID != bob.ProfileID {
		t.Fatalf("bob's copy: %+v %v", d, err)
	}
	if err := s.RemoveTemplate(ctx, alice.ProfileID, t1.ID); err != nil {
		t.Fatal(err)
	}
	if got := frozenData(t, s, d.ID, 1, d.FrozenSkills[0].File); got != "tar v1" {
		t.Fatalf("the copy's frozen skill after the original's deletion: %q", got)
	}
	for i := count(t, s, `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, bob.ProfileID); i < MaxProfileTemplates; i++ {
		if _, err := s.PublishTemplate(ctx, bob.ProfileID, "Bob", fmt.Sprintf("T%d", i), grill.PersonaFields); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DuplicateTemplate(ctx, bob.ProfileID, d.ID); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "at most 50 personas") {
		t.Fatalf("past 50: %v", err)
	}
}

// 035-AC5 035-AC6 035-AC19: the owner shares and unshares, audited; another viewer sees it only
// while shared and loses its update. The owner deletes their own, the admin a shared one but never
// a private or built-in one; rooms keep their copies, unlinked, at their version.
func TestShareAndRemoveTemplate(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	t1, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	sees := func(viewer int64) string {
		t.Helper()
		ts, err := s.Templates(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(ts, func(x Template) bool { return x.ID == t1.ID }); i >= 0 {
			return ts[i].Section
		}
		return ""
	}
	audited := func(action string) context.Context {
		return WithAudit(ctx, AuditEntry{At: t0, Actor: "profile:alice", Action: action, TargetKind: TargetTemplate, TargetID: "x"}, nil)
	}
	if err := s.ShareTemplate(ctx, bob.ProfileID, t1.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob shares alice's private one: %v", err)
	}
	if err := s.ShareTemplate(audited("template.share"), alice.ProfileID, t1.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := sees(bob.ProfileID); got != SectionInstance {
		t.Fatalf("bob sees %q, want instance", got)
	}
	// 035-AC5: sharing it again says it is already shared; another profile is refused.
	if err := s.ShareTemplate(ctx, alice.ProfileID, t1.ID, true); !errors.Is(err, ErrAlreadyShared) || err.Error() != "Grill is already shared" {
		t.Errorf("alice shares it twice: %v", err)
	}
	if err := s.ShareTemplate(ctx, bob.ProfileID, t1.ID, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob unshares it: %v", err)
	}
	// Bob takes its next version into his room copy only while he sees it.
	if _, err := s.EditTemplate(ctx, alice.ProfileID, t1.ID, TemplateEdit{StarterPersona: StarterPersona{PersonaFields: PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#123456",
		Text: "v2", Model: "sonnet", Effort: "low"}}, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.ShareTemplate(audited("template.unshare"), alice.ProfileID, t1.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := sees(bob.ProfileID); got != "" {
		t.Fatalf("bob still sees it: %q", got)
	}
	if _, _, _, err := s.UpdatePersona(ctx, "a", grill.ID, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob updates from an unshared one: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM audit_log WHERE action IN ('template.share', 'template.unshare') AND target_id = ? AND details LIKE '%"version":%'`,
		strconv.FormatInt(t1.ID, 10)); n != 2 {
		t.Fatalf("%d audit entries, want 2", n)
	}
	def := templateByPreset(t, s, PresetDefault)
	for _, c := range []struct {
		who, id int64
		want    error
	}{{0, t1.ID, ErrNotFound}, {bob.ProfileID, t1.ID, ErrNotFound}, {0, def.ID, ErrPreset}, {alice.ProfileID, def.ID, ErrPreset}} {
		if err := s.RemoveTemplate(ctx, c.who, c.id); !errors.Is(err, c.want) {
			t.Errorf("remove %d by %d: %v, want %v", c.id, c.who, err, c.want)
		}
	}
	if err := s.RemoveTemplate(ctx, alice.ProfileID, t1.ID); err != nil {
		t.Fatal(err)
	}
	p, err := s.RoomPersona(ctx, "a", grill.ID)
	if err != nil || p.TemplateID != 0 || p.TemplateVersion != 1 || p.Text != grill.Text {
		t.Fatalf("room copy after delete: %+v %v", p, err)
	}
	if n := count(t, s, `SELECT count(*) FROM persona_template_versions WHERE template_id = ?`, t1.ID); n != 0 {
		t.Fatalf("%d versions left", n)
	}
	// The admin removes a shared one; another profile cannot.
	t2, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ShareTemplate(ctx, alice.ProfileID, t2.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTemplate(ctx, bob.ProfileID, t2.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob removes a shared one: %v", err)
	}
	if err := s.RemoveTemplate(ctx, 0, t2.ID); err != nil {
		t.Fatal(err)
	}
}

// kitTemplate is a shared library persona saved from Grill of pluginRoom's room "a", with the skill
// stamp v1 and the plugin babs at its running version, v2.
func kitTemplate(t *testing.T, s *Store) *Template {
	t.Helper()
	ctx := t.Context()
	r, _ := pluginRoom(t, s)
	librarySkill(t, s, r.grill.ID)
	saver := newProfile(t, s, "saver")
	tp, err := s.SavePersonaToLibrary(ctx, "a", r.grill.ID, saver)
	if err != nil || len(tp.FrozenSkills) != 1 || len(tp.FrozenPlugins) != 1 {
		t.Fatalf("kit template: %+v %v", tp, err)
	}
	if err := s.ShareTemplate(ctx, saver, tp.ID, true); err != nil {
		t.Fatal(err)
	}
	return tp
}

// turnKit starts and ends a turn of persona p in room, run by participant by on daemon d, and
// returns the skills and plugins it loaded, as "name:vN" lists.
func turnKit(t *testing.T, s *Store, room string, by *Participant, d int64, p *RoomPersona) (skills, plugins string) {
	t.Helper()
	ctx := t.Context()
	tn := &Turn{RoomID: room, ParticipantID: by.ID, DaemonID: d, Prompt: "go", PersonaID: p.ID, PersonaName: p.Name}
	if err := s.StartTurn(ctx, tn, false); err != nil {
		t.Fatal(err)
	}
	ks, err := s.TurnSkills(ctx, room, tn.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.TurnPlugins(ctx, room, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailTurn(ctx, room, tn.ID, "done"); err != nil {
		t.Fatal(err)
	}
	var a, b []string
	for _, k := range ks {
		a = append(a, fmt.Sprintf("%s:v%d", k.Name, k.Version))
	}
	for _, p := range ps {
		b = append(b, fmt.Sprintf("%s:v%d", p.Name, p.N))
	}
	return strings.Join(a, ","), strings.Join(b, ",")
}

// kitOf is persona p's skills and plugins as the room has them, "name:vN" and names.
func kitOf(t *testing.T, s *Store, room string, p *RoomPersona) (skills, plugins string) {
	t.Helper()
	got, err := s.RoomPersona(t.Context(), room, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var a, b []string
	for _, k := range got.Skills {
		a = append(a, fmt.Sprintf("%s:v%d", k.Name, k.Version))
	}
	rps, err := s.RoomPlugins(t.Context(), room)
	if err != nil {
		t.Fatal(err)
	}
	for _, rp := range rps {
		if slices.Contains(rp.Personas, p.ID) {
			b = append(b, rp.Name)
		}
	}
	return strings.Join(a, ","), strings.Join(b, ",")
}

// 035-AC10: under "approve everything", a room admin's copy of a library persona, held for the
// host, brings its skill in held for the host and its plugin at v1 waiting for the AI host's
// inspection, both on the new persona, and the add names them; once the host approves the persona,
// the skill and the plugin, the persona runs with them.
func TestAddPersonaKitHeld(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	tp := kitTemplate(t, s)
	alice, bob, _, d := approvingRoom(t, s, "h")
	p, m, err := s.AddPersona(ctx, "h", tp.ID, bob, true, 1<<20)
	if err != nil || !p.Pending || p.Waiting == nil || strings.Join(p.Waiting.Skills, ",") != "stamp" || strings.Join(p.Waiting.Plugins, ",") != "babs" {
		t.Fatalf("held add: %+v %v", p, err)
	}
	if want := `Bob added the persona 🔥 Grill-2, from the library persona "Grill" v1; waiting for the AI host: skills stamp; plugins babs. It waits for the host's approval.`; m.Body != want {
		t.Fatalf("line %q, want %q", m.Body, want)
	}
	if k, pl := kitOf(t, s, "h", p); k != "stamp:v1" || pl != "babs" {
		t.Fatalf("kit %q %q", k, pl)
	}
	if n := count(t, s, `SELECT count(*) FROM skills WHERE room_id = 'h' AND name = 'stamp' AND pending = 1 AND from_starter = 1`); n != 1 {
		t.Fatal("the skill is not held for the host as a starter's")
	}
	rps, _ := s.RoomPlugins(ctx, "h")
	if len(rps) != 1 || versionStates(&rps[0]) != "v1:inspecting" {
		t.Fatalf("plugins %+v", rps)
	}
	// A held skill never turns on by itself, however often the room looks.
	if names, err := s.ReleaseStarterSkills(ctx, "h"); err != nil || len(names) != 0 {
		t.Fatalf("released under approve everything: %v %v", names, err)
	}

	if _, err := s.DecideChange(ctx, "h", s.onlyChange(t, "h").ID, true, alice); err != nil {
		t.Fatal(err)
	}
	var sid int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM skills WHERE room_id = 'h' AND name = 'stamp'`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if k, pl := turnKit(t, s, "h", alice, d.ID, p); k != "" || pl != "" {
		t.Fatalf("ran with %q %q before the host approved them", k, pl)
	}
	if m, err := s.DecideRoomSkill(ctx, "h", sid, true, alice); err != nil || m.Body != "Alice approved the skill stamp v1 held for the host." {
		t.Fatalf("approve skill: %+v %v", m, err)
	}
	if _, state, err := s.ReportPluginVersion(ctx, "h", rps[0].Versions[0].ID, d.ID, true, nil, ""); err != nil || state != PluginPending {
		t.Fatalf("inspected: %s %v", state, err)
	}
	if _, err := s.DecidePluginVersion(ctx, "h", rps[0].ID, 1, true, alice); err != nil {
		t.Fatal(err)
	}
	if k, pl := turnKit(t, s, "h", alice, d.ID, p); k != "stamp:v1" || pl != "babs:v1" {
		t.Fatalf("ran with %q %q", k, pl)
	}

	// The host's own add is not held, nor is the skill it brings: only the plugin's inspection waits.
	host, _, _, _ := approvingRoom(t, s, "h2")
	p, m, err = s.AddPersona(ctx, "h2", tp.ID, host, true, 1<<20)
	if err != nil || p.Pending || len(p.Waiting.Skills) != 0 || strings.Join(p.Waiting.Plugins, ",") != "babs" ||
		m.Body != `Alice added the persona 🔥 Grill-2, from the library persona "Grill" v1; waiting for the AI host: plugins babs.` {
		t.Fatalf("host add: %+v %+v %v", p, m, err)
	}
}

// 035-AC10: with a host who approves without asking, the skill is on at once, as a starter's, and
// the plugin waits for the inspection only; a second copy reuses the room's skill, at its latest
// version, and its plugin, with no second row.
func TestAddPersonaKitFree(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	tp := kitTemplate(t, s)
	alice, _, _ := personaRoom(t, s, "f")
	d, err := s.PairDaemon(ctx, "f", alice.ID, "dmn-f")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "f", alice.ID); err != nil {
		t.Fatal(err)
	}
	p, m, err := s.AddPersona(ctx, "f", tp.ID, alice, true, 1<<20)
	if err != nil || p.Pending || len(p.Waiting.Skills) != 0 || strings.Join(p.Waiting.Plugins, ",") != "babs" {
		t.Fatalf("add: %+v %v", p, err)
	}
	if want := `Alice added the persona 🔥 Grill-2, from the library persona "Grill" v1; waiting for the AI host: plugins babs.`; m.Body != want {
		t.Fatalf("line %q, want %q", m.Body, want)
	}
	if k, pl := turnKit(t, s, "f", alice, d.ID, p); k != "stamp:v1" || pl != "" {
		t.Fatalf("ran with %q %q", k, pl)
	}
	rps, _ := s.RoomPlugins(ctx, "f")
	if _, state, err := s.ReportPluginVersion(ctx, "f", rps[0].Versions[0].ID, d.ID, true, nil, ""); err != nil || state != PluginApproved {
		t.Fatalf("inspected: %s %v", state, err)
	}
	if k, pl := turnKit(t, s, "f", alice, d.ID, p); k != "stamp:v1" || pl != "babs:v1" {
		t.Fatalf("ran with %q %q", k, pl)
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at)
		SELECT id, 2, 'Ali', 'd', 1, 6, 'y', 'tar v2', ? FROM skills WHERE room_id = 'f' AND name = 'stamp'`, now()); err != nil {
		t.Fatal(err)
	}
	// A refused update leaves the approved v1 running: the plugin is reused.
	if _, err := s.AddPluginVersion(ctx, "f", rps[0].ID, "main", snap("c2", "tar2"), alice, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE plugin_versions SET state = ? WHERE plugin_id = ? AND n = 2`, PluginRefused, rps[0].ID); err != nil {
		t.Fatal(err)
	}
	again, m, err := s.AddPersona(ctx, "f", tp.ID, alice, true, 1<<20)
	if err != nil || len(again.Waiting.Skills)+len(again.Waiting.Plugins) != 0 || m.Body != `Alice added the persona 🔥 Grill-3, from the library persona "Grill" v1.` {
		t.Fatalf("second add: %+v %+v %v", again, m, err)
	}
	if k, pl := kitOf(t, s, "f", again); k != "stamp:v2" || pl != "babs" {
		t.Fatalf("reused kit %q %q", k, pl)
	}
	if k, pl := turnKit(t, s, "f", alice, d.ID, again); k != "stamp:v2" || pl != "babs:v1" {
		t.Fatalf("reused kit ran with %q %q", k, pl)
	}
	if n := count(t, s, `SELECT (SELECT count(*) FROM skills WHERE room_id = 'f') + (SELECT count(*) FROM room_plugins WHERE room_id = 'f')`); n != 2 {
		t.Fatalf("%d skills and plugins, want 2", n)
	}
}

// 035-AC10: a plugin or a skill the room cannot take is left out, as a starter's plugin, with a
// line, and the persona goes without it; the rest of its kit still comes in. A plain member's copy
// comes without plugins, which only room admins add (009-AC5), a participant without a profile's
// without skills (004-AC10), and a room plugin of that name the room refused is not turned on.
func TestAddPersonaKitLeftOut(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	tp := kitTemplate(t, s)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		room          string
		admin, anon   bool
		quota         int64
		prep          func(room string, by *Participant)
		line, kit, on string
	}{
		{"full", true, false, 1 << 20, func(room string, by *Participant) {
			for i := range MaxRoomPlugins {
				addPlugin(t, s, room, by, fmt.Sprintf("p%d", i), "c1")
			}
		}, "its plugin babs: a room holds at most 10 plugins", "stamp:v1", ""},
		{"small", true, false, 16, nil, "its plugin babs: it would take the room past its quota", "stamp:v1", ""},
		{"skills", true, false, 1 << 20, func(room string, _ *Participant) {
			for i := range MaxRoomSkills {
				exec(`INSERT INTO skills(name, created_at, room_id) VALUES(?, ?, ?)`, fmt.Sprintf("k%d", i), now(), room)
			}
		}, "its skill stamp: a room holds at most 20 skills", "", "babs"},
		{"member", false, false, 1 << 20, nil, "its plugin babs: a room admin adds plugins", "stamp:v1", ""},
		{"anonymous", true, true, 1 << 20, nil, "its skill stamp: adding skills needs a profile", "", "babs"},
		{"refused", true, false, 1 << 20, func(room string, by *Participant) {
			addPlugin(t, s, room, by, "babs", "c1")
			exec(`UPDATE plugin_versions SET state = ? WHERE plugin_id IN (SELECT id FROM room_plugins WHERE room_id = ?)`, PluginRefused, room)
		}, "its plugin babs: it was refused in this room", "stamp:v1", ""},
	} {
		alice, _, _ := personaRoom(t, s, c.room)
		if c.prep != nil {
			c.prep(c.room, alice)
		}
		plugins := count(t, s, `SELECT count(*) FROM room_plugins WHERE room_id = ?`, c.room)
		by := alice
		if c.anon {
			cp := *alice
			by, cp.ProfileID = &cp, 0
		}
		p, _, err := s.AddPersona(ctx, c.room, tp.ID, by, c.admin, c.quota)
		// A brought plugin waits for its inspection; one left out is not waiting.
		if err != nil || strings.Join(p.Waiting.Plugins, ",") != c.on {
			t.Fatalf("%s: %+v %v", c.room, p, err)
		}
		if want := "🔥 Grill-2 comes without " + c.line + "."; lastBody(t, s, c.room) != want {
			t.Fatalf("%s: last line %q, want %q", c.room, lastBody(t, s, c.room), want)
		}
		if k, pl := kitOf(t, s, c.room, p); k != c.kit || pl != c.on {
			t.Fatalf("%s: kit %q %q", c.room, k, pl)
		}
		if c.on == "" && (count(t, s, `SELECT count(*) FROM room_plugins WHERE room_id = ?`, c.room) != plugins ||
			count(t, s, `SELECT count(*) FROM room_persona_plugins WHERE persona_id = ?`, p.ID) != 0) {
			t.Fatalf("%s: a plugin came in", c.room)
		}
	}
}

// 035-AC2: a library persona's history, newest first, for the profiles that see it only.
func TestTemplateHistory(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill := personaRoom(t, s, "a")
	for range 2 {
		if _, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID); err != nil {
			t.Fatal(err)
		}
	}
	tp, vs, err := s.TemplateHistory(ctx, templateOf(t, s, "a", grill.ID), alice.ProfileID)
	if err != nil || !tp.Mine || tp.Version != 2 || len(vs) != 2 || vs[0].N != 2 || vs[1].N != 1 || vs[0].Author != "Alice" {
		t.Fatalf("history: %+v %+v %v", tp, vs, err)
	}
	for _, id := range []int64{tp.ID, 0} {
		if _, _, err := s.TemplateHistory(ctx, id, bob.ProfileID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("history of %d for bob: %v", id, err)
		}
	}
	if err := s.ShareTemplate(ctx, alice.ProfileID, tp.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, vs, err := s.TemplateHistory(ctx, tp.ID, bob.ProfileID); err != nil || got.Mine || got.SourcePersonaID != 0 || len(vs) != 2 {
		t.Fatalf("shared history for bob: %+v %v", got, err)
	}
}

// templateOf is the library persona room persona id of room is linked to.
func templateOf(t *testing.T, s *Store, room string, id int64) int64 {
	t.Helper()
	p, err := s.RoomPersona(t.Context(), room, id)
	if err != nil || p.TemplateID == 0 {
		t.Fatalf("persona %d: %+v %v", id, p, err)
	}
	return p.TemplateID
}

// 035-AC4 035-AC5: share, unshare and duplicate refuse what their caller may not do, saying why.
func TestLibraryRefusals(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "a")
	tp, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	builtin := templateByPreset(t, s, PresetDefault).ID
	for _, c := range []struct {
		name  string
		id    int64
		share bool
		want  error
		text  string
	}{
		{"share a built-in", builtin, true, ErrPreset, ""},
		{"share an unknown one", 0, true, ErrNotFound, ""},
		{"unshare a personal one", tp.ID, false, ErrNotShared, "Grill is not shared"},
	} {
		if err := s.ShareTemplate(ctx, alice.ProfileID, c.id, c.share); !errors.Is(err, c.want) || c.text != "" && err.Error() != c.text {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if _, err := s.DuplicateTemplate(ctx, 0, builtin); !errors.Is(err, ErrForbidden) {
		t.Errorf("duplicate without a profile: %v", err)
	}
	if _, err := s.EditTemplate(ctx, alice.ProfileID, 0, TemplateEdit{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("edit an unknown one: %v", err)
	}
	if _, err := s.SavePersonaToLibrary(ctx, "a", 0, alice.ProfileID); !errors.Is(err, ErrNotFound) {
		t.Errorf("save an unknown persona: %v", err)
	}
}

// 013-AC16: a persona's default voice is stored once, when it has none.
func TestDefaultVoice(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, _, grill := personaRoom(t, s, "a")
	for i, want := range []bool{true, false} {
		if ok, err := s.DefaultVoice(ctx, "a", grill.ID, fmt.Sprintf("v%d", i)); err != nil || ok != want {
			t.Fatalf("default voice #%d: %v %v", i, ok, err)
		}
	}
	if p, _ := s.RoomPersona(ctx, "a", grill.ID); p.Voice != "v0" {
		t.Fatalf("voice %q", p.Voice)
	}
}

// 035: every library call answers a lost database with an error, never a false "not found".
func TestLibraryCallsFailWithTheDatabase(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "a")
	tp, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for name, err := range map[string]error{
		"list":    func() error { _, err := s.Templates(ctx, alice.ProfileID); return err }(),
		"one":     func() error { _, err := s.Template(ctx, tp.ID); return err }(),
		"history": func() error { _, _, err := s.TemplateHistory(ctx, tp.ID, alice.ProfileID); return err }(),
		"publish": func() error {
			_, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "X", grill.PersonaFields)
			return err
		}(),
		"edit": func() error {
			_, err := s.EditTemplate(ctx, alice.ProfileID, tp.ID, TemplateEdit{Version: 1})
			return err
		}(),
		"duplicate": func() error { _, err := s.DuplicateTemplate(ctx, alice.ProfileID, tp.ID); return err }(),
		"share":     s.ShareTemplate(ctx, alice.ProfileID, tp.ID, true),
		"remove":    s.RemoveTemplate(ctx, alice.ProfileID, tp.ID),
		"save":      func() error { _, err := s.SavePersonaToLibrary(ctx, "a", grill.ID, alice.ProfileID); return err }(),
		"add":       func() error { _, _, err := s.AddPersona(ctx, "a", tp.ID, alice, true, 1<<20); return err }(),
		"personas":  func() error { _, err := s.RoomPersonas(ctx, "a"); return err }(),
		"voice":     func() error { _, err := s.DefaultVoice(ctx, "a", grill.ID, "v"); return err }(),
	} {
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// 035-AC10 035-AC4: a library persona whose frozen files cannot be read is neither added, nor
// duplicated, nor edited: the call fails whole and the room keeps its personas.
func TestLibraryFilesLost(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	tp := kitTemplate(t, s)
	alice, _, _ := personaRoom(t, s, "b")
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE persona_template_files RENAME TO lost_files`); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"add":       func() error { _, _, err := s.AddPersona(ctx, "b", tp.ID, alice, true, 1<<20); return err }(),
		"duplicate": func() error { _, err := s.DuplicateTemplate(ctx, alice.ProfileID, tp.ID); return err }(),
		"edit": func() error {
			_, err := s.EditTemplate(ctx, tp.ProfileID, tp.ID, TemplateEdit{StarterPersona: StarterPersona{PersonaFields: tp.PersonaFields}, Version: tp.Version})
			return err
		}(),
	} {
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM room_personas WHERE room_id = 'b'`); n != 1 {
		t.Fatalf("%d personas after a failed add", n)
	}
}
