package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// 009-AC31: one persona per room watches the notes. Taking the role takes it from the previous
// watcher in the same change, and the chat says so; held changes do the same once approved, and
// the database refuses a second watcher whatever writes it.
func TestOneNotesWatcher(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill, _ := approvingRoom(t, s, "w")
	sec, _, err := s.AddPersona(ctx, "w", templateByPreset(t, s, "secretary").ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	watchers := func() []string {
		ps, _ := s.RoomPersonas(ctx, "w")
		var out []string
		for _, p := range ps {
			if p.WatchesNotes {
				out = append(out, p.Name)
			}
		}
		return out
	}
	edit := func(id int64, on *bool, by *Participant) (string, bool) {
		t.Helper()
		_, m, held, err := s.EditPersona(ctx, "w", id, PersonaPatch{WatchesNotes: on}, by)
		if err != nil || m == nil {
			t.Fatalf("edit: %+v %v", m, err)
		}
		return m.Body, held
	}
	if body, _ := edit(grill.ID, yes(), alice); body != "Alice changed 🔥 Grill: now watches the notes." {
		t.Fatalf("first watcher: %q", body)
	}
	if body, _ := edit(sec.ID, yes(), alice); body != "Alice changed 📝 Secretary: now watches the notes (was 🔥 Grill)." {
		t.Fatalf("moved: %q", body)
	}
	if w := watchers(); len(w) != 1 || w[0] != "Secretary" {
		t.Fatalf("watchers %v", w)
	}
	// Held for the host, then approved: the move happens at approval, named then.
	if body, held := edit(grill.ID, yes(), bob); !held || body != "Bob asked to change 🔥 Grill: now watches the notes (was 📝 Secretary). It waits for the host's approval." {
		t.Fatalf("held: %q %v", body, held)
	}
	if w := watchers(); len(w) != 1 || w[0] != "Secretary" {
		t.Fatalf("a held change moved the role: %v", w)
	}
	m, err := s.DecideChange(ctx, "w", s.onlyChange(t, "w").ID, true, alice)
	if err != nil || m.Body != "Alice approved Bob's change to 🔥 Grill: now watches the notes (was 📝 Secretary)." {
		t.Fatalf("approve: %+v %v", m, err)
	}
	if w := watchers(); len(w) != 1 || w[0] != "Grill" {
		t.Fatalf("watchers after approval %v", w)
	}
	if body, _ := edit(grill.ID, no(), alice); body != "Alice changed 🔥 Grill: no longer watches the notes." {
		t.Fatalf("off: %q", body)
	}
	if w := watchers(); len(w) != 0 {
		t.Fatalf("watchers after off %v", w)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET watches_notes = 1 WHERE room_id = 'w'`); err == nil {
		t.Fatal("two watchers in one room")
	}
}

// 009-AC32: the turn queue takes the watcher's source, as an automatic chain link that counts and
// stops at the limit.
func TestNotesWatchLink(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "l")
	one := 1
	if _, _, err := s.ControlChain(ctx, "l", ChainControl{Budget: &one}, alice); err != nil {
		t.Fatal(err)
	}
	q := &QueuedTurn{RoomID: "l", PersonaID: grill.ID, Cause: "new notes: #1"}
	if c, err := s.EnqueueLink(ctx, q, LinkNotesWatch); err != nil || c.Count != 1 || q.Source != "notes_watch" {
		t.Fatalf("link: %+v %+v %v", c, q, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM turn_queue`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: "l", PersonaID: grill.ID}, LinkNotesWatch); !errors.Is(err, ErrChainPaused) {
		t.Fatalf("past the limit: %v", err)
	}
}

// note adds a whole-document note to doc as participant pid and returns the message id.
func note(t *testing.T, s *Store, room string, doc int64, pid int64) int64 {
	t.Helper()
	a, err := s.CreateAnnotation(t.Context(), room, doc, 0, nil, &NewMessage{ParticipantID: pid, Author: "Alice", Body: "a note"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return a.Messages[0].ID
}

func watcherIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	ws, err := s.NotesWatchers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, w := range ws {
		out = append(out, w.ID)
	}
	return out
}

// 009-AC32: the notes watcher has news only from live documents, and is not a candidate while it
// cannot queue: a turn of its own queued, or the room's auto-turn limit reached. Moving the role
// hands over the notes the previous watcher had not read; a first watcher starts from now. A
// patch that leaves the role as it is changes nothing, held or not.
func TestNotesWatcherNews(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, grill := personaRoom(t, s, "n")
	sec, _, err := s.AddPersona(ctx, "n", templateByPreset(t, s, "secretary").ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateAgentDocument(ctx, "n", "plan.md", "md", "# Plan\n", "Grill")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.CreateAgentDocument(ctx, "n", "old.md", "md", "# Old\n", "Grill")
	if err != nil {
		t.Fatal(err)
	}
	early := note(t, s, "n", doc.ID, alice.ID)
	if _, _, _, err := s.EditPersona(ctx, "n", grill.ID, PersonaPatch{WatchesNotes: yes()}, alice); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.RoomPersona(ctx, "n", grill.ID); p.NotesMark != early || len(watcherIDs(t, s)) != 0 {
		t.Fatalf("first watcher mark %d, want %d; candidates %v", p.NotesMark, early, watcherIDs(t, s))
	}
	onGone := note(t, s, "n", gone.ID, alice.ID)
	if _, err := s.db.ExecContext(ctx, `UPDATE documents SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = ?`, gone.ID); err != nil {
		t.Fatal(err)
	}
	if w := watcherIDs(t, s); len(w) != 0 {
		t.Fatalf("a note on a deleted document is news: %v", w)
	}
	p, _ := s.RoomPersona(ctx, "n", grill.ID)
	if news, err := s.NotesNews(ctx, p); err != nil || len(news) != 0 {
		t.Fatalf("news from a deleted document: %+v %v (note %d)", news, err, onGone)
	}
	pending := note(t, s, "n", doc.ID, alice.ID)
	if w := watcherIDs(t, s); len(w) != 1 || w[0] != grill.ID {
		t.Fatalf("candidates %v", w)
	}
	if news, _ := s.NotesNews(ctx, p); len(news) != 1 || news[0].MessageID != pending {
		t.Fatalf("news %+v", news)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "n", PersonaID: grill.ID, ParticipantID: alice.ID}); err != nil {
		t.Fatal(err)
	}
	if w := watcherIDs(t, s); len(w) != 0 {
		t.Fatalf("queued watcher is a candidate: %v", w)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM turn_queue; UPDATE rooms SET chain_count = chain_budget WHERE id = 'n'`); err != nil {
		t.Fatal(err)
	}
	if w := watcherIDs(t, s); len(w) != 0 {
		t.Fatalf("watcher is a candidate at the limit: %v", w)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE rooms SET chain_suspended = 1 WHERE id = 'n'`); err != nil {
		t.Fatal(err)
	}
	if w := watcherIDs(t, s); len(w) != 1 {
		t.Fatalf("the lifted limit keeps it out: %v", w)
	}
	// The role moves with the unread note.
	if _, _, _, err := s.EditPersona(ctx, "n", sec.ID, PersonaPatch{WatchesNotes: yes()}, alice); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.RoomPersona(ctx, "n", sec.ID); q.NotesMark != early || !q.WatchesNotes {
		t.Fatalf("new watcher mark %d, want %d", q.NotesMark, early)
	}
	if w := watcherIDs(t, s); len(w) != 1 || w[0] != sec.ID {
		t.Fatalf("candidates after the move %v", w)
	}
	if _, m, _, err := s.EditPersona(ctx, "n", grill.ID, PersonaPatch{WatchesNotes: no()}, alice); err != nil || m != nil {
		t.Fatalf("no-op patch: %+v %v", m, err)
	}
}

// 009-AC31: a held change that leaves the role as the persona has it holds nothing about it, so its
// approval cannot flip the role back after someone moved it.
func TestHeldWatcherNoop(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, grill, _ := approvingRoom(t, s, "h")
	if _, m, held, err := s.EditPersona(ctx, "h", grill.ID, PersonaPatch{WatchesNotes: no(), Badge: str("🧯")}, bob); err != nil || !held ||
		m.Body != "Bob asked to change 🔥 Grill: badge 🔥 → 🧯. It waits for the host's approval." {
		t.Fatalf("held: %+v %v", m, err)
	}
	if _, _, _, err := s.EditPersona(ctx, "h", grill.ID, PersonaPatch{WatchesNotes: yes()}, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideChange(ctx, "h", s.onlyChange(t, "h").ID, true, alice); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.RoomPersona(ctx, "h", grill.ID); !p.WatchesNotes || p.Badge != "🧯" {
		t.Fatalf("approval flipped the role: %+v", p)
	}
}

// 009-AC32: the migration that adds the watcher's queue source keeps the queue, takes notes_watch
// and still refuses an unknown source, with a sound database.
func TestMigrationToNotesWatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	// One connection, as Open uses: the edited CHECK must be read again on it, not on a fresh one.
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := migrateTo(ctx, db, 30); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO rooms(id, name, created_at) VALUES('a', 'R', '2026-01-01T00:00:00Z')`,
		`INSERT INTO room_personas(room_id, template_version, name, badge, colour, text, model, effort, created_at)
			VALUES('a', 1, 'Grill', 'x', '#000000', 't', 'sonnet', 'medium', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	const grill = 1
	if _, err := db.ExecContext(ctx, `INSERT INTO turn_queue(room_id, persona_id, source, fresh, created_at, cause) VALUES('a', ?, 'call', 0, '2026-01-01T00:00:00Z', 'called by @x')`, grill); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO turn_queue(room_id, persona_id, source, fresh, created_at) VALUES('a', ?, 'notes_watch', 0, '2026-01-01T00:00:00Z')`, grill); err == nil {
		t.Fatal("version 30 takes notes_watch")
	}
	if err := migrateTo(ctx, db, 31); err != nil {
		t.Fatal(err)
	}
	var source, cause string
	if err := db.QueryRowContext(ctx, `SELECT source, cause FROM turn_queue`).Scan(&source, &cause); err != nil || source != "call" || cause != "called by @x" {
		t.Fatalf("kept row: %q %q %v", source, cause, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE turn_queue SET source = 'notes_watch'`); err != nil {
		t.Fatalf("notes_watch refused: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE turn_queue SET source = 'bogus'`); err == nil {
		t.Fatal("bogus source accepted")
	}
	if _, err := db.ExecContext(ctx, `UPDATE turn_queue SET source = 'button'`); err == nil {
		t.Fatal("a button press without its participant accepted")
	}
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity: %q %v", check, err)
	}
	_ = db.Close()
}
