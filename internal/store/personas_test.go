package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// personaRoom is a room with Alice and Bob, each with a profile, and the room's Grill persona.
func personaRoom(t *testing.T, s *Store, room string) (alice, bob *Participant, grill RoomPersona) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, room, "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, room, "inv-"+room, 0); err != nil {
		t.Fatal(err)
	}
	var err error
	if alice, _, err = s.Join(ctx, "inv-"+room, "Alice", "a-"+room, JoinProfile{RecoveryHash: "ra-" + room, SessionHash: "sa-" + room}); err != nil {
		t.Fatal(err)
	}
	if bob, _, err = s.Join(ctx, "inv-"+room, "Bob", "b-"+room, JoinProfile{RecoveryHash: "rb-" + room, SessionHash: "sb-" + room}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Participant{alice, bob} {
		got, err := s.ParticipantByToken(ctx, map[string]string{"Alice": "a-", "Bob": "b-"}[p.Name]+room)
		if err != nil || got.ProfileID == 0 {
			t.Fatalf("%s: %+v %v", p.Name, got, err)
		}
		p.ProfileID = got.ProfileID
	}
	ps, err := s.RoomPersonas(ctx, room)
	if err != nil || len(ps) != 1 {
		t.Fatalf("personas: %+v %v", ps, err)
	}
	return alice, bob, ps[0]
}

func templateByPreset(t *testing.T, s *Store, preset string) Template {
	t.Helper()
	ts, err := s.Templates(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range ts {
		if tp.Preset == preset {
			return tp
		}
	}
	t.Fatalf("no preset %s in %+v", preset, ts)
	return Template{}
}

func lastBody(t *testing.T, s *Store, room string) string {
	t.Helper()
	msgs, err := s.Messages(context.Background(), room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return msgs[len(msgs)-1].Body
}

// 002-AC13 002-AC42: the library ships Default (Grill) and Secretary; a new room starts with Grill,
// copied from Default's latest version.
func TestPresetsAndNewRoom(t *testing.T) {
	s := openTest(t)
	def := templateByPreset(t, s, PresetDefault)
	sec := templateByPreset(t, s, "secretary")
	if def.Title != "Default" || def.Name != "Grill" || def.Version != 1 || def.Author != presetAuthor || def.Mine {
		t.Fatalf("default: %+v", def)
	}
	if sec.Title != "Secretary" || sec.Name != "Secretary" || strings.Contains(sec.Text, "challenge the") {
		t.Fatalf("secretary: %+v", sec)
	}
	for _, tp := range []Template{def, sec} {
		if !strings.Contains(tp.Text, "language the room speaks") || tp.Badge == "" || len(tp.Colour) != 7 {
			t.Fatalf("%s: a preset must have a badge, a colour and the room's language rule: %+v", tp.Title, tp)
		}
	}
	_, _, grill := personaRoom(t, s, "a")
	if grill.TemplateID != def.ID || grill.TemplateVersion != 1 || grill.PersonaFields != def.PersonaFields || grill.UpdateVersion != 0 {
		t.Fatalf("room persona: %+v", grill)
	}
}

// 002-AC14: a preset whose shipped fields change gets a new version at start; rooms are offered it.
func TestPresetChangeIsOffered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, grill := personaRoom(t, s, "a")
	_ = s.Close()
	old := presets[0].f.Text
	t.Cleanup(func() { presets[0].f.Text = old })
	presets[0].f.Text = old + "\nAlso check the budget.\n"
	for range 2 {
		if s, err = Open(path); err != nil {
			t.Fatal(err)
		}
		def := templateByPreset(t, s, PresetDefault)
		if def.Version != 2 || def.Text != presets[0].f.Text {
			t.Fatalf("after a preset change: %+v", def)
		}
		got, err := s.RoomPersona(context.Background(), "a", grill.ID)
		if err != nil || got.Text != old || got.TemplateVersion != 1 || got.UpdateVersion != 2 {
			t.Fatalf("room copy: %+v %v", got, err)
		}
		_ = s.Close()
	}
}

// 002-AC13: a profile publishes templates and versions of its own; versions are immutable with
// their author; the admin removes a published template, never a preset; room copies survive.
func TestTemplateLibrary(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, _ := personaRoom(t, s, "a")
	f := PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1 text", Model: "haiku", Effort: "low"}
	tp, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "Critic", f)
	if err != nil || tp.Version != 1 || tp.Author != "Alice" || tp.Title != "Critic" || tp.PersonaFields != f {
		t.Fatalf("publish: %+v %v", tp, err)
	}
	f2 := f
	f2.Text = "v2 text"
	if _, err := s.PublishVersion(ctx, bob.ProfileID, tp.ID, "Bob", f2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another profile's version: %v", err)
	}
	def := templateByPreset(t, s, PresetDefault)
	if _, err := s.PublishVersion(ctx, alice.ProfileID, def.ID, "Alice", f2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("version of a preset: %v", err)
	}
	if _, err := s.PublishVersion(ctx, alice.ProfileID, 999, "Alice", f2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown template: %v", err)
	}
	v2, err := s.PublishVersion(ctx, alice.ProfileID, tp.ID, "Alice A.", f2)
	if err != nil || v2.Version != 2 || v2.Text != "v2 text" || v2.Author != "Alice A." {
		t.Fatalf("version 2: %+v %v", v2, err)
	}
	var v1 string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM persona_template_versions WHERE template_id = ? AND n = 1`, tp.ID).Scan(&v1); err != nil || v1 != "v1 text" {
		t.Fatalf("version 1 changed: %q %v", v1, err)
	}
	ts, err := s.Templates(ctx, alice.ProfileID)
	if err != nil || len(ts) != 3 || ts[0].Preset != PresetDefault || ts[2].ID != tp.ID || !ts[2].Mine || ts[0].Mine {
		t.Fatalf("library for Alice: %+v %v", ts, err)
	}
	if ts, _ := s.Templates(ctx, bob.ProfileID); ts[2].Mine {
		t.Fatal("Bob sees Alice's template as his")
	}

	p, _, err := s.AddPersona(ctx, "a", tp.ID, bob)
	if err != nil || p.TemplateVersion != 2 || p.Text != "v2 text" {
		t.Fatalf("persona from it: %+v %v", p, err)
	}
	if err := s.RemoveTemplate(ctx, def.ID); !errors.Is(err, ErrPreset) {
		t.Fatalf("remove a preset: %v", err)
	}
	if err := s.RemoveTemplate(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove unknown: %v", err)
	}
	if err := s.RemoveTemplate(ctx, tp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Template(ctx, tp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed template: %v", err)
	}
	got, err := s.RoomPersona(ctx, "a", p.ID)
	if err != nil || got.TemplateID != 0 || got.Text != "v2 text" || got.UpdateVersion != 0 {
		t.Fatalf("room copy after removal: %+v %v", got, err)
	}

	// Deleting a profile removes the templates it published, not the presets nor room copies.
	again, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "Again", f)
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := s.AddPersona(ctx, "a", again.ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(ctx, alice.ProfileID); err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM persona_template_versions WHERE template_id = ?`, again.ID).Scan(&versions); err != nil || versions != 0 {
		t.Fatalf("versions left: %d %v", versions, err)
	}
	if ts, _ := s.Templates(ctx, 0); len(ts) != 2 {
		t.Fatalf("library after the profile went: %+v", ts)
	}
	if got, err := s.RoomPersona(ctx, "a", q.ID); err != nil || got.Name != "Critic-2" || got.TemplateID != 0 {
		t.Fatalf("room copy after the profile went: %+v %v", got, err)
	}
}

// 002-AC14 002-AC15: the room copy keeps its own changes; a newer template version is offered and
// applied only on request, keeping the room's name, badge and colour; every change is in the chat.
func TestRoomPersonaOverridesAndUpdate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, grill := personaRoom(t, s, "a")
	sec := templateByPreset(t, s, "secretary")
	p, m, err := s.AddPersona(ctx, "a", sec.ID, bob)
	if err != nil || p.Name != "Secretary" || m.ParticipantID != bob.ID || m.Kind != KindSystem ||
		m.Body != `Bob added the persona 📝 Secretary, from the template "Secretary" v1.` {
		t.Fatalf("add: %+v %+v %v", p, m, err)
	}
	if _, _, err := s.AddPersona(ctx, "a", 999, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown template: %v", err)
	}
	if _, _, err := s.AddPersona(ctx, "gone", sec.ID, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
	model, effort, text, name, colour := "haiku", "low", "Only decisions.", "Scribe", "#000000"
	p, m, _, err = s.EditPersona(ctx, "a", p.ID, PersonaPatch{Model: &model, Effort: &effort, Text: &text, Name: &name, Colour: &colour}, alice)
	if err != nil || p.Name != "Scribe" || p.Model != "haiku" || p.Effort != "low" || p.Text != text || p.Badge != "📝" ||
		m.Body != "Alice changed 📝 Secretary: name Secretary → Scribe, colour #1971c2 → #000000, model default → haiku, effort default → low, persona text." {
		t.Fatalf("edit: %+v %+v %v", p, m, err)
	}
	if _, m, _, err := s.EditPersona(ctx, "a", p.ID, PersonaPatch{Model: &model}, alice); err != nil || m != nil {
		t.Fatalf("no-op edit: %+v %v", m, err)
	}
	n, on := 3, true
	if _, m, _, err := s.EditPersona(ctx, "a", p.ID, PersonaPatch{EveryN: &n, SelfCall: &on}, alice); err != nil ||
		m.Body != "Alice changed 📝 Scribe: every N rounds off → 3, self-call off → on." {
		t.Fatalf("trigger fields: %+v %v", m, err)
	}
	upper := "GRILL"
	if _, _, _, err := s.EditPersona(ctx, "a", p.ID, PersonaPatch{Name: &upper}, alice); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("name taken whatever its case: %v", err)
	}
	if _, _, _, err := s.EditPersona(ctx, "a", 999, PersonaPatch{Name: &upper}, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown persona: %v", err)
	}
	if _, _, _, err := s.EditPersona(ctx, "b", grill.ID, PersonaPatch{Name: &upper}, alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("persona of another room: %v", err)
	}
	// A second Grill gets a number.
	def := templateByPreset(t, s, PresetDefault)
	if g2, _, err := s.AddPersona(ctx, "a", def.ID, bob); err != nil || g2.Name != "Grill-2" {
		t.Fatalf("second grill: %+v %v", g2, err)
	}

	// A new version is offered, not applied.
	tp, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "Critic", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.AddPersona(ctx, "a", tp.ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	badge := "🔎"
	if _, _, _, err := s.EditPersona(ctx, "a", c.ID, PersonaPatch{Badge: &badge}, alice); err != nil {
		t.Fatal(err)
	}
	if _, m, _, err := s.UpdatePersona(ctx, "a", c.ID, alice); err != nil || m != nil {
		t.Fatalf("update with none offered: %+v %v", m, err)
	}
	if _, err := s.PublishVersion(ctx, alice.ProfileID, tp.ID, "Alice", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v2", Model: "sonnet", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	c, err = s.RoomPersona(ctx, "a", c.ID)
	if err != nil || c.Text != "v1" || c.TemplateVersion != 1 || c.UpdateVersion != 2 {
		t.Fatalf("offered: %+v %v", c, err)
	}
	c, m, _, err = s.UpdatePersona(ctx, "a", c.ID, bob)
	if err != nil || c.Text != "v2" || c.Model != "sonnet" || c.Effort != "high" || c.Badge != "🔎" || c.TemplateVersion != 2 || c.UpdateVersion != 0 ||
		m.Body != `Bob updated 🔎 Critic to version 2 of the template "Critic".` {
		t.Fatalf("update: %+v %+v %v", c, m, err)
	}
	if got := lastBody(t, s, "a"); got != m.Body {
		t.Fatalf("the update is not in the chat: %q", got)
	}
}

// 002-AC16 002-AC17: removing a persona drops its sessions and queued turn and keeps what it wrote;
// refused while its turn runs.
func TestRemovePersona(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	tn := r.start(t, s, "a")
	if _, err := s.RemovePersona(ctx, "a", r.grill.ID, r.p); !errors.Is(err, ErrPersonaBusy) {
		t.Fatalf("remove while running: %v", err)
	}
	if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{Synthesis: "done", SessionID: "s1", Session: []byte("x\n")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "a", PersonaID: r.grill.ID, ParticipantID: r.p.ID}); err != nil {
		t.Fatal(err)
	}
	m, err := s.RemovePersona(ctx, "a", r.grill.ID, r.p)
	if err != nil || m.Body != "Alice removed the persona 🔥 Grill." {
		t.Fatalf("remove: %+v %v", m, err)
	}
	if _, err := s.RemovePersona(ctx, "a", r.grill.ID, r.p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if q, _ := s.Queue(ctx, "a"); len(q) != 0 {
		t.Fatalf("queue kept: %+v", q)
	}
	if _, err := s.AgentSession(ctx, "a", r.grill.ID, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session kept: %v", err)
	}
	msgs, _ := s.Messages(ctx, "a", 0, 100)
	for _, m := range msgs {
		if m.Kind == KindAgent && (m.Sender != "Grill" || m.PersonaID != 0 || m.Badge != "") {
			t.Fatalf("synthesis after removal: %+v", m)
		}
	}
	if done, err := s.Turn(ctx, "a", tn.ID); err != nil || done.PersonaID != 0 || done.PersonaName != "Grill" {
		t.Fatalf("turn after removal: %+v %v", done, err)
	}
}

// 002-AC17: one queue per room, first in first out, one entry per persona, cancelled only by who
// queued it.
func TestQueue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, grill := personaRoom(t, s, "a")
	_, _, other := personaRoom(t, s, "b")
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PopQueued(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pop empty: %v", err)
	}
	first := &QueuedTurn{RoomID: "a", PersonaID: sec.ID, ParticipantID: bob.ID}
	second := &QueuedTurn{RoomID: "a", PersonaID: grill.ID, ParticipantID: alice.ID, Fresh: true}
	for _, q := range []*QueuedTurn{first, second} {
		if err := s.Enqueue(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "a", PersonaID: sec.ID, ParticipantID: alice.ID}); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("persona queued twice: %v", err)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "a", PersonaID: other.ID, ParticipantID: alice.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("persona of another room: %v", err)
	}
	q, err := s.Queue(ctx, "a")
	if err != nil || len(q) != 2 || q[0].ID != first.ID || q[1].ID != second.ID || !q[1].Fresh || q[0].Source != SourceButton {
		t.Fatalf("queue: %+v %v", q, err)
	}
	if err := s.CancelQueued(ctx, "a", first.ID, alice.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cancel someone else's: %v", err)
	}
	if err := s.CancelQueued(ctx, "b", first.ID, bob.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel through another room: %v", err)
	}
	head, err := s.PopQueued(ctx, "a")
	if err != nil || head.ID != first.ID || head.PersonaID != sec.ID {
		t.Fatalf("pop: %+v %v", head, err)
	}
	if err := s.CancelQueued(ctx, "a", second.ID, alice.ID, false); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.Queue(ctx, "a"); len(q) != 0 {
		t.Fatalf("queue after cancel: %+v", q)
	}
}

// 002-AC16: each persona has its own session and cursors; another persona never resumes it.
func TestSessionsPerPersona(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, r.p)
	if err != nil {
		t.Fatal(err)
	}
	run := func(p RoomPersona, sid string, off int64, data string) *Turn {
		t.Helper()
		tn := &Turn{RoomID: "a", ParticipantID: r.p.ID, DaemonID: r.daemon, Prompt: "go", PersonaID: p.ID, PersonaName: p.Name}
		if err := s.StartTurn(ctx, tn); err != nil {
			t.Fatal(err)
		}
		done, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{Synthesis: "by " + p.Name, Model: "m", Effort: "low", SessionID: sid, SessionOffset: off, Session: []byte(data)})
		if err != nil {
			t.Fatal(err)
		}
		return done
	}
	g1 := run(r.grill, "sg", 0, "g1\n")
	s1 := run(*sec, "ss", 0, "s1\n")
	g2 := run(r.grill, "sg", 3, "g2\n")
	if d, err := s.AgentSession(ctx, "a", r.grill.ID, "sg"); err != nil || string(d) != "g1\ng2\n" {
		t.Fatalf("grill session: %q %v", d, err)
	}
	if _, err := s.AgentSession(ctx, "a", sec.ID, "sg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("secretary reads grill's session: %v", err)
	}
	if _, err := s.AgentSession(ctx, "b", r.grill.ID, "sg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session through another room: %v", err)
	}
	if last, err := s.LastDoneTurn(ctx, "a", r.grill.ID); err != nil || last.ID != g2.ID || g1.ID == g2.ID {
		t.Fatalf("grill's last turn: %+v %v", last, err)
	}
	if last, err := s.LastDoneTurn(ctx, "a", sec.ID); err != nil || last.ID != s1.ID || last.SessionID != "ss" || last.Effort != "low" {
		t.Fatalf("secretary's last turn: %+v %v", last, err)
	}
	msgs, _ := s.Messages(ctx, "a", 0, 100)
	m := msgs[len(msgs)-1]
	if m.Sender != "Grill" || m.PersonaID != r.grill.ID || m.Effort != "low" || m.Model != "m" {
		t.Fatalf("synthesis label: %+v", m)
	}
}

// legacyRoom fills a schema-6 (pre-002) database with what an agent turn left there: a done turn
// and its session, a synthesis, a failure line, a document with its revision and a thread reply.
func legacyRoom(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	ts := now()
	for _, q := range []string{
		`INSERT INTO rooms(id, name, created_at) VALUES('old', 'Old room', '` + ts + `')`,
		`INSERT INTO invites(room_id, token_hash, created_at) VALUES('old', 'inv', '` + ts + `')`,
		`INSERT INTO participants(room_id, invite_id, name, token_hash, joined_at) VALUES('old', 1, 'Agent', 'tok', '` + ts + `')`,
		`INSERT INTO daemons(room_id, participant_id, name, token_hash, created_at) VALUES('old', 1, 'laptop', 'dmn', '` + ts + `')`,
		`INSERT INTO turns(room_id, participant_id, daemon_id, host, status, fresh, resume, session_id, message_cursor, annotation_cursor, prompt, started_at, model)
			VALUES('old', 1, 1, 'laptop', 'done', 0, '', 'sid-old', 2, 0, 'go', '` + ts + `', 'claude-haiku')`,
		`INSERT INTO agent_sessions(room_id, session_id, data, updated_at) VALUES('old', 'sid-old', X'7B7D0A', '` + ts + `')`,
		`INSERT INTO messages(room_id, ts, kind, participant_id, sender, body) VALUES('old', '` + ts + `', 'text', 1, 'Agent', 'I am a person named Agent')`,
		`INSERT INTO messages(room_id, ts, kind, sender, body, model) VALUES('old', '` + ts + `', 'agent', 'Agent', 'synthesis', 'claude-haiku')`,
		`INSERT INTO messages(room_id, ts, kind, sender, body) VALUES('old', '` + ts + `', 'system', 'Agent', 'Agent turn failed: boom')`,
		`INSERT INTO documents(room_id, kind, name, format, author, created_at) VALUES('old', 'agent', 'plan.md', 'md', 'Agent', '` + ts + `')`,
		`INSERT INTO revisions(document_id, n, body, author, created_at) VALUES(1, 1, '# Plan', 'Agent', '` + ts + `')`,
		`INSERT INTO annotations(room_id, document_id, revision, created_at) VALUES('old', 1, 1, '` + ts + `')`,
		`INSERT INTO annotation_messages(annotation_id, author_kind, participant_id, author, body, created_at) VALUES(1, 'participant', 1, 'Agent', 'why?', '` + ts + `')`,
		`INSERT INTO annotation_messages(annotation_id, author_kind, author, body, created_at) VALUES(1, 'agent', 'Agent', 'because', '` + ts + `')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// 002-AC42: a pre-002 room gets the Default template's Grill persona, which takes over the room's
// turns, session and everything the agent signed; a participant who happens to be named Agent
// keeps their lines.
func TestMigrationToPersonas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(context.Background(), db, 6); err != nil {
		t.Fatal(err)
	}
	legacyRoom(t, db)
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	ps, err := s.RoomPersonas(ctx, "old")
	def := templateByPreset(t, s, PresetDefault)
	if err != nil || len(ps) != 1 || ps[0].Name != "Grill" || ps[0].TemplateID != def.ID || ps[0].TemplateVersion != 1 {
		t.Fatalf("personas: %+v %v", ps, err)
	}
	grill := ps[0]
	last, err := s.LastDoneTurn(ctx, "old", grill.ID)
	if err != nil || last.SessionID != "sid-old" || last.PersonaName != "Grill" || last.MessageCursor != 2 {
		t.Fatalf("last turn: %+v %v", last, err)
	}
	if data, err := s.AgentSession(ctx, "old", grill.ID, "sid-old"); err != nil || string(data) != "{}\n" {
		t.Fatalf("session: %q %v", data, err)
	}
	msgs, _ := s.Messages(ctx, "old", 0, 10)
	want := []struct {
		sender, body string
		persona      int64
	}{{"Agent", "I am a person named Agent", 0}, {"Grill", "synthesis", grill.ID}, {"Grill", "Grill turn failed: boom", 0}}
	for i, w := range want {
		if m := msgs[i]; m.Sender != w.sender || m.Body != w.body || m.PersonaID != w.persona {
			t.Fatalf("message %d: %+v, want %+v", i, m, w)
		}
	}
	if msgs[1].Badge != "🔥" {
		t.Fatalf("synthesis badge: %+v", msgs[1])
	}
	docs, _ := s.Documents(ctx, "old")
	rev, err := s.Revision(ctx, docs[0].ID, 1)
	if docs[0].Author != "Grill" || err != nil || rev.Author != "Grill" {
		t.Fatalf("document %+v, revision %+v %v", docs[0], rev, err)
	}
	anns, _ := s.Annotations(ctx, "old", docs[0].ID)
	if th := anns[0].Messages; th[0].Author != "Agent" || th[0].PersonaID != 0 || th[1].Author != "Grill" || th[1].PersonaID != grill.ID {
		t.Fatalf("thread: %+v", th)
	}
}

// 002-AC42 on a copy of a real pre-002 database, named by GRILL_ROOM_PRE002_DB (never committed:
// it holds a real session). Each room gets one Grill holding its agent output and its session.
func TestMigrationOnRealDatabase(t *testing.T) {
	src := os.Getenv("GRILL_ROOM_PRE002_DB")
	if src == "" {
		t.Skip("GRILL_ROOM_PRE002_DB is not set")
	}
	path := filepath.Join(t.TempDir(), "copy.db")
	copyFile(t, src, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	rooms, err := s.Rooms(ctx)
	if err != nil || len(rooms) == 0 {
		t.Fatalf("rooms: %+v %v", rooms, err)
	}
	var resumable int
	for _, r := range rooms {
		ps, err := s.RoomPersonas(ctx, r.ID)
		if err != nil || len(ps) != 1 || ps[0].Name != "Grill" {
			t.Fatalf("room %s personas: %+v %v", r.ID, ps, err)
		}
		msgs, err := s.Messages(ctx, r.ID, 0, 100000)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.Kind == KindAgent && (m.Sender != "Grill" || m.PersonaID != ps[0].ID) {
				t.Fatalf("room %s synthesis not Grill's: %+v", r.ID, m)
			}
			if m.ParticipantID == 0 && m.Sender == AgentName {
				t.Fatalf("room %s line still signed Agent: %+v", r.ID, m)
			}
		}
		last, err := s.LastDoneTurn(ctx, r.ID, ps[0].ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := s.AgentSession(ctx, r.ID, ps[0].ID, last.SessionID)
		if err != nil || len(data) == 0 {
			t.Fatalf("room %s session %s: %d bytes, %v", r.ID, last.SessionID, len(data), err)
		}
		t.Logf("room %s: Grill resumes session %s (%d bytes) after message %d", r.ID, last.SessionID, len(data), last.MessageCursor)
		resumable++
	}
	if resumable == 0 {
		t.Fatal("no room had a session to resume")
	}
	var orphans int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM turns WHERE persona_id IS NULL`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("turns without a persona: %d %v", orphans, err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src) //nolint:gosec // a test fixture path from the environment
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// A member cannot grow a room's personas, nor a profile the library, without bound.
func TestPersonaLimits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, _, _ := personaRoom(t, s, "a")
	sec := templateByPreset(t, s, "secretary")
	for i := 1; i < MaxRoomPersonas; i++ {
		if _, _, err := s.AddPersona(ctx, "a", sec.ID, alice); err != nil {
			t.Fatalf("persona %d: %v", i+1, err)
		}
	}
	if _, _, err := s.AddPersona(ctx, "a", sec.ID, alice); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "12 personas per room") {
		t.Fatalf("persona past the limit: %v", err)
	}
	f := PersonaFields{Name: "T", Badge: "x", Colour: "#000000", Text: "t"}
	var last *Template
	for i := 0; i < MaxProfileTemplates; i++ {
		var err error
		if last, err = s.PublishTemplate(ctx, alice.ProfileID, "Alice", "T", f); err != nil {
			t.Fatalf("template %d: %v", i+1, err)
		}
	}
	if _, err := s.PublishTemplate(ctx, alice.ProfileID, "Alice", "T", f); !errors.Is(err, ErrLimit) {
		t.Fatalf("template past the limit: %v", err)
	}
	for n := 2; n <= MaxTemplateVersions; n++ {
		if err := insertVersion(ctx, s.db, last.ID, n, "Alice", f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PublishVersion(ctx, alice.ProfileID, last.ID, "Alice", f); !errors.Is(err, ErrLimit) {
		t.Fatalf("version past the limit: %v", err)
	}
}
