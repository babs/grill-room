package store

import (
	"context"
	"errors"
	"fmt"
)

// MentionState is where the turn a message's @mention queued stands (005-AC4).
type MentionState struct {
	MessageID int64 `json:"message_id"`
	PersonaID int64 `json:"persona_id"`
	TurnID    int64 `json:"turn_id,omitempty"`
	// Status is queued, running, done, failed, stopped, or cancelled for a queue entry dropped
	// before it ran.
	Status string `json:"status"`
	// LineID is the message telling the turn's end: its synthesis, or the line saying it failed or
	// stopped; 0 while it runs, or when it wrote no synthesis.
	LineID int64 `json:"line_id,omitempty"`
}

// Named is one @name of a line that names a persona or a member of its room (005-AC27): At is the
// text as written, "@grill", and one of PersonaID and ParticipantID is set.
type Named struct {
	At            string `json:"at"`
	PersonaID     int64  `json:"persona_id,omitempty"`
	ParticipantID int64  `json:"participant_id,omitempty"`
}

// MentionCancelled is the status of a mention whose queued turn was dropped before it ran.
const MentionCancelled = "cancelled"

// EnqueueMention queues q, a turn that message messageID's @mention asks for, and links the message
// to it; a persona queued already answers the mention with that turn. ErrNotFound when the persona
// is not in the room or waits for the host's approval (002-AC23, 005-AC4).
func (s *Store) EnqueueMention(ctx context.Context, messageID int64, q *QueuedTurn) error {
	return s.enqueueMention(ctx, "message_turns", messageID, q)
}

// EnqueueNoteMention is EnqueueMention for note message messageID (009-AC29).
func (s *Store) EnqueueNoteMention(ctx context.Context, messageID int64, q *QueuedTurn) error {
	return s.enqueueMention(ctx, "annotation_message_turns", messageID, q)
}

// mentionTables link a mentioning message to its turn: chat lines, then note messages.
var mentionTables = []string{"message_turns", "annotation_message_turns"}

func (s *Store) enqueueMention(ctx context.Context, table string, messageID int64, q *QueuedTurn) error {
	q.Source = SourceMention
	return s.withTx(ctx, "enqueue mention", func(tx *txn) error {
		err := enqueue(ctx, tx, q)
		if errors.Is(err, ErrAlreadyQueued) {
			err = tx.QueryRowContext(ctx, `SELECT id FROM turn_queue WHERE persona_id = ? AND room_id = ?`, q.PersonaID, q.RoomID).Scan(&q.ID)
		}
		if err != nil {
			return err
		}
		//nolint:gosec // table is one of mentionTables, never user input
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(message_id, persona_id, queue_id) VALUES(?,?,?)
			ON CONFLICT DO NOTHING`, messageID, q.PersonaID, q.ID); err != nil {
			return fmt.Errorf("link mention: %w", err)
		}
		return nil
	})
}

// requeueMentions points the mentions waiting on queue entry from, or on turn, to queue entry to,
// which runs in their place. 0 matches nothing: ids start at 1.
func requeueMentions(ctx context.Context, tx *txn, to, turn, from int64) error {
	for _, table := range mentionTables {
		//nolint:gosec // table is one of mentionTables, never user input
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET queue_id = ?, turn_id = NULL WHERE turn_id = ? OR queue_id = ?`, to, turn, from); err != nil {
			return fmt.Errorf("requeue mentions: %w", err)
		}
	}
	return nil
}

// mentionState reads where a mention's turn stands: from the turn, from the queue while it waits,
// or as an import kept it. FROM follows: mt, the link, m, the message, t, the turn.
const mentionState = `
		SELECT mt.message_id, mt.persona_id, COALESCE(t.id, 0),
			CASE WHEN t.id IS NULL THEN
				CASE WHEN mt.final_status IS NOT NULL THEN mt.final_status
				WHEN EXISTS (SELECT 1 FROM turn_queue q WHERE q.id = mt.queue_id) THEN 'queued' ELSE 'cancelled' END
			WHEN t.status = 'running' AND t.host_queued_at IS NOT NULL THEN 'queued'
			ELSE t.status END,
			COALESCE((SELECT l.id FROM messages l WHERE l.turn_id = t.id AND t.status != 'running' ORDER BY l.kind != 'agent', l.id LIMIT 1),
				mt.final_line_id, 0)
		FROM `

// mentionCols reads where a chat line's mention stands.
const mentionCols = mentionState + `message_turns mt JOIN messages m ON m.id = mt.message_id LEFT JOIN turns t ON t.id = mt.turn_id`

// noteMentionCols reads where a note message's mention stands; a is its thread (009-AC29).
const noteMentionCols = mentionState + `annotation_message_turns mt JOIN annotation_messages m ON m.id = mt.message_id
		JOIN annotations a ON a.id = m.annotation_id LEFT JOIN turns t ON t.id = mt.turn_id`

// MentionStates returns where the turns of the room's latest limit mentions stand, newest first.
func (s *Store) MentionStates(ctx context.Context, roomID string, limit int) ([]MentionState, error) {
	return queryMentions(ctx, s.db, mentionCols+` WHERE m.room_id = ? ORDER BY mt.message_id DESC, mt.persona_id LIMIT ?`, roomID, limit)
}

// MessageMentions returns where the mentions of room's lines after..upTo stand, by line: the chips
// every line keeps however old (005-AC4).
func (s *Store) MessageMentions(ctx context.Context, roomID string, after, upTo int64) (map[int64][]MentionState, error) {
	list, err := queryMentions(ctx, s.db, mentionCols+` WHERE m.room_id = ? AND mt.message_id > ? AND mt.message_id <= ?
		ORDER BY mt.message_id, mt.persona_id`, roomID, after, upTo)
	if err != nil {
		return nil, err
	}
	out := map[int64][]MentionState{}
	for _, m := range list {
		out[m.MessageID] = append(out[m.MessageID], m)
	}
	return out, nil
}

func queryMentions(ctx context.Context, db rowsQuerier, query string, args ...any) ([]MentionState, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mention states: %w", err)
	}
	defer rows.Close()
	out := []MentionState{}
	for rows.Next() {
		var m MentionState
		if err := rows.Scan(&m.MessageID, &m.PersonaID, &m.TurnID, &m.Status, &m.LineID); err != nil {
			return nil, fmt.Errorf("mention states: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// NoteMentionStates returns where the turns of the room's latest limit note mentions stand, newest
// first (009-AC29).
func (s *Store) NoteMentionStates(ctx context.Context, roomID string, limit int) ([]MentionState, error) {
	return queryMentions(ctx, s.db, noteMentionCols+` WHERE a.room_id = ? ORDER BY mt.message_id DESC, mt.persona_id LIMIT ?`, roomID, limit)
}

// NoteMentionThreads returns the threads whose @mentions of persona wait on queue entry queueID,
// in the order they were written (009-AC29).
func (s *Store) NoteMentionThreads(ctx context.Context, queueID, persona int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.annotation_id FROM annotation_message_turns mt JOIN annotation_messages m ON m.id = mt.message_id
		WHERE mt.queue_id = ? AND mt.persona_id = ? GROUP BY m.annotation_id ORDER BY MIN(m.id)`, queueID, persona)
	if err != nil {
		return nil, fmt.Errorf("note mention threads: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("note mention threads: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
