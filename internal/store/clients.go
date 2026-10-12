package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Client is where a participant connected from: at join and at last activity (005-AC20). Only the
// super-admin sees it.
type Client struct {
	ParticipantID int64      `json:"participant_id"`
	JoinIP        string     `json:"join_ip"`
	JoinUA        string     `json:"join_ua"`
	LastIP        string     `json:"last_ip"`
	LastUA        string     `json:"last_ua"`
	LastSeen      *time.Time `json:"last_seen_at"`
}

// ParticipantJoined records the client participant id joined from; it is also its last activity.
func (s *Store) ParticipantJoined(ctx context.Context, id int64, ip, ua string, at time.Time) error {
	ts := at.UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE participants SET join_ip = ?, join_ua = ?, last_ip = ?, last_ua = ?, last_seen_at = ?
		WHERE id = ?`, ip, ua, ip, ua, ts, id); err != nil {
		return fmt.Errorf("participant %d joined: %w", id, err)
	}
	return nil
}

// ParticipantSeen records participant id's last activity and the client it came from.
func (s *Store) ParticipantSeen(ctx context.Context, id int64, ip, ua string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE participants SET last_ip = ?, last_ua = ?, last_seen_at = ? WHERE id = ?`,
		ip, ua, at.UTC().Format(time.RFC3339Nano), id); err != nil {
		return fmt.Errorf("participant %d seen: %w", id, err)
	}
	return nil
}

// RoomClients returns the client of each of room's participants, revoked ones included.
func (s *Store) RoomClients(ctx context.Context, roomID string) (map[int64]Client, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(join_ip, ''), COALESCE(join_ua, ''), COALESCE(last_ip, ''),
		COALESCE(last_ua, ''), last_seen_at FROM participants WHERE room_id = ?`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room clients: %w", err)
	}
	defer rows.Close()
	out := map[int64]Client{}
	for rows.Next() {
		var c Client
		var seen sql.NullString
		if err := rows.Scan(&c.ParticipantID, &c.JoinIP, &c.JoinUA, &c.LastIP, &c.LastUA, &seen); err != nil {
			return nil, fmt.Errorf("scan room client: %w", err)
		}
		if seen.Valid {
			t := parseTS(seen.String)
			c.LastSeen = &t
		}
		out[c.ParticipantID] = c
	}
	return out, rows.Err()
}

// PurgeClients clears the client fields recorded before before: the join ones by join time, the
// last ones by last activity. It returns how many participants it cleared something of.
func (s *Store) PurgeClients(ctx context.Context, before time.Time) (int64, error) {
	ts := before.UTC().Format(time.RFC3339Nano)
	var n int64
	err := s.withTx(ctx, "purge clients", func(tx *sql.Tx) error {
		for _, q := range []string{
			`UPDATE participants SET join_ip = NULL, join_ua = NULL WHERE join_ip IS NOT NULL AND joined_at < ?`,
			`UPDATE participants SET last_ip = NULL, last_ua = NULL WHERE last_ip IS NOT NULL AND last_seen_at < ?`,
		} {
			res, err := tx.ExecContext(ctx, q, ts)
			if err != nil {
				return err
			}
			k, err := res.RowsAffected()
			if err != nil {
				return err
			}
			n += k
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("purge clients: %w", err)
	}
	return n, nil
}
