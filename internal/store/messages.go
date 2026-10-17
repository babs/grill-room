package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/babs/grill-room/internal/render"
)

var (
	// ErrForbidden is returned when a participant changes a message that is not theirs to change.
	ErrForbidden = errors.New("only the author can change this message")
	// ErrMessageDeleted is returned when a deleted message is edited.
	ErrMessageDeleted = errors.New("message deleted")
)

// MessageVersion is an earlier body of an edited message, with when it was written.
type MessageVersion struct {
	Body      string    `json:"body"`
	WrittenAt time.Time `json:"written_at"`
}

// MessageChange is a message changed after an agent turn saw it: Before is the body the turn saw.
type MessageChange struct {
	Message
	Before string
}

// Message returns message id of room.
func (s *Store) Message(ctx context.Context, roomID string, id int64) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, messageCols+`WHERE id = ? AND room_id = ?`, id, roomID))
}

// EditMessage replaces the body of message id, keeping the old one as a version. Only participant
// by may, on their own text or voice line: ErrForbidden otherwise, ErrMessageDeleted once deleted,
// ErrNotFound if the room has no such message. The same body changes nothing. The caller broadcasts.
func (s *Store) EditMessage(ctx context.Context, roomID string, id, by int64, body string) (*Message, error) {
	return s.changeMessage(ctx, roomID, id, by, func(tx *sql.Tx, m *Message, seq int64) error {
		if m.DeletedAt != nil {
			return ErrMessageDeleted
		}
		if m.Body == body {
			return nil
		}
		written := m.TS
		if m.EditedAt != nil {
			written = *m.EditedAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_versions(message_id, seq, body, written_at) VALUES(?,?,?,?)`,
			m.ID, seq, m.Body, written.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		t := time.Now().UTC()
		html := render.MessageHTML(body)
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET body = ?, html = ?, edited_at = ?, change_seq = ? WHERE id = ?`,
			body, html, t.Format(time.RFC3339Nano), seq, m.ID); err != nil {
			return err
		}
		m.Body, m.HTML, m.EditedAt, m.ChangeSeq = body, html, &t, seq
		return nil
	})
}

// DeleteMessage soft-deletes message id: its body stays for the agent and for export. Same rules
// as EditMessage; deleting twice changes nothing. The caller broadcasts.
func (s *Store) DeleteMessage(ctx context.Context, roomID string, id, by int64) (*Message, error) {
	return s.changeMessage(ctx, roomID, id, by, func(tx *sql.Tx, m *Message, seq int64) error {
		if m.DeletedAt != nil {
			return nil
		}
		t := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted_at = ?, change_seq = ? WHERE id = ?`, t.Format(time.RFC3339Nano), seq, m.ID); err != nil {
			return err
		}
		m.DeletedAt, m.ChangeSeq = &t, seq
		return nil
	})
}

// changeMessage runs change on message id in one transaction, with the room's next change seq.
func (s *Store) changeMessage(ctx context.Context, roomID string, id, by int64, change func(*sql.Tx, *Message, int64) error) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("change message %d: %w", id, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	m, err := scanMessage(tx.QueryRowContext(ctx, messageCols+`WHERE id = ? AND room_id = ?`, id, roomID))
	if err != nil {
		return nil, err
	}
	if m.ParticipantID == 0 || m.ParticipantID != by || (m.Kind != KindText && m.Kind != KindVoice) {
		return nil, ErrForbidden
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(change_seq), 0) + 1 FROM messages WHERE room_id = ?`, roomID).Scan(&seq); err != nil {
		return nil, fmt.Errorf("change message %d: %w", id, err)
	}
	if err := change(tx, m, seq); err != nil {
		if errors.Is(err, ErrMessageDeleted) {
			return nil, err
		}
		return nil, fmt.Errorf("change message %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("change message %d: %w", id, err)
	}
	return m, nil
}

// MessageVersions lists the earlier bodies of message id of room, oldest first.
func (s *Store) MessageVersions(ctx context.Context, roomID string, id int64) ([]MessageVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.body, v.written_at FROM message_versions v JOIN messages m ON m.id = v.message_id
		WHERE m.id = ? AND m.room_id = ? ORDER BY v.seq`, id, roomID)
	if err != nil {
		return nil, fmt.Errorf("message versions %d: %w", id, err)
	}
	defer rows.Close()
	out := []MessageVersion{}
	for rows.Next() {
		var v MessageVersion
		var written string
		if err := rows.Scan(&v.Body, &written); err != nil {
			return nil, fmt.Errorf("scan message version: %w", err)
		}
		v.WrittenAt = parseTS(written)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ChangeCursor returns the room's last change seq, 0 when no message was ever edited or deleted.
func (s *Store) ChangeCursor(ctx context.Context, roomID string) (int64, error) {
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(change_seq), 0) FROM messages WHERE room_id = ?`, roomID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("change cursor: %w", err)
	}
	return seq, nil
}

// ChangedMessages returns up to limit messages of room edited or deleted past change seq after,
// in change order.
func (s *Store) ChangedMessages(ctx context.Context, roomID string, after int64, limit int) ([]Message, error) {
	return s.queryMessages(ctx, `WHERE room_id = ? AND change_seq > ? ORDER BY change_seq LIMIT ?`, roomID, after, limit)
}

// MessageChanges returns the messages of room up to id seen whose last change seq is in
// (after, upTo], in change order, each with the body it had at seq after.
func (s *Store) MessageChanges(ctx context.Context, roomID string, seen, after, upTo int64) ([]MessageChange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageFields+`,
		COALESCE((SELECT v.body FROM message_versions v WHERE v.message_id = messages.id AND v.seq > ? ORDER BY v.seq LIMIT 1), body)
		FROM messages WHERE room_id = ? AND id <= ? AND change_seq > ? AND change_seq <= ? ORDER BY change_seq`, after, roomID, seen, after, upTo)
	if err != nil {
		return nil, fmt.Errorf("message changes: %w", err)
	}
	defer rows.Close()
	var out []MessageChange
	for rows.Next() {
		var c MessageChange
		m, err := scanMessage(rows, &c.Before)
		if err != nil {
			return nil, err
		}
		c.Message = *m
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecentText returns the bodies of the room's last n chat, voice and agent lines, newest first:
// what a room's language is guessed from (013-AC16).
func (s *Store) RecentText(ctx context.Context, roomID string, n int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM messages WHERE room_id = ? AND deleted_at IS NULL
		AND kind IN ('voice', 'text', 'agent') ORDER BY id DESC LIMIT ?`, roomID, n)
	if err != nil {
		return nil, fmt.Errorf("recent text: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("recent text: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
