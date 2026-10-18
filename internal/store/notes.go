package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LinkNotesWatch is a turn of the room's notes watcher: an automatic chain link like every N
// rounds, and a queue source of its own (009-AC32).
const LinkNotesWatch ChainEvent = "notes_watch"

// latestNote is the room's last note message id, for an UPDATE of room_personas.
const latestNote = `(SELECT COALESCE(MAX(m.id), 0) FROM annotation_messages m JOIN annotations a ON a.id = m.annotation_id
	WHERE a.room_id = room_personas.room_id)`

// otherWatcher sets p.watchFrom when c makes p the notes watcher in place of another persona.
func otherWatcher(ctx context.Context, tx *txn, p *RoomPersona, c PersonaPatch) error {
	if c.WatchesNotes == nil || !*c.WatchesNotes || p.WatchesNotes {
		return nil
	}
	err := tx.QueryRowContext(ctx, `SELECT badge || ' ' || name FROM room_personas WHERE room_id = ? AND id != ? AND watches_notes = 1`,
		p.RoomID, p.ID).Scan(&p.watchFrom)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("notes watcher: %w", err)
	}
	return nil
}

// notesNews are the note messages m that may wake persona p as the notes watcher: past its mark,
// written by someone else, in a thread a of its room on a document d still there (009-AC32).
const notesNews = `annotations a JOIN documents d ON d.id = a.document_id AND d.deleted_at IS NULL
	JOIN annotation_messages m ON m.annotation_id = a.id AND m.id > p.notes_mark AND (m.persona_id IS NULL OR m.persona_id != p.id)
	WHERE a.room_id = p.room_id`

// NotesWatchers lists the notes watchers, of every room, that have news and could queue now: no
// turn of theirs queued or running, and the room's auto-turn limit not reached (009-AC32).
func (s *Store) NotesWatchers(ctx context.Context) ([]RoomPersona, error) {
	return s.duePersonas(ctx, `WHERE p.watches_notes = 1 AND p.pending = 0
		AND EXISTS (SELECT 1 FROM `+notesNews+`)
		AND NOT EXISTS (SELECT 1 FROM turn_queue q WHERE q.persona_id = p.id)
		AND NOT EXISTS (SELECT 1 FROM turns t WHERE t.persona_id = p.id AND t.status = 'running')
		AND EXISTS (SELECT 1 FROM rooms r WHERE r.id = p.room_id AND (r.chain_suspended = 1 OR r.chain_count < r.chain_budget))
		ORDER BY p.room_id`)
}

// NoteNews is a note message the notes watcher has not read.
type NoteNews struct {
	MessageID    int64
	AnnotationID int64
	At           time.Time
}

// NotesNews lists the note messages of p's room past its notes mark that p did not write, on
// documents still there, oldest first (009-AC32).
func (s *Store) NotesNews(ctx context.Context, p *RoomPersona) ([]NoteNews, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.annotation_id, m.created_at FROM room_personas p CROSS JOIN `+notesNews+`
		AND p.id = ? ORDER BY m.id`, p.ID)
	if err != nil {
		return nil, fmt.Errorf("notes news: %w", err)
	}
	defer rows.Close()
	var out []NoteNews
	for rows.Next() {
		var n NoteNews
		var at string
		if err := rows.Scan(&n.MessageID, &n.AnnotationID, &at); err != nil {
			return nil, fmt.Errorf("notes news: %w", err)
		}
		n.At = parseTS(at)
		out = append(out, n)
	}
	return out, rows.Err()
}

// SkipNotes moves persona's notes mark up to note message upTo: none of the messages up to it may
// wake it.
func (s *Store) SkipNotes(ctx context.Context, persona, upTo int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET notes_mark = GREATEST(notes_mark, ?) WHERE id = ?`, upTo, persona); err != nil {
		return fmt.Errorf("skip notes: %w", err)
	}
	return nil
}

// ParticipantNotes returns, by thread, what the participants wrote in the threads ids of room:
// the messages that may name the personas a thread belongs to (009-AC30).
func (s *Store) ParticipantNotes(ctx context.Context, roomID string, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.annotation_id, m.body FROM annotation_messages m JOIN annotations a ON a.id = m.annotation_id
		WHERE a.room_id = ? AND m.author_kind = 'participant' AND m.annotation_id = ANY(?) ORDER BY m.id`, roomID, ids)
	if err != nil {
		return nil, fmt.Errorf("participant notes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, fmt.Errorf("participant notes: %w", err)
		}
		out[id] = append(out[id], body)
	}
	return out, rows.Err()
}

// noteMentions fills in where the mentions of the messages of anns stand (009-AC29).
func (s *Store) noteMentions(ctx context.Context, anns []Annotation, cond string, args ...any) error {
	if len(anns) == 0 {
		return nil
	}
	//nolint:gosec // cond is a constant of annotations.go, never user input
	list, err := queryMentions(ctx, s.db, noteMentionCols+` WHERE `+cond+` ORDER BY mt.message_id, mt.persona_id`, args...)
	if err != nil || len(list) == 0 {
		return err
	}
	byMsg := map[int64][]MentionState{}
	for _, m := range list {
		byMsg[m.MessageID] = append(byMsg[m.MessageID], m)
	}
	for i := range anns {
		for j := range anns[i].Messages {
			anns[i].Messages[j].Mentions = byMsg[anns[i].Messages[j].ID]
		}
	}
	return nil
}
