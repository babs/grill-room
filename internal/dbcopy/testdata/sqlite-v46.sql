-- The SQLite schema at version 46, the last one the SQLite server ran: the source db copy reads.

CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE rooms (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	created_at TEXT NOT NULL
, host_participant_id INTEGER REFERENCES participants(id) ON DELETE SET NULL, chain_budget INTEGER NOT NULL DEFAULT 25 CHECK (chain_budget > 0), chain_count INTEGER NOT NULL DEFAULT 0 CHECK (chain_count >= 0), chain_suspended INTEGER NOT NULL DEFAULT 0 CHECK (chain_suspended IN (0, 1)), parallel_turns INTEGER NOT NULL DEFAULT 0 CHECK (parallel_turns IN (0, 1)), quota_bytes INTEGER CHECK (quota_bytes IS NULL OR quota_bytes > 0), creator_profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL, stt_glossary TEXT NOT NULL DEFAULT '', created_from TEXT, egress_admins_allow INTEGER NOT NULL DEFAULT 0 CHECK (egress_admins_allow IN (0, 1)), voice_quiet_ms INTEGER NOT NULL DEFAULT 1000 CHECK (voice_quiet_ms BETWEEN 500 AND 5000), voice_resume_s INTEGER NOT NULL DEFAULT 20 CHECK (voice_resume_s BETWEEN 0 AND 60), voice_queue_s INTEGER NOT NULL DEFAULT 40 CHECK (voice_queue_s BETWEEN 10 AND 300), voice_words INTEGER NOT NULL DEFAULT 60 CHECK (voice_words BETWEEN 40 AND 60));

CREATE TABLE invites (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	revoked_at TEXT
, created_by INTEGER REFERENCES participants(id) ON DELETE SET NULL, link TEXT);

CREATE TABLE participants (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	invite_id INTEGER NOT NULL REFERENCES invites(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	joined_at TEXT NOT NULL
, can_trigger INTEGER NOT NULL DEFAULT 0 CHECK (can_trigger IN (0, 1)), profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL, profile_uuid_hint TEXT, last_seen_at TEXT, join_ip TEXT, join_ua TEXT, last_ip TEXT, last_ua TEXT, previous_name TEXT, removed_at TEXT, end_reason TEXT CHECK (end_reason IN ('revoked', 'removed', 'left')));

CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	ts TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('voice', 'text', 'agent', 'system')),
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	sender TEXT NOT NULL,
	body TEXT NOT NULL
, model TEXT NOT NULL DEFAULT '', html TEXT, edited_at TEXT, deleted_at TEXT, change_seq INTEGER NOT NULL DEFAULT 0, persona_id INTEGER REFERENCES room_personas(id) ON DELETE SET NULL, effort TEXT NOT NULL DEFAULT '', req_model TEXT NOT NULL DEFAULT '', req_effort TEXT NOT NULL DEFAULT '', model_note TEXT NOT NULL DEFAULT '', model_mismatch INTEGER NOT NULL DEFAULT 0 CHECK (model_mismatch IN (0, 1)), turn_id INTEGER REFERENCES turns(id) ON DELETE SET NULL, source TEXT CHECK (source IN ('device', 'server')));

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

CREATE TABLE documents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('upload', 'agent')),
	name TEXT NOT NULL,
	format TEXT NOT NULL CHECK (format IN ('md', 'html', 'pdf', 'image')),
	upload_id INTEGER REFERENCES uploads(id) ON DELETE CASCADE,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL, deleted_at TEXT, change_seq INTEGER NOT NULL DEFAULT 0,
	CHECK ((kind = 'upload') = (upload_id IS NOT NULL))
);

CREATE TABLE revisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	n INTEGER NOT NULL,
	body TEXT NOT NULL,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (document_id, n)
);

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

CREATE TABLE annotation_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	annotation_id INTEGER NOT NULL REFERENCES annotations(id) ON DELETE CASCADE,
	author_kind TEXT NOT NULL CHECK (author_kind IN ('participant', 'agent')),
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	author TEXT NOT NULL,
	body TEXT NOT NULL,
	created_at TEXT NOT NULL, persona_id INTEGER REFERENCES room_personas(id) ON DELETE SET NULL,
	CHECK ((author_kind = 'participant') = (participant_id IS NOT NULL))
);

CREATE TABLE attachments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL REFERENCES annotation_messages(id) ON DELETE CASCADE,
	content_type TEXT NOT NULL,
	size INTEGER NOT NULL,
	data BLOB NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE daemons (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	participant_id INTEGER NOT NULL UNIQUE REFERENCES participants(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
, max_model TEXT NOT NULL DEFAULT '', max_effort TEXT NOT NULL DEFAULT '', approve_all INTEGER NOT NULL DEFAULT 0 CHECK (approve_all IN (0, 1)));

CREATE TABLE turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	daemon_id INTEGER REFERENCES daemons(id) ON DELETE SET NULL,
	host TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('running', 'done', 'failed', 'stopped')),
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
, model TEXT NOT NULL DEFAULT '', change_cursor INTEGER NOT NULL DEFAULT 0, persona_id INTEGER REFERENCES room_personas(id) ON DELETE SET NULL, persona_name TEXT NOT NULL DEFAULT '', persona_text TEXT NOT NULL DEFAULT '', req_model TEXT NOT NULL DEFAULT '', req_effort TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '', skills TEXT NOT NULL DEFAULT '[]', host_queued_at TEXT, base_revisions TEXT, rerun_of INTEGER REFERENCES turns(id) ON DELETE SET NULL, cause TEXT NOT NULL DEFAULT '', handoff TEXT NOT NULL DEFAULT '' CHECK (handoff IN ('', 'write', 'read')), plugins TEXT NOT NULL DEFAULT '[]');

CREATE TABLE profiles (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	recovery_hash TEXT NOT NULL UNIQUE,
	email TEXT UNIQUE,
	created_at TEXT NOT NULL
, skills_disclaimer_at TEXT, uuid TEXT, max_rooms INTEGER CHECK (max_rooms IS NULL OR max_rooms = -1 OR max_rooms > 0), name TEXT, email_nudge_until TEXT);

CREATE TABLE profile_sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at_s INTEGER NOT NULL
);

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

CREATE TABLE message_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	body TEXT NOT NULL,
	written_at TEXT NOT NULL
);

CREATE TABLE persona_templates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	preset TEXT UNIQUE,
	profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	created_at TEXT NOT NULL,
	CHECK ((preset IS NULL) = (profile_id IS NOT NULL))
);

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
, pending INTEGER NOT NULL DEFAULT 0 CHECK (pending IN (0, 1)), cap_decision TEXT NOT NULL DEFAULT '' CHECK (cap_decision IN ('', 'approved', 'refused')), cap_daemon_id INTEGER REFERENCES daemons(id) ON DELETE SET NULL, cap_model TEXT NOT NULL DEFAULT '', cap_effort TEXT NOT NULL DEFAULT '', autocompact INTEGER NOT NULL DEFAULT 0
	CHECK (autocompact = 0 OR autocompact BETWEEN 100000 AND 1000000), every_voice INTEGER CHECK (every_voice IS NULL OR every_voice > 0), every_minutes INTEGER CHECK (every_minutes IS NULL OR every_minutes > 0), mark_msg_id INTEGER NOT NULL DEFAULT 0, mark_at_ms INTEGER NOT NULL DEFAULT 0, watches_notes INTEGER NOT NULL DEFAULT 0 CHECK (watches_notes IN (0, 1)), notes_mark INTEGER NOT NULL DEFAULT 0, speaks INTEGER NOT NULL DEFAULT 1 CHECK (speaks IN (0, 1)), voice TEXT NOT NULL DEFAULT '', voice_rate REAL NOT NULL DEFAULT 1.0 CHECK (voice_rate BETWEEN 0.9 AND 1.3));

CREATE TABLE "agent_sessions" (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL,
	data BLOB NOT NULL,
	updated_at TEXT NOT NULL, context_tokens INTEGER, context_window INTEGER, context_updated_at TEXT,
	PRIMARY KEY (persona_id, session_id)
);

CREATE TABLE persona_change_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL UNIQUE REFERENCES room_personas(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('add', 'change')),
	patch TEXT NOT NULL,
	requested_by TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE "turn_queue" (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL UNIQUE REFERENCES room_personas(id) ON DELETE CASCADE,
	participant_id INTEGER REFERENCES participants(id) ON DELETE CASCADE,
	source TEXT NOT NULL CHECK (source IN ('button', 'mention', 'every_n', 'call', 'notes_watch')),
	fresh INTEGER NOT NULL CHECK (fresh IN (0, 1)),
	created_at TEXT NOT NULL, rerun_of INTEGER REFERENCES turns(id) ON DELETE CASCADE, cause TEXT NOT NULL DEFAULT '', handoff TEXT NOT NULL DEFAULT '' CHECK (handoff IN ('', 'write', 'read')),
	CHECK ((participant_id IS NULL) = (source IN ('every_n', 'call', 'notes_watch')))
);

CREATE TABLE skills (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE,
	publisher_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL, room_id TEXT REFERENCES rooms(id) ON DELETE CASCADE
	CHECK (room_id IS NULL OR (profile_id IS NULL AND publisher_id IS NULL)), pending INTEGER NOT NULL DEFAULT 0 CHECK (pending = 0 OR (pending = 1 AND room_id IS NOT NULL)), scope TEXT GENERATED ALWAYS AS
	(CASE WHEN room_id IS NOT NULL THEN 'room' WHEN profile_id IS NULL THEN 'shared' ELSE 'personal' END) VIRTUAL, from_starter INTEGER NOT NULL DEFAULT 0 CHECK (from_starter IN (0, 1)),
	CHECK (profile_id IS NULL OR publisher_id IS NULL)
);

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

CREATE TABLE room_persona_skills (
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	skill_id INTEGER NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	PRIMARY KEY (persona_id, skill_id)
);

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

CREATE TABLE room_admins (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	PRIMARY KEY (room_id, profile_id)
);

CREATE TABLE message_turns (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	queue_id INTEGER,
	turn_id INTEGER REFERENCES turns(id) ON DELETE SET NULL, final_status TEXT CHECK (final_status IN ('done', 'failed', 'stopped', 'cancelled')), final_line_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
	PRIMARY KEY (message_id, persona_id)
);

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

CREATE TABLE document_renames (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
	from_name TEXT NOT NULL,
	to_name TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE starters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	builtin TEXT UNIQUE,
	profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE,
	library TEXT NOT NULL CHECK (library IN ('builtin', 'personal', 'instance')),
	created_at TEXT NOT NULL,
	CHECK ((library = 'builtin') = (builtin IS NOT NULL) AND (library = 'builtin') = (profile_id IS NULL))
);

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

CREATE TABLE annotation_message_turns (
	message_id INTEGER NOT NULL REFERENCES annotation_messages(id) ON DELETE CASCADE,
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	queue_id INTEGER,
	turn_id INTEGER REFERENCES turns(id) ON DELETE SET NULL,
	final_status TEXT CHECK (final_status IN ('done', 'failed', 'stopped', 'cancelled')),
	final_line_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
	PRIMARY KEY (message_id, persona_id)
);

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
, needs_setup INTEGER NOT NULL DEFAULT 0 CHECK (needs_setup IN (0, 1)));

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
	decided_at TEXT, hold INTEGER NOT NULL DEFAULT 0 CHECK (hold IN (0, 1)),
	UNIQUE (plugin_id, n)
);

CREATE TABLE plugin_snapshots (
	version_id INTEGER PRIMARY KEY NOT NULL REFERENCES plugin_versions(id) ON DELETE CASCADE,
	data BLOB NOT NULL
);

CREATE TABLE room_persona_plugins (
	persona_id INTEGER NOT NULL REFERENCES room_personas(id) ON DELETE CASCADE,
	plugin_id INTEGER NOT NULL REFERENCES room_plugins(id) ON DELETE CASCADE,
	PRIMARY KEY (persona_id, plugin_id)
);

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

CREATE TABLE recovery_links (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at_ms INTEGER NOT NULL,
	used_at TEXT
);

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
, dismissed_by TEXT NOT NULL DEFAULT '');

CREATE TABLE room_blocks (
	room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	created_at TEXT NOT NULL,
	by_participant_id INTEGER REFERENCES participants(id) ON DELETE SET NULL,
	PRIMARY KEY (room_id, profile_id)
);

CREATE TABLE tts_samples (
	voice TEXT NOT NULL,
	lang TEXT NOT NULL CHECK (lang IN ('fr', 'en')),
	sha256 TEXT NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY (voice, lang)
);

CREATE TABLE pronunciations (
	id INTEGER PRIMARY KEY,
	term TEXT NOT NULL UNIQUE COLLATE NOCASE CHECK (length(term) BETWEEN 1 AND 40),
	style TEXT NOT NULL CHECK (style IN ('en', 'fr')),
	fr_en TEXT NOT NULL DEFAULT '',
	fr_fr TEXT NOT NULL DEFAULT '',
	en TEXT NOT NULL DEFAULT '',
	by TEXT NOT NULL DEFAULT '',
	at TEXT NOT NULL DEFAULT '',
	CHECK (fr_en != '' OR fr_fr != '' OR en != '')
);

CREATE INDEX invites_room ON invites(room_id);

CREATE INDEX participants_room ON participants(room_id);

CREATE INDEX participants_invite ON participants(invite_id);

CREATE INDEX messages_room_id ON messages(room_id, id);

CREATE INDEX uploads_room ON uploads(room_id);

CREATE INDEX documents_room ON documents(room_id, id);

CREATE INDEX annotations_document ON annotations(document_id);

CREATE INDEX annotations_room ON annotations(room_id);

CREATE INDEX annotation_messages_annotation ON annotation_messages(annotation_id, id);

CREATE INDEX attachments_message ON attachments(message_id);

CREATE INDEX daemons_room ON daemons(room_id);

CREATE INDEX turns_room ON turns(room_id, id);

CREATE INDEX profile_sessions_profile ON profile_sessions(profile_id);

CREATE INDEX magic_links_profile ON magic_links(profile_id);

CREATE INDEX participants_profile ON participants(profile_id);

CREATE INDEX messages_change ON messages(room_id, change_seq);

CREATE INDEX message_versions_message ON message_versions(message_id, seq);

CREATE INDEX persona_templates_profile ON persona_templates(profile_id);

CREATE UNIQUE INDEX room_personas_name ON room_personas(room_id, name COLLATE NOCASE);

CREATE INDEX room_personas_template ON room_personas(template_id);

CREATE INDEX turns_persona ON turns(persona_id, id);

CREATE INDEX agent_sessions_room ON "agent_sessions"(room_id);

CREATE INDEX persona_change_requests_room ON persona_change_requests(room_id);

CREATE INDEX turn_queue_room ON turn_queue(room_id, id);

CREATE INDEX turn_queue_participant ON turn_queue(participant_id);

CREATE UNIQUE INDEX skills_personal_name ON skills(profile_id, name) WHERE profile_id IS NOT NULL;

CREATE INDEX skills_publisher ON skills(publisher_id);

CREATE INDEX skill_versions_author ON skill_versions(author_id);

CREATE INDEX room_persona_skills_skill ON room_persona_skills(skill_id);

CREATE INDEX turns_rerun_of ON turns(rerun_of);

CREATE UNIQUE INDEX turns_one_running ON turns(persona_id) WHERE status = 'running';

CREATE INDEX turn_queue_rerun_of ON turn_queue(rerun_of);

CREATE UNIQUE INDEX profiles_uuid ON profiles(uuid);

CREATE INDEX audit_log_at ON audit_log(at_ms);

CREATE INDEX audit_log_action ON audit_log(action, id);

CREATE INDEX messages_participant ON messages(participant_id, id);

CREATE INDEX rooms_creator ON rooms(creator_profile_id);

CREATE INDEX room_admins_profile ON room_admins(profile_id);

CREATE UNIQUE INDEX skills_shared_name ON skills(name) WHERE profile_id IS NULL AND room_id IS NULL;

CREATE UNIQUE INDEX skills_room_name ON skills(room_id, name) WHERE room_id IS NOT NULL;

CREATE TRIGGER room_personas_level_insert BEFORE INSERT ON room_personas WHEN NEW.model = '' OR NEW.effort = ''
	BEGIN SELECT RAISE(ABORT, 'a persona needs a model and an effort'); END;

CREATE TRIGGER room_personas_level_update BEFORE UPDATE OF model, effort ON room_personas WHEN NEW.model = '' OR NEW.effort = ''
	BEGIN SELECT RAISE(ABORT, 'a persona needs a model and an effort'); END;

CREATE TRIGGER persona_template_versions_level BEFORE INSERT ON persona_template_versions WHEN NEW.model = '' OR NEW.effort = ''
	BEGIN SELECT RAISE(ABORT, 'a persona needs a model and an effort'); END;

CREATE INDEX message_turns_queue ON message_turns(queue_id);

CREATE INDEX message_turns_turn ON message_turns(turn_id);

CREATE INDEX messages_turn ON messages(turn_id) WHERE turn_id IS NOT NULL;

CREATE UNIQUE INDEX documents_agent_name ON documents(room_id, name) WHERE kind = 'agent' AND deleted_at IS NULL;

CREATE INDEX document_renames_message ON document_renames(message_id);

CREATE INDEX starters_profile ON starters(profile_id);

CREATE INDEX annotation_message_turns_queue ON annotation_message_turns(queue_id);

CREATE INDEX annotation_message_turns_turn ON annotation_message_turns(turn_id);

CREATE UNIQUE INDEX room_personas_watcher ON room_personas(room_id) WHERE watches_notes = 1;

CREATE UNIQUE INDEX room_plugins_name ON room_plugins(room_id, name) WHERE removed_at IS NULL;

CREATE INDEX plugin_versions_inspecting ON plugin_versions(plugin_id) WHERE state = 'inspecting';

CREATE INDEX plugin_versions_state ON plugin_versions(plugin_id, state);

CREATE INDEX room_persona_plugins_plugin ON room_persona_plugins(plugin_id);

CREATE TRIGGER turns_purge_plugins AFTER UPDATE OF status ON turns WHEN NEW.status != 'running' BEGIN
	DELETE FROM room_plugins WHERE removed_at IS NOT NULL AND id NOT IN (
	SELECT v.plugin_id FROM turns t, json_each(t.plugins) j JOIN plugin_versions v ON v.id = json_extract(j.value, '$.version_id')
	WHERE t.status = 'running');
	DELETE FROM plugin_snapshots WHERE version_id IN (SELECT v.id FROM plugin_versions v WHERE v.state != 'inspecting'
	AND v.n < (SELECT max(w.n) FROM plugin_versions w WHERE w.plugin_id = v.plugin_id AND w.state = 'approved'))
	AND version_id NOT IN (SELECT json_extract(j.value, '$.version_id') FROM turns t, json_each(t.plugins) j WHERE t.status = 'running');
END;

CREATE INDEX plugin_fetches_room ON plugin_fetches(room_id);

CREATE UNIQUE INDEX plugin_fetches_update ON plugin_fetches(plugin_id) WHERE state = 'waiting';

CREATE INDEX recovery_links_profile ON recovery_links(profile_id);

CREATE UNIQUE INDEX invites_shareable ON invites(room_id, created_by) WHERE link IS NOT NULL AND revoked_at IS NULL;

CREATE INDEX api_tokens_profile ON api_tokens(profile_id);

CREATE INDEX api_tokens_expires ON api_tokens(expires_at_ms);

CREATE UNIQUE INDEX blocked_requests_open ON blocked_requests(room_id, domain) WHERE decision = '';

CREATE INDEX participants_seated ON participants(room_id) WHERE removed_at IS NULL;

CREATE INDEX room_blocks_profile ON room_blocks(profile_id);

PRAGMA user_version = 46;
