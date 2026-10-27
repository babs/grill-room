package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ProfileSessionMaxAge is how long a profile session stays valid: Chrome caps cookies at 400 days.
const ProfileSessionMaxAge = 400 * 24 * time.Hour

// RecoveryLinkTTL is how long an admin recovery link works: the admin hands it over by another
// channel, which takes longer than an email (011-AC1).
const RecoveryLinkTTL = 24 * time.Hour

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
	// ErrLinkExpired is a stored magic link past its expiry, until a sweep drops it (011-AC23).
	ErrLinkExpired = errors.New("link expired")
	// ErrLinkUsed is a stored magic link already spent, until a sweep drops it (011-AC23).
	ErrLinkUsed = errors.New("link already used")
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
	// Name is the one the profile chose from a room (005-AC1); empty until it does.
	Name string `json:"name,omitempty"`
	// PendingEmail is the address of the newest unused bind link; only GET /api/profile sets it,
	// for its owner (011-AC21).
	PendingEmail string `json:"pending_email,omitempty"`
	// PendingTTLS and PendingAgeS are that link's seconds left and seconds since it was sent, on
	// the server clock, for the page's lifetime and resend cooldown; set with PendingEmail only.
	PendingTTLS *int64 `json:"pending_ttl_s,omitempty"`
	PendingAgeS *int64 `json:"pending_age_s,omitempty"`
	// EmailNudgeUntil hides the add-an-email banner until then (011-AC8).
	EmailNudgeUntil *time.Time `json:"email_nudge_until,omitempty"`
	// OIDCIssuer is the provider the profile is bound to, empty for none; OIDCSignedInAt its last
	// sign-in there (021-AC4, 021-AC5).
	OIDCIssuer     string    `json:"-"`
	OIDCSignedInAt time.Time `json:"-"`
}

// ProfileRoom is a room a profile is in, under the name it joined with.
type ProfileRoom struct {
	RoomID        string    `json:"room_id"`
	RoomName      string    `json:"room_name"`
	ParticipantID int64     `json:"participant_id"`
	Name          string    `json:"name"`
	JoinedAt      time.Time `json:"joined_at"`
	// People counts the room's participants with a valid invite; LastActivity is its last line, or
	// its creation; Admin says the profile is a room admin there (005-AC41).
	People       int       `json:"people"`
	LastActivity time.Time `json:"last_activity"`
	Admin        bool      `json:"admin"`
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
	QueryRowContext(ctx context.Context, query string, args ...any) *row
}

func createProfile(ctx context.Context, db querier, recoveryHash, sessionHash string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO profiles(recovery_hash, created_at, uuid) VALUES(?,?,gen_random_uuid()::text) RETURNING id`, recoveryHash, now()).Scan(&id)
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

// DeleteProfileSession ends the profile session sessionHash; none is not an error.
func (s *Store) DeleteProfileSession(ctx context.Context, sessionHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM profile_sessions WHERE token_hash = ?`, sessionHash); err != nil {
		return fmt.Errorf("delete profile session: %w", err)
	}
	return nil
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
	var nudge, oidcAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, email, created_at, COALESCE(uuid, ''), COALESCE(name, ''), email_nudge_until, COALESCE(oidc_issuer, ''), oidc_signed_in_at
		FROM profiles WHERE `+where, args...).
		Scan(&p.ID, &email, &created, &p.UUID, &p.Name, &nudge, &p.OIDCIssuer, &oidcAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	p.Email, p.CreatedAt = email.String, parseTS(created)
	if nudge.Valid {
		t := parseTS(nudge.String)
		p.EmailNudgeUntil = &t
	}
	if oidcAt.Valid {
		p.OIDCSignedInAt = parseTS(oidcAt.String)
	}
	if p.Rooms, err = s.profileRooms(ctx, p.ID); err != nil {
		return nil, err
	}
	return &p, nil
}

// profileRooms lists the rooms a profile still has access to, newest join first, once each.
func (s *Store) profileRooms(ctx context.Context, profileID int64) ([]ProfileRoom, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.name, pa.id, pa.name, pa.joined_at,
			(SELECT COUNT(DISTINCT COALESCE(p.profile_id, -p.id)) FROM participants p
				WHERE p.room_id = r.id AND p.removed_at IS NULL),
			COALESCE(`+roomActivitySQL+`, r.created_at),
			EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = r.id AND a.profile_id = pa.profile_id)
		FROM participants pa
		JOIN rooms r ON r.id = pa.room_id
		WHERE pa.profile_id = ? AND pa.removed_at IS NULL ORDER BY pa.id DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("profile rooms: %w", err)
	}
	defer rows.Close()
	out := []ProfileRoom{}
	seen := map[string]bool{}
	for rows.Next() {
		var r ProfileRoom
		var joined, last string
		if err := rows.Scan(&r.RoomID, &r.RoomName, &r.ParticipantID, &r.Name, &joined, &r.People, &last, &r.Admin); err != nil {
			return nil, fmt.Errorf("scan profile room: %w", err)
		}
		if seen[r.RoomID] {
			continue
		}
		seen[r.RoomID] = true
		r.JoinedAt, r.LastActivity = parseTS(joined), parseTS(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProfileParticipant returns the newest participant of room whose profile holds a valid session
// sessionHash and whose seat is valid. ErrNotFound otherwise.
func (s *Store) ProfileParticipant(ctx context.Context, sessionHash, roomID string) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT pa.id, pa.room_id, pa.invite_id, pa.name, pa.joined_at, pa.can_trigger, pa.profile_id
		FROM profile_sessions ps
		JOIN participants pa ON pa.profile_id = ps.profile_id
		WHERE ps.token_hash = ? AND ps.expires_at_s > ? AND pa.room_id = ? AND pa.removed_at IS NULL
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
	return s.withTx(ctx, "delete profile", func(tx *txn) error {
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM profiles WHERE id = ?`, id)
	})
}

// magicLinkKeep is how long a used or expired magic link stays stored, so opening it again still
// tells "used" or "expired" rather than "invalid" (011-AC23).
const magicLinkKeep = 24 * time.Hour

// CreateMagicLink stores l, and drops every profile's links used or expired over magicLinkKeep
// before now.
func (s *Store) CreateMagicLink(ctx context.Context, l MagicLink, now time.Time) error {
	cutoff := now.Add(-magicLinkKeep)
	// used_at is UTC RFC 3339: it orders as text, to within a second.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM magic_links WHERE used_at < ? OR expires_at_ms <= ?`,
		formatTS(cutoff), cutoff.UnixMilli()); err != nil {
		return fmt.Errorf("sweep magic links: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO magic_links(profile_id, email, purpose, token_hash, created_at, expires_at_ms) VALUES(?,?,?,?,?,?)`,
		l.ProfileID, l.Email, l.Purpose, l.TokenHash, formatTS(now), l.ExpiresAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("create magic link: %w", err)
	}
	return nil
}

// MagicUse is a request to spend a magic link.
type MagicUse struct {
	TokenHash string
	// Purpose is the kind the caller expects: a sign-in link offered as a bind link, or the
	// reverse, is unknown.
	Purpose     string
	SessionHash string
	// Current is the profile of the browser opening the link, 0 for none.
	Current int64
	// Confirm binds a bind link opened outside its profile (011-AC23).
	Confirm bool
	Now     time.Time
}

// UseMagicLink spends the link u names, in one transaction. A sign-in link opens a session with
// u.SessionHash on its profile, while the profile still holds its email. A bind link binds its
// email to its profile and opens no session. Opened outside its profile without u.Confirm, it
// answers ErrWrongProfile and stays unused; for both outside cases the profile returned holds only
// the display name and the address, as the browser is not its owner. ErrNotFound for an unknown or
// stale link, ErrLinkUsed and ErrLinkExpired for a stored one, ErrEmailTaken when another profile
// bound the email meanwhile, leaving the link unused.
func (s *Store) UseMagicLink(ctx context.Context, u MagicUse) (*Profile, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id, profileID, expires int64
	var email string
	var used sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, profile_id, email, used_at, expires_at_ms FROM magic_links WHERE token_hash = ? AND purpose = ?`,
		u.TokenHash, u.Purpose).Scan(&id, &profileID, &email, &used, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("use magic link: %w", err)
	case used.Valid:
		return nil, ErrLinkUsed
	case expires <= u.Now.UnixMilli():
		return nil, ErrLinkExpired
	}
	var holder int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM profiles WHERE email = ?`, email).Scan(&holder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	var elsewhere *Profile
	usedAt := formatTS(u.Now)
	switch {
	case u.Purpose == MagicSignIn && holder != profileID:
		return nil, ErrNotFound
	case u.Purpose == MagicSignIn:
		if err := createSession(ctx, tx, profileID, u.SessionHash); err != nil {
			return nil, err
		}
	case holder != 0 && holder != profileID:
		return nil, ErrEmailTaken
	default:
		if u.Current != profileID {
			elsewhere = &Profile{ID: profileID, Email: email}
			if err := tx.QueryRowContext(ctx, `SELECT `+displayNameSQL+` FROM profiles p WHERE p.id = ?`, profileID).Scan(&elsewhere.Name); err != nil {
				return nil, fmt.Errorf("use magic link: %w", err)
			}
			if !u.Confirm {
				return elsewhere, ErrWrongProfile
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE profiles SET email = ? WHERE id = ?`, email, profileID); err != nil {
			return nil, fmt.Errorf("bind email: %w", err)
		}
		// The bind settles the address: the profile's other pending bind links are spent with it,
		// so none of them shows as pending nor rebinds an older address (011-AC21).
		if _, err := tx.ExecContext(ctx, `UPDATE magic_links SET used_at = ? WHERE profile_id = ? AND purpose = ? AND used_at IS NULL`,
			usedAt, profileID, MagicBind); err != nil {
			return nil, fmt.Errorf("use magic link: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE magic_links SET used_at = ? WHERE id = ?`, usedAt, id); err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("use magic link: %w", err)
	}
	if elsewhere != nil {
		return elsewhere, nil
	}
	return s.profile(ctx, `id = ?`, profileID)
}

// displayNameSQL is the name a profile p shows: the one it chose, else its latest join's.
const displayNameSQL = `COALESCE(p.name, (SELECT name FROM participants WHERE profile_id = p.id ORDER BY id DESC LIMIT 1), '')`

// PendingBind is a bind link waiting in its owner's inbox.
type PendingBind struct {
	Email             string
	SentAt, ExpiresAt time.Time
}

// PendingEmail returns profileID's newest bind link while it is unused and unexpired, a zero
// PendingBind otherwise: an older link's address was replaced by the newer one (011-AC21).
func (s *Store) PendingEmail(ctx context.Context, profileID int64, now time.Time) (PendingBind, error) {
	var pb PendingBind
	var sent string
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT email, created_at, expires_at_ms FROM (SELECT email, created_at, used_at, expires_at_ms FROM magic_links
		WHERE profile_id = ? AND purpose = ? ORDER BY id DESC LIMIT 1) WHERE used_at IS NULL AND expires_at_ms > ?`,
		profileID, MagicBind, now.UnixMilli()).Scan(&pb.Email, &sent, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingBind{}, nil
	}
	if err != nil {
		return PendingBind{}, fmt.Errorf("pending email: %w", err)
	}
	pb.SentAt, pb.ExpiresAt = parseTS(sent), time.UnixMilli(expires).UTC()
	return pb, nil
}

// DeleteMagicLink drops the link tokenHash, one whose mail did not go out (011-AC28).
func (s *Store) DeleteMagicLink(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM magic_links WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete magic link: %w", err)
	}
	return nil
}

// SnoozeEmailNudge hides profileID's add-an-email banner until until. ErrNotFound for no profile.
func (s *Store) SnoozeEmailNudge(ctx context.Context, profileID int64, until time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE profiles SET email_nudge_until = ? WHERE id = ?`, formatTS(until), profileID)
	if err != nil {
		return fmt.Errorf("snooze email nudge: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("snooze email nudge: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetProfileName makes name the one the profile chose (005-AC48): joins and new rooms start from
// it, and its seats keep the names they have.
func (s *Store) SetProfileName(ctx context.Context, profileID int64, name string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET name = ? WHERE id = ?`, name, profileID); err != nil {
		return fmt.Errorf("set profile name: %w", err)
	}
	return nil
}

// CreateRecoveryLink stores a one-time recovery link tokenHash for profileID, valid
// RecoveryLinkTTL from now, and returns its expiry. It voids the profile's earlier unused links
// and sweeps every used or expired one. The audit entry ctx carries gets the profile's uuid, never
// the token (011-AC1, 011-AC3). ErrNotFound for no profile.
func (s *Store) CreateRecoveryLink(ctx context.Context, profileID int64, tokenHash string, now time.Time) (time.Time, error) {
	expires := now.Add(RecoveryLinkTTL)
	err := s.withTx(ctx, "create recovery link", func(tx *txn) error {
		var uuid string
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(uuid, '') FROM profiles WHERE id = ?`, profileID).Scan(&uuid)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("create recovery link: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_links WHERE used_at IS NOT NULL OR expires_at_ms <= ? OR profile_id = ?`,
			now.UnixMilli(), profileID); err != nil {
			return fmt.Errorf("create recovery link: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_links(profile_id, token_hash, created_at, expires_at_ms) VALUES(?,?,?,?)`,
			profileID, tokenHash, formatTS(now), expires.UnixMilli()); err != nil {
			return fmt.Errorf("create recovery link: %w", err)
		}
		return auditTx(ctx, tx, "", map[string]any{"profile_uuid": uuid})
	})
	if err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

// UseRecoveryLink spends the unused, unexpired recovery link tokenHash and opens a session with
// sessionHash on its profile, in one transaction, writing the audit entry ctx carries for the
// profile. ErrNotFound for an unknown, used, voided or expired link (011-AC2).
func (s *Store) UseRecoveryLink(ctx context.Context, tokenHash, sessionHash string, now time.Time) (*Profile, error) {
	var id int64
	err := s.withTx(ctx, "use recovery link", func(tx *txn) error {
		err := tx.QueryRowContext(ctx, `UPDATE recovery_links SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at_ms > ? RETURNING profile_id`,
			formatTS(now), tokenHash, now.UnixMilli()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("use recovery link: %w", err)
		}
		if err := createSession(ctx, tx, id, sessionHash); err != nil {
			return err
		}
		var uuid string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(uuid, '') FROM profiles WHERE id = ?`, id).Scan(&uuid); err != nil {
			return fmt.Errorf("use recovery link: %w", err)
		}
		return auditTx(ctx, tx, strconv.FormatInt(id, 10), map[string]any{"profile_uuid": uuid})
	})
	if err != nil {
		return nil, err
	}
	return s.profile(ctx, `id = ?`, id)
}
