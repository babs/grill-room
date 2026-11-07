package store

import (
	"context"
	"fmt"
)

// Hand is a raised hand: who, and since when in ms (031-AC1).
type Hand struct {
	ParticipantID int64
	RaisedMS      int64
}

// RaiseHand raises pid's hand in room at nowMS, or at the start of pid's current stay if later:
// pod clocks skew, and a hand older than its stay lapsed (031-AC6). A hand raised already keeps its
// time, unless it was raised before pid's current stay: that one starts again.
func (s *Store) RaiseHand(ctx context.Context, room string, pid, nowMS int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		const stay = `COALESCE((SELECT since_at_ms FROM presence WHERE room_id = ?1 AND participant_id = ?2 AND tab = ''), 0)`
		_, err := t.ExecContext(ctx, `INSERT INTO hands (room_id, participant_id, raised_at_ms) VALUES (?1, ?2, GREATEST(CAST(?3 AS BIGINT), `+stay+`))
			ON CONFLICT (room_id, participant_id) DO UPDATE SET raised_at_ms = CASE
				WHEN hands.raised_at_ms < `+stay+` THEN excluded.raised_at_ms ELSE hands.raised_at_ms END`, room, pid, nowMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("raise hand: %w", err)
	}
	return nil
}

// LowerHand lowers pid's hand in room; lowering a lowered hand does nothing.
func (s *Store) LowerHand(ctx context.Context, room string, pid int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		_, err := t.ExecContext(ctx, `DELETE FROM hands WHERE room_id = ? AND participant_id = ?`, room, pid)
		return err
	})
	if err != nil {
		return fmt.Errorf("lower hand: %w", err)
	}
	return nil
}

// liveHand admits hand h while the stay of its participant that began before it lasts, a sign of
// life within the grace: a closing tab drops its row at once, while the person row's beat stays
// (031-AC6). Params: ?1 now, ?2 grace, both ms.
const liveHand = `EXISTS (SELECT 1 FROM presence p WHERE p.room_id = h.room_id AND p.participant_id = h.participant_id
	AND p.tab = '' AND p.since_at_ms <= h.raised_at_ms AND p.beat_at_ms >= CAST(?1 AS BIGINT) - ?2)`

// Hands returns room's hands live at nowMS with graceMS, earliest raised first (031-AC2).
func (s *Store) Hands(ctx context.Context, room string, nowMS, graceMS int64) ([]Hand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.participant_id, h.raised_at_ms FROM hands h WHERE `+liveHand+` AND h.room_id = ?3
		ORDER BY h.raised_at_ms, h.participant_id`, nowMS, graceMS, room)
	if err != nil {
		return nil, fmt.Errorf("hands: %w", err)
	}
	defer rows.Close()
	var out []Hand
	for rows.Next() {
		var h Hand
		if err := rows.Scan(&h.ParticipantID, &h.RaisedMS); err != nil {
			return nil, fmt.Errorf("hands: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hands: %w", err)
	}
	return out, nil
}
