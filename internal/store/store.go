// Package store persists rooms, invites, participants and messages in SQLite.
//
// Derived from github.com/babs/airc pkg/store at commit b7afc63.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds

	"github.com/babs/grill-room/internal/render"
)

// Message kinds. The STT worker writes voice lines; agent is reserved for the agent turn.
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
	// HostParticipantID is the participant whose daemon runs the agent; 0 before any daemon hosted.
	HostParticipantID int64 `json:"host_participant_id,omitempty"`
	Chain             Chain `json:"chain"`
}

// Invite is a link token granting access to one room. Only its hash is stored.
type Invite struct {
	ID        int64     `json:"id"`
	RoomID    string    `json:"room_id"`
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy is the participant who created it; 0 for the admin.
	CreatedBy int64      `json:"created_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Participant is a guest who joined a room through an invite.
type Participant struct {
	ID       int64     `json:"id"`
	RoomID   string    `json:"room_id"`
	InviteID int64     `json:"-"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
	// CanTrigger is the agent button right the host granted; the host holds it anyway.
	CanTrigger bool `json:"can_trigger"`
	// ProfileID links the participant across rooms; never sent, as it would let others do the same.
	ProfileID int64 `json:"-"`
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
	// HTML is Body rendered by render.MessageHTML when written; empty for system lines, and for
	// lines written before rendering existed.
	HTML string `json:"html,omitempty"`
	// PersonaID is the room persona that wrote an agent synthesis; Badge and Colour are its current
	// ones, empty once it is removed.
	PersonaID int64  `json:"persona_id,omitempty"`
	Badge     string `json:"badge,omitempty"`
	Colour    string `json:"colour,omitempty"`
	// Model and Effort are what an agent synthesis ran with: the model as claude reported it.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// ReqModel and ReqEffort are what the persona asked for, set only when the host cap lowered
	// them (002-AC19).
	ReqModel  string     `json:"requested_model,omitempty"`
	ReqEffort string     `json:"requested_effort,omitempty"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// ChangeSeq orders the room's edits and deletes: each one sets it past every other in the room.
	ChangeSeq int64 `json:"change_seq,omitempty"`
}

// Store wraps the SQLite handle.
type Store struct{ db *sql.DB }

// migrations run in order, each once, tracked by PRAGMA user_version. Append only.
// Every table must reach rooms or profiles through NOT NULL, ON DELETE CASCADE foreign keys, and
// any other reference to profiles must be ON DELETE SET NULL: DeleteRoom and DeleteProfile are
// single DELETEs, and TestEveryTableCascadesFromAnOwner enforces it.
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
`, `
ALTER TABLE rooms ADD COLUMN host_participant_id INTEGER REFERENCES participants(id) ON DELETE SET NULL;
ALTER TABLE participants ADD COLUMN can_trigger INTEGER NOT NULL DEFAULT 0 CHECK (can_trigger IN (0, 1));
CREATE UNIQUE INDEX documents_agent_name ON documents(room_id, name) WHERE kind = 'agent';
CREATE TABLE turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	daemon_id INTEGER REFERENCES daemons(id) ON DELETE SET NULL,
	host TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('running', 'done', 'failed')),
	fresh INTEGER NOT NULL CHECK (fresh IN (0, 1)),
	resume TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	message_cursor INTEGER NOT NULL,
	annotation_cursor INTEGER NOT NULL,
	prompt TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	report TEXT NOT NULL DEFAULT '{}',
	started_at TEXT NOT NULL,
	ended_at TEXT
);
CREATE INDEX turns_room ON turns(room_id, id);
CREATE UNIQUE INDEX turns_one_running ON turns(room_id) WHERE status = 'running';
CREATE TABLE agent_sessions (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL,
	data BLOB NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (room_id, session_id)
);
`, `
ALTER TABLE turns ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN model TEXT NOT NULL DEFAULT '';
`, `
CREATE TABLE profiles (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	recovery_hash TEXT NOT NULL UNIQUE,
	email TEXT UNIQUE,
	created_at TEXT NOT NULL
);
CREATE TABLE profile_sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at_s INTEGER NOT NULL
);
CREATE INDEX profile_sessions_profile ON profile_sessions(profile_id);
CREATE TABLE magic_links (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	email TEXT NOT NULL,
	purpose TEXT NOT NULL CHECK (purpose IN ('bind', 'signin')),
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at_ms INTEGER NOT NULL,
	used_at TEXT
);
CREATE INDEX magic_links_profile ON magic_links(profile_id);
ALTER TABLE participants ADD COLUMN profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL;
CREATE INDEX participants_profile ON participants(profile_id);
ALTER TABLE invites ADD COLUMN created_by INTEGER REFERENCES participants(id) ON DELETE SET NULL;
`, `
ALTER TABLE messages ADD COLUMN html TEXT;
ALTER TABLE messages ADD COLUMN edited_at TEXT;
ALTER TABLE messages ADD COLUMN deleted_at TEXT;
ALTER TABLE messages ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
CREATE INDEX messages_change ON messages(room_id, change_seq);
CREATE TABLE message_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	body TEXT NOT NULL,
	written_at TEXT NOT NULL
);
CREATE INDEX message_versions_message ON message_versions(message_id, seq);
ALTER TABLE turns ADD COLUMN change_cursor INTEGER NOT NULL DEFAULT 0;
`, `
CREATE TABLE persona_templates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	preset TEXT UNIQUE,
	profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	created_at TEXT NOT NULL,
	CHECK ((preset IS NULL) = (profile_id IS NOT NULL))
);
CREATE INDEX persona_templates_profile ON persona_templates(profile_id);
CREATE TABLE persona_template_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	template_id INTEGER NOT NULL REFERENCES persona_templates(id) ON DELETE CASCADE,
	n INTEGER NOT NULL,
	author TEXT NOT NULL,
	name TEXT NOT NULL,
	badge TEXT NOT NULL,
	colour TEXT NOT NULL,
	text TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (template_id, n)
);
CREATE TABLE room_personas (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	template_id INTEGER REFERENCES persona_templates(id) ON DELETE SET NULL,
	template_version INTEGER NOT NULL,
	name TEXT NOT NULL,
	badge TEXT NOT NULL,
	colour TEXT NOT NULL,
	text TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	every_n INTEGER CHECK (every_n IS NULL OR every_n > 0),
	self_call INTEGER NOT NULL DEFAULT 0 CHECK (self_call IN (0, 1)),
	created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX room_personas_name ON room_personas(room_id, name COLLATE NOCASE);
CREATE INDEX room_personas_template ON room_personas(template_id);
CREATE TABLE turn_queue (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL UNIQUE REFERENCES room_personas(id) ON DELETE CASCADE,
	participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
	fresh INTEGER NOT NULL CHECK (fresh IN (0, 1)),
	created_at TEXT NOT NULL
);
CREATE INDEX turn_queue_room ON turn_queue(room_id, id);
CREATE INDEX turn_queue_participant ON turn_queue(participant_id);
ALTER TABLE turns ADD COLUMN persona_id INTEGER REFERENCES room_personas(id) ON DELETE SET NULL;
ALTER TABLE turns ADD COLUMN persona_name TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN persona_text TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN req_model TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN req_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN effort TEXT NOT NULL DEFAULT '';
CREATE INDEX turns_persona ON turns(persona_id, id);
ALTER TABLE messages ADD COLUMN persona_id INTEGER REFERENCES room_personas(id) ON DELETE SET NULL;
ALTER TABLE messages ADD COLUMN effort TEXT NOT NULL DEFAULT '';
ALTER TABLE annotation_messages ADD COLUMN persona_id INTEGER REFERENCES room_personas(id) ON DELETE SET NULL;
CREATE TABLE agent_sessions_by_persona (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL,
	data BLOB NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (persona_id, session_id)
);
CREATE INDEX agent_sessions_room ON agent_sessions_by_persona(room_id);
`, `
ALTER TABLE daemons ADD COLUMN max_model TEXT NOT NULL DEFAULT '';
ALTER TABLE daemons ADD COLUMN max_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE daemons ADD COLUMN approve_all INTEGER NOT NULL DEFAULT 0 CHECK (approve_all IN (0, 1));
ALTER TABLE room_personas ADD COLUMN pending INTEGER NOT NULL DEFAULT 0 CHECK (pending IN (0, 1));
ALTER TABLE room_personas ADD COLUMN cap_decision TEXT NOT NULL DEFAULT '' CHECK (cap_decision IN ('', 'approved', 'refused'));
ALTER TABLE room_personas ADD COLUMN cap_daemon_id INTEGER REFERENCES daemons(id) ON DELETE SET NULL;
ALTER TABLE room_personas ADD COLUMN cap_model TEXT NOT NULL DEFAULT '';
ALTER TABLE room_personas ADD COLUMN cap_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN req_model TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN req_effort TEXT NOT NULL DEFAULT '';
CREATE TABLE persona_change_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL UNIQUE REFERENCES room_personas(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('add', 'change')),
	patch TEXT NOT NULL,
	requested_by TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX persona_change_requests_room ON persona_change_requests(room_id);
`, `
ALTER TABLE rooms ADD COLUMN chain_budget INTEGER NOT NULL DEFAULT 25 CHECK (chain_budget > 0);
ALTER TABLE rooms ADD COLUMN chain_count INTEGER NOT NULL DEFAULT 0 CHECK (chain_count >= 0);
ALTER TABLE rooms ADD COLUMN chain_suspended INTEGER NOT NULL DEFAULT 0 CHECK (chain_suspended IN (0, 1));
CREATE TABLE turn_queue_v2 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL UNIQUE REFERENCES room_personas(id) ON DELETE CASCADE,
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	source TEXT NOT NULL CHECK (source IN ('button', 'mention', 'every_n', 'call')),
	fresh INTEGER NOT NULL CHECK (fresh IN (0, 1)),
	created_at TEXT NOT NULL,
	CHECK ((participant_id IS NULL) = (source IN ('every_n', 'call')))
);
INSERT INTO turn_queue_v2(id, room_id, persona_id, participant_id, source, fresh, created_at)
	SELECT id, room_id, persona_id, participant_id, 'button', fresh, created_at FROM turn_queue;
DROP TABLE turn_queue;
ALTER TABLE turn_queue_v2 RENAME TO turn_queue;
CREATE INDEX turn_queue_room ON turn_queue(room_id, id);
CREATE INDEX turn_queue_participant ON turn_queue(participant_id);
`, `
CREATE TABLE skills (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE,
	publisher_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL,
	CHECK (profile_id IS NULL OR publisher_id IS NULL)
);
CREATE UNIQUE INDEX skills_shared_name ON skills(name) WHERE profile_id IS NULL;
CREATE UNIQUE INDEX skills_personal_name ON skills(profile_id, name) WHERE profile_id IS NOT NULL;
CREATE INDEX skills_publisher ON skills(publisher_id);
CREATE TABLE skill_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	skill_id INTEGER NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
	n INTEGER NOT NULL,
	author TEXT NOT NULL,
	author_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL,
	description TEXT NOT NULL,
	files INTEGER NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	data BLOB NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (skill_id, n)
);
CREATE INDEX skill_versions_author ON skill_versions(author_id);
CREATE TABLE room_persona_skills (
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	skill_id INTEGER NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	PRIMARY KEY (persona_id, skill_id)
);
CREATE INDEX room_persona_skills_skill ON room_persona_skills(skill_id);
ALTER TABLE profiles ADD COLUMN skills_disclaimer_at TEXT;
ALTER TABLE turns ADD COLUMN skills TEXT NOT NULL DEFAULT '[]';
`}

// migrationData runs Go code inside the transaction of the migration to version key, after its SQL.
var migrationData = map[int]func(context.Context, *sql.Tx) error{9: migratePersonas}

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
	// A preset whose shipped text changed gets a new version: rooms are offered it (002-AC14).
	if err := ensurePresets(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error { return migrateTo(ctx, db, len(migrations)) }

// migrateTo runs the migrations up to version target; tests build older schemas with it.
func migrateTo(ctx context.Context, db *sql.DB, target int) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for v := version; v < target; v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
		_, err = tx.ExecContext(ctx, migrations[v])
		if f := migrationData[v+1]; err == nil && f != nil {
			err = f(ctx, tx)
		}
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

// CreateRoom inserts a room with the given id, and the Default template's persona (002-AC42).
func (s *Store) CreateRoom(ctx context.Context, id, name string) (*Room, error) {
	r := &Room{ID: id, Name: name, CreatedAt: time.Now().UTC(), Chain: Chain{Budget: DefaultChainBudget}}
	err := s.withTx(ctx, "create room", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(id, name, created_at) VALUES(?,?,?)`, r.ID, r.Name, r.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("create room: %w", err)
		}
		def, err := presetID(ctx, tx, PresetDefault)
		if err != nil {
			return err
		}
		_, _, err = addPersona(ctx, tx, r.ID, def)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Room fetches one room.
func (s *Store) Room(ctx context.Context, id string) (*Room, error) {
	var r Room
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at, COALESCE(host_participant_id, 0), chain_budget, chain_count, chain_suspended
		FROM rooms WHERE id = ?`, id).Scan(&r.ID, &r.Name, &created, &r.HostParticipantID, &r.Chain.Budget, &r.Chain.Count, &r.Chain.Suspended)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, chain_budget, chain_count, chain_suspended FROM rooms ORDER BY rowid DESC`)
	if err != nil {
		return nil, fmt.Errorf("query rooms: %w", err)
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		var created string
		if err := rows.Scan(&r.ID, &r.Name, &created, &r.Chain.Budget, &r.Chain.Count, &r.Chain.Suspended); err != nil {
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

// RenameRoom renames room and records who did it in a system message, in one transaction. by is
// the participant renaming it, nil for the admin. Renaming to the current name changes nothing and
// returns a nil message. ErrNotFound if the room does not exist. The caller broadcasts.
func (s *Store) RenameRoom(ctx context.Context, roomID, name string, by *Participant) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("rename room: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var old string
	err = tx.QueryRowContext(ctx, `SELECT name FROM rooms WHERE id = ?`, roomID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rename room: %w", err)
	}
	if old == name {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET name = ? WHERE id = ?`, name, roomID); err != nil {
		return nil, fmt.Errorf("rename room: %w", err)
	}
	m := &Message{RoomID: roomID, Kind: KindSystem, Sender: "admin", Body: fmt.Sprintf("The admin renamed the room from %q to %q", old, name)}
	if by != nil {
		m.ParticipantID, m.Sender = by.ID, by.Name
		m.Body = fmt.Sprintf("%s renamed the room from %q to %q", by.Name, old, name)
	}
	if err := insertMessage(ctx, tx, m); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rename room: %w", err)
	}
	return m, nil
}

// CreateInvite stores an invite for room, created by participant createdBy or, when 0, by the
// admin. ErrNotFound if the room does not exist, or createdBy is not in it with a valid invite: the
// check and the insert are one statement, so a revoke cannot slip between them.
func (s *Store) CreateInvite(ctx context.Context, roomID, tokenHash string, createdBy int64) (*Invite, error) {
	inv := &Invite{RoomID: roomID, CreatedAt: time.Now().UTC(), CreatedBy: createdBy}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO invites(room_id, token_hash, created_at, created_by) SELECT id, ?, ?, ? FROM rooms r WHERE r.id = ?
		AND (? = 0 OR EXISTS (SELECT 1 FROM participants p JOIN invites i ON i.id = p.invite_id
			WHERE p.id = ? AND p.room_id = r.id AND i.revoked_at IS NULL))`,
		tokenHash, inv.CreatedAt.Format(time.RFC3339Nano), nullID(createdBy), roomID, createdBy, createdBy)
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

// Invite returns invite id of room; ErrNotFound if room has none.
func (s *Store) Invite(ctx context.Context, roomID string, id int64) (*Invite, error) {
	invs, err := s.invites(ctx, roomID, id, 0)
	if err != nil {
		return nil, err
	}
	if len(invs) == 0 {
		return nil, ErrNotFound
	}
	return &invs[0], nil
}

// Invites lists room's invites, revoked ones included, oldest first: those participant createdBy
// created, or all of them when createdBy is 0.
func (s *Store) Invites(ctx context.Context, roomID string, createdBy int64) ([]Invite, error) {
	return s.invites(ctx, roomID, 0, createdBy)
}

// invites lists room's invites, narrowed to one id and to one creator when those are not 0.
func (s *Store) invites(ctx context.Context, roomID string, id, createdBy int64) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, room_id, created_at, COALESCE(created_by, 0), revoked_at FROM invites
		WHERE room_id = ? AND (? = 0 OR id = ?) AND (? = 0 OR created_by = ?) ORDER BY id`, roomID, id, id, createdBy, createdBy)
	if err != nil {
		return nil, fmt.Errorf("query invites: %w", err)
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var inv Invite
		var created string
		var revoked sql.NullString
		if err := rows.Scan(&inv.ID, &inv.RoomID, &created, &inv.CreatedBy, &revoked); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		inv.CreatedAt = parseTS(created)
		if revoked.Valid {
			t := parseTS(revoked.String)
			inv.RevokedAt = &t
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite marks an invite revoked, with every invite created by someone who joined through a
// revoked one: a member cut off must not walk back in through an invite they made. Idempotent.
// Returns the ids of the invites it covers. ErrNotFound if inviteID is not in room.
func (s *Store) RevokeInvite(ctx context.Context, roomID string, inviteID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE gone(id) AS (
			SELECT id FROM invites WHERE id = ? AND room_id = ?
			UNION
			SELECT i.id FROM invites i JOIN participants p ON p.id = i.created_by JOIN gone g ON g.id = p.invite_id
		)
		UPDATE invites SET revoked_at = COALESCE(revoked_at, ?) WHERE id IN gone RETURNING id`, inviteID, roomID, now())
	if err != nil {
		return nil, fmt.Errorf("revoke invite %d: %w", inviteID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("revoke invite %d: %w", inviteID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("revoke invite %d: %w", inviteID, err)
	}
	if len(ids) == 0 {
		return nil, ErrNotFound
	}
	slices.Sort(ids)
	return ids, nil
}

// JoinProfile says which profile a join attaches the participant to. ID names an existing one.
// With ID 0, RecoveryHash and SessionHash create one; all empty joins without a profile.
type JoinProfile struct {
	ID                        int64
	RecoveryHash, SessionHash string
}

// Join adds a participant through a valid invite and records a system message announcing them,
// in one transaction, creating its profile when prof asks. ErrNotFound if the invite is unknown or
// revoked. The caller broadcasts.
func (s *Store) Join(ctx context.Context, inviteHash, name, tokenHash string, prof JoinProfile) (*Participant, *Message, error) {
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
	p.ProfileID = prof.ID
	if p.ProfileID == 0 && prof.RecoveryHash != "" {
		if p.ProfileID, err = createProfile(ctx, tx, prof.RecoveryHash, prof.SessionHash); err != nil {
			return nil, nil, fmt.Errorf("join: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO participants(room_id, invite_id, name, token_hash, joined_at, profile_id) VALUES(?,?,?,?,?,?)`,
		p.RoomID, p.InviteID, p.Name, tokenHash, p.JoinedAt.Format(time.RFC3339Nano), nullID(p.ProfileID))
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
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger, COALESCE(p.profile_id, 0) FROM participants p
		JOIN invites i ON i.id = p.invite_id
		WHERE p.token_hash = ? AND i.revoked_at IS NULL`, tokenHash).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.ProfileID)
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
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger FROM participants p
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
		if err := rows.Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger); err != nil {
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

// nullStr stores "" as NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullID stores 0 as NULL, for optional foreign keys.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func insertMessage(ctx context.Context, db execer, m *Message) error {
	m.TS = time.Now().UTC()
	pid := nullID(m.ParticipantID)
	// Rendered once here, not on every stream replay: a crafted 4000-character line takes ~10 ms.
	if m.Kind != KindSystem {
		m.HTML = render.MessageHTML(m.Body)
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO messages(room_id, ts, kind, participant_id, sender, body, html, model, effort, persona_id, req_model, req_effort) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.RoomID, m.TS.Format(time.RFC3339Nano), m.Kind, pid, m.Sender, m.Body, nullStr(m.HTML), m.Model, m.Effort, nullID(m.PersonaID), m.ReqModel, m.ReqEffort)
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
	return s.queryMessages(ctx, `WHERE room_id = ? AND id > ? ORDER BY id ASC LIMIT ?`, roomID, afterID, limit)
}

// LastVoice returns the room's latest voice line still shown; ErrNotFound if there is none.
func (s *Store) LastVoice(ctx context.Context, roomID string) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, messageCols+`WHERE room_id = ? AND kind = 'voice' AND deleted_at IS NULL ORDER BY id DESC LIMIT 1`, roomID))
}

const (
	messageFields = `id, ts, room_id, kind, COALESCE(participant_id, 0), sender, body, COALESCE(html, ''), model, edited_at, deleted_at, change_seq,
		COALESCE(persona_id, 0), effort, COALESCE((SELECT badge FROM room_personas WHERE id = messages.persona_id), ''),
		COALESCE((SELECT colour FROM room_personas WHERE id = messages.persona_id), ''), req_model, req_effort`
	messageCols = `SELECT ` + messageFields + ` FROM messages `
)

func (s *Store) queryMessages(ctx context.Context, where string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, messageCols+where, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func scanMessage(sc scanner, extra ...any) (*Message, error) {
	var m Message
	var ts string
	var edited, deleted sql.NullString
	err := sc.Scan(append([]any{&m.ID, &ts, &m.RoomID, &m.Kind, &m.ParticipantID, &m.Sender, &m.Body, &m.HTML, &m.Model, &edited, &deleted, &m.ChangeSeq,
		&m.PersonaID, &m.Effort, &m.Badge, &m.Colour, &m.ReqModel, &m.ReqEffort}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	m.TS = parseTS(ts)
	m.EditedAt, m.DeletedAt = parseNullTS(edited), parseNullTS(deleted)
	return &m, nil
}

func parseNullTS(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// RoomParticipant returns participant id of room, provided its invite is not revoked.
func (s *Store) RoomParticipant(ctx context.Context, roomID string, id int64) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger FROM participants p
		JOIN invites i ON i.id = p.invite_id
		WHERE p.id = ? AND p.room_id = ? AND i.revoked_at IS NULL`, id, roomID).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room participant %d: %w", id, err)
	}
	p.JoinedAt = parseTS(joined)
	return &p, nil
}

// InviteParticipantIDs lists the participants who joined room through invite, revoked or not.
func (s *Store) InviteParticipantIDs(ctx context.Context, roomID string, inviteID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM participants WHERE room_id = ? AND invite_id = ? ORDER BY id`, roomID, inviteID)
	if err != nil {
		return nil, fmt.Errorf("invite participants: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan invite participant: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
