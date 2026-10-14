package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testSetup() *RoomSetup {
	return &RoomSetup{From: &Provenance{Title: "Weekly", Version: 2}, Agenda: "- [ ] news",
		Personas: []StarterPersona{{PersonaFields: PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#112233", Text: "t", Model: "sonnet", Effort: "high"},
			EveryVoice: 2, Skills: []string{"stamp"}}},
		Skills: []StarterSkill{{Name: "stamp", Version: 3, Author: "Alice", Description: "d", Files: 1, Size: 4, SHA256: "x", File: "skill/0"}},
		Documents: []StarterDocument{{Name: "plan.md", Kind: DocUpload, Format: "md", ContentType: "text/markdown", Text: "# Plan", File: "doc/0"},
			{Name: "notes.md", Kind: DocAgent, Format: "md", Text: "# Notes"}},
		Files: map[string][]byte{"skill/0": []byte("tar!"), "doc/0": []byte("# Plan")}, SkillsPending: true,
		Chain: StarterChain{Budget: 9, Suspended: true}, Glossary: "Kafka", UploadQuota: 1 << 20}
}

// 006-AC11 006-AC13 006-AC14: a room set up from a starter holds its copies: personas with their
// trigger and skill pins, documents, the agenda first, pending skills, chain and glossary, its
// provenance and a chat line; nothing else, the Default persona included.
func TestSetUpRoom(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	r, err := s.CreateProfileRoom(ctx, "r", "Weekly", alice.ProfileID, "Ali", "inv-r", "tok-r", testSetup())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Room(ctx, "r")
	if *got.CreatedFrom != (Provenance{"Weekly", 2}) || got.Chain.Budget != 9 || !got.Chain.Suspended || r.CreatedFrom == nil {
		t.Fatalf("room: %+v", got)
	}
	ps, _ := s.RoomPersonas(ctx, "r")
	if len(ps) != 1 || ps[0].Name != "Grill" || ps[0].EveryVoice != 2 || ps[0].NextVoice != 2 || len(ps[0].Skills) != 1 || ps[0].Skills[0].Version != 3 {
		t.Fatalf("personas: %+v", ps)
	}
	docs, _ := s.Documents(ctx, "r")
	var names []string
	for _, d := range docs {
		names = append(names, d.Kind+":"+d.Name)
	}
	if strings.Join(names, ",") != "agent:agenda.md,upload:plan.md,agent:notes.md" || docs[1].Size != 6 || docs[1].Revisions != 1 {
		t.Fatalf("documents: %v %+v", names, docs)
	}
	ks, _ := s.RoomSkills(ctx, "r")
	if len(ks) != 1 || !ks[0].Pending || !ks[0].FromStarter {
		t.Fatalf("skills: %+v", ks)
	}
	if g, _ := s.STTGlossary(ctx, "r"); g != "Kafka" {
		t.Fatalf("glossary %q", g)
	}
	ms, _ := s.Messages(ctx, "r", 0, 10)
	if len(ms) != 2 || ms[1].Body != `Ali created the room from the starter "Weekly" v2.` {
		t.Fatalf("messages: %+v", ms)
	}
	// Two voice lines make Grill due; a turn of it starts the count again.
	for range 2 {
		if err := s.InsertMessage(ctx, &Message{RoomID: "r", Kind: KindVoice, ParticipantID: ms[0].ParticipantID, Sender: "Ali", Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if due, _ := s.DueVoicePersonas(ctx, "r"); len(due) != 1 || due[0].NextVoice != 0 {
		t.Fatalf("due: %+v", due)
	}
	last, _ := s.LastMessageID(ctx)
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET mark_msg_id = ? WHERE room_id = 'r'`, last); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueVoicePersonas(ctx, "r"); len(due) != 0 {
		t.Fatalf("due after the mark: %+v", due)
	}
}

// 006-AC11 004-AC9: a starter whose documents pass the room's upload quota, or that names a blob
// it lacks, creates nothing.
func TestSetUpRoomRefusals(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	full := testSetup()
	full.UploadQuota = 3
	lacking := testSetup()
	delete(lacking.Files, "skill/0")
	noDoc := testSetup()
	delete(noDoc.Files, "doc/0")
	for name, c := range map[string]struct {
		setup *RoomSetup
		want  string
	}{"quota": {full, "room creation refused"}, "skill blob": {lacking, `no file "skill/0"`}, "doc blob": {noDoc, `no file "doc/0"`}} {
		_, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-"+name, "tok-"+name, c.setup)
		if err == nil || !strings.Contains(err.Error(), c.want) || (name == "quota" && !errors.Is(err, ErrCreationRefused)) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := s.Room(ctx, "r"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: room left behind", name)
		}
	}
}

// 006-AC18: the timer is due T minutes after the persona's mark, only with a voice or text line
// since; a system line is no activity.
func TestDueTimerPersonas(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	setup := testSetup()
	setup.Personas[0].EveryVoice, setup.Personas[0].EveryMinutes = 0, 5
	if _, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", setup); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(6 * time.Minute)
	if due, _ := s.DueTimerPersonas(ctx, later, ""); len(due) != 0 {
		t.Fatalf("due with only system lines: %+v", due)
	}
	if err := s.InsertMessage(ctx, &Message{RoomID: "r", Kind: KindText, Sender: "Ali", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueTimerPersonas(ctx, time.Now().Add(4*time.Minute), "r"); len(due) != 0 {
		t.Fatalf("due early: %+v", due)
	}
	if due, _ := s.DueTimerPersonas(ctx, later, ""); len(due) != 1 || due[0].RoomID != "r" {
		t.Fatalf("not due: %+v", due)
	}
}

// 006-AC13: a starter's pending skills turn on once the host approves without asking, and only
// those: a skill copied in under "approve everything" keeps waiting.
func TestReleaseStarterSkills(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if _, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", testSetup()); err != nil {
		t.Fatal(err)
	}
	if names, err := s.ReleaseStarterSkills(ctx, "r"); err != nil || len(names) != 0 {
		t.Fatalf("no host: %v %v", names, err)
	}
	ps, _ := s.Participants(ctx, "r")
	d, err := s.PairDaemon(ctx, "r", ps[0].ID, "dtok")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "r", ps[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES('held', ?, 'r', 1)`, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDaemonSettings(ctx, d.ID, DaemonSettings{ApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.ReleaseStarterSkills(ctx, "r"); len(names) != 0 {
		t.Fatalf("released under approve everything: %v", names)
	}
	if _, err := s.SetDaemonSettings(ctx, d.ID, DaemonSettings{}); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.ReleaseStarterSkills(ctx, "r"); strings.Join(names, ",") != "stamp" {
		t.Fatalf("released %v", names)
	}
	if n := count(t, s, `SELECT count(*) FROM skills WHERE room_id = 'r' AND pending = 1`); n != 1 {
		t.Fatalf("%d pending, want the copy-in only", n)
	}
}

// voices adds n voice lines to room by participant pid.
func voices(t *testing.T, s *Store, room string, pid int64, n int) {
	t.Helper()
	for range n {
		if err := s.InsertMessage(t.Context(), &Message{RoomID: room, Kind: KindVoice, ParticipantID: pid, Sender: "Ali", Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
}

// 006-AC11 006-AC18: an exported room's created_from and triggers travel; its imported lines are
// history, so the copy's voice-line count starts from the import.
func TestImportKeepsTriggersQuiet(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	// Without skills: the import checks their tars, which the test setup fakes.
	setup := testSetup()
	setup.Skills, setup.Personas[0].Skills = nil, nil
	if _, err := s.CreateProfileRoom(ctx, "orig", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", setup); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "orig")
	voices(t, s, "orig", ps[0].ID, 3)
	if due, _ := s.DueVoicePersonas(ctx, "orig"); len(due) != 1 {
		t.Fatalf("source not due: %+v", due)
	}
	if _, err := s.ImportRoom(ctx, export(t, s, "orig"), importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	cp, _ := s.RoomPersonas(ctx, "copy")
	if due, _ := s.DueVoicePersonas(ctx, "copy"); len(due) != 0 || len(cp) != 1 || cp[0].EveryVoice != 2 || cp[0].NextVoice != 2 {
		t.Fatalf("copy: due %+v, personas %+v", due, cp)
	}
	if r, _ := s.Room(ctx, "copy"); r.CreatedFrom == nil || *r.CreatedFrom != (Provenance{"Weekly", 2}) {
		t.Fatalf("copy provenance: %+v", r.CreatedFrom)
	}
}

// 006-AC12: the starter's documents and skills count against the instance storage quota.
func TestSetUpRoomStorageQuota(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	used, _ := s.UsedBytes(ctx)
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used + 32<<10}); err != nil {
		t.Fatal(err)
	}
	setup := testSetup()
	setup.Files["doc/0"] = make([]byte, 64<<10)
	_, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", setup)
	if !errors.Is(err, ErrCreationRefused) || !strings.Contains(err.Error(), "the starter's") {
		t.Fatalf("past the quota: %v", err)
	}
	if _, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", testSetup()); err != nil {
		t.Fatalf("within the quota: %v", err)
	}
}

// 006-AC18: turning a voice-line trigger on counts from then, not from a turn long past; a change
// of its N keeps the count.
func TestTriggerOnCountsFromNow(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	setup := testSetup()
	setup.Personas[0].EveryVoice = 0
	if _, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", setup); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "r")
	voices(t, s, "r", ps[0].ID, 5)
	grill, _ := s.RoomPersonas(ctx, "r")
	two, three := 2, 3
	if _, _, _, err := s.EditPersona(ctx, "r", grill[0].ID, PersonaPatch{EveryVoice: &two}, &ps[0]); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueVoicePersonas(ctx, "r"); len(due) != 0 {
		t.Fatalf("due from old lines: %+v", due)
	}
	voices(t, s, "r", ps[0].ID, 2)
	if _, _, _, err := s.EditPersona(ctx, "r", grill[0].ID, PersonaPatch{EveryVoice: &three}, &ps[0]); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.RoomPersona(ctx, "r", grill[0].ID); p.NextVoice != 1 {
		t.Fatalf("next voice %d, want 1", p.NextVoice)
	}
}

// 006-AC13: under "approve everything", the host's answer names the starter's skill as such.
func TestDecideStarterSkill(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if _, err := s.CreateProfileRoom(ctx, "r", "R", alice.ProfileID, "Ali", "inv-r", "tok-r", testSetup()); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "r")
	if err := s.SetHost(ctx, "r", ps[0].ID); err != nil {
		t.Fatal(err)
	}
	ks, _ := s.RoomSkills(ctx, "r")
	m, err := s.DecideRoomSkill(ctx, "r", ks[0].ID, true, &ps[0])
	if err != nil || m.Body != "Ali approved the starter's skill stamp v3." {
		t.Fatalf("decide: %+v %v", m, err)
	}
	if ks, _ := s.RoomSkills(ctx, "r"); ks[0].Pending || ks[0].FromStarter {
		t.Fatalf("after: %+v", ks)
	}
}

// 006-AC15 006-AC17: a room's draft holds its personas, the skills they use, agenda.md as the
// agenda and its other documents at their current revision, blobs only on demand; a pending
// persona or skill, older revisions and the chat are left out.
func TestRoomDraft(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	setup := testSetup()
	setup.SkillsPending = false
	if _, err := s.CreateProfileRoom(ctx, "r", "Weekly", alice.ProfileID, "Ali", "inv-r", "tok-r", setup); err != nil {
		t.Fatal(err)
	}
	docs, _ := s.Documents(ctx, "r")
	if _, err := s.AddRevision(ctx, "r", docs[2].ID, "# Notes v2", "Grill"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES('held', ?, 'r', 1)`, now()); err != nil {
		t.Fatal(err)
	}
	// An agent session holds everything Claude read: never in a starter (006-AC17).
	ps, _ := s.RoomPersonas(ctx, "r")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_sessions(room_id, persona_id, session_id, data, updated_at) VALUES('r', ?, 'sid', 'SECRET-SESSION', ?)`,
		ps[0].ID, now()); err != nil {
		t.Fatal(err)
	}
	m, err := s.RoomDraft(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(m); strings.Contains(string(raw), "SECRET-SESSION") {
		t.Fatal("the draft holds an agent session")
	}
	if m.Title != "Weekly" || m.Agenda != "- [ ] news" || len(m.Personas) != 1 || m.Personas[0].EveryVoice != 2 || strings.Join(m.Personas[0].Skills, ",") != "stamp" ||
		len(m.Skills) != 1 || m.Skills[0].Version != 3 || len(m.Documents) != 2 || m.Documents[1].Text != "# Notes v2" || m.Documents[0].File == "" ||
		m.Chain.Budget != 9 || m.Glossary != "Kafka" {
		t.Fatalf("draft: %+v", m)
	}
	files, err := s.RoomDraftFiles(ctx, "r", []string{m.Documents[0].File, m.Skills[0].File})
	for _, f := range files {
		if strings.Contains(string(f), "SECRET-SESSION") {
			t.Fatal("a draft blob holds an agent session")
		}
	}
	if err != nil || string(files[m.Documents[0].File]) != "# Plan" || string(files[m.Skills[0].File]) != "tar!" || len(files) != 2 {
		t.Fatalf("files: %v %v", files, err)
	}
	// Another room's upload and skill, named by their ids: not this room's to read.
	other := testSetup()
	other.SkillsPending = false
	if _, err := s.CreateProfileRoom(ctx, "o", "Other", alice.ProfileID, "Ali", "inv-o", "tok-o", other); err != nil {
		t.Fatal(err)
	}
	om, _ := s.RoomDraft(ctx, "o")
	for _, bad := range []string{"doc/999", "skill/999", "other/1", "doc", om.Documents[0].File, om.Skills[0].File} {
		if _, err := s.RoomDraftFiles(ctx, "r", []string{bad}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := s.RoomDraft(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
	// A skill waiting for the host stays out, and the persona goes without it.
	if _, err := s.db.ExecContext(ctx, `UPDATE skills SET pending = 1 WHERE room_id = 'r' AND name = 'stamp'`); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.RoomDraft(ctx, "r"); len(m.Skills) != 0 || len(m.Personas[0].Skills) != 0 {
		t.Fatalf("pending skill in the draft: %+v", m)
	}
}
