package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// MaxRoomReads is how many rooms one room reads at most (023-AC4).
const MaxRoomReads = 10

// ErrLinkCap refuses a link past MaxRoomReads; its error text is the refusal to show.
var ErrLinkCap = errors.New("room reads too many rooms")

type linkCap string

func (c linkCap) Error() string { return string(c) }
func (linkCap) Unwrap() error   { return ErrLinkCap }

// RoomLink is a directed link: ReaderRoom reads ReadRoom (023). Member and CanRemove are the
// viewer's, set by RoomLinks only.
type RoomLink struct {
	ID         int64     `json:"id"`
	ReaderRoom string    `json:"reader_room"`
	ReaderName string    `json:"reader_name"`
	ReadRoom   string    `json:"read_room"`
	ReadName   string    `json:"read_name"`
	LinkedBy   string    `json:"linked_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// Member tells the viewer sits in the other room, so its name may link there (023-AC8).
	Member    bool `json:"member"`
	CanRemove bool `json:"can_remove"`
}

func (l *RoomLink) details() map[string]any {
	return map[string]any{"link": l.ID, "reader_room": l.ReaderRoom, "reader_name": l.ReaderName, "read_room": l.ReadRoom, "read_name": l.ReadName}
}

// creatorName is "who linked it": the profile's chosen name, else its newest seat in either linked
// room; a seat elsewhere would name it to people who never met it there.
const creatorName = `COALESCE((SELECT COALESCE(f.name, (SELECT name FROM participants WHERE profile_id = f.id
	AND room_id IN (l.reader_room_id, l.read_room_id) ORDER BY id DESC LIMIT 1), '')
	FROM profiles f WHERE f.id = l.created_by_profile_id), '')`

// maxAuditLinks caps the link ids a room delete's audit entry lists: the read-by side has no cap,
// and the entry must stay under MaxAuditDetails. links_count keeps the total.
const maxAuditLinks = 100

const linkCols = `l.id, l.reader_room_id, ra.name, l.read_room_id, rb.name, ` + creatorName + `, l.created_at`

const linkFrom = ` FROM room_links l JOIN rooms ra ON ra.id = l.reader_room_id JOIN rooms rb ON rb.id = l.read_room_id`

func scanLink(sc scanner, extra ...any) (*RoomLink, error) {
	var l RoomLink
	var created string
	if err := sc.Scan(append([]any{&l.ID, &l.ReaderRoom, &l.ReaderName, &l.ReadRoom, &l.ReadName, &l.LinkedBy, &created}, extra...)...); err != nil {
		return nil, err
	}
	l.CreatedAt = parseTS(created)
	return &l, nil
}

// RoomLinks lists the links of room, both directions, oldest first, as profile viewer sees them.
func (s *Store) RoomLinks(ctx context.Context, roomID string, viewer int64) ([]RoomLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+linkCols+`,
		EXISTS (SELECT 1 FROM participants p WHERE p.profile_id = ? AND p.removed_at IS NULL
			AND p.room_id = CASE WHEN l.reader_room_id = ? THEN l.read_room_id ELSE l.reader_room_id END),
		EXISTS (SELECT 1 FROM room_admins a WHERE a.profile_id = ? AND a.room_id IN (l.reader_room_id, l.read_room_id))`+linkFrom+`
		WHERE l.reader_room_id = ? OR l.read_room_id = ? ORDER BY l.id`, viewer, roomID, viewer, roomID, roomID)
	if err != nil {
		return nil, fmt.Errorf("room links: %w", err)
	}
	defer rows.Close()
	out := []RoomLink{}
	for rows.Next() {
		var member, admin bool
		l, err := scanLink(rows, &member, &admin)
		if err != nil {
			return nil, fmt.Errorf("scan room link: %w", err)
		}
		l.Member, l.CanRemove = member, admin
		out = append(out, *l)
	}
	return out, rows.Err()
}

// RoomLink returns link id when it involves room; ErrNotFound otherwise.
func (s *Store) RoomLink(ctx context.Context, roomID string, id int64) (*RoomLink, error) {
	return roomLink(ctx, s.db, roomID, id)
}

func roomLink(ctx context.Context, db rowQuerier, roomID string, id int64) (*RoomLink, error) {
	l, err := scanLink(db.QueryRowContext(ctx, `SELECT `+linkCols+linkFrom+` WHERE l.id = ? AND ? IN (l.reader_room_id, l.read_room_id)`, id, roomID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room link %d: %w", id, err)
	}
	return l, nil
}

// CreateRoomLinks links room and other in one transaction: room reads other when reads, other
// reads room when readBy. by's profile must administer both rooms (023-AC2). A link that exists is
// returned as is (023-AC3); each new one gets a line in both rooms and an audit entry (023-AC6).
// created tells whether any link is new. ErrForbidden, ErrLinkCap.
func (s *Store) CreateRoomLinks(ctx context.Context, roomID, other string, reads, readBy bool, by *Participant) (links []RoomLink, created bool, err error) {
	err = s.withTx(ctx, "link rooms", func(tx *txn) error {
		for _, r := range []string{roomID, other} {
			if _, err := roomAdmin(ctx, tx, r, by.ProfileID); err != nil {
				return err
			}
		}
		pairs := [][2]string{}
		if reads {
			pairs = append(pairs, [2]string{roomID, other})
		}
		if readBy {
			pairs = append(pairs, [2]string{other, roomID})
		}
		for _, p := range pairs {
			l, isNew, err := linkRoom(ctx, tx, roomID, p[0], p[1], by)
			if err != nil {
				return err
			}
			// The caller administers both rooms and sits in them.
			l.Member, l.CanRemove = true, true
			links, created = append(links, *l), created || isNew
		}
		return nil
	})
	return links, created, err
}

// linkRoom makes reader read read, or returns the link that does.
func linkRoom(ctx context.Context, tx *txn, roomID, reader, read string, by *Participant) (*RoomLink, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM room_links WHERE reader_room_id = ? AND read_room_id = ?`, reader, read).Scan(&id)
	if err == nil {
		l, err := roomLink(ctx, tx, reader, id)
		return l, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("link rooms: %w", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM room_links WHERE reader_room_id = ?`, reader).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("link rooms: %w", err)
	}
	if n >= MaxRoomReads {
		who := "This room"
		if reader != roomID {
			if err := tx.QueryRowContext(ctx, `SELECT name FROM rooms WHERE id = ?`, reader).Scan(&who); err != nil {
				return nil, false, fmt.Errorf("link rooms: %w", err)
			}
		}
		return nil, false, linkCap(who + " already reads " + strconv.Itoa(MaxRoomReads) + " rooms. Remove one first.")
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO room_links(reader_room_id, read_room_id, created_by_profile_id, created_at) VALUES(?,?,?,?) RETURNING id`,
		reader, read, nullID(by.ProfileID), now()).Scan(&id); err != nil {
		return nil, false, fmt.Errorf("link rooms: %w", err)
	}
	l, err := roomLink(ctx, tx, reader, id)
	if err != nil {
		return nil, false, err
	}
	if err := linkLines(ctx, tx, l, by, "linked this room: it now reads "+l.ReadName, "linked "+l.ReaderName+": it now reads this room"); err != nil {
		return nil, false, err
	}
	return l, true, auditTx(ctx, tx, "", l.details())
}

// DeleteRoomLink removes link id of room; by's profile must administer either of its rooms
// (023-AC5). ErrNotFound, ErrForbidden.
func (s *Store) DeleteRoomLink(ctx context.Context, roomID string, id int64, by *Participant) (*RoomLink, error) {
	var l *RoomLink
	err := s.withTx(ctx, "unlink rooms", func(tx *txn) error {
		var err error
		if l, err = roomLink(ctx, tx, roomID, id); err != nil {
			return err
		}
		if _, err := roomAdmin(ctx, tx, l.ReaderRoom, by.ProfileID); errors.Is(err, ErrForbidden) {
			if _, err := roomAdmin(ctx, tx, l.ReadRoom, by.ProfileID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := execOne(ctx, tx, `DELETE FROM room_links WHERE id = ?`, id); err != nil {
			return err
		}
		if err := linkLines(ctx, tx, l, by, "removed the link: this room no longer reads "+l.ReadName,
			"removed the link: "+l.ReaderName+" no longer reads this room"); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", l.details())
	})
	return l, err
}

// linkLines says what by did in the reader room and in the read room, under by's seat there.
func linkLines(ctx context.Context, tx *txn, l *RoomLink, by *Participant, reader, read string) error {
	for _, line := range [][2]string{{l.ReaderRoom, reader}, {l.ReadRoom, read}} {
		seat := by
		if line[0] != by.RoomID {
			seat = &Participant{RoomID: line[0], Name: by.Name}
			err := tx.QueryRowContext(ctx, `SELECT id, name FROM participants WHERE room_id = ? AND profile_id = ? AND removed_at IS NULL ORDER BY id DESC LIMIT 1`,
				line[0], by.ProfileID).Scan(&seat.ID, &seat.Name)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("link line: %w", err)
			}
		}
		m := &Message{RoomID: line[0], Kind: KindSystem, ParticipantID: seat.ID, Sender: seat.Name, Body: seat.Name + " " + line[1]}
		if err := insertMessage(ctx, tx, m); err != nil {
			return err
		}
	}
	return nil
}

// roomLinksGone tells each room linked to room id, about to be deleted, that its links go, and
// returns those links and rooms (023-AC7).
func roomLinksGone(ctx context.Context, tx *txn, id string) ([]int64, []string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+linkCols+linkFrom+` WHERE ? IN (l.reader_room_id, l.read_room_id) ORDER BY l.id`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("room links: %w", err)
	}
	var ls []*RoomLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan room link: %w", err)
		}
		ls = append(ls, l)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("room links: %w", err)
	}
	var ids []int64
	var others []string
	told := map[string]bool{}
	for _, l := range ls {
		ids = append(ids, l.ID)
		other, name := l.ReaderRoom, l.ReadName
		if other == id {
			other, name = l.ReadRoom, l.ReaderName
		}
		if told[other] {
			continue
		}
		told[other] = true
		others = append(others, other)
		m := &Message{RoomID: other, Kind: KindSystem, Sender: name, Body: name + " was deleted; the link is gone"}
		if err := insertMessage(ctx, tx, m); err != nil {
			return nil, nil, err
		}
	}
	return ids, others, nil
}
