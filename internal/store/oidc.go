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

// SignInOIDC finds the profile bound to in's issuer and subject, giving it in.Email when it has
// none and no profile holds it, else binds the unbound profile
// holding in.Email, signing it out everywhere first, else creates one bound to them, with in.Email
// when no profile holds it; it stamps the sign-in, opens a session with in.SessionHash and writes
// the audit entry ctx carries with the profile as actor, in one transaction (021-AC4 as amended by 026). It returns
// the profile, how it was found and what the binding by email ended, for the caller to wake its
// rooms and take its seats out of the audio.
func (s *Store) SignInOIDC(ctx context.Context, in OIDCSignIn) (*Profile, string, EndedCredentials, error) {
	var profileID int64
	var found string
	var ended EndedCredentials
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
		} else {
			// Its other devices got in without the provider: they sign in through it now (021-AC4 as amended by 026).
			if how == OIDCEmail {
				if ended, err = endCredentials(ctx, tx, id); err != nil {
					return err
				}
			}
			if err := createSession(ctx, tx, id, in.SessionHash); err != nil {
				return err
			}
			// A bound profile made without an address gets the verified one, when no profile holds it;
			// never another address over its own.
			if how == OIDCExisting && email != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE profiles SET email = ? WHERE id = ? AND email IS NULL
					AND NOT EXISTS (SELECT 1 FROM profiles WHERE email = ?)`, email, id, email); err != nil {
					return fmt.Errorf("oidc sign-in: %w", err)
				}
			}
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
		return nil, "", EndedCredentials{}, err
	}
	p, err := s.profile(ctx, `id = ?`, profileID)
	return p, found, ended, err
}

// OIDCEmailDaemons returns the daemons SignInOIDC(in) removes by binding the unbound profile
// holding in.Email: none when in's account is bound already or no unbound profile holds it (021-AC4 as amended by 026).
func (s *Store) OIDCEmailDaemons(ctx context.Context, in OIDCSignIn) ([]Daemon, error) {
	if in.Email == "" {
		return nil, nil
	}
	return s.daemonsWhere(ctx, `f.email = ? AND f.oidc_issuer IS NULL AND NOT EXISTS (SELECT 1 FROM profiles b WHERE b.oidc_issuer = ? AND b.oidc_subject = ?)`,
		in.Email, in.Issuer, in.Subject)
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
			w AS (DELETE FROM push_subscriptions WHERE participant_id IN (SELECT id FROM participants WHERE profile_id IN (SELECT id FROM b))),
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
