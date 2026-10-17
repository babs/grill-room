package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrIsRoomAdmin is returned when a room admin is removed, or leaves, before stepping down.
	ErrIsRoomAdmin = errors.New("a room admin must step down first")
	// ErrBlocked is returned when a profile a room admin blocked joins the room again (016-AC13).
	ErrBlocked = errors.New("blocked from rejoining the room")
)

// Ending is how a person's seats in a room end: by a room admin (By, nil for the super-admin),
// blocked from rejoining or not, or by the person leaving.
type Ending struct {
	By    *Participant
	Block bool
	Leave bool
}

// Ended is what ending a person's seats did.
type Ended struct {
	Name string
	// Seats are the seats it ended; Daemons, their apps; Host, set when one of them was the AI
	// host's, which the room no longer has.
	Seats   []int64
	Daemons []int64
	Host    bool
	Blocked bool
}

// EndSeats ends every valid seat of the person holding seat target in room: all the seats of
// its profile, or the seat alone without one (016-AC7, 016-AC12). It revokes the invites those
// seats made (016-AC16), drops the turns they queued that have not started (016-AC8), and leaves
// the room without a host when one of them hosted (016-AC9). A removal posts who removed whom and
// writes the audit entry ctx carries; a leave posts the leaving line only. ErrNotFound without a
// valid seat target in room; ErrIsRoomAdmin when the person is a room admin. The caller broadcasts.
func (s *Store) EndSeats(ctx context.Context, roomID string, target int64, e Ending) (*Ended, error) {
	out := &Ended{}
	err := s.withTx(ctx, "end seats", func(tx *sql.Tx) error {
		var profile sql.NullInt64
		var admin bool
		err := tx.QueryRowContext(ctx, `SELECT p.name, p.profile_id,
			EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = p.room_id AND a.profile_id = p.profile_id)
			FROM participants p WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, target, roomID).Scan(&out.Name, &profile, &admin)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("end seats: %w", err)
		}
		if admin {
			return ErrIsRoomAdmin
		}
		reason := EndRemoved
		if e.Leave {
			reason = EndLeft
		}
		at := now()
		if out.Seats, err = queryIDs(ctx, tx, `UPDATE participants SET removed_at = ?, end_reason = ?
			WHERE room_id = ? AND removed_at IS NULL AND (id = ? OR profile_id = ?) RETURNING id`, at, reason, roomID, target, profile); err != nil {
			return fmt.Errorf("end seats: %w", err)
		}
		ids := jsonIDs(out.Seats)
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET revoked_at = ? WHERE revoked_at IS NULL AND created_by IN (SELECT value FROM json_each(?))`, at, ids); err != nil {
			return fmt.Errorf("end seats: invites: %w", err)
		}
		if out.Daemons, out.Host, err = seatsGone(ctx, tx, roomID, out.Seats); err != nil {
			return fmt.Errorf("end seats: %w", err)
		}
		m := &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: target, Sender: out.Name, Body: out.Name + " left the room"}
		if !e.Leave {
			who := "The admin"
			if e.By != nil {
				who = e.By.Name
				m.ParticipantID, m.Sender = e.By.ID, e.By.Name
			} else {
				m.ParticipantID, m.Sender = 0, who
			}
			m.Body = who + " removed " + out.Name + " from the room"
			if e.Block && profile.Valid {
				var by any
				if e.By != nil {
					by = e.By.ID
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO room_blocks(room_id, profile_id, name, created_at, by_participant_id) VALUES(?,?,?,?,?)
					ON CONFLICT(room_id, profile_id) DO UPDATE SET name = excluded.name, created_at = excluded.created_at, by_participant_id = excluded.by_participant_id`,
					roomID, profile.Int64, out.Name, at, by); err != nil {
					return fmt.Errorf("end seats: block: %w", err)
				}
				out.Blocked = true
				m.Body += " and blocked them from rejoining"
			}
		}
		if err := insertMessage(ctx, tx, m); err != nil {
			return err
		}
		if e.Leave {
			return nil
		}
		return auditTx(ctx, tx, "", map[string]any{"seats": out.Seats, "name": out.Name, "blocked": out.Blocked})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// seatsGone applies the end of seats of room: the turns they queued that have not started go, and
// the room loses its host when one of them hosted (016-AC8, 016-AC9). It returns the seats' apps,
// whose running turns the caller fails, and whether the host went.
func seatsGone(ctx context.Context, tx *sql.Tx, roomID string, seats []int64) (daemons []int64, host bool, err error) {
	ids := jsonIDs(seats)
	if _, err := tx.ExecContext(ctx, `DELETE FROM turn_queue WHERE participant_id IN (SELECT value FROM json_each(?))`, ids); err != nil {
		return nil, false, fmt.Errorf("queue: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE rooms SET host_participant_id = NULL WHERE id = ? AND host_participant_id IN (SELECT value FROM json_each(?))`, roomID, ids)
	if err != nil {
		return nil, false, fmt.Errorf("host: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		host = true
		if err := hostChanged(ctx, tx, roomID); err != nil {
			return nil, false, err
		}
	}
	if daemons, err = queryIDs(ctx, tx, `SELECT id FROM daemons WHERE participant_id IN (SELECT value FROM json_each(?)) ORDER BY id`, ids); err != nil {
		return nil, false, fmt.Errorf("daemons: %w", err)
	}
	return daemons, host, nil
}

// jsonIDs is ids as a JSON array, for json_each.
func jsonIDs(ids []int64) string {
	b := []byte{'['}
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, "%d", id)
	}
	return string(append(b, ']'))
}

// Block is a profile a room admin blocked from rejoining a room (016-AC14).
type Block struct {
	ProfileID int64     `json:"profile_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// By names the room admin who blocked, "" for the super-admin or a seat gone since.
	By string `json:"by,omitempty"`
}

// Blocks lists the profiles blocked from rejoining room, newest first.
func (s *Store) Blocks(ctx context.Context, roomID string) ([]Block, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT b.profile_id, b.name, b.created_at, COALESCE(p.name, '') FROM room_blocks b
		LEFT JOIN participants p ON p.id = b.by_participant_id WHERE b.room_id = ? ORDER BY b.created_at DESC, b.profile_id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("blocks: %w", err)
	}
	defer rows.Close()
	out := []Block{}
	for rows.Next() {
		var b Block
		var at string
		if err := rows.Scan(&b.ProfileID, &b.Name, &at, &b.By); err != nil {
			return nil, fmt.Errorf("blocks: %w", err)
		}
		b.CreatedAt = parseTS(at)
		out = append(out, b)
	}
	return out, rows.Err()
}

// Unblock lifts the block of profile on room, posting who lifted it, by (nil for the super-admin),
// and writing the audit entry ctx carries (016-AC14). ErrNotFound when profile is not blocked.
func (s *Store) Unblock(ctx context.Context, roomID string, profile int64, by *Participant) error {
	return s.withTx(ctx, "unblock", func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, `DELETE FROM room_blocks WHERE room_id = ? AND profile_id = ? RETURNING name`, roomID, profile).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("unblock: %w", err)
		}
		m := &Message{RoomID: roomID, Kind: KindSystem, Sender: "The admin", Body: "The admin unblocked " + name}
		if by != nil {
			m.ParticipantID, m.Sender, m.Body = by.ID, by.Name, by.Name+" unblocked "+name
		}
		if err := insertMessage(ctx, tx, m); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"profile": profile, "name": name})
	})
}

// Departed returns, by seat, why the room's seats ended whose person holds no valid seat there:
// "removed", or "left" (016-AC11). A seat revoked before 016 reads removed.
func (s *Store) Departed(ctx context.Context, roomID string) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.end_reason FROM participants p WHERE p.room_id = ? AND p.removed_at IS NOT NULL
		AND (p.profile_id IS NULL OR NOT EXISTS (SELECT 1 FROM participants q WHERE q.room_id = p.room_id AND q.profile_id = p.profile_id AND q.removed_at IS NULL))`, roomID)
	if err != nil {
		return nil, fmt.Errorf("departed: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, fmt.Errorf("departed: %w", err)
		}
		if reason != EndLeft {
			reason = EndRemoved
		}
		out[id] = reason
	}
	return out, rows.Err()
}
