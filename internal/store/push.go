package store

import (
	"context"
	"database/sql"
	"fmt"
)

// MaxPushSubscriptions bounds the browsers one participant subscribes: past it the oldest goes.
const MaxPushSubscriptions = 10

// PushSubscription is one browser's Web Push subscription for a participant (005-AC24).
type PushSubscription struct {
	ID            int64
	ParticipantID int64
	Endpoint      string
	P256dh, Auth  string
	// AllMessages opts in to every line from others, not only mentions, syntheses and the
	// participant's own turns ending.
	AllMessages bool
}

// SavePushSubscription stores sub for its participant, or updates the one with its endpoint, and
// drops the participant's oldest past MaxPushSubscriptions.
func (s *Store) SavePushSubscription(ctx context.Context, sub PushSubscription) error {
	return s.withTx(ctx, "save push subscription", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO push_subscriptions(participant_id, endpoint, p256dh, auth, all_messages, created_at)
			VALUES(?,?,?,?,?,?) ON CONFLICT(participant_id, endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
			all_messages = excluded.all_messages`, sub.ParticipantID, sub.Endpoint, sub.P256dh, sub.Auth, sub.AllMessages, now())
		if err != nil {
			return fmt.Errorf("save push subscription: %w", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE participant_id = ? AND id NOT IN
			(SELECT id FROM push_subscriptions WHERE participant_id = ? ORDER BY id DESC LIMIT ?)`, sub.ParticipantID, sub.ParticipantID, MaxPushSubscriptions)
		if err != nil {
			return fmt.Errorf("trim push subscriptions: %w", err)
		}
		return nil
	})
}

// DeletePushSubscription removes participant's subscription at endpoint; none is no error.
func (s *Store) DeletePushSubscription(ctx context.Context, participantID int64, endpoint string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE participant_id = ? AND endpoint = ?`, participantID, endpoint); err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

// DropPushSubscription removes a subscription the push service says is gone.
func (s *Store) DropPushSubscription(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("drop push subscription: %w", err)
	}
	return nil
}

// PushSubscriptions lists the subscriptions of room's participants whose seat is valid.
func (s *Store) PushSubscriptions(ctx context.Context, roomID string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.participant_id, s.endpoint, s.p256dh, s.auth, s.all_messages FROM push_subscriptions s
		JOIN participants p ON p.id = s.participant_id
		WHERE p.room_id = ? AND p.removed_at IS NULL ORDER BY s.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("push subscriptions: %w", err)
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.ID, &p.ParticipantID, &p.Endpoint, &p.P256dh, &p.Auth, &p.AllMessages); err != nil {
			return nil, fmt.Errorf("scan push subscription: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
