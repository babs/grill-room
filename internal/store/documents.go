package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Document kinds. Uploads are read-only sources (D30); only agent documents take revisions.
const (
	DocUpload = "upload"
	DocAgent  = "agent"
)

var (
	// ErrReadOnly is returned when a revision targets an uploaded document.
	ErrReadOnly = errors.New("uploaded documents are read-only")
	// ErrRoomFull is returned when an upload would take the room past its storage limit.
	ErrRoomFull = errors.New("room upload limit reached")
)

// Document is a file shown in a room: an upload or a document the agent writes.
type Document struct {
	ID     int64  `json:"id"`
	RoomID string `json:"room_id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	// Format is how the room shows it: md, html, pdf or image. An office upload is md: its
	// Markdown copy is what the room shows and annotates.
	Format string `json:"format"`
	// ContentType and Size describe an upload's original; empty for agent documents.
	ContentType string    `json:"content_type,omitempty"`
	Size        int64     `json:"size,omitempty"`
	Author      string    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
	// Revisions counts the text revisions: 0 for a PDF or an image, 1 for other uploads.
	Revisions int `json:"revisions"`
}

// Revision is one version of a document's text.
type Revision struct {
	ID         int64     `json:"id"`
	DocumentID int64     `json:"document_id"`
	N          int       `json:"n"`
	Author     string    `json:"author"`
	CreatedAt  time.Time `json:"created_at"`
	Body       string    `json:"-"`
}

// Upload is a file a participant adds to a room.
type Upload struct {
	RoomID        string
	ParticipantID int64
	Author        string
	Filename      string
	ContentType   string
	Format        string
	Data          []byte
	// Text becomes revision 1 when the format is shown as text (md, html): the file itself, or
	// the Markdown copy of an office file.
	Text string
}

// RoomBytes returns what a room's blobs weigh: upload originals plus annotation attachments.
func (s *Store) RoomBytes(ctx context.Context, roomID string) (int64, error) {
	return roomBytes(ctx, s.db, roomID)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func roomBytes(ctx context.Context, db queryer, roomID string) (int64, error) {
	var used int64
	err := db.QueryRowContext(ctx, `
		SELECT (SELECT COALESCE(SUM(size), 0) FROM uploads WHERE room_id = ?)
			+ (SELECT COALESCE(SUM(t.size), 0) FROM attachments t
				JOIN annotation_messages m ON m.id = t.message_id
				JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = ?)`, roomID, roomID).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("room bytes: %w", err)
	}
	return used, nil
}

// AddUpload stores the original and its document in one transaction. ErrRoomFull if the room's
// blobs would then exceed maxRoomBytes; ErrNotFound if the room is gone.
func (s *Store) AddUpload(ctx context.Context, u *Upload, maxRoomBytes int64) (*Document, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	// Checked inside the transaction: the single connection serialises it with concurrent uploads.
	used, err := roomBytes(ctx, tx, u.RoomID)
	if err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	}
	if used+int64(len(u.Data)) > maxRoomBytes {
		return nil, ErrRoomFull
	}
	ts := now()
	var pid any
	if u.ParticipantID != 0 {
		pid = u.ParticipantID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO uploads(room_id, participant_id, filename, content_type, size, data, created_at)
		SELECT id, ?, ?, ?, ?, ?, ? FROM rooms WHERE id = ?`,
		pid, u.Filename, u.ContentType, len(u.Data), u.Data, ts, u.RoomID)
	if err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	uploadID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add upload id: %w", err)
	}
	d := &Document{RoomID: u.RoomID, Kind: DocUpload, Name: u.Filename, Format: u.Format,
		ContentType: u.ContentType, Size: int64(len(u.Data)), Author: u.Author}
	if err := insertDocument(ctx, tx, d, uploadID, ts); err != nil {
		return nil, err
	}
	if u.Format == "md" || u.Format == "html" {
		if _, err := insertRevision(ctx, tx, d.ID, 1, u.Text, u.Author); err != nil {
			return nil, err
		}
		d.Revisions = 1
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	}
	return d, nil
}

// CreateAgentDocument creates an agent document with body as revision 1. format is md or html.
func (s *Store) CreateAgentDocument(ctx context.Context, roomID, name, format, body, author string) (*Document, error) {
	if format != "md" && format != "html" {
		return nil, fmt.Errorf("create agent document: format %q is not md or html", format)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create agent document: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	d := &Document{RoomID: roomID, Kind: DocAgent, Name: name, Format: format, Author: author, Revisions: 1}
	if err := insertDocument(ctx, tx, d, 0, now()); err != nil {
		return nil, err
	}
	if _, err := insertRevision(ctx, tx, d.ID, 1, body, author); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create agent document: %w", err)
	}
	return d, nil
}

// AddRevision appends body as the next revision of an agent document of room and re-anchors the
// document's annotations on it. ErrReadOnly for an upload, ErrNotFound if the document is not in
// room.
func (s *Store) AddRevision(ctx context.Context, roomID string, docID int64, body, author string) (*Revision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("add revision: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var kind, format string
	var last int
	err = tx.QueryRowContext(ctx, `
		SELECT d.kind, d.format, COALESCE((SELECT MAX(n) FROM revisions WHERE document_id = d.id), 0)
		FROM documents d WHERE d.id = ? AND d.room_id = ?`, docID, roomID).Scan(&kind, &format, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("add revision: %w", err)
	}
	if kind != DocAgent {
		return nil, ErrReadOnly
	}
	r, err := insertRevision(ctx, tx, docID, last+1, body, author)
	if err != nil {
		return nil, err
	}
	if err := reanchor(ctx, tx, docID, format, r.N, body); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("add revision: %w", err)
	}
	return r, nil
}

func insertDocument(ctx context.Context, tx *sql.Tx, d *Document, uploadID int64, ts string) error {
	var up any
	if uploadID != 0 {
		up = uploadID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO documents(room_id, kind, name, format, upload_id, author, created_at)
		SELECT id, ?, ?, ?, ?, ?, ? FROM rooms WHERE id = ?`,
		d.Kind, d.Name, d.Format, up, d.Author, ts, d.RoomID)
	if err != nil {
		return fmt.Errorf("insert document: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("insert document: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	if d.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("insert document id: %w", err)
	}
	d.CreatedAt = parseTS(ts)
	return nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, docID int64, n int, body, author string) (*Revision, error) {
	r := &Revision{DocumentID: docID, N: n, Author: author, Body: body, CreatedAt: time.Now().UTC()}
	res, err := tx.ExecContext(ctx, `INSERT INTO revisions(document_id, n, body, author, created_at) VALUES(?,?,?,?,?)`,
		docID, n, body, author, r.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("insert revision: %w", err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return nil, fmt.Errorf("insert revision id: %w", err)
	}
	return r, nil
}

const documentCols = `
	SELECT d.id, d.room_id, d.kind, d.name, d.format, COALESCE(u.content_type, ''), COALESCE(u.size, 0),
		d.author, d.created_at, (SELECT COUNT(*) FROM revisions r WHERE r.document_id = d.id)
	FROM documents d LEFT JOIN uploads u ON u.id = d.upload_id`

type scanner interface{ Scan(dest ...any) error }

func scanDocument(sc scanner) (*Document, error) {
	var d Document
	var created string
	if err := sc.Scan(&d.ID, &d.RoomID, &d.Kind, &d.Name, &d.Format, &d.ContentType, &d.Size, &d.Author, &created, &d.Revisions); err != nil {
		return nil, err
	}
	d.CreatedAt = parseTS(created)
	return &d, nil
}

// Documents lists a room's documents, oldest first.
func (s *Store) Documents(ctx context.Context, roomID string) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, documentCols+` WHERE d.room_id = ? ORDER BY d.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// Document fetches one document of room; ErrNotFound if it is not in room.
func (s *Store) Document(ctx context.Context, roomID string, docID int64) (*Document, error) {
	d, err := scanDocument(s.db.QueryRowContext(ctx, documentCols+` WHERE d.id = ? AND d.room_id = ?`, docID, roomID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document %d: %w", docID, err)
	}
	return d, nil
}

// Revisions lists a document's revisions without their bodies, oldest first.
func (s *Store) Revisions(ctx context.Context, docID int64) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, document_id, n, author, created_at FROM revisions WHERE document_id = ? ORDER BY n`, docID)
	if err != nil {
		return nil, fmt.Errorf("query revisions: %w", err)
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		var created string
		if err := rows.Scan(&r.ID, &r.DocumentID, &r.N, &r.Author, &created); err != nil {
			return nil, fmt.Errorf("scan revision: %w", err)
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revision fetches revision n of a document, body included; ErrNotFound if it does not exist.
func (s *Store) Revision(ctx context.Context, docID int64, n int) (*Revision, error) {
	var r Revision
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, document_id, n, author, created_at, body FROM revisions WHERE document_id = ? AND n = ?`, docID, n).
		Scan(&r.ID, &r.DocumentID, &r.N, &r.Author, &created, &r.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("revision %d of document %d: %w", n, docID, err)
	}
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// Original returns the uploaded bytes of a document of room; ErrNotFound for an agent document.
func (s *Store) Original(ctx context.Context, roomID string, docID int64) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT u.data FROM documents d JOIN uploads u ON u.id = d.upload_id
		WHERE d.id = ? AND d.room_id = ?`, docID, roomID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("original of document %d: %w", docID, err)
	}
	return data, nil
}

// DocumentsVersion changes whenever a document or a revision is added to room.
func (s *Store) DocumentsVersion(ctx context.Context, roomID string) (string, error) {
	var docs, revs int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(d.id), 0), COALESCE(MAX(r.id), 0)
		FROM documents d LEFT JOIN revisions r ON r.document_id = d.id WHERE d.room_id = ?`, roomID).Scan(&docs, &revs)
	if err != nil {
		return "", fmt.Errorf("documents version: %w", err)
	}
	return fmt.Sprintf("%d.%d", docs, revs), nil
}
