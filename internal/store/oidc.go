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
		if err := tx.QueryRowContext(ctx, `UPDATE profiles SET oidc_issuer = ?, oidc_subject = ?, oidc_signed_in_at = ? WHERE id = ? RETURNING COALESCE(uuid, '')`,
			in.Issuer, in.Subject, formatTS(in.Now), id).Scan(&uuid); err != nil {
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
