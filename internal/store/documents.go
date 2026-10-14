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

// RoomUsage returns what a room's blobs weigh, upload originals, annotation attachments and plugin
// snapshots (009-AC9), and the quota they count against: the room's own, else defaultQuota
// (004-AC9).
func (s *Store) RoomUsage(ctx context.Context, roomID string, defaultQuota int64) (used, quota int64, err error) {
	return roomUsage(ctx, s.db, roomID, defaultQuota)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func roomUsage(ctx context.Context, db queryer, roomID string, defaultQuota int64) (used, quota int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT quota_bytes FROM rooms WHERE id = ?), ?),
			(SELECT COALESCE(SUM(size), 0) FROM uploads WHERE room_id = ?)
			+ (SELECT COALESCE(SUM(t.size), 0) FROM attachments t
				JOIN annotation_messages m ON m.id = t.message_id
				JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = ?)
			+ (SELECT COALESCE(SUM(length(v.data)), 0) FROM plugin_versions v
				JOIN room_plugins p ON p.id = v.plugin_id WHERE p.room_id = ?)`, roomID, defaultQuota, roomID, roomID, roomID).Scan(&quota, &used)
	if err != nil {
		return 0, 0, fmt.Errorf("room usage: %w", err)
	}
	return used, quota, nil
}

// AddUpload stores the original and its document in one transaction. ErrRoomFull if the room's
// blobs would then exceed its quota, defaultQuota when it has none of its own; ErrNotFound if the
// room is gone.
func (s *Store) AddUpload(ctx context.Context, u *Upload, defaultQuota int64) (*Document, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	// Checked inside the transaction: the single connection serialises it with concurrent uploads.
	used, quota, err := roomUsage(ctx, tx, u.RoomID, defaultQuota)
	if err != nil {
		return nil, fmt.Errorf("add upload: %w", err)
	}
	if used+int64(len(u.Data)) > quota {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create agent document: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	d, err := createAgentDocument(ctx, tx, roomID, name, format, body, author)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create agent document: %w", err)
	}
	return d, nil
}

func createAgentDocument(ctx context.Context, tx *sql.Tx, roomID, name, format, body, author string) (*Document, error) {
	if format != "md" && format != "html" {
		return nil, fmt.Errorf("create agent document: format %q is not md or html", format)
	}
	d := &Document{RoomID: roomID, Kind: DocAgent, Name: name, Format: format, Author: author, Revisions: 1}
	if err := insertDocument(ctx, tx, d, 0, now()); err != nil {
		return nil, err
	}
	if _, err := insertRevision(ctx, tx, d.ID, 1, body, author); err != nil {
		return nil, err
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
	r, err := addRevision(ctx, tx, roomID, docID, body, author)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("add revision: %w", err)
	}
	return r, nil
}

func addRevision(ctx context.Context, tx *sql.Tx, roomID string, docID int64, body, author string) (*Revision, error) {
	var kind, format string
	var last int
	err := tx.QueryRowContext(ctx, `
		SELECT d.kind, d.format, COALESCE((SELECT MAX(n) FROM revisions WHERE document_id = d.id), 0)
		FROM documents d WHERE d.id = ? AND d.room_id = ? AND d.deleted_at IS NULL`, docID, roomID).Scan(&kind, &format, &last)
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

// Documents lists a room's documents, oldest first, the agenda ahead of them.
func (s *Store) Documents(ctx context.Context, roomID string) ([]Document, error) {
	// The agenda leads the list (006-AC14).
	rows, err := s.db.QueryContext(ctx, documentCols+` WHERE d.room_id = ? AND d.deleted_at IS NULL
		ORDER BY d.kind = 'agent' AND d.name = '`+AgendaName+`' DESC, d.id`, roomID)
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
	d, err := scanDocument(s.db.QueryRowContext(ctx, documentCols+` WHERE d.id = ? AND d.room_id = ? AND d.deleted_at IS NULL`, docID, roomID))
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
		WHERE d.id = ? AND d.room_id = ? AND d.deleted_at IS NULL`, docID, roomID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("original of document %d: %w", docID, err)
	}
	return data, nil
}

// DocumentsVersion changes whenever a document or a revision is added to room, and when a
// document is renamed, deleted or restored.
func (s *Store) DocumentsVersion(ctx context.Context, roomID string) (string, error) {
	var docs, revs, changes int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(d.id), 0), COALESCE(MAX(r.id), 0), COALESCE(MAX(d.change_seq), 0)
		FROM documents d LEFT JOIN revisions r ON r.document_id = d.id WHERE d.room_id = ?`, roomID).Scan(&docs, &revs, &changes)
	if err != nil {
		return "", fmt.Errorf("documents version: %w", err)
	}
	return fmt.Sprintf("%d.%d.%d", docs, revs, changes), nil
}

var (
	// ErrDocNameTaken is returned when another document of the room holds the name (005-AC25).
	ErrDocNameTaken = errors.New("another document of the room has that name")
	// ErrTurnRunning is returned when renaming or deleting an agent document while a turn runs:
	// its result would write the document back under the name it read (005-AC25, 005-AC26).
	ErrTurnRunning = errors.New("an agent turn is running: try again once it ends")
)

// DocumentChange is a document renamed, deleted or restored, with the chat line saying so.
type DocumentChange struct {
	Document Document
	From     string
	Line     *Message
}

// docTx loads document docID of room in tx, deleted or not as deleted says, and refuses an
// agent document while a turn runs.
func docTx(ctx context.Context, tx *sql.Tx, roomID string, docID int64, deleted bool) (*Document, error) {
	cond := ` AND d.deleted_at IS NULL`
	if deleted {
		cond = ` AND d.deleted_at IS NOT NULL`
	}
	d, err := scanDocument(tx.QueryRowContext(ctx, documentCols+` WHERE d.id = ? AND d.room_id = ?`+cond, docID, roomID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document %d: %w", docID, err)
	}
	if d.Kind == DocAgent {
		var running bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM turns WHERE room_id = ? AND status = 'running')`, roomID).Scan(&running); err != nil {
			return nil, fmt.Errorf("document %d: %w", docID, err)
		}
		if running {
			return nil, ErrTurnRunning
		}
	}
	return d, nil
}

// nameFree reports whether no live document of room but docID holds name.
func nameFree(ctx context.Context, tx *sql.Tx, roomID string, docID int64, name string) error {
	var taken bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM documents WHERE room_id = ? AND name = ? AND id != ? AND deleted_at IS NULL)`,
		roomID, name, docID).Scan(&taken)
	if err != nil {
		return fmt.Errorf("document name: %w", err)
	}
	if taken {
		return ErrDocNameTaken
	}
	return nil
}

// bumpDocs moves the room's documents version: pages and daemons read the list again.
func bumpDocs(ctx context.Context, tx *sql.Tx, roomID string, docID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE documents SET change_seq = (SELECT COALESCE(MAX(change_seq), 0) + 1 FROM documents WHERE room_id = ?)
		WHERE id = ?`, roomID, docID)
	if err != nil {
		return fmt.Errorf("documents version: %w", err)
	}
	return nil
}

// docLine writes the chat line of a document change: by a member, or by the super-admin.
func docLine(ctx context.Context, tx *sql.Tx, roomID string, by *Participant, body string) (*Message, error) {
	if by == nil {
		m := &Message{RoomID: roomID, Kind: KindSystem, Sender: "admin", Body: "The admin " + body}
		return m, insertMessage(ctx, tx, m)
	}
	return memberLine(ctx, tx, roomID, by, body)
}

// RenameDocument gives document docID of room the name name, keeping its id, revisions and
// annotations, says so in the chat and records the rename for the personas' next turns
// (005-AC25). A name unchanged changes nothing: nil.
func (s *Store) RenameDocument(ctx context.Context, roomID string, docID int64, name string, by *Participant) (*DocumentChange, error) {
	var out *DocumentChange
	err := s.withTx(ctx, "rename document", func(tx *sql.Tx) error {
		d, err := docTx(ctx, tx, roomID, docID, false)
		if err != nil || d.Name == name {
			return err
		}
		if err := nameFree(ctx, tx, roomID, docID, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET name = ? WHERE id = ?`, name, docID); err != nil {
			return fmt.Errorf("rename document: %w", err)
		}
		if err := bumpDocs(ctx, tx, roomID, docID); err != nil {
			return err
		}
		m, err := docLine(ctx, tx, roomID, by, "renamed `"+d.Name+"` → `"+name+"`")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_renames(document_id, message_id, from_name, to_name, created_at) VALUES(?,?,?,?,?)`,
			docID, m.ID, d.Name, name, now()); err != nil {
			return fmt.Errorf("rename document: %w", err)
		}
		from := d.Name
		d.Name = name
		out = &DocumentChange{Document: *d, From: from, Line: m}
		return nil
	})
	return out, err
}

// SetDocumentDeleted deletes document docID of room softly, or restores it with deleted false
// (005-AC26). by must be a room admin, checked in the same transaction, or nil for the
// super-admin. The data stays until the room goes. A restore whose name another document took
// meanwhile is ErrDocNameTaken.
func (s *Store) SetDocumentDeleted(ctx context.Context, roomID string, docID int64, deleted bool, by *Participant) (*DocumentChange, error) {
	var out *DocumentChange
	err := s.withTx(ctx, "delete document", func(tx *sql.Tx) error {
		if by != nil {
			if _, err := roomAdmin(ctx, tx, roomID, by.ProfileID); err != nil {
				return err
			}
		}
		d, err := docTx(ctx, tx, roomID, docID, !deleted)
		if err != nil {
			return err
		}
		at, verb := sql.NullString{String: now(), Valid: deleted}, "deleted"
		if !deleted {
			if err := nameFree(ctx, tx, roomID, docID, d.Name); err != nil {
				return err
			}
			verb = "restored"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET deleted_at = ? WHERE id = ?`, at, docID); err != nil {
			return fmt.Errorf("%s document: %w", verb, err)
		}
		if err := bumpDocs(ctx, tx, roomID, docID); err != nil {
			return err
		}
		m, err := docLine(ctx, tx, roomID, by, verb+" the document `"+d.Name+"`")
		if err != nil {
			return err
		}
		out = &DocumentChange{Document: *d, Line: m}
		return auditTx(ctx, tx, "", map[string]any{"document": docID, "name": d.Name})
	})
	return out, err
}

// DeletedDocuments lists a room's deleted documents, for its admins to restore (005-AC26).
func (s *Store) DeletedDocuments(ctx context.Context, roomID string) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, documentCols+` WHERE d.room_id = ? AND d.deleted_at IS NOT NULL ORDER BY d.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query deleted documents: %w", err)
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

// DocumentRename is a rename a persona's next turn hears of: the document and its names.
type DocumentRename struct {
	DocumentID int64
	From, To   string
	// LineID is the chat line that told the rename.
	LineID int64
}

// DocumentRenames lists the renames of room whose chat line is past after, oldest first.
func (s *Store) DocumentRenames(ctx context.Context, roomID string, after int64) ([]DocumentRename, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.document_id, r.from_name, r.to_name, COALESCE(r.message_id, 0) FROM document_renames r
		JOIN documents d ON d.id = r.document_id WHERE d.room_id = ? AND r.message_id > ? ORDER BY r.id`, roomID, after)
	if err != nil {
		return nil, fmt.Errorf("document renames: %w", err)
	}
	defer rows.Close()
	var out []DocumentRename
	for rows.Next() {
		var r DocumentRename
		if err := rows.Scan(&r.DocumentID, &r.From, &r.To, &r.LineID); err != nil {
			return nil, fmt.Errorf("document renames: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
