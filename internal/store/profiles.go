package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProfileSessionMaxAge is how long a profile session stays valid: Chrome caps cookies at 400 days.
const ProfileSessionMaxAge = 400 * 24 * time.Hour

// Magic link purposes.
const (
	MagicBind   = "bind"
	MagicSignIn = "signin"
)

var (
	// ErrEmailTaken is returned when binding an email another profile already holds.
	ErrEmailTaken = errors.New("email bound to another profile")
	// ErrWrongProfile is returned when a bind link is opened outside the profile that asked for it.
	ErrWrongProfile = errors.New("bind link opened outside its profile")
)

// Profile is a person across rooms. Only hashes of its recovery and session tokens are stored.
type Profile struct {
	ID int64 `json:"id"`
	// Email is personal data: only its owner and the admin may see it.
	Email     string        `json:"email,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	Rooms     []ProfileRoom `json:"rooms"`
	// UUID names the profile in the audit log of its room admin actions; never sent.
	UUID string `json:"-"`
}

// ProfileRoom is a room a profile is in, under the name it joined with.
type ProfileRoom struct {
	RoomID        string    `json:"room_id"`
	RoomName      string    `json:"room_name"`
	ParticipantID int64     `json:"participant_id"`
	Name          string    `json:"name"`
	JoinedAt      time.Time `json:"joined_at"`
}

// MagicLink is an emailed one-time sign-in link. Only the hash of its token is stored.
type MagicLink struct {
	ProfileID int64
	Email     string
	Purpose   string
	TokenHash string
	ExpiresAt time.Time
}

type querier interface {
	execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func createProfile(ctx context.Context, db querier, recoveryHash, sessionHash string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO profiles(recovery_hash, created_at, uuid) VALUES(?,?,`+uuidSQL+`) RETURNING id`, recoveryHash, now()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create profile: %w", err)
	}
	if err := createSession(ctx, db, id, sessionHash); err != nil {
		return 0, err
	}
	return id, nil
}

func createSession(ctx context.Context, db execer, profileID int64, sessionHash string) error {
	t := time.Now()
	// Expired sessions are swept here: they are the only writes that make them.
	if _, err := db.ExecContext(ctx, `DELETE FROM profile_sessions WHERE expires_at_s <= ?`, t.Unix()); err != nil {
		return fmt.Errorf("sweep profile sessions: %w", err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO profile_sessions(profile_id, token_hash, created_at, expires_at_s) VALUES(?,?,?,?)`,
		profileID, sessionHash, now(), t.Add(ProfileSessionMaxAge).Unix())
	if err != nil {
		return fmt.Errorf("create profile session: %w", err)
	}
	return nil
}

// ProfileBySession returns the profile of a valid session, with its rooms. ErrNotFound otherwise.
func (s *Store) ProfileBySession(ctx context.Context, sessionHash string) (*Profile, error) {
	return s.profile(ctx, `id = (SELECT profile_id FROM profile_sessions WHERE token_hash = ? AND expires_at_s > ?)`, sessionHash, time.Now().Unix())
}

// SignInByRecovery opens a session with sessionHash on the profile holding recoveryHash.
// ErrNotFound if none does.
func (s *Store) SignInByRecovery(ctx context.Context, recoveryHash, sessionHash string) (*Profile, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM profiles WHERE recovery_hash = ?`, recoveryHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("profile by recovery: %w", err)
	}
	if err := createSession(ctx, s.db, id, sessionHash); err != nil {
		return nil, err
	}
	return s.profile(ctx, `id = ?`, id)
}

// ProfileIDByEmail returns the id of the profile email is bound to; ErrNotFound if none.
func (s *Store) ProfileIDByEmail(ctx context.Context, email string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM profiles WHERE email = ?`, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("profile by email: %w", err)
	}
	return id, nil
}

func (s *Store) profile(ctx context.Context, where string, args ...any) (*Profile, error) {
	var p Profile
	var email sql.NullString
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, email, created_at, COALESCE(uuid, '') FROM profiles WHERE `+where, args...).Scan(&p.ID, &email, &created, &p.UUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	p.Email, p.CreatedAt = email.String, parseTS(created)
	if p.Rooms, err = s.profileRooms(ctx, p.ID); err != nil {
		return nil, err
	}
	return &p, nil
}

// profileRooms lists the rooms a profile still has access to, newest join first, once each.
func (s *Store) profileRooms(ctx context.Context, profileID int64) ([]ProfileRoom, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.name, pa.id, pa.name, pa.joined_at FROM participants pa
		JOIN invites i ON i.id = pa.invite_id
		JOIN rooms r ON r.id = pa.room_id
		WHERE pa.profile_id = ? AND i.revoked_at IS NULL ORDER BY pa.id DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("profile rooms: %w", err)
	}
	defer rows.Close()
	out := []ProfileRoom{}
	seen := map[string]bool{}
	for rows.Next() {
		var r ProfileRoom
		var joined string
		if err := rows.Scan(&r.RoomID, &r.RoomName, &r.ParticipantID, &r.Name, &joined); err != nil {
			return nil, fmt.Errorf("scan profile room: %w", err)
		}
		if seen[r.RoomID] {
			continue
		}
		seen[r.RoomID] = true
		r.JoinedAt = parseTS(joined)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProfileParticipant returns the newest participant of room whose profile holds a valid session
// sessionHash and whose invite is not revoked. ErrNotFound otherwise.
func (s *Store) ProfileParticipant(ctx context.Context, sessionHash, roomID string) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT pa.id, pa.room_id, pa.invite_id, pa.name, pa.joined_at, pa.can_trigger, pa.profile_id
		FROM profile_sessions ps
		JOIN participants pa ON pa.profile_id = ps.profile_id
		JOIN invites i ON i.id = pa.invite_id
		WHERE ps.token_hash = ? AND ps.expires_at_s > ? AND pa.room_id = ? AND i.revoked_at IS NULL
		ORDER BY pa.id DESC LIMIT 1`, sessionHash, time.Now().Unix(), roomID).
		Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.ProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("profile participant: %w", err)
	}
	p.JoinedAt = parseTS(joined)
	return &p, nil
}

// DeleteProfile removes a profile with its sessions, magic links and email, and detaches its
// participants, which keep their name and their room access.
func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	err := s.withTx(ctx, "delete profile", func(tx *sql.Tx) error {
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM profiles WHERE id = ?`, id)
	})
	if err != nil {
		return err
	}
	// The WAL still holds the deleted email's old pages until a checkpoint rewrites the main file.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint after delete profile %d: %w", id, err)
	}
	return nil
}

// CreateMagicLink stores l, and drops the used and expired links of every profile.
func (s *Store) CreateMagicLink(ctx context.Context, l MagicLink) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM magic_links WHERE used_at IS NOT NULL OR expires_at_ms <= ?`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("sweep magic links: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO magic_links(profile_id, email, purpose, token_hash, created_at, expires_at_ms) VALUES(?,?,?,?,?,?)`,
		l.ProfileID, l.Email, l.Purpose, l.TokenHash, now(), l.ExpiresAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("create magic link: %w", err)
	}
	return nil
}

// UseMagicLink spends the unused, unexpired link tokenHash of the purpose the caller expects, in one
// transaction: a sign-in link offered as a bind link, or the reverse, is unknown. A sign-in link
// opens a session with sessionHash on its profile, while the profile still holds its email. A bind
// link binds its email to its profile and opens no session; current must be that profile, as only
// the browser that asked may confirm. ErrNotFound for an unknown, used, expired or stale link;
// ErrWrongProfile for a bind link opened elsewhere and ErrEmailTaken when another profile bound the
// email meanwhile, both leaving the link unused.
func (s *Store) UseMagicLink(ctx context.Context, tokenHash, purpose, sessionHash string, current int64) (*Profile, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id, profileID int64
	var email string
	err = tx.QueryRowContext(ctx, `SELECT id, profile_id, email FROM magic_links WHERE token_hash = ? AND purpose = ? AND used_at IS NULL AND expires_at_ms > ?`,
		tokenHash, purpose, time.Now().UnixMilli()).Scan(&id, &profileID, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	var holder int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM profiles WHERE email = ?`, email).Scan(&holder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	switch {
	case purpose == MagicSignIn && holder != profileID:
		return nil, ErrNotFound
	case purpose == MagicSignIn:
		if err := createSession(ctx, tx, profileID, sessionHash); err != nil {
			return nil, err
		}
	case current != profileID:
		return nil, ErrWrongProfile
	case holder != 0 && holder != profileID:
		return nil, ErrEmailTaken
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE profiles SET email = ? WHERE id = ?`, email, profileID); err != nil {
			return nil, fmt.Errorf("bind email: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE magic_links SET used_at = ? WHERE id = ?`, now(), id); err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	return s.profile(ctx, `id = ?`, profileID)
}
