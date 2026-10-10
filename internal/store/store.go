// Package store persists rooms, invites, participants and messages in SQLite.
//
// Derived from github.com/babs/airc pkg/store at commit b7afc63.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds
)

// Message kinds. Only text and system are written in Phase 1; voice and agent are reserved.
const (
	KindVoice  = "voice"
	KindText   = "text"
	KindAgent  = "agent"
	KindSystem = "system"
)

// ErrNotFound is returned when a row does not exist, or an invite or participant is no longer valid.
var ErrNotFound = errors.New("not found")

// Room is one grill session space.
type Room struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Invite is a link token granting access to one room. Only its hash is stored.
type Invite struct {
	ID        int64     `json:"id"`
	RoomID    string    `json:"room_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Participant is a guest who joined a room through an invite.
type Participant struct {
	ID       int64     `json:"id"`
	RoomID   string    `json:"room_id"`
	InviteID int64     `json:"-"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
}

// Message is one line in a room.
type Message struct {
	ID     int64     `json:"id"`
	TS     time.Time `json:"ts"`
	RoomID string    `json:"room_id"`
	Kind   string    `json:"kind"`
	// ParticipantID is 0 for messages no participant wrote (system, agent).
	ParticipantID int64  `json:"participant_id,omitempty"`
	Sender        string `json:"sender"`
	Body          string `json:"body"`
}

// Store wraps the SQLite handle.
type Store struct{ db *sql.DB }

// migrations run in order, each once, tracked by PRAGMA user_version. Append only.
// Every table holding room data must reach rooms through NOT NULL, ON DELETE CASCADE foreign keys:
// DeleteRoom is a single DELETE on rooms, and TestEveryTableCascadesFromRooms enforces it.
var migrations = []string{`
CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE rooms (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE invites (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	revoked_at TEXT
);
CREATE INDEX invites_room ON invites(room_id);
CREATE TABLE participants (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	invite_id INTEGER NOT NULL REFERENCES invites(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	joined_at TEXT NOT NULL
);
CREATE INDEX participants_room ON participants(room_id);
CREATE INDEX participants_invite ON participants(invite_id);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	ts TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('voice', 'text', 'agent', 'system')),
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	sender TEXT NOT NULL,
	body TEXT NOT NULL
);
CREATE INDEX messages_room_id ON messages(room_id, id);
`, `
CREATE TABLE uploads (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	filename TEXT NOT NULL,
	content_type TEXT NOT NULL,
	size INTEGER NOT NULL,
	data BLOB NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX uploads_room ON uploads(room_id);
CREATE TABLE documents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('upload', 'agent')),
	name TEXT NOT NULL,
	format TEXT NOT NULL CHECK (format IN ('md', 'html', 'pdf', 'image')),
	upload_id INTEGER REFERENCES uploads(id) ON DELETE CASCADE,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	CHECK ((kind = 'upload') = (upload_id IS NOT NULL))
);
CREATE INDEX documents_room ON documents(room_id, id);
CREATE TABLE revisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	n INTEGER NOT NULL,
	body TEXT NOT NULL,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (document_id, n)
);
`, `
CREATE TABLE annotations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	revision INTEGER NOT NULL,
	anchor TEXT,
	orphaned INTEGER NOT NULL DEFAULT 0 CHECK (orphaned IN (0, 1)),
	status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done', 'reopened')),
	version INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL
);
CREATE INDEX annotations_document ON annotations(document_id);
CREATE INDEX annotations_room ON annotations(room_id);
CREATE TABLE annotation_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	annotation_id INTEGER NOT NULL REFERENCES annotations(id) ON DELETE CASCADE,
	author_kind TEXT NOT NULL CHECK (author_kind IN ('participant', 'agent')),
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	author TEXT NOT NULL,
	body TEXT NOT NULL,
	created_at TEXT NOT NULL,
	CHECK ((author_kind = 'participant') = (participant_id IS NOT NULL))
);
CREATE INDEX annotation_messages_annotation ON annotation_messages(annotation_id, id);
CREATE TABLE attachments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL REFERENCES annotation_messages(id) ON DELETE CASCADE,
	content_type TEXT NOT NULL,
	size INTEGER NOT NULL,
	data BLOB NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX attachments_message ON attachments(message_id);
`, `
CREATE TABLE daemons (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	participant_id INTEGER NOT NULL UNIQUE REFERENCES participants(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);
CREATE INDEX daemons_room ON daemons(room_id);
`}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	// busy_timeout: HTTP handlers write concurrently; WAL keeps readers unblocked.
	// secure_delete: a deleted room's personal data is zeroed on disk, not left in free pages.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serialises writes; SQLite has no row-level locking anyway.
	db.SetMaxOpenConns(1)
	if err := migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for v := version; v < len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
		_, err = tx.ExecContext(ctx, migrations[v])
		if err == nil {
			_, err = tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, v+1))
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Writable reports whether the database accepts a write.
func (s *Store) Writable(ctx context.Context) error {
	return s.SetSetting(ctx, "healthz", now())
}

// Setting reads a key; empty string if absent.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting writes a key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// CreateRoom inserts a room with the given id.
func (s *Store) CreateRoom(ctx context.Context, id, name string) (*Room, error) {
	r := &Room{ID: id, Name: name, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO rooms(id, name, created_at) VALUES(?,?,?)`, r.ID, r.Name, r.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	return r, nil
}

// Room fetches one room.
func (s *Store) Room(ctx context.Context, id string) (*Room, error) {
	var r Room
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at FROM rooms WHERE id = ?`, id).Scan(&r.ID, &r.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", id, err)
	}
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// Rooms lists every room, newest first.
func (s *Store) Rooms(ctx context.Context) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM rooms ORDER BY rowid DESC`)
	if err != nil {
		return nil, fmt.Errorf("query rooms: %w", err)
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		var created string
		if err := rows.Scan(&r.ID, &r.Name, &created); err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoom purges a room and, through ON DELETE CASCADE, every row that belongs to it.
func (s *Store) DeleteRoom(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete room %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete room %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	// The WAL still holds the deleted rows' old pages until a checkpoint rewrites the main file.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint after delete room %s: %w", id, err)
	}
	return nil
}

// CreateInvite stores an invite for room; ErrNotFound if the room does not exist.
func (s *Store) CreateInvite(ctx context.Context, roomID, tokenHash string) (*Invite, error) {
	inv := &Invite{RoomID: roomID, CreatedAt: time.Now().UTC()}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO invites(room_id, token_hash, created_at) SELECT id, ?, ? FROM rooms WHERE id = ?`,
		tokenHash, inv.CreatedAt.Format(time.RFC3339Nano), roomID)
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	if inv.ID, err = res.LastInsertId(); err != nil {
		return nil, fmt.Errorf("create invite id: %w", err)
	}
	return inv, nil
}

// RevokeInvite marks an invite revoked; idempotent. ErrNotFound if it is not in room.
func (s *Store) RevokeInvite(ctx context.Context, roomID string, inviteID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE invites SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND room_id = ?`, now(), inviteID, roomID)
	if err != nil {
		return fmt.Errorf("revoke invite %d: %w", inviteID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("revoke invite %d: %w", inviteID, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Join adds a participant through a valid invite and records a system message announcing them,
// in one transaction. ErrNotFound if the invite is unknown or revoked. The caller broadcasts.
func (s *Store) Join(ctx context.Context, inviteHash, name, tokenHash string) (*Participant, *Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	p := &Participant{Name: name, JoinedAt: time.Now().UTC()}
	err = tx.QueryRowContext(ctx, `SELECT id, room_id FROM invites WHERE token_hash = ? AND revoked_at IS NULL`, inviteHash).Scan(&p.InviteID, &p.RoomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO participants(room_id, invite_id, name, token_hash, joined_at) VALUES(?,?,?,?,?)`,
		p.RoomID, p.InviteID, p.Name, tokenHash, p.JoinedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	if p.ID, err = res.LastInsertId(); err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	m := &Message{RoomID: p.RoomID, Kind: KindSystem, ParticipantID: p.ID, Sender: name, Body: name + " joined"}
	if err := insertMessage(ctx, tx, m); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	return p, m, nil
}

// ParticipantByToken returns the participant holding tokenHash, provided its invite is not revoked.
func (s *Store) ParticipantByToken(ctx context.Context, tokenHash string) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at FROM participants p
		JOIN invites i ON i.id = p.invite_id
		WHERE p.token_hash = ? AND i.revoked_at IS NULL`, tokenHash).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("participant by token: %w", err)
	}
	p.JoinedAt = parseTS(joined)
	return &p, nil
}

// Participants lists a room's participants whose invite is still valid, in join order.
func (s *Store) Participants(ctx context.Context, roomID string) ([]Participant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at FROM participants p
		JOIN invites i ON i.id = p.invite_id
		WHERE p.room_id = ? AND i.revoked_at IS NULL ORDER BY p.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer rows.Close()
	out := []Participant{}
	for rows.Next() {
		var p Participant
		var joined string
		if err := rows.Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		p.JoinedAt = parseTS(joined)
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertMessage stores m and fills its ID and TS.
func (s *Store) InsertMessage(ctx context.Context, m *Message) error {
	return insertMessage(ctx, s.db, m)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertMessage(ctx context.Context, db execer, m *Message) error {
	m.TS = time.Now().UTC()
	var pid any
	if m.ParticipantID != 0 {
		pid = m.ParticipantID
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO messages(room_id, ts, kind, participant_id, sender, body) VALUES(?,?,?,?,?,?)`,
		m.RoomID, m.TS.Format(time.RFC3339Nano), m.Kind, pid, m.Sender, m.Body)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("insert message id: %w", err)
	}
	return nil
}

// Messages returns up to limit messages of room with id > afterID, oldest first.
func (s *Store) Messages(ctx context.Context, roomID string, afterID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, room_id, kind, COALESCE(participant_id, 0), sender, body FROM messages
		WHERE room_id = ? AND id > ? ORDER BY id ASC LIMIT ?`, roomID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts string
		if err := rows.Scan(&m.ID, &ts, &m.RoomID, &m.Kind, &m.ParticipantID, &m.Sender, &m.Body); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.TS = parseTS(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
