package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// DaemonStatus is what a daemon's heartbeat last said: whether it can host and why not, its
// version, whether it applies website approvals, its connectors as JSON.
type DaemonStatus struct {
	CanHost    bool
	Reason     string
	Version    string
	Egress     bool
	Connectors string
}

// LiveDaemon is a daemon with a connection open on any pod. Since is when its oldest live
// connection opened: the earliest eligible daemon hosts.
type LiveDaemon struct {
	ID            int64
	RoomID        string
	ParticipantID int64
	Since         time.Time
	DaemonStatus
}

// lockDaemon serialises the connection writes of daemon id, so two of them never both miss the
// other's row and misjudge whether the daemon is gone.
func lockDaemon(ctx context.Context, t *txn, id int64) error {
	return xactLock(ctx, t, LockDaemon, strconv.FormatInt(id, 10))
}

// DaemonConnected records connection id of daemon d, opened at connectedMS, live until expiresMS;
// called again it renews the lease. A connection opened while another of the daemon is live takes
// the earlier one's start, so a reconnect keeps the daemon's place in the election. It fails once
// the daemon is gone: its stream must end.
func (s *Store) DaemonConnected(ctx context.Context, id string, d *Daemon, pod string, connectedMS, expiresMS int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		if err := lockDaemon(ctx, t, d.ID); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `INSERT INTO daemon_connections(id, daemon_id, room_id, pod, connected_at_ms, expires_at_ms)
			VALUES(?1, ?2, ?3, ?4, LEAST(CAST(?5 AS BIGINT), COALESCE((SELECT MIN(connected_at_ms) FROM daemon_connections
				WHERE daemon_id = ?2 AND expires_at_ms > ?5), ?5)), ?6)
			ON CONFLICT(id) DO UPDATE SET expires_at_ms = excluded.expires_at_ms`, id, d.ID, d.RoomID, pod, connectedMS, expiresMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("daemon connected: %w", err)
	}
	return nil
}

// daemonGone reports that daemon id holds no connection live at nowMS, and then drops its status,
// as a new connection starts unable to host. The caller holds lockDaemon.
func daemonGone(ctx context.Context, t *txn, id, nowMS int64) (gone bool, err error) {
	if err := t.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM daemon_connections WHERE daemon_id = ? AND expires_at_ms > ?)`,
		id, nowMS).Scan(&gone); err != nil || !gone {
		return false, err
	}
	_, err = t.ExecContext(ctx, `DELETE FROM daemon_status WHERE daemon_id = ?`, id)
	return err == nil, err
}

// DaemonDisconnected drops connection id of daemon daemonID. gone reports that no live connection
// of the daemon remains, on any pod: its status goes too.
func (s *Store) DaemonDisconnected(ctx context.Context, id string, daemonID, nowMS int64) (gone bool, err error) {
	err = s.db.unlocked(ctx, func(t *txn) error {
		if err := lockDaemon(ctx, t, daemonID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM daemon_connections WHERE id = ?`, id); err != nil {
			return err
		}
		gone, err = daemonGone(ctx, t, daemonID, nowMS)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("daemon disconnected: %w", err)
	}
	return gone, nil
}

// DropDaemonConnections ends every connection and the status of daemon id, on every pod: the
// daemon leaves the room's daemons before its row goes.
func (s *Store) DropDaemonConnections(ctx context.Context, id int64) error {
	err := s.db.unlocked(ctx, func(t *txn) error {
		if _, err := t.ExecContext(ctx, `DELETE FROM daemon_connections WHERE daemon_id = ?`, id); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `DELETE FROM daemon_status WHERE daemon_id = ?`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("drop daemon connections: %w", err)
	}
	return nil
}

// SetDaemonStatus records daemon id's status; changed is false when it said the same already,
// its version aside.
func (s *Store) SetDaemonStatus(ctx context.Context, id int64, st DaemonStatus) (changed bool, err error) {
	if st.Connectors == "" {
		st.Connectors = "[]"
	}
	err = s.db.unlocked(ctx, func(t *txn) error {
		old := DaemonStatus{Connectors: "[]"}
		err := t.QueryRowContext(ctx, `SELECT can_host, reason, egress, connectors FROM daemon_status WHERE daemon_id = ?`, id).
			Scan(&old.CanHost, &old.Reason, &old.Egress, &old.Connectors)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		changed = old.CanHost != st.CanHost || old.Reason != st.Reason || old.Egress != st.Egress || old.Connectors != st.Connectors
		_, err = t.ExecContext(ctx, `INSERT INTO daemon_status(daemon_id, can_host, reason, version, egress, connectors) VALUES(?, ?, ?, ?, ?, ?)
			ON CONFLICT(daemon_id) DO UPDATE SET can_host = excluded.can_host, reason = excluded.reason, version = excluded.version,
				egress = excluded.egress, connectors = excluded.connectors`, id, st.CanHost, st.Reason, st.Version, st.Egress, st.Connectors)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("set daemon status: %w", err)
	}
	return changed, nil
}

// liveDaemons selects the daemons with a connection live at ?2 whose seat stands, in room ?1, or
// in every room when ?1 is "".
const liveDaemons = `SELECT d.id, d.room_id, d.participant_id, MIN(c.connected_at_ms), COALESCE(st.can_host, 0), COALESCE(st.reason, ''),
		COALESCE(st.version, ''), COALESCE(st.egress, 0), COALESCE(st.connectors, '[]')
	FROM daemon_connections c JOIN daemons d ON d.id = c.daemon_id
	JOIN participants p ON p.id = d.participant_id AND p.removed_at IS NULL
	LEFT JOIN daemon_status st ON st.daemon_id = d.id
	WHERE c.expires_at_ms > ?2 AND (?1 = '' OR c.room_id = ?1)
	GROUP BY d.id, d.room_id, d.participant_id, st.can_host, st.reason, st.version, st.egress, st.connectors `

// LiveDaemons lists the daemons of room, every room when room is "", with a connection live at
// nowMS on any pod, by participant.
func (s *Store) LiveDaemons(ctx context.Context, roomID string, nowMS int64) ([]LiveDaemon, error) {
	rows, err := s.db.QueryContext(ctx, liveDaemons+`ORDER BY d.participant_id, d.id`, roomID, nowMS)
	if err != nil {
		return nil, fmt.Errorf("live daemons: %w", err)
	}
	defer rows.Close()
	out := []LiveDaemon{}
	for rows.Next() {
		var d LiveDaemon
		var since int64
		if err := rows.Scan(&d.ID, &d.RoomID, &d.ParticipantID, &since, &d.CanHost, &d.Reason, &d.Version, &d.Egress, &d.Connectors); err != nil {
			return nil, fmt.Errorf("live daemons: %w", err)
		}
		d.Since = time.UnixMilli(since)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("live daemons: %w", err)
	}
	return out, nil
}

// ElectHost keeps room's host while its daemon is live and can host; otherwise the participant
// whose eligible daemon connected first becomes host, and the room is told: the returned line, nil
// when the host stays (D7). One transaction under the write lock, so pods electing at once agree.
// ErrNotFound for an unknown room.
func (s *Store) ElectHost(ctx context.Context, roomID string, nowMS int64) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "elect host", func(tx *txn) error {
		var host int64
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(host_participant_id, 0) FROM rooms WHERE id = ?`, roomID).Scan(&host)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("elect host: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT d.participant_id, p.name FROM daemon_connections c JOIN daemons d ON d.id = c.daemon_id
			JOIN participants p ON p.id = d.participant_id AND p.removed_at IS NULL
			JOIN daemon_status st ON st.daemon_id = d.id AND st.can_host = 1
			WHERE c.room_id = ? AND c.expires_at_ms > ?
			GROUP BY d.id, d.participant_id, p.name ORDER BY MIN(c.connected_at_ms), d.id`, roomID, nowMS)
		if err != nil {
			return fmt.Errorf("elect host: %w", err)
		}
		type cand struct {
			id   int64
			name string
		}
		var eligible []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.name); err != nil {
				_ = rows.Close()
				return fmt.Errorf("elect host: %w", err)
			}
			eligible = append(eligible, c)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("elect host: %w", err)
		}
		for _, c := range eligible {
			if c.id == host {
				return nil
			}
		}
		if len(eligible) == 0 {
			return nil
		}
		to := eligible[0]
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET host_participant_id = ?, host_moved_line = '' WHERE id = ?`, to.id, roomID); err != nil {
			return fmt.Errorf("elect host: %w", err)
		}
		if err := hostChanged(ctx, tx, roomID); err != nil {
			return err
		}
		m = &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: to.id, Sender: to.name, Body: to.name + " holds the agent(s)."}
		return insertMessage(ctx, tx, m)
	})
	return m, err
}

// liveHost tells whether participant pid of room has a daemon live at nowMS able to host.
func liveHost(ctx context.Context, tx *txn, roomID string, pid, nowMS int64) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM daemon_connections c JOIN daemons d ON d.id = c.daemon_id
		JOIN daemon_status st ON st.daemon_id = d.id AND st.can_host = 1
		WHERE c.room_id = ? AND d.participant_id = ? AND c.expires_at_ms > ?)`, roomID, pid, nowMS).Scan(&ok)
	return ok, err
}

// Pairing is a daemon's request to pair, waiting for a participant's answer, then for the
// daemon's poll. Status is "pending", "approved" or "denied".
type Pairing struct {
	ID, RoomID, Code string
	SecretHash       string
	Status           string
	ExpiresAt        time.Time
}

// CreatePairing records p unless room holds perRoom unexpired requests already, or the server
// total; ok false then. Creates run one at a time, so the caps are exact.
func (s *Store) CreatePairing(ctx context.Context, p *Pairing, perRoom, total int, nowMS int64) (ok bool, err error) {
	err = s.db.unlocked(ctx, func(t *txn) error {
		if err := xactLock(ctx, t, LockPairing, ""); err != nil {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO pairings(id, room_id, code, secret_hash, expires_at_ms)
			SELECT ?1, ?2, ?3, ?4, CAST(?5 AS BIGINT)
			WHERE (SELECT count(*) FROM pairings WHERE room_id = ?2 AND expires_at_ms > ?6) < ?7
				AND (SELECT count(*) FROM pairings WHERE expires_at_ms > ?6) < ?8`,
			p.ID, p.RoomID, p.Code, p.SecretHash, p.ExpiresAt.UnixMilli(), nowMS, perRoom, total)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	if err != nil {
		return false, fmt.Errorf("create pairing: %w", err)
	}
	return ok, nil
}

// PendingPairing returns room's unanswered, unexpired request id; ErrNotFound if there is none.
func (s *Store) PendingPairing(ctx context.Context, roomID, id string, nowMS int64) (*Pairing, error) {
	p := Pairing{ID: id, RoomID: roomID, Status: "pending"}
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT code, expires_at_ms FROM pairings WHERE id = ? AND room_id = ? AND status = 'pending' AND expires_at_ms > ?`,
		id, roomID, nowMS).Scan(&p.Code, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pending pairing: %w", err)
	}
	p.ExpiresAt = time.UnixMilli(exp)
	return &p, nil
}

// ApprovePairing answers room's pending request id: participant pid's daemon is paired when the
// daemon polls. ErrNotFound when the request expired or was answered, or pid holds no seat in room.
func (s *Store) ApprovePairing(ctx context.Context, roomID, id string, pid, nowMS int64) error {
	var n int64
	err := s.db.unlocked(ctx, func(t *txn) error {
		res, err := t.ExecContext(ctx, `UPDATE pairings SET status = 'approved', approved_by = ?3
			WHERE id = ?1 AND room_id = ?2 AND status = 'pending' AND expires_at_ms > ?4
				AND EXISTS (SELECT 1 FROM participants WHERE id = ?3 AND room_id = ?2 AND removed_at IS NULL)`, id, roomID, pid, nowMS)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("approve pairing: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DenyPairing answers room's pending request id with a no; ErrNotFound when it expired or was
// answered.
func (s *Store) DenyPairing(ctx context.Context, roomID, id string, nowMS int64) error {
	var n int64
	err := s.db.unlocked(ctx, func(t *txn) error {
		res, err := t.ExecContext(ctx, `UPDATE pairings SET status = 'denied' WHERE id = ? AND room_id = ? AND status = 'pending' AND expires_at_ms > ?`,
			id, roomID, nowMS)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("deny pairing: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PollPairing answers the daemon holding request id's secret: "pending", or the answer, given
// once, the request then gone. On "approved" the approving participant's daemon is paired with
// tokenHash, a token minted for this answer, and returned: the secret itself never authenticates a
// daemon. ErrNotFound for an unknown, expired or answered-and-polled request, another secret, or an
// approver whose seat ended.
func (s *Store) PollPairing(ctx context.Context, id, secretHash, tokenHash string, nowMS int64) (string, *Daemon, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM pairings WHERE id = ? AND secret_hash = ? AND expires_at_ms > ?`, id, secretHash, nowMS).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("poll pairing: %w", err)
	}
	if status == "pending" {
		return status, nil, nil
	}
	var d *Daemon
	err = s.withTx(ctx, "poll pairing", func(tx *txn) error {
		var room string
		var by sql.NullInt64
		err := tx.QueryRowContext(ctx, `DELETE FROM pairings WHERE id = ? AND secret_hash = ? AND status != 'pending' AND expires_at_ms > ?
			RETURNING status, room_id, approved_by`, id, secretHash, nowMS).Scan(&status, &room, &by)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("poll pairing: %w", err)
		}
		if status != "approved" {
			return nil
		}
		var seated bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM participants WHERE id = ? AND room_id = ? AND removed_at IS NULL)`,
			by.Int64, room).Scan(&seated); err != nil {
			return fmt.Errorf("poll pairing: %w", err)
		}
		if !seated {
			return ErrNotFound
		}
		d, err = pairDaemon(ctx, tx, room, by.Int64, tokenHash)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return status, d, nil
}
