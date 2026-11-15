package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrForeignLine refuses a read marker at a line of another room (041-AC9).
var ErrForeignLine = errors.New("that line is not in this room")

// startReads starts the read marker of profileID in room, or of every profile with a live seat
// there when 0, at the room's newest line; a marker kept from an earlier seat stays (041-AC13).
func startReads(ctx context.Context, tx *txn, room string, profileID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO room_reads(profile_id, room_id, read_through)
		SELECT DISTINCT p.profile_id, p.room_id, (SELECT COALESCE(MAX(m.id), 0) FROM messages m WHERE m.room_id = p.room_id)
		FROM participants p WHERE p.room_id = ?1 AND p.profile_id IS NOT NULL AND p.removed_at IS NULL AND (?2::bigint = 0 OR p.profile_id = ?2)
		ON CONFLICT DO NOTHING`, room, profileID)
	if err != nil {
		return fmt.Errorf("start read marker: %w", err)
	}
	return nil
}

// MarkRead moves profileID's marker in room up to through, clamped to the room's newest line, in
// one statement so reordered writes keep the greatest; ErrForeignLine when through is a line of
// another room (041-AC9). No id cursor reads the markers: they skip the write lock.
func (s *Store) MarkRead(ctx context.Context, profileID int64, room string, through int64) error {
	var n int64
	err := s.db.unlocked(ctx, func(t *txn) error {
		res, err := t.ExecContext(ctx, `INSERT INTO room_reads(profile_id, room_id, read_through)
			SELECT ?1::bigint, ?2::text, LEAST(?3::bigint, (SELECT COALESCE(MAX(id), 0) FROM messages WHERE room_id = ?2))
			WHERE NOT EXISTS (SELECT 1 FROM messages WHERE id = ?3 AND room_id <> ?2)
			ON CONFLICT (profile_id, room_id) DO UPDATE SET read_through = GREATEST(room_reads.read_through, EXCLUDED.read_through)`,
			profileID, room, through)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	// A profile merged away or a room deleted meanwhile leaves nothing to keep.
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	if n == 0 {
		return ErrForeignLine
	}
	return nil
}

// Unread is a room's unread lines for a profile: how many, up to 100 (the page shows "99+"), and
// the bodies among them holding "@", for the caller's @name rule (041-AC11).
type Unread struct {
	Count  int
	Bodies []string
}

// unreadSQL reads, per room where profile ?1 has a live seat and a marker, the newest 100
// written lines past the marker that no seat of that profile wrote, deleted ones aside (041-AC10).
// The kind test stays literal for the written lines' index (041-AC14).
const unreadSQL = `SELECT rr.room_id, CASE WHEN strpos(u.body, '@') > 0 THEN u.body END
	FROM room_reads rr
	CROSS JOIN LATERAL (SELECT m.body FROM messages m
		WHERE m.room_id = rr.room_id AND m.` + written + ` AND m.id > rr.read_through AND m.deleted_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM participants o WHERE o.id = m.participant_id AND o.profile_id = rr.profile_id)
		ORDER BY m.id DESC LIMIT 100) u
	WHERE rr.profile_id = ?1 AND EXISTS (SELECT 1 FROM participants pa WHERE pa.room_id = rr.room_id AND pa.profile_id = rr.profile_id AND pa.removed_at IS NULL)`

// UnreadLines returns profileID's unread lines by room id; a room with nothing unread, or no
// marker, is absent (041-AC11).
func (s *Store) UnreadLines(ctx context.Context, profileID int64) (map[string]Unread, error) {
	rows, err := s.db.QueryContext(ctx, unreadSQL, profileID)
	if err != nil {
		return nil, fmt.Errorf("unread lines: %w", err)
	}
	defer rows.Close()
	out := map[string]Unread{}
	for rows.Next() {
		var room string
		var body *string
		if err := rows.Scan(&room, &body); err != nil {
			return nil, fmt.Errorf("scan unread line: %w", err)
		}
		u := out[room]
		u.Count++
		if body != nil {
			u.Bodies = append(u.Bodies, *body)
		}
		out[room] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unread lines: %w", err)
	}
	return out, nil
}
