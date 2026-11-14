package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
)

// Proposal states (023-AC19): a proposal is acted on once.
const (
	ProposalOpen      = "open"
	ProposalCreated   = "created"
	ProposalDismissed = "dismissed"
)

// ErrHandled is a proposal created or dismissed already (023-AC19).
var ErrHandled = errors.New("already handled")

// MaxProposalMembers bounds the members one creation seats.
const MaxProposalMembers = 1000

// BriefDoc is the agent document a room created from a proposal starts with (023-AC18).
const BriefDoc = "brief.md"

// Proposal is a related room a persona proposed, as its chat card shows it (023-AC16).
type Proposal struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Room is the room created from it; "" before, or once that room is deleted.
	Room string `json:"room,omitempty"`
	// Trigger is the participant who started the turn: they may dismiss it (023-AC19).
	Trigger int64 `json:"trigger,omitempty"`
}

// NewProposal is a turn's valid room-new.json; HTML is its brief rendered by render.MessageHTML,
// before the write lock is taken.
type NewProposal struct {
	Name  string
	Brief string
	HTML  string
}

// ProposedIn is the room a room was proposed in (023-AC20): its current name, or the one it had
// when deleted, Room then "". Member, set by the caller, tells the viewer sits in it.
type ProposedIn struct {
	Room   string `json:"room,omitempty"`
	Name   string `json:"name"`
	Member bool   `json:"member,omitempty"`
}

func proposedIn(room, name sql.NullString) *ProposedIn {
	if !name.Valid {
		return nil
	}
	return &ProposedIn{Room: room.String, Name: name.String}
}

// proposeRoom posts turn t's proposal p as a card, signed by author, under its synthesis.
func proposeRoom(ctx context.Context, tx *txn, t *Turn, author string, p *NewProposal) error {
	m := &Message{RoomID: t.RoomID, Kind: KindSystem, Sender: author, Body: author + " proposes a room: " + p.Name, HTML: p.HTML}
	if err := insertMessage(ctx, tx, m); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO room_proposals(room_id, message_id, turn_id, name, brief, created_at) VALUES(?,?,?,?,?,?)`,
		t.RoomID, m.ID, t.ID, p.Name, p.Brief, now()); err != nil {
		return fmt.Errorf("propose room: %w", err)
	}
	return nil
}

// openProposal reads room's proposal id while open; the write lock keeps a second action out until
// this one commits. ErrNotFound, ErrHandled.
// A card whose line is gone has message 0, which touchMessage leaves alone.
func openProposal(ctx context.Context, tx *txn, roomID string, id int64) (name, brief string, msg, trigger, triggerProfile int64, err error) {
	var status string
	err = tx.QueryRowContext(ctx, `SELECT p.name, p.brief, p.status, COALESCE(p.message_id, 0), COALESCE(t.participant_id, 0), COALESCE(pa.profile_id, 0)
		FROM room_proposals p LEFT JOIN turns t ON t.id = p.turn_id LEFT JOIN participants pa ON pa.id = t.participant_id
		WHERE p.id = ? AND p.room_id = ?`, id, roomID).Scan(&name, &brief, &status, &msg, &trigger, &triggerProfile)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = ErrNotFound
	case err != nil:
		err = fmt.Errorf("proposal %d: %w", id, err)
	case status != ProposalOpen:
		err = ErrHandled
	}
	return name, brief, msg, trigger, triggerProfile, err
}

// DismissProposal puts room's open proposal id aside for by, who started its turn or, with admin,
// administers the room; its card says so (023-AC19). ErrNotFound, ErrForbidden, ErrHandled.
func (s *Store) DismissProposal(ctx context.Context, roomID string, id int64, by *Participant, admin bool) error {
	return s.withTx(ctx, "dismiss proposal", func(tx *txn) error {
		name, _, msg, trigger, triggerProfile, err := openProposal(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		if !admin && by.ID != trigger && (by.ProfileID == 0 || by.ProfileID != triggerProfile) {
			return ErrForbidden
		}
		if _, err := tx.ExecContext(ctx, `UPDATE room_proposals SET status = ?, handled_by_profile_id = ? WHERE id = ?`, ProposalDismissed, nullID(by.ProfileID), id); err != nil {
			return fmt.Errorf("dismiss proposal: %w", err)
		}
		if err := closeCard(ctx, tx, roomID, msg, "Dismissed by "+by.Name+": "+name); err != nil {
			return err
		}
		// Who dismisses may hold no profile: the audit actor is then the participant.
		if p, _ := ctx.Value(auditKey{}).(*pendingAudit); p != nil {
			if p.e.Actor, err = participantActor(ctx, tx, by.ID); err != nil {
				return err
			}
		}
		return auditTx(ctx, tx, "", map[string]any{"proposal": id, "name": name})
	})
}

// ProposedRoom is the room a member creates from a proposal (023-AC18): ID and Name, By the
// member's seat in the proposal's room, the hashes CreateProfileRoom takes, Members the
// participants of that room to seat through By's shareable invite of the new one, each with a seat
// token hash from SeatHash, and the links to make when By administers that room.
type ProposedRoom struct {
	ID, Name                              string
	By                                    *Participant
	InviteHash, TokenHash, Link, LinkHash string
	Members                               []int64
	SeatHash                              func() string
	Reads, ReadBy                         bool
}

// Seated is a member seated in a room created from a proposal: their participant in the
// proposal's room, and their join line in the new room.
type Seated struct {
	From int64
	Line int64
}

// CreateProposedRoom creates in one transaction the room in for room's open proposal id: the
// creation limits of CreateProfileRoom, brief.md, "proposed in", the seats and the links, then
// the card says who created it (023-AC18). The audit entry ctx carries records the creation; each
// link gets its own as ActionRoomLinkCreate. ErrNotFound, ErrHandled, ErrCreationRefused, ErrLinkCap.
func (s *Store) CreateProposedRoom(ctx context.Context, roomID string, id int64, in ProposedRoom) (*Room, []Seated, []RoomLink, error) {
	var r *Room
	var seats []Seated
	var links []RoomLink
	err := s.withTx(ctx, "create proposed room", func(tx *txn) error {
		_, brief, msg, _, _, err := openProposal(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		by := in.By
		if r, err = createProfileRoom(ctx, tx, in.ID, in.Name, by.ProfileID, by.Name, in.InviteHash, in.TokenHash, in.Link, in.LinkHash, nil); err != nil {
			return err
		}
		if _, err := createAgentDocument(ctx, tx, r.ID, BriefDoc, "md", brief, by.Name); err != nil {
			return err
		}
		var from sql.NullString
		if err := tx.QueryRowContext(ctx, `UPDATE rooms SET proposed_in_room_id = ?, proposed_in_name = (SELECT name FROM rooms WHERE id = ?) WHERE id = ? RETURNING proposed_in_name`,
			roomID, roomID, r.ID).Scan(&from); err != nil {
			return fmt.Errorf("create proposed room: %w", err)
		}
		r.ProposedIn = proposedIn(sql.NullString{String: roomID, Valid: true}, from)
		creator := &Participant{RoomID: r.ID, Name: by.Name, ProfileID: by.ProfileID}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM participants WHERE room_id = ? AND profile_id = ?`, r.ID, by.ProfileID).Scan(&creator.ID); err != nil {
			return fmt.Errorf("create proposed room: %w", err)
		}
		members, err := proposalMembers(ctx, tx, roomID, by.ProfileID, in.Members)
		if err != nil {
			return err
		}
		for _, m := range members {
			// Through the creator's shareable invite: revoking it ends these seats too (016).
			_, line, err := join(ctx, tx, in.LinkHash, m.Name, in.SeatHash(), JoinProfile{ID: m.ProfileID})
			if err != nil {
				return err
			}
			seats = append(seats, Seated{From: m.ID, Line: line.ID})
		}
		if in.Reads || in.ReadBy {
			// The caller administers the new room; the links need the proposal's room too.
			if _, err := roomAdmin(ctx, tx, roomID, by.ProfileID); err == nil {
				lctx := withAction(ctx, ActionRoomLinkCreate, r.ID)
				pairs := [][2]string{}
				if in.Reads {
					pairs = append(pairs, [2]string{r.ID, roomID})
				}
				if in.ReadBy {
					pairs = append(pairs, [2]string{roomID, r.ID})
				}
				for _, p := range pairs {
					l, _, err := linkRoom(lctx, tx, r.ID, p[0], p[1], creator)
					if err != nil {
						return err
					}
					links = append(links, *l)
				}
			} else if !errors.Is(err, ErrForbidden) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE room_proposals SET status = ?, handled_by_profile_id = ?, created_room_id = ? WHERE id = ?`,
			ProposalCreated, by.ProfileID, r.ID, id); err != nil {
			return fmt.Errorf("create proposed room: %w", err)
		}
		return closeCard(ctx, tx, roomID, msg, "Created by "+by.Name+": "+in.Name)
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return r, seats, links, nil
}

// proposalMembers resolves ids to room's live seats with a profile, one per profile, the caller's
// profile left out: a seat gone since the sheet opened, or of another room, is skipped.
func proposalMembers(ctx context.Context, tx *txn, roomID string, caller int64, ids []int64) ([]Participant, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (profile_id) id, name, profile_id FROM participants
		WHERE id = ANY(?) AND room_id = ? AND removed_at IS NULL AND profile_id IS NOT NULL AND profile_id <> ? ORDER BY profile_id, id`, ids, roomID, caller)
	if err != nil {
		return nil, fmt.Errorf("proposal members: %w", err)
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.ID, &p.Name, &p.ProfileID); err != nil {
			return nil, fmt.Errorf("proposal members: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// closeCard gives a proposal's card line body, and drops its brief: only an open card shows it.
func closeCard(ctx context.Context, tx *txn, roomID string, msg int64, body string) error {
	if err := touchMessage(ctx, tx, roomID, msg, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET html = NULL WHERE id = ?`, msg); err != nil {
		return fmt.Errorf("close proposal card: %w", err)
	}
	return nil
}

// withAction is ctx with its audit entry, if any, as action on room target instead.
func withAction(ctx context.Context, action, target string) context.Context {
	p, _ := ctx.Value(auditKey{}).(*pendingAudit)
	if p == nil {
		return ctx
	}
	e := p.e
	e.Action, e.TargetKind, e.TargetID, e.TargetName = action, TargetRoom, target, ""
	return context.WithValue(ctx, auditKey{}, &pendingAudit{e: e, details: maps.Clone(p.details)})
}
