package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// voiceLookback bounds the lines of a participant the repeat check reads: far more than one
// speaker says within a dedupe window.
const voiceLookback = 50

// PostVoiceLine stores m, a voice line at now, unless dup reports it repeats one of the bodies of
// its participant's voice lines from the other source since now-window (010-AC16). The check and
// the insert are one write transaction: two pods posting the same utterance store it once. It
// reports whether it stored m.
// ponytail: the check and the insert hold the cluster-wide write lock; take a per-participant
// advisory lock instead if voice lines ever queue behind other writes.
func (s *Store) PostVoiceLine(ctx context.Context, m *Message, now time.Time, window time.Duration, dup func(other []string) bool) (bool, error) {
	stored := false
	err := s.withTx(ctx, "post voice line", func(t *txn) error {
		rows, err := t.QueryContext(ctx, `SELECT body FROM (SELECT body, kind, source, ts FROM messages WHERE participant_id = ? ORDER BY id DESC LIMIT ?) m
			WHERE kind = 'voice' AND source <> ? AND ts >= ?`, m.ParticipantID, voiceLookback, m.Source, formatTS(now.Add(-window)))
		if err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		var other []string
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				_ = rows.Close()
				return fmt.Errorf("post voice line: %w", err)
			}
			other = append(other, b)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		if len(other) > 0 && dup(other) {
			return nil
		}
		if err := insertMessage(ctx, t, m); err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		// The window reads the caller's clock, which tests move: the line carries that time.
		m.TS = now.UTC()
		if _, err := t.ExecContext(ctx, `UPDATE messages SET ts = ? WHERE id = ?`, formatTS(m.TS), m.ID); err != nil {
			return fmt.Errorf("post voice line: %w", err)
		}
		stored = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return stored, nil
}
