package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/babs/grill-room/internal/turn"
)

// Cap decision statuses.
const (
	CapApproved = "approved"
	CapRefused  = "refused"
)

// ErrStale is returned when the host decides on a persona request that changed since they saw it.
var ErrStale = errors.New("the persona changed since: look at its request again")

// CapDecision is the host's answer to a persona asking above the host cap. Approved lets it run up
// to Level on daemon DaemonID: an exception per persona and host daemon, so another host's cap
// applies again after a hand-over. Refused keeps it at the cap and settles the request while the
// persona asks for that same Level on that daemon.
type CapDecision struct {
	Status   string
	DaemonID int64
	Level    turn.Level
}

// Approved is the level this decision lets the persona run at on daemon id; zero when none.
func (c CapDecision) Approved(daemonID int64) turn.Level {
	if c.Status != CapApproved || c.DaemonID != daemonID {
		return turn.Level{}
	}
	return c.Level
}

// Settles reports whether the host already answered a request for asked on daemon id.
func (c CapDecision) Settles(daemonID int64, asked turn.Level) bool {
	return c.Status != "" && c.DaemonID == daemonID && c.Level == asked
}

// Asks is the level persona p asks for.
func (p *RoomPersona) Asks() turn.Level { return turn.Level{Model: p.Model, Effort: p.Effort} }

// asHost returns ErrForbidden unless by hosts the room, checked inside the decision's transaction:
// a hand-over between a handler's check and the write must not let the former host decide.
func asHost(ctx context.Context, tx *sql.Tx, roomID string, by *Participant) error {
	var host sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT host_participant_id FROM rooms WHERE id = ?`, roomID).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("host check: %w", err)
	}
	if host.Int64 != by.ID {
		return ErrForbidden
	}
	return nil
}

// DecideCap records the host's answer on persona id asking for asked above the cap of host daemon
// d, and posts it in the room so the requester sees it (002-AC20). runs is the capped level a
// refused persona keeps. ErrForbidden unless by hosts the room on d, ErrStale when the persona asks
// for something else by now.
func (s *Store) DecideCap(ctx context.Context, roomID string, id int64, d *Daemon, asked, runs turn.Level, approve bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "decide cap", func(tx *sql.Tx) error {
		if err := asHost(ctx, tx, roomID, by); err != nil {
			return err
		}
		if d.ParticipantID != by.ID {
			return ErrForbidden
		}
		p, err := roomPersona(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		if p.Asks() != asked {
			return ErrStale
		}
		status, body := CapRefused, fmt.Sprintf("refused %s for %s: it keeps running at the host cap, %s.", asked, p.label(), runs)
		if approve {
			status, body = CapApproved, fmt.Sprintf("approved %s for %s: it runs as asked from its next turn.", asked, p.label())
		}
		if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET cap_decision = ?, cap_daemon_id = ?, cap_model = ?, cap_effort = ? WHERE id = ?`,
			status, d.ID, asked.Model, asked.Effort, id); err != nil {
			return fmt.Errorf("decide cap: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	return m, err
}

// Change request kinds: a new persona, or a change to one.
const (
	ChangeAdd  = "add"
	ChangeEdit = "change"
)

// waitsNote ends the chat line of a held change.
const waitsNote = " It waits for the host's approval."

// PersonaChange is a persona change waiting for the host's approval (002-AC21).
type PersonaChange struct {
	ID        int64  `json:"id"`
	PersonaID int64  `json:"persona_id"`
	Kind      string `json:"kind"`
	// RequestedBy names who asked last.
	RequestedBy string `json:"requested_by"`
	// Summary says what approving it changes, as the chat does.
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

// heldBy reports whether a persona change by participant by waits for the host: the host daemon
// approves everything, and by is not the host. The host's daemon setting holds while it is offline.
func heldBy(ctx context.Context, tx *sql.Tx, roomID string, by *Participant) (bool, error) {
	var held bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rooms r JOIN daemons d ON d.participant_id = r.host_participant_id
		WHERE r.id = ? AND d.approve_all = 1 AND r.host_participant_id != ?)`, roomID, by.ID).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("approval mode: %w", err)
	}
	return held, nil
}

func pick[T any](later, earlier *T) *T {
	if later != nil {
		return later
	}
	return earlier
}

// over returns c laid over earlier: what c sets wins, the rest of earlier stays.
func (c PersonaPatch) over(earlier PersonaPatch) PersonaPatch {
	return PersonaPatch{Name: pick(c.Name, earlier.Name), Badge: pick(c.Badge, earlier.Badge), Colour: pick(c.Colour, earlier.Colour),
		Text: pick(c.Text, earlier.Text), Model: pick(c.Model, earlier.Model), Effort: pick(c.Effort, earlier.Effort),
		EveryN: pick(c.EveryN, earlier.EveryN), SelfCall: pick(c.SelfCall, earlier.SelfCall), TemplateVersion: pick(c.TemplateVersion, earlier.TemplateVersion),
		Skills: mergePins(c.Skills, earlier.Skills)}
}

// holdChange keeps c for the host to decide on persona p: a persona has one request, which a later
// change adds to, so the host decides on what the room wants now.
func holdChange(ctx context.Context, tx *sql.Tx, roomID string, p *RoomPersona, kind string, c PersonaPatch, by *Participant) error {
	var earlier string
	err := tx.QueryRowContext(ctx, `SELECT patch FROM persona_change_requests WHERE persona_id = ?`, p.ID).Scan(&earlier)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		b, err := json.Marshal(c)
		if err != nil {
			return fmt.Errorf("hold change: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO persona_change_requests(room_id, persona_id, kind, patch, requested_by, created_at) VALUES(?,?,?,?,?,?)`,
			roomID, p.ID, kind, string(b), by.Name, now())
		if err != nil {
			return fmt.Errorf("hold change: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("hold change: %w", err)
	}
	var prev PersonaPatch
	if err := json.Unmarshal([]byte(earlier), &prev); err != nil {
		return fmt.Errorf("hold change: %w", err)
	}
	b, err := json.Marshal(c.over(prev))
	if err != nil {
		return fmt.Errorf("hold change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE persona_change_requests SET patch = ?, requested_by = ? WHERE persona_id = ?`, string(b), by.Name, p.ID); err != nil {
		return fmt.Errorf("hold change: %w", err)
	}
	return nil
}

type heldChange struct {
	PersonaChange
	patch PersonaPatch
}

const changeCols = `SELECT id, persona_id, kind, patch, requested_by, created_at FROM persona_change_requests `

func scanChange(sc scanner) (*heldChange, error) {
	var c heldChange
	var patch, created string
	err := sc.Scan(&c.ID, &c.PersonaID, &c.Kind, &patch, &c.RequestedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan change: %w", err)
	}
	if err := json.Unmarshal([]byte(patch), &c.patch); err != nil {
		return nil, fmt.Errorf("change %d: %w", c.ID, err)
	}
	c.CreatedAt = parseTS(created)
	return &c, nil
}

// summary describes what approving c does to p, leaving p as it is.
func (c *heldChange) summary(p RoomPersona) string {
	changes := c.patch.apply(&p)
	if c.Kind == ChangeAdd {
		return strings.Join(append([]string{"new persona"}, changes...), ", ")
	}
	return strings.Join(changes, ", ")
}

// PersonaChanges lists the room's changes waiting for the host, oldest first.
func (s *Store) PersonaChanges(ctx context.Context, roomID string) ([]PersonaChange, error) {
	rows, err := s.db.QueryContext(ctx, changeCols+`WHERE room_id = ? ORDER BY id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("persona changes: %w", err)
	}
	var held []*heldChange
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		held = append(held, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persona changes: %w", err)
	}
	out := []PersonaChange{}
	if len(held) == 0 {
		return out, nil
	}
	ps, err := s.RoomPersonas(ctx, roomID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]RoomPersona{}
	for _, p := range ps {
		byID[p.ID] = p
	}
	for _, c := range held {
		// The persona cascades its request away: a missing one was removed after the list was read.
		if p, ok := byID[c.PersonaID]; ok {
			c.Summary = c.summary(p)
			out = append(out, c.PersonaChange)
		}
	}
	return out, nil
}

// DecideChange applies or drops held change id of room and posts the outcome (002-AC21). Approving
// a new persona lets it run; refusing one removes it. ErrForbidden unless by hosts the room,
// ErrNameTaken when the name it brings is used by now: the change stays held.
func (s *Store) DecideChange(ctx context.Context, roomID string, id int64, approve bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "decide change", func(tx *sql.Tx) error {
		if err := asHost(ctx, tx, roomID, by); err != nil {
			return err
		}
		c, err := scanChange(tx.QueryRowContext(ctx, changeCols+`WHERE id = ? AND room_id = ?`, id, roomID))
		if err != nil {
			return err
		}
		p, err := roomPersona(ctx, tx, roomID, c.PersonaID)
		if err != nil {
			return err
		}
		before := p.label()
		var body string
		switch {
		case !approve && c.Kind == ChangeAdd:
			if _, err := tx.ExecContext(ctx, `DELETE FROM room_personas WHERE id = ?`, p.ID); err != nil {
				return fmt.Errorf("decide change: %w", err)
			}
			body = fmt.Sprintf("refused the persona %s, added by %s: it is removed.", before, c.RequestedBy)
		case !approve:
			body = fmt.Sprintf("refused %s's change to %s: %s.", c.RequestedBy, before, c.summary(*p))
		default:
			body = fmt.Sprintf("approved %s's change to %s: %s.", c.RequestedBy, before, c.summary(*p))
			c.patch.apply(p)
			if c.Kind == ChangeAdd {
				body = fmt.Sprintf("approved the persona %s, added by %s.", p.label(), c.RequestedBy)
			}
			p.Pending = false
			if err := updatePersona(ctx, tx, p); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM persona_change_requests WHERE id = ?`, c.ID); err != nil {
			return fmt.Errorf("decide change: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	return m, err
}
