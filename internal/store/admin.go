package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// AdminSessionTTL is how long one /admin login lasts (004-AC1).
const AdminSessionTTL = 12 * time.Hour

// AdminSession is a browser signed in with the admin token. Only its token's hash is stored.
type AdminSession struct {
	ID         int64     `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
}

// CreateAdminSession opens a session for tokenHash at now, and drops the expired ones.
func (s *Store) CreateAdminSession(ctx context.Context, tokenHash, userAgent, ip string, now time.Time) (*AdminSession, error) {
	if _, err := s.PurgeAdminSessions(ctx, now); err != nil {
		return nil, err
	}
	a := &AdminSession{CreatedAt: now.UTC(), LastUsedAt: now.UTC(), ExpiresAt: time.Unix(now.Add(AdminSessionTTL).Unix(), 0).UTC(), UserAgent: userAgent, IP: ip}
	err := s.withTx(ctx, "create admin session", func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `INSERT INTO admin_sessions(token_hash, created_at, last_used_at, expires_at_s, user_agent, ip) VALUES(?,?,?,?,?,?) RETURNING id`,
			tokenHash, a.CreatedAt.Format(time.RFC3339Nano), a.LastUsedAt.Format(time.RFC3339Nano), a.ExpiresAt.Unix(), userAgent, ip).Scan(&a.ID)
		if err != nil {
			return fmt.Errorf("create admin session: %w", err)
		}
		// A login is audited under the session it opens.
		return auditTx(ctx, tx, strconv.FormatInt(a.ID, 10), map[string]any{"session": a.ID})
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// TouchAdminSession marks the session holding tokenHash used at now and returns its id. ErrNotFound
// when no unexpired session holds it: revoked, expired or never issued.
func (s *Store) TouchAdminSession(ctx context.Context, tokenHash string, now time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `UPDATE admin_sessions SET last_used_at = ? WHERE token_hash = ? AND expires_at_s > ? RETURNING id`,
		now.UTC().Format(time.RFC3339Nano), tokenHash, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("touch admin session: %w", err)
	}
	return id, nil
}

// AdminSessions lists the sessions still valid at now, newest first.
func (s *Store) AdminSessions(ctx context.Context, now time.Time) ([]AdminSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, last_used_at, expires_at_s, user_agent, ip FROM admin_sessions
		WHERE expires_at_s > ? ORDER BY id DESC`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("query admin sessions: %w", err)
	}
	defer rows.Close()
	out := []AdminSession{}
	for rows.Next() {
		var a AdminSession
		var created, used string
		var expires int64
		if err := rows.Scan(&a.ID, &created, &used, &expires, &a.UserAgent, &a.IP); err != nil {
			return nil, fmt.Errorf("scan admin session: %w", err)
		}
		a.CreatedAt, a.LastUsedAt, a.ExpiresAt = parseTS(created), parseTS(used), time.Unix(expires, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// PurgeAdminSessions deletes the sessions expired at now, with the IP and user agent they hold.
func (s *Store) PurgeAdminSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at_s <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("purge admin sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge admin sessions: %w", err)
	}
	return n, nil
}

// RevokeAdminSession ends session id. ErrNotFound if there is none.
func (s *Store) RevokeAdminSession(ctx context.Context, id int64) error {
	return s.withTx(ctx, "revoke admin session", func(tx *sql.Tx) error {
		if err := execOne(ctx, tx, `DELETE FROM admin_sessions WHERE id = ?`, id); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", nil)
	})
}

// deleteOne runs a DELETE that must remove a row: ErrNotFound when it removed none.
func execOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("exec: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ActorSuperAdmin is the audit actor of the admin token and its sessions.
const ActorSuperAdmin = "super-admin"

// ActorSystem is the audit actor of what no signed-in person did, such as using a recovery link.
const ActorSystem = "system"

// Audit target kinds.
const (
	TargetRoom          = "room"
	TargetProfile       = "profile"
	TargetSkill         = "skill"
	TargetTemplate      = "template"
	TargetStarter       = "starter"
	TargetAdminSession  = "admin_session"
	TargetInstance      = "instance"
	TargetPronunciation = "pronunciation"
)

// MaxAuditDetails caps an entry's details; larger ones are stored as {"truncated":true}.
const MaxAuditDetails = 4096

// AuditEntry is one admin action (004-AC6). TargetName is the target's name when the action ran:
// the entry keeps it after the target is gone.
type AuditEntry struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	TargetKind string          `json:"target_kind"`
	TargetID   string          `json:"target_id"`
	TargetName string          `json:"target_name"`
	Details    json.RawMessage `json:"details"`
}

// Audit stores e. Empty details are stored as {}.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error { return insertAudit(ctx, s.db, e) }

func insertAudit(ctx context.Context, db execer, e AuditEntry) error {
	switch {
	case len(e.Details) == 0:
		e.Details = json.RawMessage(`{}`)
	case !json.Valid(e.Details):
		return fmt.Errorf("audit %s: details are not JSON", e.Action)
	case len(e.Details) > MaxAuditDetails:
		e.Details = json.RawMessage(`{"truncated":true}`)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO audit_log(at_ms, actor, action, target_kind, target_id, target_name, details) VALUES(?,?,?,?,?,?,?)`,
		e.At.UnixMilli(), e.Actor, e.Action, e.TargetKind, e.TargetID, e.TargetName, string(e.Details))
	if err != nil {
		return fmt.Errorf("audit %s: %w", e.Action, err)
	}
	return nil
}

type auditKey struct{}

type pendingAudit struct {
	e       AuditEntry
	details map[string]any
}

// WithAudit returns ctx carrying e and its details: the audited mutation run with it writes e in
// its own transaction, so the change and its entry commit together or not at all, and a failed
// audit write fails the change (004-AC6).
func WithAudit(ctx context.Context, e AuditEntry, details map[string]any) context.Context {
	return context.WithValue(ctx, auditKey{}, &pendingAudit{e: e, details: details})
}

// auditTx writes the entry ctx carries, if any, in tx. id, when set, is the target's id, for a
// target the mutation creates; extra adds details. An entry without a target name gets the one the
// target has in tx: run it before a delete, after a rename.
func auditTx(ctx context.Context, tx *sql.Tx, id string, extra map[string]any) error {
	p, _ := ctx.Value(auditKey{}).(*pendingAudit)
	if p == nil {
		return nil
	}
	e := p.e
	if id != "" {
		e.TargetID = id
	}
	if q, ok := targetNames[e.TargetKind]; ok && e.TargetName == "" {
		if err := tx.QueryRowContext(ctx, q, e.TargetID).Scan(&e.TargetName); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("audit %s: target name: %w", e.Action, err)
		}
	}
	details := maps.Clone(p.details)
	if details == nil {
		details = map[string]any{}
	}
	maps.Copy(details, extra)
	if id, ok := ctx.Value(apiTokenKey{}).(int64); ok {
		details["api_token"] = id
	}
	var err error
	if e.Details, err = json.Marshal(details); err != nil {
		return fmt.Errorf("audit %s: %w", e.Action, err)
	}
	return insertAudit(ctx, tx, e)
}

// AuditQuery selects audit entries: those before entry Before when not 0, of Action when set,
// whose target id is Target or whose target name contains it, case-insensitively, when set.
type AuditQuery struct {
	Action, Target string
	Before         int64
	Limit          int
}

// AuditLog returns the entries q selects, newest first.
func (s *Store) AuditLog(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Target) + "%"
	rows, err := s.db.QueryContext(ctx, `SELECT id, at_ms, actor, action, target_kind, target_id, target_name, details FROM audit_log
		WHERE (? = 0 OR id < ?) AND (? = '' OR action = ?) AND (? = '' OR target_id = ? OR target_name LIKE ? ESCAPE '\')
		ORDER BY id DESC LIMIT ?`, q.Before, q.Before, q.Action, q.Action, q.Target, q.Target, like, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.TargetKind, &e.TargetID, &e.TargetName, &details); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		e.At, e.Details = time.UnixMilli(at).UTC(), json.RawMessage(details)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeAudit deletes the entries older than before and returns how many it deleted.
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE at_ms < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge audit log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge audit log: %w", err)
	}
	return n, nil
}

// targetNames reads the name an audit entry keeps for each target kind; a profile's is the one it
// chose (005-AC1), else the name it last joined a room with, else its uuid.
var targetNames = map[string]string{
	TargetRoom:          `SELECT name FROM rooms WHERE id = ?`,
	TargetProfile:       `SELECT COALESCE(p.name, (SELECT name FROM participants WHERE profile_id = p.id ORDER BY id DESC LIMIT 1), p.uuid, '') FROM profiles p WHERE p.id = ?`,
	TargetSkill:         `SELECT name FROM skills WHERE id = ?`,
	TargetTemplate:      `SELECT title FROM persona_templates WHERE id = ?`,
	TargetStarter:       `SELECT json_extract(manifest, '$.title') FROM starter_versions WHERE starter_id = ? ORDER BY n DESC LIMIT 1`,
	TargetPronunciation: `SELECT term FROM pronunciations WHERE id = ?`,
}
