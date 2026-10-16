package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Personal API token scopes (012-AC1).
const (
	ScopeRead = "read"
	ScopeFull = "full"
)

// MaxAPITokens caps a profile's unexpired API tokens (012-AC11).
const MaxAPITokens = 100

// APITokenKeep is how long an expired API token stays listed before the purge deletes it (012-AC7).
const APITokenKeep = 30 * 24 * time.Hour

// ErrTokenExpired is an API token past its expiry, until the purge deletes it (012-AC7).
var ErrTokenExpired = errors.New("token expired")

// APIToken is a personal API token as its owner lists it: never the token, never its hash.
type APIToken struct {
	ID        int64 `json:"id"`
	ProfileID int64 `json:"-"`
	// ProfileUUID names the profile in the audit log.
	ProfileUUID string     `json:"-"`
	Name        string     `json:"name"`
	Scope       string     `json:"scope"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	// LastUsedAt and LastUsedIP are the owner's to see (012-AC9); the purge clears the IP.
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
}

// Expired reports whether t is past its expiry at now.
func (t *APIToken) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

type apiTokenKey struct{}

// WithAPIToken returns ctx naming the API token a request came with: every audit entry written
// with it carries the token's id (012-AC10).
func WithAPIToken(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, apiTokenKey{}, id)
}

func msTime(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := time.UnixMilli(ms.Int64).UTC()
	return &t
}

const apiTokenCols = `id, profile_id, (SELECT COALESCE(uuid, '') FROM profiles WHERE id = profile_id), name, scope, created_at, expires_at_ms, last_used_at_ms, COALESCE(last_used_ip, '')` //nolint:gosec // G101: column names, no credential

func scanAPIToken(sc interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var created string
	var expires, used sql.NullInt64
	if err := sc.Scan(&t.ID, &t.ProfileID, &t.ProfileUUID, &t.Name, &t.Scope, &created, &expires, &used, &t.LastUsedIP); err != nil {
		return nil, err
	}
	t.CreatedAt, t.ExpiresAt, t.LastUsedAt = parseTS(created), msTime(expires), msTime(used)
	return &t, nil
}

// CreateAPIToken stores a token of profileID by its hash, with the audit entry ctx carries.
// expires nil never expires. ErrLimit past MaxAPITokens unexpired ones at now.
func (s *Store) CreateAPIToken(ctx context.Context, profileID int64, name, hash, scope string, expires *time.Time, now time.Time) (*APIToken, error) {
	var t *APIToken
	err := s.withTx(ctx, "create api token", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE profile_id = ? AND (expires_at_ms IS NULL OR expires_at_ms > ?)`,
			profileID, now.UnixMilli()).Scan(&n); err != nil {
			return err
		}
		if n >= MaxAPITokens {
			return ErrLimit
		}
		var exp sql.NullInt64
		if expires != nil {
			exp = sql.NullInt64{Int64: expires.UnixMilli(), Valid: true}
		}
		row := tx.QueryRowContext(ctx, `INSERT INTO api_tokens(profile_id, name, token_hash, scope, created_at, expires_at_ms) VALUES(?,?,?,?,?,?)
			RETURNING `+apiTokenCols, profileID, name, hash, scope, now.UTC().Format(time.RFC3339Nano), exp)
		var err error
		if t, err = scanAPIToken(row); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", tokenDetails(t))
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func tokenDetails(t *APIToken) map[string]any {
	d := map[string]any{"token_id": t.ID, "name": t.Name, "scope": t.Scope, "expires_at": nil}
	if t.ExpiresAt != nil {
		d["expires_at"] = t.ExpiresAt
	}
	return d
}

// APITokens lists profileID's tokens, newest first, expired ones included until purged.
func (s *Store) APITokens(ctx context.Context, profileID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE profile_id = ? ORDER BY id DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("api tokens: %w", err)
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// RevokeAPIToken deletes token id of profileID, with the audit entry ctx carries. ErrNotFound.
func (s *Store) RevokeAPIToken(ctx context.Context, profileID, id int64) error {
	return s.withTx(ctx, "revoke api token", func(tx *sql.Tx) error {
		t, err := scanAPIToken(tx.QueryRowContext(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE id = ? AND profile_id = ?`, id, profileID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := auditTx(ctx, tx, "", tokenDetails(t)); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM api_tokens WHERE id = ?`, id)
	})
}

// APITokenByHash returns the token with hash: ErrNotFound for none, ErrTokenExpired, with the
// token, once past its expiry at now.
func (s *Store) APITokenByHash(ctx context.Context, hash string, now time.Time) (*APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRowContext(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE token_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("api token: %w", err)
	}
	if t.Expired(now) {
		return t, ErrTokenExpired
	}
	return t, nil
}

// TouchAPIToken records a use of token id from ip at now, unless one was recorded less than every
// before (012-AC9). It reports whether it wrote.
func (s *Store) TouchAPIToken(ctx context.Context, id int64, ip string, now time.Time, every time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at_ms = ?, last_used_ip = ? WHERE id = ? AND (last_used_at_ms IS NULL OR last_used_at_ms <= ?)`,
		now.UnixMilli(), ip, id, now.Add(-every).UnixMilli())
	if err != nil {
		return false, fmt.Errorf("touch api token: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// APITokenParticipant returns the newest participant of room whose profile holds the unexpired
// token hash and whose invite is not revoked, as ProfileParticipant does for a session (012-AC3).
func (s *Store) APITokenParticipant(ctx context.Context, hash, roomID string, now time.Time) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT pa.id, pa.room_id, pa.invite_id, pa.name, pa.joined_at, pa.can_trigger, pa.profile_id
		FROM api_tokens t
		JOIN participants pa ON pa.profile_id = t.profile_id
		JOIN invites i ON i.id = pa.invite_id
		WHERE t.token_hash = ? AND (t.expires_at_ms IS NULL OR t.expires_at_ms > ?) AND pa.room_id = ? AND i.revoked_at IS NULL
		ORDER BY pa.id DESC LIMIT 1`, hash, now.UnixMilli(), roomID).
		Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.ProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("api token participant: %w", err)
	}
	p.JoinedAt = parseTS(joined)
	return &p, nil
}

// ProfileByID returns profile id with its rooms. ErrNotFound.
func (s *Store) ProfileByID(ctx context.Context, id int64) (*Profile, error) {
	return s.profile(ctx, `id = ?`, id)
}

// PurgeAPITokens deletes the tokens expired for over APITokenKeep at now, and clears the IP of
// the last uses before ipBefore (012-AC7, 012-AC9). It returns how many tokens it deleted.
func (s *Store) PurgeAPITokens(ctx context.Context, now, ipBefore time.Time) (int64, error) {
	var n int64
	err := s.withTx(ctx, "purge api tokens", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at_ms <= ?`, now.Add(-APITokenKeep).UnixMilli())
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE api_tokens SET last_used_ip = NULL WHERE last_used_ip IS NOT NULL AND last_used_at_ms < ?`, ipBefore.UnixMilli())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("purge api tokens: %w", err)
	}
	return n, nil
}
