// Package store persists rooms, invites, participants and messages in PostgreSQL.
//
// Derived from github.com/babs/airc pkg/store at commit b7afc63.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // pure-Go driver: keeps CGO_ENABLED=0 builds

	"github.com/babs/grill-room/internal/render"
)

// Message kinds. The STT worker and device transcription write voice lines; agent is reserved for
// the agent turn.
const (
	KindVoice  = "voice"
	KindText   = "text"
	KindAgent  = "agent"
	KindSystem = "system"
)

// Voice line sources: the participant's own browser, or the server's STT worker (010-AC10).
const (
	VoiceSourceDevice = "device"
	VoiceSourceServer = "server"
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
	// ParallelTurns lets two personas of the room run at once, each in its own sandbox; the host
	// switches it (003-AC20).
	ParallelTurns bool `json:"parallel_turns"`
	// CreatedFrom is the starter version the room was created from, nil for a blank room (006-AC11).
	CreatedFrom *Provenance `json:"created_from,omitempty"`
	// EgressAdmins lets the room's admins allow websites for the room; the AI host switches it
	// (014-AC12).
	EgressAdmins bool `json:"egress_admins_allow"`
	// Speech is how personas take turns in the call (013-AC25).
	Speech Speech `json:"speech"`
	// HostMovedLine is the chat line of the latest move of the AI host role by someone other than
	// the previous host, "" since the host moved on its own (009-AC34). Only that host's page shows it.
	HostMovedLine string `json:"-"`
}

// Speech is a room's call settings for its personas' voices (013-AC25): the quiet before a persona
// starts, the window to resume an interrupted line, the most a line waits, and its words.
type Speech struct {
	QuietMS int `json:"quiet_ms"`
	ResumeS int `json:"resume_s"`
	QueueS  int `json:"queue_s"`
	Words   int `json:"words"`
}

// SpeechLimits are the bounds of each Speech field, inclusive; DefaultSpeech its defaults.
var (
	SpeechMin     = Speech{QuietMS: 500, ResumeS: 0, QueueS: 10, Words: 40}
	SpeechMax     = Speech{QuietMS: 5000, ResumeS: 60, QueueS: 300, Words: 60}
	DefaultSpeech = Speech{QuietMS: 1000, ResumeS: 20, QueueS: 40, Words: 60}
)

// Valid reports s within SpeechMin and SpeechMax.
func (s Speech) Valid() bool {
	in := func(v, lo, hi int) bool { return v >= lo && v <= hi }
	return in(s.QuietMS, SpeechMin.QuietMS, SpeechMax.QuietMS) && in(s.ResumeS, SpeechMin.ResumeS, SpeechMax.ResumeS) &&
		in(s.QueueS, SpeechMin.QueueS, SpeechMax.QueueS) && in(s.Words, SpeechMin.Words, SpeechMax.Words)
}

// Invite is a link token granting access to one room. Only its hash is stored.
type Invite struct {
	ID        int64     `json:"id"`
	RoomID    string    `json:"room_id"`
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy is the participant who created it; 0 for the admin.
	CreatedBy int64      `json:"created_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// Shareable marks its creator's shareable link (011-AC26); the token itself never leaves here.
	Shareable bool `json:"shareable"`
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
	// ProfileUUID is the profile's uuid, empty without one: the presence preamble shows its start
	// (003-AC32). Never sent, like ProfileID.
	ProfileUUID string `json:"-"`
	// Admin tells that its profile administers the room (004-AC8). Set by Participants only.
	Admin bool `json:"admin"`
	// LastSeen is when the participant was last active (005-AC7). Set by Participants only.
	LastSeen *time.Time `json:"last_seen_at,omitempty"`
	// PreviousName is the name before the latest rename (009-AC36). Set by Participants only.
	PreviousName string `json:"previous_name,omitempty"`
	// Self marks the caller's own seats in the room answer, so no profile id leaves (011-AC26).
	Self bool `json:"self,omitempty"`
	// NoProfile marks a seat without a profile, which a block cannot keep out (016-AC6).
	NoProfile bool `json:"no_profile,omitempty"`
	// STTOff is set when they turned off the transcription of their voice (010-AC23). Set by
	// ParticipantByToken and RoomParticipant only.
	STTOff bool `json:"stt_off,omitempty"`
	// OIDCBinding is its profile's, set by ParticipantByToken, ProfileParticipant and
	// APITokenParticipant only (021-AC8).
	OIDCBinding
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
	// Source is who transcribed a voice line, VoiceSourceDevice or VoiceSourceServer; empty on other kinds
	// (010-AC10).
	Source string `json:"source,omitempty"`
	// HTML is Body rendered by render.MessageHTML when written; empty for system lines, and for
	// lines written before rendering existed.
	HTML string `json:"html,omitempty"`
	// PersonaID is the room persona that wrote an agent synthesis; Badge and Colour are its current
	// ones, empty once it is removed.
	PersonaID int64  `json:"persona_id,omitempty"`
	Badge     string `json:"badge,omitempty"`
	Colour    string `json:"colour,omitempty"`
	// Model and Effort are what an agent synthesis ran with. Since 005-AC10 the model is the one
	// asked for, checked against claude's report, ModelNote and ModelMismatch as in turn.Result;
	// before, the one claude reported.
	Model         string `json:"model,omitempty"`
	ModelNote     string `json:"model_note,omitempty"`
	ModelMismatch bool   `json:"model_mismatch,omitempty"`
	Effort        string `json:"effort,omitempty"`
	// ReqModel and ReqEffort are what the persona asked for, set only when the host cap lowered
	// them (002-AC19).
	ReqModel  string `json:"requested_model,omitempty"`
	ReqEffort string `json:"requested_effort,omitempty"`
	// Spoken is what an agent synthesis's turn wrote to say in the call, sanitised: the chat shows it
	// to those not in the call (013-AC40).
	Spoken    string     `json:"spoken,omitempty"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// AppendedAt is when a voice paragraph last grew by a line of its speaker (018-AC5).
	AppendedAt *time.Time `json:"appended_at,omitempty"`
	// ChangeSeq orders the room's edits and deletes: each one sets it past every other in the room.
	ChangeSeq int64 `json:"change_seq,omitempty"`
	// TurnID is the turn whose synthesis, failure or stop the message tells (005-AC4).
	TurnID int64 `json:"-"`
	// Named are the personas and members the line names with @name, as the server's rule reads it:
	// the page draws them as chips (005-AC27).
	Named []Named `json:"named,omitempty"`
	// Mentions are where the turns this line's @mentions queued stand, as the event stream's
	// replay sends them: the chips stay with a line however old (005-AC4).
	Mentions []MentionState `json:"mentions,omitempty"`
	// Blocked is set on the system line of a website a turn was refused (014-AC8).
	Blocked *Blocked `json:"blocked,omitempty"`
}

// Store wraps the PostgreSQL pool.
type Store struct{ db *database }

//go:embed schema.sql
var baseline string

// baselineVersion is the SQLite schema version schema.sql reproduces; migrations[i] moves the
// schema to baselineVersion+1+i.
// Every table must reach rooms or profiles through NOT NULL, ON DELETE CASCADE foreign keys, and
// any other reference to profiles must be ON DELETE SET NULL: DeleteRoom and DeleteProfile are
// single DELETEs, and TestEveryTableCascadesFromAnOwner enforces it. Server-wide tables, listed in
// that test, are the exceptions. Append only.
const baselineVersion = 46

var migrations = []string{
	// 47 (017 Phase 3): the state pods share. UNLOGGED tables hold what lives seconds to minutes,
	// out of WAL and backups; tts_lines and rate_events take ids, so they stay logged.
	`CREATE UNLOGGED TABLE daemon_connections (
	id TEXT PRIMARY KEY,
	daemon_id BIGINT NOT NULL REFERENCES daemons(id) ON DELETE CASCADE,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	pod TEXT NOT NULL,
	connected_at_ms BIGINT NOT NULL,
	expires_at_ms BIGINT NOT NULL);
CREATE INDEX daemon_connections_room ON daemon_connections(room_id);
CREATE INDEX daemon_connections_daemon ON daemon_connections(daemon_id);
CREATE UNLOGGED TABLE daemon_status (
	daemon_id BIGINT PRIMARY KEY REFERENCES daemons(id) ON DELETE CASCADE,
	can_host SMALLINT NOT NULL DEFAULT 0 CHECK (can_host IN (0, 1)),
	reason TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	egress SMALLINT NOT NULL DEFAULT 0 CHECK (egress IN (0, 1)),
	connectors TEXT NOT NULL DEFAULT '[]');
CREATE UNLOGGED TABLE presence (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	participant_id BIGINT NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
	tab TEXT NOT NULL,
	since_at_ms BIGINT NOT NULL DEFAULT 0,
	beat_at_ms BIGINT NOT NULL DEFAULT 0,
	device_at_ms BIGINT NOT NULL DEFAULT 0,
	audio SMALLINT NOT NULL DEFAULT 0 CHECK (audio IN (0, 1)),
	mic SMALLINT NOT NULL DEFAULT 0 CHECK (mic IN (0, 1)),
	muted SMALLINT NOT NULL DEFAULT 0 CHECK (muted IN (0, 1)),
	deaf SMALLINT NOT NULL DEFAULT 0 CHECK (deaf IN (0, 1)),
	focused SMALLINT NOT NULL DEFAULT 0 CHECK (focused IN (0, 1)),
	expires_at_ms BIGINT NOT NULL,
	PRIMARY KEY (room_id, participant_id, tab));
CREATE INDEX presence_expires ON presence(expires_at_ms);
CREATE UNLOGGED TABLE stt_workers (
	worker TEXT PRIMARY KEY,
	rooms TEXT NOT NULL,
	voices TEXT NOT NULL,
	engine TEXT NOT NULL,
	glossary SMALLINT NOT NULL CHECK (glossary IN (0, 1)),
	expires_at_ms BIGINT NOT NULL);
CREATE UNLOGGED TABLE tts_pod (
	id SMALLINT PRIMARY KEY CHECK (id = 1),
	pod TEXT NOT NULL,
	rooms TEXT NOT NULL,
	bots TEXT NOT NULL,
	states TEXT NOT NULL,
	beat_at_ms BIGINT NOT NULL,
	stream_until_ms BIGINT NOT NULL DEFAULT 0);
CREATE UNLOGGED TABLE pairings (
	id TEXT PRIMARY KEY,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	code TEXT NOT NULL,
	secret_hash TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied')),
	approved_by BIGINT REFERENCES participants(id) ON DELETE CASCADE,
	expires_at_ms BIGINT NOT NULL);
CREATE INDEX pairings_room ON pairings(room_id);
CREATE TABLE tts_lines (
	seq BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id BIGINT NOT NULL,
	persona TEXT NOT NULL,
	voice TEXT NOT NULL,
	rate DOUBLE PRECISION NOT NULL,
	text TEXT NOT NULL,
	quiet_ms BIGINT NOT NULL,
	resume_s BIGINT NOT NULL,
	queue_s BIGINT NOT NULL,
	queued_at_ms BIGINT NOT NULL,
	sent SMALLINT NOT NULL DEFAULT 0 CHECK (sent IN (0, 1)));
-- Above the clock-based seqs a TTS pod may resume from with Last-Event-ID.
SELECT setval(pg_get_serial_sequence('tts_lines', 'seq'), (extract(epoch FROM clock_timestamp()) * 1e6)::bigint);
CREATE TABLE rate_events (
	id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
	scope TEXT NOT NULL CHECK (scope IN ('magic', 'login')),
	key_hash TEXT NOT NULL,
	at_ms BIGINT NOT NULL);
CREATE INDEX rate_events_key ON rate_events(scope, key_hash, at_ms);
ALTER TABLE rooms ADD COLUMN host_moved_line TEXT NOT NULL DEFAULT '';
INSERT INTO settings(key, value) VALUES ('lexicon_version', '0') ON CONFLICT DO NOTHING;`,
	// 48 (017 Phase 4): a turn a stream wrote to its daemon. A daemon drops its running turns with
	// its stream, so its next stream fails the delivered ones and announces the others. A turn
	// running at the upgrade counts as delivered: its daemon's next stream fails it, as before.
	`ALTER TABLE turns ADD COLUMN delivered SMALLINT NOT NULL DEFAULT 0 CHECK (delivered IN (0, 1));
UPDATE turns SET delivered = 1 WHERE status = 'running';`,
	// 49 (013-AC40): the spoken text of an agent synthesis, shown under it in the chat.
	`ALTER TABLE messages ADD COLUMN spoken TEXT NOT NULL DEFAULT '';`,
	// 50 (010-AC23): a participant who turned off the transcription of their own voice.
	`ALTER TABLE participants ADD COLUMN stt_off SMALLINT NOT NULL DEFAULT 0 CHECK (stt_off IN (0, 1));
CREATE INDEX participants_stt_off ON participants(room_id) WHERE stt_off = 1;`,
	// 51 (018-AC5): the last time a voice paragraph grew by a line.
	`ALTER TABLE messages ADD COLUMN appended_at TEXT;`,
	// 52 (020-AC1, 020-AC17): the latest non-typed message of a room or a seat, without walking past
	// typed ones.
	`CREATE INDEX messages_room_nontext ON messages(room_id, id) WHERE ` + nonText + `;
CREATE INDEX messages_participant_nontext ON messages(participant_id, id) WHERE ` + nonText + `;`,
	// 53 (019-AC4): the anonymous room creations, counted per client IP.
	`ALTER TABLE rate_events DROP CONSTRAINT rate_events_scope_check;
ALTER TABLE rate_events ADD CONSTRAINT rate_events_scope_check CHECK (scope IN ('magic', 'login', 'anon-room'));`,
	// 54 (021 Phase 1): a profile's provider binding; rerunnable, as older migrations' tests replay it.
	`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS oidc_issuer TEXT, ADD COLUMN IF NOT EXISTS oidc_subject TEXT, ADD COLUMN IF NOT EXISTS oidc_signed_in_at TEXT;
ALTER TABLE profiles DROP CONSTRAINT IF EXISTS profiles_oidc_unique;
ALTER TABLE profiles ADD CONSTRAINT profiles_oidc_unique UNIQUE (oidc_issuer, oidc_subject);`,
	// 55 (021-AC11): the admin group a profile's last sign-in held, and the admin sessions it opened
	// with it, which go with the profile.
	`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS oidc_admin_group TEXT;
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS profile_id BIGINT REFERENCES profiles(id) ON DELETE CASCADE;`,
	// 56 (028-AC3): the text size a profile picked per room area, a JSON map; NULL for none.
	`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS text_sizes TEXT;`,
}

// nonText is the predicate of the non-typed message indexes, written into every query that uses
// them (020-AC1): it must stay literal SQL, as a bind parameter keeps the planner off a partial index.
const nonText = "kind <> 'text'"

// CreateDatabaseOptions makes a database that sorts as the server needs (017-AC6).
const CreateDatabaseOptions = "TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C' ENCODING 'UTF8' LOCALE_PROVIDER libc"

// Option configures Open.
type Option func(*options)

type options struct {
	lockWait    func(time.Duration)
	lockTimeout time.Duration
}

// WithLockWait has Open report how long each write waited for the write lock, from asking for it
// to holding it: main feeds grill_room_db_lock_wait_seconds (017-AC20).
func WithLockWait(f func(time.Duration)) Option { return func(o *options) { o.lockWait = f } }

func withLockTimeout(d time.Duration) Option { return func(o *options) { o.lockTimeout = d } }

// maxConns caps the pool, idle connections included.
const maxConns = 16

// Open connects to the PostgreSQL database at dsn, creates or migrates its schema and writes the
// boot data, all under the write lock, so pods starting together give one result (017-AC1).
func Open(dsn string, opts ...Option) (*Store, error) {
	o := options{lockTimeout: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// A session that dies or hangs holding the write lock would block every write and boot for
	// good: a lock wait fails after lock_timeout, an abandoned transaction ends after a minute.
	cfg.RuntimeParams["lock_timeout"] = strconv.FormatInt(o.lockTimeout.Milliseconds(), 10)
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	sqlDB := stdlib.OpenDB(*cfg)
	// Streams hold no connection between reads, so a few connections serve many open pages.
	sqlDB.SetMaxOpenConns(maxConns)
	// database/sql keeps 2 idle by default: every burst past that reconnects and authenticates again.
	sqlDB.SetMaxIdleConns(maxConns)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	db := &database{db: sqlDB, writer: make(chan struct{}, 1), lockWait: o.lockWait, lockTimeout: o.lockTimeout, cfg: cfg}
	if err := boot(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// CheckCollation refuses a database that does not sort as C: lists sort by name as SQLite did,
// byte by byte, only under libc's C collation (017-AC6). Boot and db copy both call it.
func CheckCollation(ctx context.Context, db *sql.DB) error {
	var name, provider, collate, ctype string
	if err := db.QueryRowContext(ctx, `SELECT datname, datlocprovider::text, datcollate, datctype FROM pg_database WHERE datname = current_database()`).
		Scan(&name, &provider, &collate, &ctype); err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	c := func(l string) bool { return l == "C" || l == "POSIX" }
	if provider != "c" || !c(collate) || !c(ctype) {
		return fmt.Errorf("database %q sorts with collation %q, ctype %q of provider %q, the server needs C: CREATE DATABASE %s %s",
			name, collate, ctype, provider, pgx.Identifier{name}.Sanitize(), CreateDatabaseOptions)
	}
	return nil
}

func boot(ctx context.Context, db *database) error {
	if err := CheckCollation(ctx, db.db); err != nil {
		return err
	}
	return db.locked(ctx, func(tx *txn) error {
		if err := migrate(ctx, tx); err != nil {
			return err
		}
		// A preset whose shipped text changed gets a new version: rooms are offered it (002-AC14).
		if err := ensurePresets(ctx, tx); err != nil {
			return err
		}
		// Likewise a built-in starter whose shipped content changed (006-AC2).
		if err := ensureStarters(ctx, tx); err != nil {
			return err
		}
		// A removed plugin whose last turn ended while the server was down (009-AC12).
		for _, q := range []string{purgePlugins, dropSuperseded} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("purge plugins: %w", err)
			}
		}
		return nil
	})
}

// migrate creates the baseline on an empty database, then runs the migrations past its version.
func migrate(ctx context.Context, tx *txn) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if !exists {
		if err := CreateBaseline(ctx, tx.tx); err != nil {
			return err
		}
		if err := seedPronunciations(ctx, tx); err != nil {
			return err
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	last := baselineVersion + len(migrations)
	if version > last {
		return fmt.Errorf("schema version %d is newer than this server's %d", version, last)
	}
	for v := version; v < last; v++ {
		if _, err := tx.ExecContext(ctx, migrations[v-baselineVersion]); err != nil {
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, v+1); err != nil {
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
	}
	return nil
}

// CreateBaseline creates the schema at baselineVersion in tx, without boot data: db copy fills it.
func CreateBaseline(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, baseline); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES($1)`, baselineVersion); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// LockTimeout is how long a write, or a wait for a room lock, waits for its lock before it fails.
func (s *Store) LockTimeout() time.Duration { return s.db.lockTimeout }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Writable reports whether the database accepts a write, within a few seconds.
func (s *Store) Writable(ctx context.Context) error {
	// Unlocked: a queue of writers does not fail the probe.
	err := s.db.unlocked(ctx, func(t *txn) error {
		if _, err := t.ExecContext(ctx, `SET LOCAL statement_timeout = 5000`); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES('healthz', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, now())
		return err
	})
	if err != nil {
		return fmt.Errorf("writable: %w", err)
	}
	return nil
}

// Size is the database's size on disk in bytes, for the instance view.
func (s *Store) Size(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&n); err != nil {
		return 0, fmt.Errorf("database size: %w", err)
	}
	return n, nil
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

// LastMessageID is the newest line's id across rooms, 0 with none.
func (s *Store) LastMessageID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&id); err != nil {
		return 0, fmt.Errorf("last message id: %w", err)
	}
	return id, nil
}

// SettingOrInit reads key, writing init first when it is absent: a key generated once, as the
// VAPID key (005-AC24), keeps the first value written.
func (s *Store) SettingOrInit(ctx context.Context, key, init string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, key, init); err != nil {
		return "", fmt.Errorf("init setting %s: %w", key, err)
	}
	return s.Setting(ctx, key)
}

// CreateRoom inserts a room with the given id, and the Grill template's persona (002-AC42).
func (s *Store) CreateRoom(ctx context.Context, id, name string) (*Room, error) {
	var r *Room
	err := s.withTx(ctx, "create room", func(tx *txn) error {
		var err error
		if r, err = insertRoom(ctx, tx, id, name, 0, true); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", nil)
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// insertRoom inserts a room created by profile creator (0 for the super-admin), with the Default
// template's persona when withDefault.
func insertRoom(ctx context.Context, tx *txn, id, name string, creator int64, withDefault bool) (*Room, error) {
	r := &Room{ID: id, Name: name, CreatedAt: time.Now().UTC(), Chain: Chain{Budget: DefaultChainBudget}}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(id, name, created_at, creator_profile_id) VALUES(?,?,?,?)`,
		r.ID, r.Name, formatTS(r.CreatedAt), nullID(creator)); err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	if !withDefault {
		return r, nil
	}
	def, err := presetID(ctx, tx, PresetDefault)
	if err != nil {
		return nil, err
	}
	if _, _, err = addPersona(ctx, tx, r.ID, def); err != nil {
		return nil, err
	}
	return r, nil
}

// Room fetches one room.
func (s *Store) Room(ctx context.Context, id string) (*Room, error) {
	var r Room
	var created string
	var from sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at, COALESCE(host_participant_id, 0), chain_budget, chain_count, chain_suspended, parallel_turns,
		created_from, egress_admins_allow, voice_quiet_ms, voice_resume_s, voice_queue_s, voice_words, host_moved_line FROM rooms WHERE id = ?`, id).Scan(&r.ID, &r.Name, &created,
		&r.HostParticipantID, &r.Chain.Budget, &r.Chain.Count, &r.Chain.Suspended, &r.ParallelTurns, &from, &r.EgressAdmins,
		&r.Speech.QuietMS, &r.Speech.ResumeS, &r.Speech.QueueS, &r.Speech.Words, &r.HostMovedLine)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", id, err)
	}
	r.CreatedAt, r.CreatedFrom = parseTS(created), parseProvenance(from)
	return &r, nil
}

// Rooms lists every room, newest first.
func (s *Store) Rooms(ctx context.Context) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, chain_budget, chain_count, chain_suspended FROM rooms ORDER BY created_at DESC, id DESC`)
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
	return s.withTx(ctx, "delete room", func(tx *txn) error {
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM rooms WHERE id = ?`, id)
	})
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
	if err := auditTx(ctx, tx, "", map[string]any{"from": old}); err != nil {
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
	var inv *Invite
	err := s.withTx(ctx, "create invite", func(tx *txn) error {
		var err error
		if inv, err = insertInvite(ctx, tx, roomID, tokenHash, createdBy, ""); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"invite": inv.ID})
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// insertInvite is CreateInvite in tx, storing link as the token of a shareable invite when not
// empty. A shareable one the unique index refuses is ErrNotFound too: the caller looks again.
func insertInvite(ctx context.Context, tx *txn, roomID, tokenHash string, createdBy int64, link string) (*Invite, error) {
	inv := &Invite{RoomID: roomID, CreatedAt: time.Now().UTC(), CreatedBy: createdBy, Shareable: link != ""}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO invites(room_id, token_hash, created_at, created_by, link) SELECT id, ?, ?, ?, ? FROM rooms r WHERE r.id = ?
		AND (? = 0 OR EXISTS (SELECT 1 FROM participants p
			WHERE p.id = ? AND p.room_id = r.id AND p.removed_at IS NULL))
		ON CONFLICT(room_id, created_by) WHERE link IS NOT NULL AND revoked_at IS NULL DO NOTHING RETURNING id`,
		tokenHash, formatTS(inv.CreatedAt), nullID(createdBy), nullStr(link), roomID, createdBy, createdBy).Scan(&inv.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	return inv, nil
}

// ShareInvite returns participant by's shareable invite of room and its token, creating it with
// link and its hash when by has none valid (011-AC26); created says which. ErrNotFound unless by is
// a participant in room with a valid invite.
func (s *Store) ShareInvite(ctx context.Context, roomID string, by int64, link, linkHash string) (inv *Invite, tok string, created bool, err error) {
	if by == 0 {
		// The super-admin has no shareable link: insertInvite would take 0 for it.
		return nil, "", false, ErrNotFound
	}
	err = s.withTx(ctx, "share invite", func(tx *txn) error {
		if inv, tok, err = shareable(ctx, tx, roomID, by); !errors.Is(err, ErrNotFound) {
			return err
		}
		inv, err = insertInvite(ctx, tx, roomID, linkHash, by, link)
		if errors.Is(err, ErrNotFound) {
			// Another request made it first, or by has no valid seat: either way, look again.
			inv, tok, err = shareable(ctx, tx, roomID, by)
			return err
		}
		if err != nil {
			return err
		}
		tok, created = link, true
		return auditTx(ctx, tx, "", map[string]any{"invite": inv.ID})
	})
	if err != nil {
		return nil, "", false, err
	}
	return inv, tok, created, nil
}

// shareable returns by's valid shareable invite of room, provided by's own seat is valid.
func shareable(ctx context.Context, tx *txn, roomID string, by int64) (*Invite, string, error) {
	inv := &Invite{RoomID: roomID, CreatedBy: by, Shareable: true}
	var created, link string
	err := tx.QueryRowContext(ctx, `
		SELECT s.id, s.created_at, s.link FROM invites s JOIN participants p ON p.id = s.created_by
		WHERE s.room_id = ? AND s.created_by = ? AND s.link IS NOT NULL AND s.revoked_at IS NULL AND p.removed_at IS NULL`,
		roomID, by).Scan(&inv.ID, &created, &link)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("shareable invite: %w", err)
	}
	inv.CreatedAt = parseTS(created)
	return inv, link, nil
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
		SELECT id, room_id, created_at, COALESCE(created_by, 0), revoked_at, link IS NOT NULL FROM invites
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
		if err := rows.Scan(&inv.ID, &inv.RoomID, &created, &inv.CreatedBy, &revoked, &inv.Shareable); err != nil {
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

// RevokeSeat is a person a revoke concerns, by their seat in the invites it cuts (011-AC19).
type RevokeSeat struct {
	ParticipantID int64  `json:"participant_id"`
	Name          string `json:"name"`
	ProfileID     int64  `json:"-"`
	Admin         bool   `json:"admin"`
}

// RevokePlan is what revoking an invite does: Invites, sorted, are the ones it revokes; Lose the
// people a cut removes; Keep the people with a seat in the chain who keep access through another
// one; Removed the seats a cut ended, and Daemons their apps, whose running turns the caller fails.
type RevokePlan struct {
	Invites    []int64
	Lose, Keep []RevokeSeat
	Removed    []int64
	Daemons    []int64
}

// Why a seat ended (016-AC1).
const (
	EndRevoked = "revoked"
	EndRemoved = "removed"
	EndLeft    = "left"
)

// inviteCut names, for invite ?1 of room ?2, its chain (the invite and every invite made by someone
// who joined through one in the chain) and cut, the chain's invites not yet revoked. The revoke and
// its preview both start with it, so the preview cannot drift from what the revoke does (011-AC20).
const inviteCut = `WITH RECURSIVE chain(id) AS (
		SELECT id FROM invites WHERE id = ?1 AND room_id = ?2
		UNION
		SELECT i.id FROM invites i JOIN participants p ON p.id = i.created_by JOIN chain c ON c.id = p.invite_id
	), cut(id) AS (SELECT id FROM invites WHERE id IN (SELECT id FROM chain) AND revoked_at IS NULL) `

// RevokePreview returns what revoking invite inviteID of room with the cut would do now, writing
// nothing (011-AC19, 016-AC4). ErrNotFound if inviteID is not in room.
func (s *Store) RevokePreview(ctx context.Context, roomID string, inviteID int64) (*RevokePlan, error) {
	var plan *RevokePlan
	err := s.withTx(ctx, "revoke preview", func(tx *txn) error {
		var err error
		plan, err = revokePlan(ctx, tx, roomID, inviteID)
		return err
	})
	return plan, err
}

// RevokeInvite marks invite inviteID revoked, so no one joins through it any more; its joiners keep
// their seats (016-AC3). With cut, by a room admin, it revokes the invite's whole chain and ends
// the seats in it as removed: a member cut off must not walk back in through an invite they made
// (011-AC20, 016-AC4). A cut that removes someone posts who did it, as who, and the removed names
// (Q4). Idempotent. Returns what it did, read in its own transaction so a joiner arriving after a
// preview is counted. ErrNotFound if inviteID is not in room. The caller broadcasts.
func (s *Store) RevokeInvite(ctx context.Context, roomID string, inviteID int64, cut bool, who string) (*RevokePlan, error) {
	var plan *RevokePlan
	err := s.withTx(ctx, "revoke invite", func(tx *txn) error {
		var err error
		if plan, err = revokePlan(ctx, tx, roomID, inviteID); err != nil {
			return err
		}
		at := now()
		revoke := `UPDATE invites SET revoked_at = ?3 WHERE id = ?1 AND room_id = ?2 AND revoked_at IS NULL`
		if !cut {
			plan.Invites = slices.DeleteFunc(plan.Invites, func(id int64) bool { return id != inviteID })
			plan.Lose, plan.Keep = []RevokeSeat{}, []RevokeSeat{}
		} else {
			revoke = inviteCut + `UPDATE invites SET revoked_at = ?3 WHERE id IN (SELECT id FROM cut)`
			if plan.Removed, err = queryIDs(ctx, tx, inviteCut+`UPDATE participants SET removed_at = ?3, end_reason = ?4
				WHERE invite_id IN (SELECT id FROM chain) AND removed_at IS NULL RETURNING id`, inviteID, roomID, at, EndRemoved); err != nil {
				return fmt.Errorf("revoke invite %d: %w", inviteID, err)
			}
			if plan.Daemons, _, err = seatsGone(ctx, tx, roomID, plan.Removed); err != nil {
				return fmt.Errorf("revoke invite %d: %w", inviteID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, revoke, inviteID, roomID, at); err != nil {
			return fmt.Errorf("revoke invite %d: %w", inviteID, err)
		}
		if len(plan.Lose) > 0 {
			names := make([]string, len(plan.Lose))
			for i, p := range plan.Lose {
				names[i] = p.Name
			}
			verb := " were removed"
			if len(names) == 1 {
				verb = " was removed"
			}
			m := &Message{RoomID: roomID, Kind: KindSystem, Sender: who, Body: who + " revoked an invite; " + strings.Join(names, ", ") + verb}
			if err := insertMessage(ctx, tx, m); err != nil {
				return err
			}
		}
		return auditTx(ctx, tx, "", map[string]any{"invite": inviteID, "revoked": plan.Invites, "cut": cut, "removed": plan.Removed})
	})
	return plan, err
}

// queryIDs runs query, in tx, and returns the ids its rows hold, one per row, ascending:
// PostgreSQL gives UPDATE .. RETURNING rows in no set order.
func queryIDs(ctx context.Context, tx *txn, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out, rows.Err()
}

func revokePlan(ctx context.Context, tx *txn, roomID string, inviteID int64) (*RevokePlan, error) {
	plan := &RevokePlan{Invites: []int64{}, Lose: []RevokeSeat{}, Keep: []RevokeSeat{}, Removed: []int64{}}
	found, err := cutInvites(ctx, tx, roomID, inviteID, plan)
	if err != nil {
		return nil, fmt.Errorf("revoke plan %d: %w", inviteID, err)
	}
	if !found {
		return nil, ErrNotFound
	}
	// A person is a profile, or a seat without one; they keep access through any valid seat outside
	// the chain. A seat of the chain counts while valid, whether or not its invite was revoked alone.
	rows, err := tx.QueryContext(ctx, inviteCut+`
		SELECT p.id, p.name, COALESCE(p.profile_id, 0),
			EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = p.room_id AND a.profile_id = p.profile_id),
			EXISTS (SELECT 1 FROM participants q WHERE q.room_id = p.room_id
				AND q.profile_id = p.profile_id AND q.removed_at IS NULL AND q.invite_id NOT IN (SELECT id FROM chain))
		FROM participants p WHERE p.invite_id IN (SELECT id FROM chain) AND p.removed_at IS NULL ORDER BY p.id`, inviteID, roomID)
	if err != nil {
		return nil, fmt.Errorf("revoke plan %d: %w", inviteID, err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	for rows.Next() {
		var p RevokeSeat
		var keeps bool
		if err := rows.Scan(&p.ParticipantID, &p.Name, &p.ProfileID, &p.Admin, &keeps); err != nil {
			return nil, fmt.Errorf("revoke plan %d: %w", inviteID, err)
		}
		person := -p.ParticipantID
		if p.ProfileID != 0 {
			person = p.ProfileID
		}
		if seen[person] {
			continue
		}
		seen[person] = true
		if keeps {
			plan.Keep = append(plan.Keep, p)
		} else {
			plan.Lose = append(plan.Lose, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("revoke plan %d: %w", inviteID, err)
	}
	return plan, nil
}

// cutInvites fills plan.Invites with the invites a revoke cuts, and reports whether inviteID is in room.
func cutInvites(ctx context.Context, tx *txn, roomID string, inviteID int64, plan *RevokePlan) (bool, error) {
	rows, err := tx.QueryContext(ctx, inviteCut+`SELECT id, id IN (SELECT id FROM cut) FROM chain ORDER BY id`, inviteID, roomID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id int64
		var cut bool
		if err := rows.Scan(&id, &cut); err != nil {
			return false, err
		}
		found = true
		if cut {
			plan.Invites = append(plan.Invites, id)
		}
	}
	return found, rows.Err()
}

// JoinProfile says which profile a join attaches the participant to. ID names an existing one.
// With ID 0, RecoveryHash and SessionHash create one; all empty joins without a profile.
type JoinProfile struct {
	ID                        int64
	RecoveryHash, SessionHash string
	// Issuer, set in restricted mode, refuses an invite whose creator is now an external, not bound
	// to it, who is not a room admin (021-AC7), or an internal whose last sign-in is not after
	// SignedInAfter, room admin or not (021-AC8).
	Issuer        string
	SignedInAfter time.Time
}

// Join adds a participant through a valid invite and records a system message announcing them,
// in one transaction, creating its profile when prof asks. ErrNotFound if the invite is unknown or
// revoked, ErrInviteLapsed if prof.Issuer and prof.SignedInAfter refuse it, ErrBlocked if a room admin blocked prof's
// profile from the room (016-AC13). The caller broadcasts.
func (s *Store) Join(ctx context.Context, inviteHash, name, tokenHash string, prof JoinProfile) (*Participant, *Message, error) {
	var p *Participant
	var m *Message
	err := s.withTx(ctx, "join", func(tx *txn) error {
		var err error
		p, m, err = join(ctx, tx, inviteHash, name, tokenHash, prof)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return p, m, nil
}

func join(ctx context.Context, tx *txn, inviteHash, name, tokenHash string, prof JoinProfile) (*Participant, *Message, error) {
	p := &Participant{Name: name, JoinedAt: time.Now().UTC()}
	err := tx.QueryRowContext(ctx, `SELECT id, room_id FROM invites WHERE token_hash = ? AND revoked_at IS NULL`, inviteHash).Scan(&p.InviteID, &p.RoomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	if prof.Issuer != "" {
		// A NULL creator is the super-admin: its invites always work.
		var lapsed bool
		// oidc_signed_in_at is fixed-width UTC: it orders as text.
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM invites i JOIN participants c ON c.id = i.created_by
			LEFT JOIN profiles f ON f.id = c.profile_id WHERE i.id = ? AND (f.oidc_issuer IS DISTINCT FROM ?
			AND NOT EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = i.room_id AND a.profile_id = c.profile_id)
			OR f.oidc_issuer = ? AND f.oidc_signed_in_at <= ?))`, p.InviteID, prof.Issuer, prof.Issuer, formatTS(prof.SignedInAfter)).Scan(&lapsed); err != nil {
			return nil, nil, fmt.Errorf("join: %w", err)
		}
		if lapsed {
			return nil, nil, ErrInviteLapsed
		}
	}
	p.ProfileID = prof.ID
	if p.ProfileID != 0 {
		var blocked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_blocks WHERE room_id = ? AND profile_id = ?)`, p.RoomID, p.ProfileID).Scan(&blocked); err != nil {
			return nil, nil, fmt.Errorf("join: %w", err)
		}
		if blocked {
			return nil, nil, ErrBlocked
		}
	}
	if p.ProfileID == 0 && prof.RecoveryHash != "" {
		if p.ProfileID, err = createProfile(ctx, tx, prof.RecoveryHash, prof.SessionHash); err != nil {
			return nil, nil, fmt.Errorf("join: %w", err)
		}
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO participants(room_id, invite_id, name, token_hash, joined_at, profile_id) VALUES(?,?,?,?,?,?) RETURNING id`,
		p.RoomID, p.InviteID, p.Name, tokenHash, formatTS(p.JoinedAt), nullID(p.ProfileID)).Scan(&p.ID); err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	m := &Message{RoomID: p.RoomID, Kind: KindSystem, ParticipantID: p.ID, Sender: name, Body: name + " joined"}
	if err := insertMessage(ctx, tx, m); err != nil {
		return nil, nil, err
	}
	return p, m, nil
}

// ParticipantByToken returns the participant holding tokenHash, provided its seat is valid.
func (s *Store) ParticipantByToken(ctx context.Context, tokenHash string) (*Participant, error) {
	var p Participant
	var joined, iss, at string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger, COALESCE(p.profile_id, 0), p.stt_off, `+bindingCols+` FROM participants p
		LEFT JOIN profiles f ON f.id = p.profile_id
		WHERE p.token_hash = ? AND p.removed_at IS NULL`, tokenHash).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.ProfileID, &p.STTOff, &iss, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("participant by token: %w", err)
	}
	p.JoinedAt = parseTS(joined)
	p.scan(iss, at)
	return &p, nil
}

// Participants lists a room's participants whose seat is valid, in join order.
func (s *Store) Participants(ctx context.Context, roomID string) ([]Participant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger, COALESCE(p.profile_id, 0), COALESCE(f.uuid, ''),
			EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = p.room_id AND a.profile_id = p.profile_id), p.last_seen_at,
			COALESCE(p.previous_name, '') FROM participants p
		LEFT JOIN profiles f ON f.id = p.profile_id
		WHERE p.room_id = ? AND p.removed_at IS NULL ORDER BY p.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer rows.Close()
	out := []Participant{}
	for rows.Next() {
		var p Participant
		var joined string
		var seen sql.NullString
		if err := rows.Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.ProfileID, &p.ProfileUUID, &p.Admin, &seen, &p.PreviousName); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		p.JoinedAt = parseTS(joined)
		if seen.Valid {
			t := parseTS(seen.String)
			p.LastSeen = &t
		}
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

// nullID stores 0 as NULL: an optional foreign key, or a count not known.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func insertMessage(ctx context.Context, db querier, m *Message) error {
	// A voice line without its source would read as neither device nor server (010-AC10).
	if (m.Kind == KindVoice) != (m.Source != "") {
		return fmt.Errorf("insert message: a %q line with source %q", m.Kind, m.Source)
	}
	m.TS = time.Now().UTC()
	pid := nullID(m.ParticipantID)
	// Rendered once here, not on every stream replay: a crafted 4000-character line takes ~10 ms.
	if m.Kind != KindSystem {
		m.HTML = render.MessageHTML(m.Body)
	}
	err := db.QueryRowContext(ctx,
		`INSERT INTO messages(room_id, ts, kind, participant_id, sender, body, html, model, effort, persona_id, req_model, req_effort, model_note, model_mismatch, turn_id, source, spoken)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		m.RoomID, formatTS(m.TS), m.Kind, pid, m.Sender, m.Body, nullStr(m.HTML), m.Model, m.Effort, nullID(m.PersonaID), m.ReqModel, m.ReqEffort,
		m.ModelNote, m.ModelMismatch, nullID(m.TurnID), nullStr(m.Source), m.Spoken).Scan(&m.ID)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// Messages returns up to limit messages of room with id > afterID, oldest first.
func (s *Store) Messages(ctx context.Context, roomID string, afterID int64, limit int) ([]Message, error) {
	return s.queryMessages(ctx, `WHERE room_id = ? AND id > ? ORDER BY id ASC LIMIT ?`, roomID, afterID, limit)
}

// MessagesAfter returns up to limit lines of every room with an id above afterID, in id order.
func (s *Store) MessagesAfter(ctx context.Context, afterID int64, limit int) ([]Message, error) {
	return s.queryMessages(ctx, `WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, limit)
}

// LastVoice returns the room's latest voice line still shown; ErrNotFound if there is none.
func (s *Store) LastVoice(ctx context.Context, roomID string) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, messageCols+`WHERE room_id = ? AND kind = 'voice' AND deleted_at IS NULL ORDER BY id DESC LIMIT 1`, roomID))
}

const (
	messageFields = `id, ts, room_id, kind, COALESCE(participant_id, 0), sender, body, COALESCE(html, ''), model, edited_at, deleted_at, change_seq,
		COALESCE(persona_id, 0), effort, COALESCE((SELECT badge FROM room_personas WHERE id = messages.persona_id), ''),
		COALESCE((SELECT colour FROM room_personas WHERE id = messages.persona_id), ''), req_model, req_effort, model_note, model_mismatch, COALESCE(turn_id, 0), COALESCE(source, ''), spoken, appended_at,
		(SELECT jsonb_build_object('domain', domain, 'persona', persona, 'count', count, 'decision', decision, 'pending', pending_scope, 'error', pending_error,
			'dismissed_by', dismissed_by)::text
			FROM blocked_requests WHERE message_id = messages.id)`
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
	var edited, deleted, appended, blocked sql.NullString
	err := sc.Scan(append([]any{&m.ID, &ts, &m.RoomID, &m.Kind, &m.ParticipantID, &m.Sender, &m.Body, &m.HTML, &m.Model, &edited, &deleted, &m.ChangeSeq,
		&m.PersonaID, &m.Effort, &m.Badge, &m.Colour, &m.ReqModel, &m.ReqEffort, &m.ModelNote, &m.ModelMismatch, &m.TurnID, &m.Source, &m.Spoken, &appended, &blocked}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	m.TS = parseTS(ts)
	m.EditedAt, m.DeletedAt, m.AppendedAt = parseNullTS(edited), parseNullTS(deleted), parseNullTS(appended)
	if blocked.Valid {
		m.Blocked = &Blocked{}
		if err := json.Unmarshal([]byte(blocked.String), m.Blocked); err != nil {
			return nil, fmt.Errorf("scan message %d: blocked: %w", m.ID, err)
		}
	}
	return &m, nil
}

func parseNullTS(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTS(s.String)
	return &t
}

// tsLayout is RFC 3339 at a fixed width, in UTC: text order of stored timestamps is time order.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

func now() string { return formatTS(time.Now()) }

// NormalizeTS rewrites an RFC 3339 timestamp in the stored fixed-width layout; ok is false, and s
// comes back as is, when s does not parse.
func NormalizeTS(s string) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s, false
	}
	return formatTS(t), true
}

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// SetSTTOff turns the transcription of participant pid's voice off, or back on (010-AC23).
func (s *Store) SetSTTOff(ctx context.Context, roomID string, pid int64, off bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE participants SET stt_off = ? WHERE id = ? AND room_id = ? AND removed_at IS NULL`, off, pid, roomID)
	if err != nil {
		return fmt.Errorf("set stt_off: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set stt_off: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// STTOff lists, for each of rooms with any, the participants who turned off the transcription of
// their voice, sorted (010-AC23).
func (s *Store) STTOff(ctx context.Context, rooms []string) (map[string][]int64, error) {
	out := map[string][]int64{}
	if len(rooms) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT room_id, id FROM participants WHERE stt_off = 1 AND removed_at IS NULL AND room_id = ANY(?)
		ORDER BY room_id, id`, rooms)
	if err != nil {
		return nil, fmt.Errorf("stt off: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var room string
		var id int64
		if err := rows.Scan(&room, &id); err != nil {
			return nil, fmt.Errorf("stt off: %w", err)
		}
		out[room] = append(out[room], id)
	}
	return out, rows.Err()
}

// RoomParticipant returns participant id of room, provided its seat is valid.
func (s *Store) RoomParticipant(ctx context.Context, roomID string, id int64) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger, p.stt_off FROM participants p
		WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, id, roomID).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.STTOff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room participant %d: %w", id, err)
	}
	p.JoinedAt = parseTS(joined)
	return &p, nil
}
