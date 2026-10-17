// Package store persists rooms, invites, participants and messages in SQLite.
//
// Derived from github.com/babs/airc pkg/store at commit b7afc63.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds

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
	ReqModel  string     `json:"requested_model,omitempty"`
	ReqEffort string     `json:"requested_effort,omitempty"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
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

// Store wraps the SQLite handle.
type Store struct{ db *sql.DB }

// migrations run in order, each once, tracked by PRAGMA user_version. Append only.
// Every table must reach rooms or profiles through NOT NULL, ON DELETE CASCADE foreign keys, and
// any other reference to profiles must be ON DELETE SET NULL: DeleteRoom and DeleteProfile are
// single DELETEs, and TestEveryTableCascadesFromAnOwner enforces it. Server-wide tables, listed in
// that test, are the exceptions.
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
`, `
-- 003-AC17: the room shows who holds the agent, never their machine's name.
UPDATE messages SET body = substr(body, 1, instr(body, ' hosts the agent, on ') - 1) || ' holds the agent(s).'
	WHERE kind = 'system' AND instr(body, ' hosts the agent, on ') > 0;
UPDATE daemons SET name = '';
UPDATE turns SET host = '';
`, `
-- 003-AC18 to 003-AC21: concurrent turns. host_queued_at is set while the host's daemon holds an
-- accepted turn in its local queue; base_revisions pins the agent documents a turn's workspace holds.
ALTER TABLE rooms ADD COLUMN parallel_turns INTEGER NOT NULL DEFAULT 0 CHECK (parallel_turns IN (0, 1));
ALTER TABLE turns ADD COLUMN host_queued_at TEXT;
ALTER TABLE turns ADD COLUMN base_revisions TEXT;
ALTER TABLE turns ADD COLUMN rerun_of INTEGER REFERENCES turns(id) ON DELETE SET NULL;
ALTER TABLE turns ADD COLUMN cause TEXT NOT NULL DEFAULT '';
CREATE INDEX turns_rerun_of ON turns(rerun_of);
-- One running turn per persona; StartTurn keeps one per room unless the room runs in parallel.
DROP INDEX turns_one_running;
CREATE UNIQUE INDEX turns_one_running ON turns(persona_id) WHERE status = 'running';
ALTER TABLE turn_queue ADD COLUMN rerun_of INTEGER REFERENCES turns(id) ON DELETE CASCADE;
ALTER TABLE turn_queue ADD COLUMN cause TEXT NOT NULL DEFAULT '';
CREATE INDEX turn_queue_rerun_of ON turn_queue(rerun_of);
`, `
-- 003-AC22 to 003-AC25, 003-AC32: the turn contract. A profile's uuid tells two people of the same
-- name apart in the presence preamble; context_* is the session's context use after its last turn;
-- handoff marks the turn that writes a handoff note ('write') and the new session reading it ('read').
ALTER TABLE profiles ADD COLUMN uuid TEXT;
UPDATE profiles SET uuid = ` + uuidSQL + `;
CREATE UNIQUE INDEX profiles_uuid ON profiles(uuid);
ALTER TABLE room_personas ADD COLUMN autocompact INTEGER NOT NULL DEFAULT 0
	CHECK (autocompact = 0 OR autocompact BETWEEN 100000 AND 1000000);
ALTER TABLE agent_sessions ADD COLUMN context_tokens INTEGER;
ALTER TABLE agent_sessions ADD COLUMN context_window INTEGER;
ALTER TABLE agent_sessions ADD COLUMN context_updated_at TEXT;
ALTER TABLE turns ADD COLUMN handoff TEXT NOT NULL DEFAULT '' CHECK (handoff IN ('', 'write', 'read'));
ALTER TABLE turn_queue ADD COLUMN handoff TEXT NOT NULL DEFAULT '' CHECK (handoff IN ('', 'write', 'read'));
`, `
-- 004-AC1, 004-AC6: server-wide tables. The audit log has no foreign key on purpose: an entry about
-- a room or profile outlives it, with the name it had, until the retention purge.
CREATE TABLE admin_sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	last_used_at TEXT NOT NULL,
	expires_at_s INTEGER NOT NULL,
	user_agent TEXT NOT NULL,
	ip TEXT NOT NULL
);
CREATE TABLE audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at_ms INTEGER NOT NULL,
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	target_kind TEXT NOT NULL,
	target_id TEXT NOT NULL,
	target_name TEXT NOT NULL,
	details TEXT NOT NULL
);
CREATE INDEX audit_log_at ON audit_log(at_ms);
CREATE INDEX audit_log_action ON audit_log(action, id);
`, `
-- 004-AC2, 004-AC3, 004-AC9: a room's own quota, NULL for the instance default; the profiles view
-- finds a profile's last line through its participants.
ALTER TABLE rooms ADD COLUMN quota_bytes INTEGER CHECK (quota_bytes IS NULL OR quota_bytes > 0);
CREATE INDEX messages_participant ON messages(participant_id, id);
`, `
-- 004-AC7, 004-AC8: the rooms a profile created count against its limit; room admins are profiles.
ALTER TABLE rooms ADD COLUMN creator_profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL;
CREATE INDEX rooms_creator ON rooms(creator_profile_id);
CREATE TABLE room_admins (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	PRIMARY KEY (room_id, profile_id)
);
CREATE INDEX room_admins_profile ON room_admins(profile_id);
-- 004-AC10: a room's own skills belong to no profile and go with the room; one a member copies in
-- while the host approves everything waits as pending. scope names what profile_id and room_id say.
ALTER TABLE skills ADD COLUMN room_id TEXT REFERENCES rooms(id) ON DELETE CASCADE
	CHECK (room_id IS NULL OR (profile_id IS NULL AND publisher_id IS NULL));
ALTER TABLE skills ADD COLUMN pending INTEGER NOT NULL DEFAULT 0 CHECK (pending = 0 OR (pending = 1 AND room_id IS NOT NULL));
ALTER TABLE skills ADD COLUMN scope TEXT GENERATED ALWAYS AS
	(CASE WHEN room_id IS NOT NULL THEN 'room' WHEN profile_id IS NULL THEN 'shared' ELSE 'personal' END) VIRTUAL;
DROP INDEX skills_shared_name;
CREATE UNIQUE INDEX skills_shared_name ON skills(name) WHERE profile_id IS NULL AND room_id IS NULL;
CREATE UNIQUE INDEX skills_room_name ON skills(room_id, name) WHERE room_id IS NOT NULL;
`, `
-- 004-AC7: a profile's own rooms limit, set by the super-admin: NULL follows the instance's, -1 is
-- unlimited.
ALTER TABLE profiles ADD COLUMN max_rooms INTEGER CHECK (max_rooms IS NULL OR max_rooms = -1 OR max_rooms > 0);
`, `
-- 004-AC13: an imported participant whose profile uuid matches no profile here keeps it, unlinked.
ALTER TABLE participants ADD COLUMN profile_uuid_hint TEXT;
`, `
-- 005-AC9: a persona always names its model and effort. Those left to the host run on sonnet,
-- medium, held changes included; the triggers stand for a CHECK, which ALTER TABLE cannot add.
UPDATE room_personas SET model = 'sonnet' WHERE model = '';
UPDATE room_personas SET effort = 'medium' WHERE effort = '';
UPDATE persona_template_versions SET model = 'sonnet' WHERE model = '';
UPDATE persona_template_versions SET effort = 'medium' WHERE effort = '';
UPDATE persona_change_requests SET patch = json_set(patch, '$.Model', 'sonnet') WHERE json_extract(patch, '$.Model') = '';
UPDATE persona_change_requests SET patch = json_set(patch, '$.Effort', 'medium') WHERE json_extract(patch, '$.Effort') = '';
CREATE TRIGGER room_personas_level_insert BEFORE INSERT ON room_personas WHEN NEW.model = '' OR NEW.effort = ''
	BEGIN SELECT RAISE(ABORT, 'a persona needs a model and an effort'); END;
CREATE TRIGGER room_personas_level_update BEFORE UPDATE OF model, effort ON room_personas WHEN NEW.model = '' OR NEW.effort = ''
	BEGIN SELECT RAISE(ABORT, 'a persona needs a model and an effort'); END;
CREATE TRIGGER persona_template_versions_level BEFORE INSERT ON persona_template_versions WHEN NEW.model = '' OR NEW.effort = ''
	BEGIN SELECT RAISE(ABORT, 'a persona needs a model and an effort'); END;
-- 005-AC10: the other models claude reported for a synthesis, or a mismatch with the one asked for.
ALTER TABLE messages ADD COLUMN model_note TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN model_mismatch INTEGER NOT NULL DEFAULT 0 CHECK (model_mismatch IN (0, 1));
`, `
-- 005-AC6, 005-AC20: when a participant was last active, and the client IP and user agent at join
-- and at last activity, for the super-admin only. The retention purge clears the client fields.
ALTER TABLE participants ADD COLUMN last_seen_at TEXT;
ALTER TABLE participants ADD COLUMN join_ip TEXT;
ALTER TABLE participants ADD COLUMN join_ua TEXT;
ALTER TABLE participants ADD COLUMN last_ip TEXT;
ALTER TABLE participants ADD COLUMN last_ua TEXT;
`, `
-- 005-AC23: the terms the STT worker is told to expect in the room.
ALTER TABLE rooms ADD COLUMN stt_glossary TEXT NOT NULL DEFAULT '';
`, `
-- 005-AC5: a turn someone stopped. ALTER TABLE cannot change a CHECK, but loosening one in the
-- stored schema is safe: no row on disk changes (sqlite.org/lang_altertable.html#otheralter).
PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql = replace(sql, 'CHECK (status IN (''running'', ''done'', ''failed''))',
	'CHECK (status IN (''running'', ''done'', ''failed'', ''stopped''))') WHERE type = 'table' AND name = 'turns';
PRAGMA writable_schema = OFF;
`, `
-- 005-AC4: the turn a message's @mention queued: its queue entry, then the turn it became. Queue
-- ids are never reused, and the entry is deleted as its turn starts, so queue_id has no key.
CREATE TABLE message_turns (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	queue_id INTEGER,
	turn_id INTEGER REFERENCES turns(id) ON DELETE SET NULL,
	PRIMARY KEY (message_id, persona_id)
);
CREATE INDEX message_turns_queue ON message_turns(queue_id);
CREATE INDEX message_turns_turn ON message_turns(turn_id);
-- The turn whose synthesis, failure or stop a message tells.
ALTER TABLE messages ADD COLUMN turn_id INTEGER REFERENCES turns(id) ON DELETE SET NULL;
CREATE INDEX messages_turn ON messages(turn_id) WHERE turn_id IS NOT NULL;
`, `
-- 005-AC1: the name a profile chose for itself; NULL keeps the one it last joined a room with.
ALTER TABLE profiles ADD COLUMN name TEXT;
`, `
-- 005-AC24: a participant's Web Push subscriptions, one per browser; they go with the participant
-- and so with the room. all_messages opts in to every line, not only the default triggers.
CREATE TABLE push_subscriptions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
	endpoint TEXT NOT NULL,
	p256dh TEXT NOT NULL,
	auth TEXT NOT NULL,
	all_messages INTEGER NOT NULL DEFAULT 0 CHECK (all_messages IN (0, 1)),
	created_at TEXT NOT NULL,
	UNIQUE (participant_id, endpoint)
);
`, `
-- 005-AC25, 005-AC26: a document renamed or deleted, softly; change_seq moves the room's
-- documents version on either. The renames tell the personas' next turns the old paths.
ALTER TABLE documents ADD COLUMN deleted_at TEXT;
ALTER TABLE documents ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
-- A deleted agent document leaves its name to a new one; restoring it checks the name is free.
DROP INDEX documents_agent_name;
CREATE UNIQUE INDEX documents_agent_name ON documents(room_id, name) WHERE kind = 'agent' AND deleted_at IS NULL;
CREATE TABLE document_renames (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
	from_name TEXT NOT NULL,
	to_name TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX document_renames_message ON document_renames(message_id);
-- 005-AC4: an imported mention keeps how its turn ended, as the turn history stays behind.
ALTER TABLE message_turns ADD COLUMN final_status TEXT CHECK (final_status IN ('done', 'failed', 'stopped', 'cancelled'));
ALTER TABLE message_turns ADD COLUMN final_line_id INTEGER REFERENCES messages(id) ON DELETE SET NULL;
`, `
-- 006-AC1 to 006-AC3: room starters. Built-ins belong to no profile; personal and instance ones
-- cascade from the profile that owns or published them, like persona templates. A version is a
-- manifest plus the blobs it names (document originals, skill tars), never changed once written.
CREATE TABLE starters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	builtin TEXT UNIQUE,
	profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE,
	library TEXT NOT NULL CHECK (library IN ('builtin', 'personal', 'instance')),
	created_at TEXT NOT NULL,
	CHECK ((library = 'builtin') = (builtin IS NOT NULL) AND (library = 'builtin') = (profile_id IS NULL))
);
CREATE INDEX starters_profile ON starters(profile_id);
CREATE TABLE starter_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	starter_id INTEGER NOT NULL REFERENCES starters(id) ON DELETE CASCADE,
	n INTEGER NOT NULL,
	author TEXT NOT NULL,
	manifest TEXT NOT NULL,
	size INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (starter_id, n)
);
CREATE TABLE starter_files (
	version_id INTEGER NOT NULL REFERENCES starter_versions(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY (version_id, name)
);
`, `
-- 006-AC11: the starter a room was created from, {"title": ..., "version": n}; NULL for a blank room.
ALTER TABLE rooms ADD COLUMN created_from TEXT;
-- 006-AC18: a persona speaks up every N voice lines or every T minutes. Its mark is the last
-- message and the time its latest turn started: the counts and the timer run from there.
ALTER TABLE room_personas ADD COLUMN every_voice INTEGER CHECK (every_voice IS NULL OR every_voice > 0);
ALTER TABLE room_personas ADD COLUMN every_minutes INTEGER CHECK (every_minutes IS NULL OR every_minutes > 0);
ALTER TABLE room_personas ADD COLUMN mark_msg_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE room_personas ADD COLUMN mark_at_ms INTEGER NOT NULL DEFAULT 0;
UPDATE room_personas SET mark_msg_id = (SELECT COALESCE(MAX(id), 0) FROM messages), mark_at_ms = CAST(unixepoch('subsec') * 1000 AS INTEGER);
-- 006-AC13: a skill a starter brought waits for the room's first host.
ALTER TABLE skills ADD COLUMN from_starter INTEGER NOT NULL DEFAULT 0 CHECK (from_starter IN (0, 1));
`, `
-- 009-AC29: the turn a note message's @mention queued, as message_turns does for chat lines.
CREATE TABLE annotation_message_turns (
	message_id INTEGER NOT NULL REFERENCES annotation_messages(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	queue_id INTEGER,
	turn_id INTEGER REFERENCES turns(id) ON DELETE SET NULL,
	final_status TEXT CHECK (final_status IN ('done', 'failed', 'stopped', 'cancelled')),
	final_line_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
	PRIMARY KEY (message_id, persona_id)
);
CREATE INDEX annotation_message_turns_queue ON annotation_message_turns(queue_id);
CREATE INDEX annotation_message_turns_turn ON annotation_message_turns(turn_id);
-- 009-AC31, 009-AC32: one persona per room watches the notes. notes_mark is the last note message
-- its latest turn read, or the latest one when it started watching: newer ones may wake it.
ALTER TABLE room_personas ADD COLUMN watches_notes INTEGER NOT NULL DEFAULT 0 CHECK (watches_notes IN (0, 1));
ALTER TABLE room_personas ADD COLUMN notes_mark INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX room_personas_watcher ON room_personas(room_id) WHERE watches_notes = 1;
PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql = replace(sql, '''every_n'', ''call'')', '''every_n'', ''call'', ''notes_watch'')')
	WHERE type = 'table' AND name = 'turn_queue';
PRAGMA writable_schema = OFF;
`, `
-- 009-AC36: the details card shows a participant's earlier name after a rename.
ALTER TABLE participants ADD COLUMN previous_name TEXT;
`, `
-- 009-AC5 to 009-AC18: plugins a room admin adds from git. ref is what the admin typed ('' for the
-- default branch), ref_name what the room shows. A removed plugin stays, removed_at set, while a
-- running turn loads one of its versions; purgePlugins drops it once none does.
CREATE TABLE room_plugins (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	source_url TEXT NOT NULL,
	ref TEXT NOT NULL,
	ref_name TEXT NOT NULL,
	entry TEXT NOT NULL DEFAULT '',
	private INTEGER NOT NULL DEFAULT 0 CHECK (private IN (0, 1)),
	created_by_name TEXT NOT NULL,
	created_at TEXT NOT NULL,
	removed_at TEXT
);
CREATE UNIQUE INDEX room_plugins_name ON room_plugins(room_id, name) WHERE removed_at IS NULL;
-- A version is a snapshot pinned at a commit, never changed once written but for its state. The
-- running version of a plugin is its latest approved one.
CREATE TABLE plugin_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	plugin_id INTEGER NOT NULL REFERENCES room_plugins(id) ON DELETE CASCADE,
	n INTEGER NOT NULL,
	commit_sha TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	files INTEGER NOT NULL,
	file_list TEXT NOT NULL,
	manifest TEXT NOT NULL,
	report TEXT,
	state TEXT NOT NULL CHECK (state IN ('inspecting', 'pending', 'approved', 'refused', 'invalid')),
	reason TEXT NOT NULL DEFAULT '',
	added_by_name TEXT NOT NULL,
	added_by_id INTEGER REFERENCES participants(id) ON DELETE SET NULL,
	added_at TEXT NOT NULL,
	decided_by_name TEXT,
	decided_at TEXT,
	UNIQUE (plugin_id, n)
);
CREATE INDEX plugin_versions_inspecting ON plugin_versions(plugin_id) WHERE state = 'inspecting';
-- Covers the room stream's PluginsVersion (version ids and states) without touching a row.
CREATE INDEX plugin_versions_state ON plugin_versions(plugin_id, state);
-- A version's tar sits in a table of its own: a scan of plugin_versions (the list, the stream's
-- version) never walks a snapshot's overflow pages. dropSuperseded deletes the row.
CREATE TABLE plugin_snapshots (
	version_id INTEGER PRIMARY KEY NOT NULL REFERENCES plugin_versions(id) ON DELETE CASCADE,
	data BLOB NOT NULL
);
CREATE TABLE room_persona_plugins (
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	plugin_id INTEGER NOT NULL REFERENCES room_plugins(id) ON DELETE CASCADE,
	PRIMARY KEY (persona_id, plugin_id)
);
CREATE INDEX room_persona_plugins_plugin ON room_persona_plugins(plugin_id);
-- The versions a turn loads, copied as it starts: [{"name", "version_id", "n", "sha256"}].
ALTER TABLE turns ADD COLUMN plugins TEXT NOT NULL DEFAULT '[]';
-- 009-AC12: every way a turn ends updates its status, so the purges ride on that.
CREATE TRIGGER turns_purge_plugins AFTER UPDATE OF status ON turns WHEN NEW.status != 'running' BEGIN
	` + purgePlugins + `;
	` + dropSuperseded + `;
END;
`, `
-- 009-AC19 to 009-AC21: a room admin asks the AI host to fetch a repo the server cannot read.
-- plugin_id is set for an update of that private plugin. A fetch done is deleted: the plugin or
-- its version holds the result. A failed one stays, with its reason, until an admin dismisses it.
CREATE TABLE plugin_fetches (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	plugin_id INTEGER REFERENCES room_plugins(id) ON DELETE CASCADE,
	source_url TEXT NOT NULL,
	ref TEXT NOT NULL,
	entries TEXT NOT NULL DEFAULT '[]',
	personas TEXT NOT NULL DEFAULT '[]',
	state TEXT NOT NULL CHECK (state IN ('waiting', 'failed')),
	reason TEXT NOT NULL DEFAULT '',
	requested_by_name TEXT NOT NULL,
	requested_by_id INTEGER REFERENCES participants(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX plugin_fetches_room ON plugin_fetches(room_id);
CREATE UNIQUE INDEX plugin_fetches_update ON plugin_fetches(plugin_id) WHERE state = 'waiting';
-- 009-AC24: the last turn that pinned the plugin left it out for an option the host has not set.
ALTER TABLE room_plugins ADD COLUMN needs_setup INTEGER NOT NULL DEFAULT 0 CHECK (needs_setup IN (0, 1));
`, `
-- 009-AC27: an imported version waits for the host's approval once inspected, whatever the mode.
ALTER TABLE plugin_versions ADD COLUMN hold INTEGER NOT NULL DEFAULT 0 CHECK (hold IN (0, 1));
`, `
-- 011-AC1: one-time recovery links the admin hands over; only the token's hash is kept.
CREATE TABLE recovery_links (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at_ms INTEGER NOT NULL,
	used_at TEXT
);
CREATE INDEX recovery_links_profile ON recovery_links(profile_id);
-- 011-AC8: the email nudge stays hidden until then, on every device of the profile.
ALTER TABLE profiles ADD COLUMN email_nudge_until TEXT;
`, `
-- 011-AC26: a member's shareable invite keeps its token so the popup can show it again; NULL for
-- every other invite. One valid one per member and room.
ALTER TABLE invites ADD COLUMN link TEXT;
CREATE UNIQUE INDEX invites_shareable ON invites(room_id, created_by) WHERE link IS NOT NULL AND revoked_at IS NULL;
`, `
-- 010-AC10: who transcribed a voice line; NULL on every other kind. Lines before 010 came from the
-- server's worker.
ALTER TABLE messages ADD COLUMN source TEXT CHECK (source IN ('device', 'server'));
UPDATE messages SET source = 'server' WHERE kind = 'voice';
`, `
-- 012-AC1: personal API tokens; only the token's hash is kept. expires_at_ms NULL never expires.
CREATE TABLE api_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	scope TEXT NOT NULL CHECK (scope IN ('read', 'full')),
	created_at TEXT NOT NULL,
	expires_at_ms INTEGER,
	last_used_at_ms INTEGER,
	last_used_ip TEXT
);
CREATE INDEX api_tokens_profile ON api_tokens(profile_id);
CREATE INDEX api_tokens_expires ON api_tokens(expires_at_ms);
`, `
-- 014-AC12: the AI host lets the room's admins allow websites for the room; off on a host change.
ALTER TABLE rooms ADD COLUMN egress_admins_allow INTEGER NOT NULL DEFAULT 0 CHECK (egress_admins_allow IN (0, 1));
-- 014-AC8: the website a turn was refused, on its system line. decision is '' until someone's
-- approval is confirmed by the AI host's app; pending_* hold one it has not confirmed yet.
CREATE TABLE blocked_requests (
	message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona TEXT NOT NULL,
	domain TEXT NOT NULL,
	count INTEGER NOT NULL DEFAULT 1 CHECK (count > 0),
	decision TEXT NOT NULL DEFAULT '' CHECK (decision IN ('', 'room', 'server')),
	pending_scope TEXT NOT NULL DEFAULT '' CHECK (pending_scope IN ('', 'room', 'server')),
	pending_by TEXT NOT NULL DEFAULT '',
	pending_actor TEXT NOT NULL DEFAULT '',
	pending_error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX blocked_requests_open ON blocked_requests(room_id, domain) WHERE decision = '';
`, `
-- 014: the host or a delegated admin put a blocked website aside; its next refusal raises it again.
ALTER TABLE blocked_requests ADD COLUMN dismissed_by TEXT NOT NULL DEFAULT '';
`, `
-- 016-AC1: a seat is valid on its own, no longer through its invite. A seat whose invite was
-- revoked ends when the invite did.
ALTER TABLE participants ADD COLUMN removed_at TEXT;
ALTER TABLE participants ADD COLUMN end_reason TEXT CHECK (end_reason IN ('revoked', 'removed', 'left'));
UPDATE participants SET removed_at = (SELECT revoked_at FROM invites i WHERE i.id = participants.invite_id), end_reason = 'revoked'
	WHERE invite_id IN (SELECT id FROM invites WHERE revoked_at IS NOT NULL);
CREATE INDEX participants_seated ON participants(room_id) WHERE removed_at IS NULL;
`, `
-- 016-AC13: a profile a room admin blocked from rejoining the room; name is theirs at removal.
CREATE TABLE room_blocks (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	created_at TEXT NOT NULL,
	by_participant_id INTEGER REFERENCES participants(id) ON DELETE SET NULL,
	PRIMARY KEY (room_id, profile_id)
);
CREATE INDEX room_blocks_profile ON room_blocks(profile_id);
`}

// uuidSQL is a random version 4 UUID; SQLite has no function for it.
const uuidSQL = `lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)), 2) || '-' ||
	substr('89ab', 1 + abs(random() % 4), 1) || substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6)))`

// migrationData runs Go code inside the transaction of the migration to version key, after its SQL.
var migrationData = map[int]func(context.Context, *sql.Tx) error{9: migratePersonas, 24: migrateStopped, 31: migrateNotesWatch}

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
	// Likewise a built-in starter whose shipped content changed (006-AC2).
	if err := ensureStarters(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// A removed plugin whose last turn ended while the server was down (009-AC12).
	for _, q := range []string{purgePlugins, dropSuperseded} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("purge plugins: %w", err)
		}
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

// CreateRoom inserts a room with the given id, and the Default template's persona (002-AC42).
func (s *Store) CreateRoom(ctx context.Context, id, name string) (*Room, error) {
	var r *Room
	err := s.withTx(ctx, "create room", func(tx *sql.Tx) error {
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
func insertRoom(ctx context.Context, tx *sql.Tx, id, name string, creator int64, withDefault bool) (*Room, error) {
	r := &Room{ID: id, Name: name, CreatedAt: time.Now().UTC(), Chain: Chain{Budget: DefaultChainBudget}}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(id, name, created_at, creator_profile_id) VALUES(?,?,?,?)`,
		r.ID, r.Name, r.CreatedAt.Format(time.RFC3339Nano), nullID(creator)); err != nil {
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
		created_from, egress_admins_allow FROM rooms WHERE id = ?`, id).Scan(&r.ID, &r.Name, &created, &r.HostParticipantID, &r.Chain.Budget, &r.Chain.Count, &r.Chain.Suspended,
		&r.ParallelTurns, &from, &r.EgressAdmins)
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

// DeleteRoom purges a room and, through ON DELETE CASCADE, every row that belongs to it. Run
// Checkpoint after it: the WAL holds the deleted rows' old pages until then.
func (s *Store) DeleteRoom(ctx context.Context, id string) error {
	return s.withTx(ctx, "delete room", func(tx *sql.Tx) error {
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
	err := s.withTx(ctx, "create invite", func(tx *sql.Tx) error {
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
func insertInvite(ctx context.Context, tx *sql.Tx, roomID, tokenHash string, createdBy int64, link string) (*Invite, error) {
	inv := &Invite{RoomID: roomID, CreatedAt: time.Now().UTC(), CreatedBy: createdBy, Shareable: link != ""}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO invites(room_id, token_hash, created_at, created_by, link) SELECT id, ?, ?, ?, ? FROM rooms r WHERE r.id = ?
		AND (? = 0 OR EXISTS (SELECT 1 FROM participants p
			WHERE p.id = ? AND p.room_id = r.id AND p.removed_at IS NULL))
		ON CONFLICT(room_id, created_by) WHERE link IS NOT NULL AND revoked_at IS NULL DO NOTHING RETURNING id`,
		tokenHash, inv.CreatedAt.Format(time.RFC3339Nano), nullID(createdBy), nullStr(link), roomID, createdBy, createdBy).Scan(&inv.ID)
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
	err = s.withTx(ctx, "share invite", func(tx *sql.Tx) error {
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
func shareable(ctx context.Context, tx *sql.Tx, roomID string, by int64) (*Invite, string, error) {
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
// one; Removed the seats a cut ended.
type RevokePlan struct {
	Invites    []int64
	Lose, Keep []RevokeSeat
	Removed    []int64
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
	), cut(id) AS (SELECT id FROM invites WHERE id IN chain AND revoked_at IS NULL) `

// RevokePreview returns what revoking invite inviteID of room with the cut would do now, writing
// nothing (011-AC19, 016-AC4). ErrNotFound if inviteID is not in room.
func (s *Store) RevokePreview(ctx context.Context, roomID string, inviteID int64) (*RevokePlan, error) {
	var plan *RevokePlan
	err := s.withTx(ctx, "revoke preview", func(tx *sql.Tx) error {
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
	err := s.withTx(ctx, "revoke invite", func(tx *sql.Tx) error {
		var err error
		if plan, err = revokePlan(ctx, tx, roomID, inviteID); err != nil {
			return err
		}
		at := now()
		revoke := `UPDATE invites SET revoked_at = ?3 WHERE id = ?1 AND revoked_at IS NULL`
		if !cut {
			plan.Invites = slices.DeleteFunc(plan.Invites, func(id int64) bool { return id != inviteID })
			plan.Lose, plan.Keep = []RevokeSeat{}, []RevokeSeat{}
		} else {
			revoke = inviteCut + `UPDATE invites SET revoked_at = ?3 WHERE id IN cut`
			if plan.Removed, err = endSeats(ctx, tx, inviteCut+`UPDATE participants SET removed_at = ?3, end_reason = ?4
				WHERE invite_id IN chain AND removed_at IS NULL RETURNING id`, inviteID, roomID, at, EndRemoved); err != nil {
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

// endSeats runs query, an UPDATE of participants returning the ids of the seats it ended.
func endSeats(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
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
	return out, rows.Err()
}

func revokePlan(ctx context.Context, tx *sql.Tx, roomID string, inviteID int64) (*RevokePlan, error) {
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
				AND q.profile_id = p.profile_id AND q.removed_at IS NULL AND q.invite_id NOT IN chain)
		FROM participants p WHERE p.invite_id IN chain AND p.removed_at IS NULL ORDER BY p.id`, inviteID, roomID)
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
func cutInvites(ctx context.Context, tx *sql.Tx, roomID string, inviteID int64, plan *RevokePlan) (bool, error) {
	rows, err := tx.QueryContext(ctx, inviteCut+`SELECT id, id IN cut FROM chain ORDER BY id`, inviteID, roomID)
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
}

// Join adds a participant through a valid invite and records a system message announcing them,
// in one transaction, creating its profile when prof asks. ErrNotFound if the invite is unknown or
// revoked, ErrBlocked if a room admin blocked prof's profile from the room (016-AC13). The caller
// broadcasts.
func (s *Store) Join(ctx context.Context, inviteHash, name, tokenHash string, prof JoinProfile) (*Participant, *Message, error) {
	var p *Participant
	var m *Message
	err := s.withTx(ctx, "join", func(tx *sql.Tx) error {
		var err error
		p, m, err = join(ctx, tx, inviteHash, name, tokenHash, prof)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return p, m, nil
}

func join(ctx context.Context, tx *sql.Tx, inviteHash, name, tokenHash string, prof JoinProfile) (*Participant, *Message, error) {
	p := &Participant{Name: name, JoinedAt: time.Now().UTC()}
	err := tx.QueryRowContext(ctx, `SELECT id, room_id FROM invites WHERE token_hash = ? AND revoked_at IS NULL`, inviteHash).Scan(&p.InviteID, &p.RoomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
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
	return p, m, nil
}

// ParticipantByToken returns the participant holding tokenHash, provided its seat is valid.
func (s *Store) ParticipantByToken(ctx context.Context, tokenHash string) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger, COALESCE(p.profile_id, 0) FROM participants p
		WHERE p.token_hash = ? AND p.removed_at IS NULL`, tokenHash).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger, &p.ProfileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("participant by token: %w", err)
	}
	p.JoinedAt = parseTS(joined)
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

func insertMessage(ctx context.Context, db execer, m *Message) error {
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
	res, err := db.ExecContext(ctx,
		`INSERT INTO messages(room_id, ts, kind, participant_id, sender, body, html, model, effort, persona_id, req_model, req_effort, model_note, model_mismatch, turn_id, source)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.RoomID, m.TS.Format(time.RFC3339Nano), m.Kind, pid, m.Sender, m.Body, nullStr(m.HTML), m.Model, m.Effort, nullID(m.PersonaID), m.ReqModel, m.ReqEffort,
		m.ModelNote, m.ModelMismatch, nullID(m.TurnID), nullStr(m.Source))
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
		COALESCE((SELECT colour FROM room_personas WHERE id = messages.persona_id), ''), req_model, req_effort, model_note, model_mismatch, COALESCE(turn_id, 0), COALESCE(source, ''),
		(SELECT json_object('domain', domain, 'persona', persona, 'count', count, 'decision', decision, 'pending', pending_scope, 'error', pending_error,
			'dismissed_by', dismissed_by)
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
	var edited, deleted, blocked sql.NullString
	err := sc.Scan(append([]any{&m.ID, &ts, &m.RoomID, &m.Kind, &m.ParticipantID, &m.Sender, &m.Body, &m.HTML, &m.Model, &edited, &deleted, &m.ChangeSeq,
		&m.PersonaID, &m.Effort, &m.Badge, &m.Colour, &m.ReqModel, &m.ReqEffort, &m.ModelNote, &m.ModelMismatch, &m.TurnID, &m.Source, &blocked}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	m.TS = parseTS(ts)
	m.EditedAt, m.DeletedAt = parseNullTS(edited), parseNullTS(deleted)
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

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// RoomParticipant returns participant id of room, provided its seat is valid.
func (s *Store) RoomParticipant(ctx context.Context, roomID string, id int64) (*Participant, error) {
	var p Participant
	var joined string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.room_id, p.invite_id, p.name, p.joined_at, p.can_trigger FROM participants p
		WHERE p.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, id, roomID).Scan(&p.ID, &p.RoomID, &p.InviteID, &p.Name, &joined, &p.CanTrigger)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("room participant %d: %w", id, err)
	}
	p.JoinedAt = parseTS(joined)
	return &p, nil
}
