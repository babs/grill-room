package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Daemon is a participant's paired host daemon. Only the hash of its token is stored.
type Daemon struct {
	ID            int64     `json:"id"`
	RoomID        string    `json:"room_id"`
	ParticipantID int64     `json:"participant_id"`
	CreatedAt     time.Time `json:"created_at"`
	// Settings are the cap and approval mode the daemon last reported; the room applies the
	// current host's, even while its daemon is offline.
	Settings DaemonSettings `json:"-"`
}

// DaemonSettings are a host daemon's cap and approval mode (002-AC19, 002-AC21).
type DaemonSettings struct {
	MaxModel   string `json:"max_model,omitempty"`
	MaxEffort  string `json:"max_effort,omitempty"`
	ApproveAll bool   `json:"approve_all"`
}

const daemonCols = `SELECT d.id, d.room_id, d.participant_id, d.created_at, d.max_model, d.max_effort, d.approve_all FROM daemons d `

func scanDaemon(sc scanner) (*Daemon, error) {
	var d Daemon
	var created string
	err := sc.Scan(&d.ID, &d.RoomID, &d.ParticipantID, &created, &d.Settings.MaxModel, &d.Settings.MaxEffort, &d.Settings.ApproveAll)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan daemon: %w", err)
	}
	d.CreatedAt = parseTS(created)
	return &d, nil
}

// PairDaemon records participant's daemon with tokenHash. A participant has one daemon: pairing
// again replaces the previous one and its token. ErrNotFound if the participant is not in room.
// The daemon's machine name is not kept: the room never shows it (003-AC17).
func (s *Store) PairDaemon(ctx context.Context, roomID string, participantID int64, tokenHash string) (*Daemon, error) {
	d := &Daemon{RoomID: roomID, ParticipantID: participantID, CreatedAt: time.Now().UTC()}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO daemons(room_id, participant_id, name, token_hash, created_at)
		SELECT room_id, id, '', ?, ? FROM participants WHERE id = ? AND room_id = ?
		ON CONFLICT(participant_id) DO UPDATE SET token_hash = excluded.token_hash, created_at = excluded.created_at
		RETURNING id`, tokenHash, d.CreatedAt.Format(time.RFC3339Nano), participantID, roomID).Scan(&d.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pair daemon: %w", err)
	}
	return d, nil
}

// DaemonByToken returns the daemon holding tokenHash, provided its participant's invite is not revoked.
func (s *Store) DaemonByToken(ctx context.Context, tokenHash string) (*Daemon, error) {
	return scanDaemon(s.db.QueryRowContext(ctx, daemonCols+`
		JOIN participants p ON p.id = d.participant_id
		JOIN invites i ON i.id = p.invite_id
		WHERE d.token_hash = ? AND i.revoked_at IS NULL`, tokenHash))
}

// HostDaemon returns the paired daemon of the room's host, connected or not; ErrNotFound when the
// room has no host or the host no daemon.
func (s *Store) HostDaemon(ctx context.Context, roomID string) (*Daemon, error) {
	return scanDaemon(s.db.QueryRowContext(ctx, daemonCols+`JOIN rooms r ON r.host_participant_id = d.participant_id WHERE r.id = ?`, roomID))
}

// SetDaemonSettings records what daemon id reported; changed is false when it held them already.
func (s *Store) SetDaemonSettings(ctx context.Context, id int64, set DaemonSettings) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE daemons SET max_model = ?, max_effort = ?, approve_all = ?
		WHERE id = ? AND NOT (max_model = ? AND max_effort = ? AND approve_all = ?)`,
		set.MaxModel, set.MaxEffort, set.ApproveAll, id, set.MaxModel, set.MaxEffort, set.ApproveAll)
	if err != nil {
		return false, fmt.Errorf("set daemon settings: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set daemon settings: %w", err)
	}
	return n > 0, nil
}
