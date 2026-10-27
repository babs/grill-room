package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// How an OIDC sign-in found its profile (021-AC4).
const (
	OIDCExisting = "existing"
	OIDCEmail    = "email"
	OIDCNew      = "new"
)

// OIDCSignIn is a verified provider sign-in to bind and open a session for.
type OIDCSignIn struct {
	Issuer, Subject string
	// Email is the claim's address when the provider verified it, empty otherwise.
	Email string
	// Name is the claim's display name, cleaned; empty sets none.
	Name string
	// AdminGroup is the configured admin group when the sign-in's groups hold it, "" otherwise (021-AC11).
	AdminGroup string
	// RecoveryHash and SessionHash are a new profile's recovery hash, and this browser's session.
	RecoveryHash, SessionHash string
	Now                       time.Time
}

// SignInOIDC finds the profile bound to in's issuer and subject, else binds the unbound profile
// holding in.Email, else creates one bound to them, with in.Email when no profile holds it; it
// stamps the sign-in, opens a session with in.SessionHash and writes the audit entry ctx carries
// with the profile as actor, in one transaction (021-AC4). It returns the profile and how it
// was found.
func (s *Store) SignInOIDC(ctx context.Context, in OIDCSignIn) (*Profile, string, error) {
	var profileID int64
	var found string
	err := s.withTx(ctx, "oidc sign-in", func(tx *txn) error {
		var id int64
		how := OIDCExisting
		err := tx.QueryRowContext(ctx, `SELECT id FROM profiles WHERE oidc_issuer = ? AND oidc_subject = ?`, in.Issuer, in.Subject).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("oidc sign-in: %w", err)
		}
		email := in.Email
		if id == 0 && email != "" {
			var bound bool
			err := tx.QueryRowContext(ctx, `SELECT id, oidc_issuer IS NOT NULL FROM profiles WHERE email = ?`, email).Scan(&id, &bound)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("oidc sign-in: %w", err)
			case bound:
				// Another account holds the address: the new profile goes without it.
				id, email = 0, ""
			default:
				how = OIDCEmail
			}
		}
		if id == 0 {
			how = OIDCNew
			if id, err = createProfile(ctx, tx, in.RecoveryHash, in.SessionHash); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE profiles SET email = NULLIF(?, ''), name = NULLIF(?, '') WHERE id = ?`, email, in.Name, id); err != nil {
				return fmt.Errorf("oidc sign-in: %w", err)
			}
		} else if err := createSession(ctx, tx, id, in.SessionHash); err != nil {
			return err
		}
		var uuid string
		// The group gone at the provider ends the admin sessions it opened (021-AC11).
		if err := tx.QueryRowContext(ctx, `WITH gone AS (DELETE FROM admin_sessions WHERE profile_id = ? AND ? = '')
			UPDATE profiles SET oidc_issuer = ?, oidc_subject = ?, oidc_signed_in_at = ?, oidc_admin_group = NULLIF(?, '') WHERE id = ?
			RETURNING COALESCE(uuid, '')`, id, in.AdminGroup, in.Issuer, in.Subject, formatTS(in.Now), in.AdminGroup, id).Scan(&uuid); err != nil {
			return fmt.Errorf("oidc sign-in: %w", err)
		}
		if p, _ := ctx.Value(auditKey{}).(*pendingAudit); p != nil {
			p.e.Actor = ActorProfile + uuid
		}
		if err := auditTx(ctx, tx, strconv.FormatInt(id, 10), map[string]any{"how": how, "iss": in.Issuer}); err != nil {
			return err
		}
		profileID, found = id, how
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	p, err := s.profile(ctx, `id = ?`, profileID)
	return p, found, err
}

// UnbindOIDC clears profile id's provider binding, making it external, signs it out everywhere
// (browser sessions, API tokens, its seats' own tokens, their paired daemons, the admin sessions
// it opened through the provider) and writes the audit entry ctx carries, in one transaction
// (021-AC10, 021-AC11), with the pairings its seats approved and no daemon polled yet. Its seats
// stay, reached again by a sign-in on the profile; it returns them (RoomID and ID only), for the
// caller to take them out of the audio, and the rooms of the daemons it removed. ErrNotFound when
// no bound profile has that id.
func (s *Store) UnbindOIDC(ctx context.Context, id int64) (seats []Participant, daemonRooms []string, err error) {
	err = s.withTx(ctx, "unbind oidc", func(tx *txn) error {
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		// Every change hangs on b, the profile found bound: no row out means none was, and nothing
		// changed. b LEFT JOIN p keeps one row for a profile without seats. A seat's new hash is no
		// SHA-256 hex: no token hashes to it.
		rows, err := tx.QueryContext(ctx, `WITH b AS (UPDATE profiles SET oidc_issuer = NULL, oidc_subject = NULL, oidc_signed_in_at = NULL, oidc_admin_group = NULL
				WHERE id = ? AND oidc_issuer IS NOT NULL RETURNING id),
			a AS (DELETE FROM admin_sessions WHERE profile_id IN (SELECT id FROM b)),
			s AS (DELETE FROM profile_sessions WHERE profile_id IN (SELECT id FROM b)),
			t AS (DELETE FROM api_tokens WHERE profile_id IN (SELECT id FROM b)),
			d AS (DELETE FROM daemons WHERE participant_id IN (SELECT id FROM participants WHERE profile_id IN (SELECT id FROM b)) RETURNING room_id),
			q AS (DELETE FROM pairings WHERE approved_by IN (SELECT id FROM participants WHERE profile_id IN (SELECT id FROM b))),
			p AS (UPDATE participants SET token_hash = 'unbound-' || gen_random_uuid()::text WHERE profile_id IN (SELECT id FROM b) RETURNING room_id, id)
			SELECT false, p.room_id, p.id FROM b LEFT JOIN p ON true UNION ALL SELECT true, d.room_id, NULL FROM d`, id)
		if err != nil {
			return fmt.Errorf("unbind oidc: %w", err)
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			var daemon bool
			var room sql.NullString
			var pid sql.NullInt64
			if err := rows.Scan(&daemon, &room, &pid); err != nil {
				return fmt.Errorf("unbind oidc: %w", err)
			}
			switch {
			case daemon:
				daemonRooms = append(daemonRooms, room.String)
			case pid.Valid:
				found = true
				seats = append(seats, Participant{ID: pid.Int64, RoomID: room.String})
			default:
				found = true
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("unbind oidc: %w", err)
		}
		if !found {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return seats, daemonRooms, nil
}
