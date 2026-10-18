package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/render"
)

// Annotation statuses. A participant message on a done annotation reopens it.
const (
	StatusOpen     = "open"
	StatusDone     = "done"
	StatusReopened = "reopened"
)

// Authors of annotation messages.
const (
	ByParticipant = "participant"
	ByAgent       = "agent"
)

var (
	// ErrStaleRevision is returned when an anchor was taken on a revision that is no longer the latest.
	ErrStaleRevision = errors.New("the document has a newer revision")
	// ErrNotAnchorable is returned for an anchor on a document with no text view (PDF, image).
	ErrNotAnchorable = errors.New("only whole-document comments fit a document with no text view")
)

// Annotation is a thread on a document: on a text quote, an element, or the whole document when
// Anchor is nil.
type Annotation struct {
	ID         int64  `json:"id"`
	RoomID     string `json:"room_id"`
	DocumentID int64  `json:"document_id"`
	// Revision is the last revision the anchor was found in.
	Revision int            `json:"revision"`
	Anchor   *anchor.Anchor `json:"anchor"`
	// Orphaned is set when a later revision no longer holds the anchor; Anchor keeps the old one.
	Orphaned  bool                `json:"orphaned"`
	Status    string              `json:"status"`
	CreatedAt time.Time           `json:"created_at"`
	Messages  []AnnotationMessage `json:"messages"`
}

// AnnotationMessage is one message of an annotation's thread.
type AnnotationMessage struct {
	ID           int64  `json:"id"`
	AnnotationID int64  `json:"annotation_id"`
	By           string `json:"by"`
	// ParticipantID is 0 for the agent.
	ParticipantID int64 `json:"participant_id,omitempty"`
	// PersonaID is the room persona that wrote an agent message; Badge is its current one.
	PersonaID   int64        `json:"persona_id,omitempty"`
	Badge       string       `json:"badge,omitempty"`
	Author      string       `json:"author"`
	Body        string       `json:"body"`
	CreatedAt   time.Time    `json:"created_at"`
	Attachments []Attachment `json:"attachments"`
	// Mentions are the turns its @mentions queued, where each stands (009-AC29).
	Mentions []MentionState `json:"mentions,omitempty"`
}

// Attachment is an image on an annotation message.
type Attachment struct {
	ID          int64  `json:"id"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// Data is loaded only by Attachment and AnnotationsSince.
	Data []byte `json:"-"`
}

// NewMessage is a message to add to a thread. ParticipantID 0 means the agent wrote it, as the
// room persona PersonaID.
type NewMessage struct {
	ParticipantID int64
	PersonaID     int64
	Author        string
	Body          string
	// At is when it was written, now when zero: the server's clock times the notes watcher (009-AC32).
	At time.Time
	// Attachments need ContentType and Data; the caller has checked both.
	Attachments []Attachment
}

// CreateAnnotation opens a thread on docID of room with msg as its first message. A non-nil a
// must be valid and taken on revision, the document's latest (ErrStaleRevision otherwise).
// ErrNotFound if the document is not in room, ErrRoomFull if the attachments do not fit in the
// room's quota, defaultQuota when it has none of its own.
func (s *Store) CreateAnnotation(ctx context.Context, roomID string, docID int64, revision int, a *anchor.Anchor, msg *NewMessage, defaultQuota int64) (*Annotation, error) {
	var anchorJSON any
	if a != nil {
		if err := a.Validate(); err != nil {
			return nil, fmt.Errorf("create annotation: %w", err)
		}
		b, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("create annotation: %w", err)
		}
		anchorJSON = string(b)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create annotation: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var last int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT MAX(n) FROM revisions WHERE document_id = d.id), 0)
		FROM documents d WHERE d.id = ? AND d.room_id = ? AND d.deleted_at IS NULL`, docID, roomID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create annotation: %w", err)
	}
	switch {
	case a != nil && last == 0:
		return nil, ErrNotAnchorable
	case a != nil && revision != last:
		return nil, ErrStaleRevision
	}
	ann := &Annotation{RoomID: roomID, DocumentID: docID, Revision: last, Anchor: a, Status: StatusOpen, CreatedAt: time.Now().UTC()}
	if err := tx.QueryRowContext(ctx, `INSERT INTO annotations(room_id, document_id, revision, anchor, created_at) VALUES(?,?,?,?,?) RETURNING id`,
		roomID, docID, last, anchorJSON, formatTS(ann.CreatedAt)).Scan(&ann.ID); err != nil {
		return nil, fmt.Errorf("create annotation: %w", err)
	}
	m, err := insertAnnotationMessage(ctx, tx, roomID, ann.ID, msg, defaultQuota)
	if err != nil {
		return nil, err
	}
	ann.Messages = []AnnotationMessage{*m}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create annotation: %w", err)
	}
	return ann, nil
}

// AddAnnotationMessage appends msg to annotation annID of room. A participant message reopens a
// done annotation. ErrNotFound if the annotation is not in room, ErrRoomFull if the attachments
// do not fit in the room's quota, defaultQuota when it has none of its own.
func (s *Store) AddAnnotationMessage(ctx context.Context, roomID string, annID int64, msg *NewMessage, defaultQuota int64) (*AnnotationMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("add annotation message: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	reopen := msg.ParticipantID != 0
	res, err := tx.ExecContext(ctx, `
		UPDATE annotations SET version = version + 1,
			status = CASE WHEN ? = 1 AND status = 'done' THEN 'reopened' ELSE status END
		WHERE id = ? AND room_id = ?`, reopen, annID, roomID)
	if err != nil {
		return nil, fmt.Errorf("add annotation message: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("add annotation message: %w", err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	m, err := insertAnnotationMessage(ctx, tx, roomID, annID, msg, defaultQuota)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("add annotation message: %w", err)
	}
	return m, nil
}

func insertAnnotationMessage(ctx context.Context, tx *txn, roomID string, annID int64, msg *NewMessage, defaultQuota int64) (*AnnotationMessage, error) {
	if len(msg.Attachments) > 0 {
		// Checked inside the transaction: the write lock serialises it with uploads.
		used, quota, err := roomUsage(ctx, tx, roomID, defaultQuota)
		if err != nil {
			return nil, fmt.Errorf("add annotation message: %w", err)
		}
		for _, at := range msg.Attachments {
			used += int64(len(at.Data))
		}
		if used > quota {
			return nil, ErrRoomFull
		}
	}
	at := msg.At
	if at.IsZero() {
		at = time.Now()
	}
	m := &AnnotationMessage{AnnotationID: annID, By: ByAgent, ParticipantID: msg.ParticipantID, PersonaID: msg.PersonaID, Author: msg.Author, Body: msg.Body,
		CreatedAt: at.UTC(), Attachments: []Attachment{}}
	var pid any
	if msg.ParticipantID != 0 {
		m.By, pid = ByParticipant, msg.ParticipantID
	}
	ts := formatTS(m.CreatedAt)
	if err := tx.QueryRowContext(ctx, `INSERT INTO annotation_messages(annotation_id, author_kind, participant_id, persona_id, author, body, created_at)
		VALUES(?,?,?,?,?,?,?) RETURNING id`, annID, m.By, pid, nullID(m.PersonaID), m.Author, m.Body, ts).Scan(&m.ID); err != nil {
		return nil, fmt.Errorf("insert annotation message: %w", err)
	}
	for _, at := range msg.Attachments {
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO attachments(message_id, content_type, size, data, created_at) VALUES(?,?,?,?,?) RETURNING id`,
			m.ID, at.ContentType, len(at.Data), at.Data, ts).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert attachment: %w", err)
		}
		m.Attachments = append(m.Attachments, Attachment{ID: id, ContentType: at.ContentType, Size: int64(len(at.Data))})
	}
	return m, nil
}

// SetAnnotationStatus sets the status of annotation annID of room. ErrNotFound if it is not in room.
func (s *Store) SetAnnotationStatus(ctx context.Context, roomID string, annID int64, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE annotations SET status = ?, version = version + 1 WHERE id = ? AND room_id = ?`, status, annID, roomID)
	if err != nil {
		return fmt.Errorf("set annotation status: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set annotation status: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Annotations lists the annotations of a document of room, oldest first, with their threads.
// Attachments come without their data.
func (s *Store) Annotations(ctx context.Context, roomID string, docID int64) ([]Annotation, error) {
	const cond = `a.room_id = ? AND a.document_id = ?`
	out, err := s.loadAnnotations(ctx, false, cond, roomID, docID)
	if err != nil {
		return nil, err
	}
	return out, s.noteMentions(ctx, out, cond, roomID, docID)
}

// AnnotationsSince is what the agent reads at the start of a turn: every annotation of room with
// a message past cursor, with its whole thread and the attachments' data, and the cursor to pass
// next time. Messages with an ID above cursor are the new ones. Cursor 0 reads everything.
func (s *Store) AnnotationsSince(ctx context.Context, roomID string, cursor int64) ([]Annotation, int64, error) {
	var next int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(m.id), 0) FROM annotation_messages m JOIN annotations a ON a.id = m.annotation_id
		WHERE a.room_id = ?`, roomID).Scan(&next)
	if err != nil {
		return nil, 0, fmt.Errorf("annotations since: %w", err)
	}
	// Read with the same cut: a message landing between the two queries waits for the next turn.
	// A deleted document's threads leave the agents' view with it (005-AC26).
	out, err := s.loadAnnotations(ctx, true, `a.room_id = ? AND a.id IN (
		SELECT annotation_id FROM annotation_messages WHERE id > ? AND id <= ?)
		AND a.document_id IN (SELECT id FROM documents WHERE deleted_at IS NULL)`, roomID, cursor, next)
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		msgs := out[i].Messages[:0]
		for _, m := range out[i].Messages {
			if m.ID <= next {
				msgs = append(msgs, m)
			}
		}
		out[i].Messages = msgs
	}
	return out, max(next, cursor), nil
}

// Attachment returns one attachment of room with its data; ErrNotFound if it is not in room.
func (s *Store) Attachment(ctx context.Context, roomID string, id int64) (*Attachment, error) {
	var at Attachment
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.content_type, t.size, t.data FROM attachments t
		JOIN annotation_messages m ON m.id = t.message_id JOIN annotations a ON a.id = m.annotation_id
		WHERE t.id = ? AND a.room_id = ?`, id, roomID).Scan(&at.ID, &at.ContentType, &at.Size, &at.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("attachment %d: %w", id, err)
	}
	return &at, nil
}

// AnnotationsVersion changes whenever an annotation of room is added or changed.
func (s *Store) AnnotationsVersion(ctx context.Context, roomID string) (string, error) {
	var n, sum int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(version), 0) FROM annotations WHERE room_id = ?`, roomID).Scan(&n, &sum)
	if err != nil {
		return "", fmt.Errorf("annotations version: %w", err)
	}
	return fmt.Sprintf("%d.%d", n, sum), nil
}

// loadAnnotations reads the annotations matching cond (over alias a), their messages and their
// attachments in three queries. cond is always a constant of this file, never user input.
func (s *Store) loadAnnotations(ctx context.Context, withData bool, cond string, args ...any) ([]Annotation, error) {
	out := []Annotation{}
	byID := map[int64]int{}
	//nolint:gosec // cond is a constant of this file, never user input
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.room_id, a.document_id, a.revision, a.anchor, a.orphaned, a.status, a.created_at
		FROM annotations a WHERE `+cond+` ORDER BY a.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query annotations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a Annotation
		var anchorJSON sql.NullString
		var created string
		if err := rows.Scan(&a.ID, &a.RoomID, &a.DocumentID, &a.Revision, &anchorJSON, &a.Orphaned, &a.Status, &created); err != nil {
			return nil, fmt.Errorf("scan annotation: %w", err)
		}
		if anchorJSON.Valid {
			a.Anchor = &anchor.Anchor{}
			if err := json.Unmarshal([]byte(anchorJSON.String), a.Anchor); err != nil {
				return nil, fmt.Errorf("annotation %d anchor: %w", a.ID, err)
			}
		}
		a.CreatedAt = parseTS(created)
		a.Messages = []AnnotationMessage{}
		byID[a.ID] = len(out)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query annotations: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	type pos struct{ ann, msg int }
	msgAt := map[int64]pos{}
	//nolint:gosec // cond is a constant of this file, never user input
	mrows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.annotation_id, m.author_kind, COALESCE(m.participant_id, 0), COALESCE(m.persona_id, 0),
			COALESCE((SELECT badge FROM room_personas WHERE id = m.persona_id), ''), m.author, m.body, m.created_at
		FROM annotation_messages m JOIN annotations a ON a.id = m.annotation_id WHERE `+cond+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query annotation messages: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var m AnnotationMessage
		var created string
		if err := mrows.Scan(&m.ID, &m.AnnotationID, &m.By, &m.ParticipantID, &m.PersonaID, &m.Badge, &m.Author, &m.Body, &created); err != nil {
			return nil, fmt.Errorf("scan annotation message: %w", err)
		}
		m.CreatedAt = parseTS(created)
		m.Attachments = []Attachment{}
		i := byID[m.AnnotationID]
		msgAt[m.ID] = pos{i, len(out[i].Messages)}
		out[i].Messages = append(out[i].Messages, m)
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("query annotation messages: %w", err)
	}

	data := "NULL"
	if withData {
		data = "t.data"
	}
	//nolint:gosec // cond is a constant of this file, never user input
	trows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.message_id, t.content_type, t.size, `+data+` FROM attachments t
		JOIN annotation_messages m ON m.id = t.message_id JOIN annotations a ON a.id = m.annotation_id
		WHERE `+cond+` ORDER BY t.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer trows.Close()
	for trows.Next() {
		var at Attachment
		var msgID int64
		if err := trows.Scan(&at.ID, &msgID, &at.ContentType, &at.Size, &at.Data); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		p := msgAt[msgID]
		m := &out[p.ann].Messages[p.msg]
		m.Attachments = append(m.Attachments, at)
	}
	return out, trows.Err()
}

// reanchor moves the anchored annotations of a document onto its revision n: each one found in
// body follows it there; one that is gone is flagged orphaned and keeps its last anchor and
// revision, and comes back if a later revision holds it again.
func reanchor(ctx context.Context, tx *txn, docID int64, format string, n int, body string) error {
	type row struct {
		id int64
		a  anchor.Anchor
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, anchor FROM annotations WHERE document_id = ? AND anchor IS NOT NULL`, docID)
	if err != nil {
		return fmt.Errorf("reanchor: %w", err)
	}
	var anchored []row
	for rows.Next() {
		var r row
		var js string
		if err := rows.Scan(&r.id, &js); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reanchor: %w", err)
		}
		if err := json.Unmarshal([]byte(js), &r.a); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reanchor annotation %d: %w", r.id, err)
		}
		anchored = append(anchored, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reanchor: %w", err)
	}
	if len(anchored) == 0 {
		return nil
	}
	page := []byte(body)
	if format == "md" {
		if page, err = render.MdToHTML(page); err != nil {
			return fmt.Errorf("reanchor: %w", err)
		}
	}
	doc, err := anchor.Parse(page)
	if err != nil {
		return fmt.Errorf("reanchor: %w", err)
	}
	for _, r := range anchored {
		a, ok := doc.Reanchor(r.a)
		var q string
		args := []any{}
		if ok {
			js, err := json.Marshal(a)
			if err != nil {
				return fmt.Errorf("reanchor annotation %d: %w", r.id, err)
			}
			q, args = `UPDATE annotations SET anchor = ?, revision = ?, orphaned = 0, version = version + 1 WHERE id = ?`, append(args, string(js), n)
		} else {
			q = `UPDATE annotations SET orphaned = 1, version = version + 1 WHERE id = ?`
		}
		if _, err := tx.ExecContext(ctx, q, append(args, r.id)...); err != nil {
			return fmt.Errorf("reanchor annotation %d: %w", r.id, err)
		}
	}
	return nil
}
