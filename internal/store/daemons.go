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
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
}

// PairDaemon records participant's daemon with tokenHash. A participant has one daemon: pairing
// again replaces the previous one and its token. ErrNotFound if the participant is not in room.
func (s *Store) PairDaemon(ctx context.Context, roomID string, participantID int64, name, tokenHash string) (*Daemon, error) {
	d := &Daemon{RoomID: roomID, ParticipantID: participantID, Name: name, CreatedAt: time.Now().UTC()}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO daemons(room_id, participant_id, name, token_hash, created_at)
		SELECT room_id, id, ?, ?, ? FROM participants WHERE id = ? AND room_id = ?
		ON CONFLICT(participant_id) DO UPDATE SET name = excluded.name, token_hash = excluded.token_hash, created_at = excluded.created_at
		RETURNING id`, name, tokenHash, d.CreatedAt.Format(time.RFC3339Nano), participantID, roomID).Scan(&d.ID)
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
	var d Daemon
	var created string
	err := s.db.QueryRowContext(ctx, `
		SELECT d.id, d.room_id, d.participant_id, d.name, d.created_at FROM daemons d
		JOIN participants p ON p.id = d.participant_id
		JOIN invites i ON i.id = p.invite_id
		WHERE d.token_hash = ? AND i.revoked_at IS NULL`, tokenHash).Scan(&d.ID, &d.RoomID, &d.ParticipantID, &d.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("daemon by token: %w", err)
	}
	d.CreatedAt = parseTS(created)
	return &d, nil
}
