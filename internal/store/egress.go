package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Blocked is a website a turn was refused, on its system line (014-AC8). Decision is "" while
// undecided, then the scope it was allowed for, "room" or "server"; Pending is the scope of an
// approval the AI host's app has not confirmed yet, and Error why it last refused one.
type Blocked struct {
	Domain   string `json:"domain"`
	Persona  string `json:"persona"`
	Count    int    `json:"count"`
	Decision string `json:"decision"`
	Pending  string `json:"pending,omitempty"`
	Error    string `json:"error,omitempty"`
	// DismissedBy names who put it aside, undecided; its next refusal clears it.
	DismissedBy string `json:"dismissed_by,omitempty"`
}

// BlockedRequest is a website a turn was refused that still waits for the AI host: the banner's
// entry, one per domain (014-AC9).
type BlockedRequest struct {
	MessageID int64 `json:"message_id"`
	Blocked
}

// BlockedRequests lists room's undecided, undismissed blocked websites, oldest first.
func (s *Store) BlockedRequests(ctx context.Context, roomID string) ([]BlockedRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, domain, persona, count, pending_scope, pending_error FROM blocked_requests
		WHERE room_id = ? AND decision = '' AND dismissed_by = '' ORDER BY message_id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("blocked requests: %w", err)
	}
	defer rows.Close()
	out := []BlockedRequest{}
	for rows.Next() {
		var b BlockedRequest
		if err := rows.Scan(&b.MessageID, &b.Domain, &b.Persona, &b.Count, &b.Pending, &b.Error); err != nil {
			return nil, fmt.Errorf("blocked requests: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DismissBlocked puts room's undecided blocked line id aside for by, the line saying so; the next
// refusal of its website raises it again. ErrDecided for a line decided or waiting for the host's app.
func (s *Store) DismissBlocked(ctx context.Context, roomID string, id int64, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "dismiss blocked", func(tx *txn) error {
		var persona, domain string
		var count int
		err := tx.QueryRowContext(ctx, `SELECT persona, domain, count FROM blocked_requests
			WHERE message_id = ? AND room_id = ? AND decision = '' AND pending_scope = '' AND dismissed_by = ''`, id, roomID).Scan(&persona, &domain, &count)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDecided
		}
		if err != nil {
			return fmt.Errorf("dismiss blocked: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blocked_requests SET dismissed_by = ? WHERE message_id = ?`, by.Name, id); err != nil {
			return fmt.Errorf("dismiss blocked: %w", err)
		}
		if err := touchMessage(ctx, tx, roomID, id, blockedBody(persona, domain, count)+"; "+by.Name+" dismissed it"); err != nil {
			return err
		}
		m, err = scanMessage(tx.QueryRowContext(ctx, messageCols+`WHERE id = ?`, id))
		return err
	})
	return m, err
}

// Egress scopes: the room's own list, or the AI host's server list, every room of it.
const (
	EgressRoom   = "room"
	EgressServer = "server"
)

// Audit actions of the rooms' network access (014-AC12, 014-AC14, 014-AC15).
const (
	ActionEgressAllow    = "egress.allow"
	ActionEgressRevoke   = "egress.revoke"
	ActionEgressDelegate = "egress.delegate"
)

// ActorProfile prefixes a profile's uuid as an audit actor; ActorParticipant a participant id, for
// a member with no profile (004-AC6).
const (
	ActorProfile     = "profile:"
	ActorParticipant = "participant:"
)

// ErrDecided is a blocked line decided already, or with an approval on its way.
var ErrDecided = errors.New("this website was decided already")

// participantActor is the audit actor of participant pid: its profile, or the participant itself.
func participantActor(ctx context.Context, tx *txn, pid int64) (string, error) {
	var uuid string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(f.uuid, '') FROM participants p LEFT JOIN profiles f ON f.id = p.profile_id WHERE p.id = ?`, pid).Scan(&uuid)
	if err != nil {
		return "", fmt.Errorf("audit actor: %w", err)
	}
	if uuid == "" {
		return ActorParticipant + strconv.FormatInt(pid, 10), nil
	}
	return ActorProfile + uuid, nil
}

func egressAudit(ctx context.Context, tx *txn, roomID, actor, action string, details map[string]any) error {
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return insertAudit(ctx, tx, AuditEntry{At: time.Now(), Actor: actor, Action: action, TargetKind: TargetRoom, TargetID: roomID, Details: b})
}

// touchMessage gives message id the room's next change seq, so streams send it again as changed.
func touchMessage(ctx context.Context, tx *txn, roomID string, id int64, body string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET body = ?, change_seq = (SELECT COALESCE(MAX(change_seq), 0) + 1 FROM messages WHERE room_id = ?)
		WHERE id = ?`, body, roomID, id); err != nil {
		return fmt.Errorf("update blocked line: %w", err)
	}
	return nil
}

func blockedBody(persona, domain string, count int) string {
	if count == 1 {
		return persona + " tried to reach " + domain + " (blocked)"
	}
	return fmt.Sprintf("%s tried to reach %s (blocked, %d times)", persona, domain, count)
}

// RecordBlocked posts one line per domain persona's turn was refused, the caller having checked
// each, or adds to the count of the room's undecided line for it (014-AC8). It returns the lines.
func (s *Store) RecordBlocked(ctx context.Context, roomID, persona string, domains []string) ([]Message, error) {
	var out []Message
	err := s.withTx(ctx, "record blocked", func(tx *txn) error {
		for _, d := range domains {
			var id int64
			var count int
			err := tx.QueryRowContext(ctx, `SELECT message_id, count FROM blocked_requests WHERE room_id = ? AND domain = ? AND decision = ''`, roomID, d).Scan(&id, &count)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				m := &Message{RoomID: roomID, Kind: KindSystem, Sender: persona, Body: blockedBody(persona, d, 1)}
				if err := insertMessage(ctx, tx, m); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO blocked_requests(message_id, room_id, persona, domain) VALUES(?, ?, ?, ?)`, m.ID, roomID, persona, d); err != nil {
					return fmt.Errorf("record blocked: %w", err)
				}
				id = m.ID
			case err != nil:
				return fmt.Errorf("record blocked: %w", err)
			default:
				var first string
				if err := tx.QueryRowContext(ctx, `SELECT persona FROM blocked_requests WHERE message_id = ?`, id).Scan(&first); err != nil {
					return fmt.Errorf("record blocked: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE blocked_requests SET count = count + 1, dismissed_by = '' WHERE message_id = ?`, id); err != nil {
					return fmt.Errorf("record blocked: %w", err)
				}
				if err := touchMessage(ctx, tx, roomID, id, blockedBody(first, d, count+1)); err != nil {
					return err
				}
			}
			m, err := scanMessage(tx.QueryRowContext(ctx, messageCols+`WHERE id = ?`, id))
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return nil
	})
	return out, err
}

// BlockedLine returns the blocked line id of room; ErrNotFound for any other line.
func (s *Store) BlockedLine(ctx context.Context, roomID string, id int64) (*Message, error) {
	m, err := s.Message(ctx, roomID, id)
	if err == nil && m.Blocked == nil {
		err = ErrNotFound
	}
	return m, err
}

// RequestAllow records by's approval of the undecided blocked line id for scope; the AI host's app
// confirms it with ConfirmAllow. ErrDecided when the line is decided or waits for a confirmation.
func (s *Store) RequestAllow(ctx context.Context, roomID string, id int64, scope string, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "request allow", func(tx *txn) error {
		actor, err := participantActor(ctx, tx, by.ID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE blocked_requests SET pending_scope = ?, pending_by = ?, pending_actor = ?, pending_error = ''
			WHERE message_id = ? AND room_id = ? AND decision = '' AND pending_scope = ''`, scope, by.Name, actor, id, roomID)
		if err != nil {
			return fmt.Errorf("request allow: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("request allow: %w", err)
		} else if n == 0 {
			return ErrDecided
		}
		var body string
		if err := tx.QueryRowContext(ctx, `SELECT body FROM messages WHERE id = ?`, id).Scan(&body); err != nil {
			return fmt.Errorf("request allow: %w", err)
		}
		if err := touchMessage(ctx, tx, roomID, id, body); err != nil {
			return err
		}
		m, err = scanMessage(tx.QueryRowContext(ctx, messageCols+`WHERE id = ?`, id))
		return err
	})
	return m, err
}

// PendingAllow is an approval the AI host's app has to apply.
type PendingAllow struct {
	ID     int64  `json:"id"`
	Domain string `json:"domain"`
	Scope  string `json:"scope"`
	By     string `json:"by"`
}

// PendingAllows lists room's approvals waiting for the AI host's app, oldest first.
func (s *Store) PendingAllows(ctx context.Context, roomID string) ([]PendingAllow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, domain, pending_scope, pending_by FROM blocked_requests
		WHERE room_id = ? AND decision = '' AND pending_scope != '' ORDER BY message_id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("pending allows: %w", err)
	}
	defer rows.Close()
	var out []PendingAllow
	for rows.Next() {
		var p PendingAllow
		if err := rows.Scan(&p.ID, &p.Domain, &p.Scope, &p.By); err != nil {
			return nil, fmt.Errorf("pending allows: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ConfirmAllow is the AI host's app's answer to the approval of blocked line id: with no failure the
// line becomes the approval, with its audit entry (014-AC10, 014-AC11, 014-AC14); with one the
// approval is dropped and the line says why. ErrNotFound when no approval waits on it.
func (s *Store) ConfirmAllow(ctx context.Context, roomID string, id int64, failure string) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "confirm allow", func(tx *txn) error {
		var b Blocked
		var by, actor, body string
		err := tx.QueryRowContext(ctx, `SELECT b.domain, b.persona, b.pending_scope, b.pending_by, b.pending_actor, m.body FROM blocked_requests b
			JOIN messages m ON m.id = b.message_id WHERE b.message_id = ? AND b.room_id = ? AND b.decision = '' AND b.pending_scope != ''`, id, roomID).
			Scan(&b.Domain, &b.Persona, &b.Pending, &by, &actor, &body)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("confirm allow: %w", err)
		}
		if failure != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE blocked_requests SET pending_scope = '', pending_by = '', pending_actor = '', pending_error = ? WHERE message_id = ?`,
				failure, id); err != nil {
				return fmt.Errorf("confirm allow: %w", err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE blocked_requests SET decision = pending_scope, pending_scope = '', pending_error = '' WHERE message_id = ?`, id); err != nil {
				return fmt.Errorf("confirm allow: %w", err)
			}
			body = by + " allowed " + b.Domain + " for this room"
			if b.Pending == EgressServer {
				body = by + " allowed " + b.Domain + " for every room on this AI host"
			}
			if err := egressAudit(ctx, tx, roomID, actor, ActionEgressAllow, map[string]any{"domain": b.Domain, "scope": b.Pending, "persona": b.Persona, "as": by}); err != nil {
				return err
			}
		}
		if err := touchMessage(ctx, tx, roomID, id, body); err != nil {
			return err
		}
		m, err = scanMessage(tx.QueryRowContext(ctx, messageCols+`WHERE id = ?`, id))
		return err
	})
	return m, err
}

// EgressRevoked says in room that its AI host removed domain from the room's websites, or from
// every room's, and audits it as the host (014-AC15).
func (s *Store) EgressRevoked(ctx context.Context, roomID, domain, scope string) (*Message, error) {
	where := "this room's websites"
	if scope == EgressServer {
		where = "every room"
	}
	return s.hostEvent(ctx, roomID, "removed "+domain+" from "+where, ActionEgressRevoke, map[string]any{"domain": domain, "scope": scope})
}

// Audit actions of the rooms' claude.ai connectors (014-AC19, 014-AC23).
const (
	ActionConnectorEnable  = "connector.enable"
	ActionConnectorDisable = "connector.disable"
	ActionConnectorActions = "connector.actions"
)

// ConnectorChanged says in room what its AI host changed of connector name on their computer, and
// audits it as the host: kind "enable", "disable" or "actions", actions allowed or not (014-AC19,
// 014-AC23). ErrNotFound for a room with no host, or another kind.
func (s *Store) ConnectorChanged(ctx context.Context, roomID, name, kind string, actions bool) (*Message, error) {
	var body, action string
	switch kind {
	case "enable":
		body, action = "turned on the connector "+name+" for this room, read only", ActionConnectorEnable
		if actions {
			body = "turned on the connector " + name + " for this room, with its actions"
		}
	case "disable":
		body, action = "turned off the connector "+name+" for this room", ActionConnectorDisable
	case "actions":
		body, action = "limited the connector "+name+" to reading", ActionConnectorActions
		if actions {
			body = "let the connector " + name + " act: send, create, change"
		}
	default:
		return nil, ErrNotFound
	}
	return s.hostEvent(ctx, roomID, body, action, map[string]any{"connector": name, "actions": actions})
}

// hostEvent is a line in room as its AI host, "<host> <body>", with its audit entry as them.
func (s *Store) hostEvent(ctx context.Context, roomID, body, action string, details map[string]any) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, action, func(tx *txn) error {
		var host int64
		var name string
		err := tx.QueryRowContext(ctx, `SELECT p.id, p.name FROM rooms r JOIN participants p ON p.id = r.host_participant_id WHERE r.id = ?`, roomID).Scan(&host, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
		actor, err := participantActor(ctx, tx, host)
		if err != nil {
			return err
		}
		m = &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: host, Sender: name, Body: name + " " + body}
		if err := insertMessage(ctx, tx, m); err != nil {
			return err
		}
		details["as"] = name
		return egressAudit(ctx, tx, roomID, actor, action, details)
	})
	return m, err
}

// SetEgressDelegation lets room's admins allow websites for the room, or stops it, as the AI host
// by; it returns nil when nothing changed (014-AC12).
func (s *Store) SetEgressDelegation(ctx context.Context, roomID string, on bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "egress delegation", func(tx *txn) error {
		res, err := tx.ExecContext(ctx, `UPDATE rooms SET egress_admins_allow = ? WHERE id = ? AND egress_admins_allow != ?`, on, roomID, on)
		if err != nil {
			return fmt.Errorf("egress delegation: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		body := "stopped the room admins allowing websites for this room."
		if on {
			body = "let the room admins allow websites for this room."
		}
		if m, err = memberLine(ctx, tx, roomID, by, body); err != nil {
			return err
		}
		actor, err := participantActor(ctx, tx, by.ID)
		if err != nil {
			return err
		}
		return egressAudit(ctx, tx, roomID, actor, ActionEgressDelegate, map[string]any{"on": on, "as": by.Name})
	})
	return m, err
}

// errHostChanged is why an approval waiting for the previous AI host's app was dropped.
const errHostChanged = "the AI host changed before their app applied it"

// hostChanged turns the delegation off and drops the approvals waiting for the previous host's
// app: they would land on the new host's computer, which nobody asked (014-AC12).
func hostChanged(ctx context.Context, tx *txn, roomID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET egress_admins_allow = 0 WHERE id = ?`, roomID); err != nil {
		return fmt.Errorf("host changed: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.message_id, m.body FROM blocked_requests b JOIN messages m ON m.id = b.message_id
		WHERE b.room_id = ? AND b.decision = '' AND b.pending_scope != ''`, roomID)
	if err != nil {
		return fmt.Errorf("host changed: %w", err)
	}
	type line struct {
		id   int64
		body string
	}
	var drop []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.id, &l.body); err != nil {
			_ = rows.Close()
			return fmt.Errorf("host changed: %w", err)
		}
		drop = append(drop, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("host changed: %w", err)
	}
	for _, l := range drop {
		if _, err := tx.ExecContext(ctx, `UPDATE blocked_requests SET pending_scope = '', pending_by = '', pending_actor = '', pending_error = ? WHERE message_id = ?`,
			errHostChanged, l.id); err != nil {
			return fmt.Errorf("host changed: %w", err)
		}
		if err := touchMessage(ctx, tx, roomID, l.id, l.body); err != nil {
			return err
		}
	}
	return nil
}
