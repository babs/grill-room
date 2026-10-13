package store

import (
	"context"
	"fmt"
	"time"
)

// DueVoicePersonas lists the room's personas whose "every N voice lines" is due: N voice lines
// since their latest turn started (006-AC18).
func (s *Store) DueVoicePersonas(ctx context.Context, roomID string) ([]RoomPersona, error) {
	return s.duePersonas(ctx, `WHERE p.room_id = ? AND p.every_voice IS NOT NULL AND p.pending = 0
		AND (SELECT COUNT(*) FROM messages m WHERE m.room_id = p.room_id AND m.kind = 'voice' AND m.deleted_at IS NULL
			AND m.id > p.mark_msg_id) >= p.every_voice ORDER BY p.id`, roomID)
}

// DueTimerPersonas lists the personas of room, of every room for "", whose "every T minutes" is
// due at now: T minutes since their latest turn started, with a voice or text line since
// (006-AC18). Whether someone is here is the caller's to check.
func (s *Store) DueTimerPersonas(ctx context.Context, now time.Time, room string) ([]RoomPersona, error) {
	return s.duePersonas(ctx, `WHERE (? = '' OR p.room_id = ?) AND p.every_minutes IS NOT NULL AND p.pending = 0 AND p.mark_at_ms + p.every_minutes * 60000 <= ?
		AND EXISTS (SELECT 1 FROM messages m WHERE m.room_id = p.room_id AND m.kind IN ('voice', 'text') AND m.deleted_at IS NULL
			AND m.id > p.mark_msg_id) ORDER BY p.room_id, p.id`, room, room, now.UnixMilli())
}

func (s *Store) duePersonas(ctx context.Context, where string, args ...any) ([]RoomPersona, error) {
	rows, err := s.db.QueryContext(ctx, personaCols+where, args...)
	if err != nil {
		return nil, fmt.Errorf("due personas: %w", err)
	}
	defer rows.Close()
	var out []RoomPersona
	for rows.Next() {
		p, err := scanPersona(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
