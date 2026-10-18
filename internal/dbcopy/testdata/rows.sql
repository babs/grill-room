-- One row or more in every table of sqlite-v46.sql, with the edge values db copy must carry:
-- NULLs, empty text and blobs, NUL bytes, invalid UTF-8, variable-width and unparsable
-- timestamps, max int64, non-ASCII text, a reference cycle (rooms, invites, participants) and a
-- turn re-running a later one.

INSERT INTO settings(key, value) VALUES ('admin_token_hash', 'h'), ('empty', '');

INSERT INTO profiles(id, recovery_hash, email, created_at, skills_disclaimer_at, uuid, max_rooms, name, email_nudge_until) VALUES
	(3, 'rh3', 'zoe@example.org', '2026-01-02T03:04:05Z', '', 'u-3', -1, 'Zoé 🔥', '2026-01-02T03:04:05.5+02:00'),
	(9, 'rh9', NULL, '2026-01-02T03:04:05.123456789Z', NULL, NULL, 9223372036854775807, NULL, NULL);

INSERT INTO rooms(id, name, created_at, host_participant_id, parallel_turns, quota_bytes, creator_profile_id, created_from) VALUES
	('r1', 'Grill', '2026-01-02T03:04:05.1Z', 4, 1, 9223372036854775807, 3, '{"kind":"import"}'),
	('r2', CAST(X'526F6F6DFF0021' AS TEXT), '2026-01-02T03:04:06Z', NULL, 0, NULL, NULL, NULL);

INSERT INTO invites(id, room_id, token_hash, created_at, revoked_at, created_by, link) VALUES
	(2, 'r1', 'ih2', '2026-01-02T03:04:05Z', NULL, 4, 'https://example.org/i'),
	(3, 'r1', 'ih3', '2026-01-02T03:04:05Z', 'yesterday', NULL, NULL);

INSERT INTO participants(id, room_id, invite_id, name, token_hash, joined_at, can_trigger, profile_id, last_seen_at, removed_at, end_reason) VALUES
	(4, 'r1', 2, 'Ana', 'ph4', '2026-01-02T03:04:05Z', 1, 3, '2026-01-02T03:04:07.25Z', NULL, NULL),
	(5, 'r1', 3, 'Bo', 'ph5', '2026-01-02T03:04:05Z', 0, NULL, NULL, '2026-01-03T00:00:00Z', 'left');

INSERT INTO daemons(id, room_id, participant_id, name, token_hash, created_at, approve_all) VALUES
	(6, 'r1', 4, 'laptop', 'dh6', '2026-01-02T03:04:05Z', 1);

INSERT INTO persona_templates(id, preset, profile_id, title, created_at) VALUES
	(1, 'critic', NULL, 'Critic', '2026-01-02T03:04:05Z'),
	(2, NULL, 3, 'Mine', '2026-01-02T03:04:05Z');

INSERT INTO persona_template_versions(id, template_id, n, author, name, badge, colour, text, model, effort, created_at) VALUES
	(1, 1, 1, 'server', 'Critic', 'C', '#fff', 'Be critical.', 'm', 'e', '2026-01-02T03:04:05Z');

INSERT INTO room_personas(id, room_id, template_id, template_version, name, badge, colour, text, model, effort, created_at, cap_daemon_id, voice_rate) VALUES
	(8, 'r1', 1, 1, 'Critic', 'C', '#fff', 'Be critical.', 'm', 'e', '2026-01-02T03:04:05Z', 6, 1.15);

INSERT INTO turns(id, room_id, participant_id, daemon_id, host, status, fresh, resume, message_cursor, annotation_cursor, prompt, started_at, ended_at, persona_id, rerun_of) VALUES
	(10, 'r1', 4, 6, 'h', 'done', 1, '', 0, 0, 'p', '2026-01-02T03:04:05Z', '2026-01-02T03:04:09Z', 8, 11),
	(11, 'r1', NULL, NULL, 'h', 'failed', 0, '', 0, 0, 'p', '2026-01-02T03:04:05Z', NULL, 8, NULL);

INSERT INTO messages(id, room_id, ts, kind, participant_id, sender, body, html, persona_id, turn_id, source) VALUES
	(20, 'r1', '2026-01-02T03:04:05.1+01:00', 'text', 4, 'Ana', CAST(X'6869FF00' AS TEXT), NULL, 8, 10, 'device'),
	(21, 'r1', 'not a time', 'system', NULL, '', '', '<p></p>', NULL, NULL, NULL);

INSERT INTO uploads(id, room_id, participant_id, filename, content_type, size, data, created_at) VALUES
	(30, 'r1', 4, 'a.bin', 'application/octet-stream', 3, X'00FF00', '2026-01-02T03:04:05Z');

INSERT INTO documents(id, room_id, kind, name, format, upload_id, author, created_at, deleted_at) VALUES
	(31, 'r1', 'upload', 'a.bin', 'image', 30, 'Ana', '2026-01-02T03:04:05Z', NULL),
	(32, 'r1', 'agent', 'notes.md', 'md', NULL, 'Critic', '2026-01-02T03:04:05Z', NULL);

INSERT INTO revisions(id, document_id, n, body, author, created_at) VALUES
	(33, 32, 1, '# Notes', 'Critic', '2026-01-02T03:04:05Z');

INSERT INTO annotations(id, room_id, document_id, revision, anchor, created_at) VALUES
	(34, 'r1', 32, 1, NULL, '2026-01-02T03:04:05Z');

INSERT INTO annotation_messages(id, annotation_id, author_kind, participant_id, author, body, created_at) VALUES
	(35, 34, 'participant', 4, 'Ana', 'why?', '2026-01-02T03:04:05Z');

INSERT INTO attachments(id, message_id, content_type, size, data, created_at) VALUES
	(36, 35, 'image/png', 0, X'', '2026-01-02T03:04:05Z');

INSERT INTO profile_sessions(id, profile_id, token_hash, created_at, expires_at_s) VALUES
	(37, 3, 'sh37', '2026-01-02T03:04:05Z', 1767323045);

INSERT INTO magic_links(id, profile_id, email, purpose, token_hash, created_at, expires_at_ms, used_at) VALUES
	(38, 3, 'zoe@example.org', 'signin', 'mh38', '2026-01-02T03:04:05Z', 1767323045000, NULL);

INSERT INTO message_versions(id, message_id, seq, body, written_at) VALUES
	(39, 20, 1, 'hi', '2026-01-02T03:04:05Z');

INSERT INTO agent_sessions(room_id, persona_id, session_id, data, updated_at, context_tokens, context_window, context_updated_at) VALUES
	('r1', 8, 's1', X'0001', '2026-01-02T03:04:05Z', NULL, 200000, NULL);

INSERT INTO persona_change_requests(id, room_id, persona_id, kind, patch, requested_by, created_at) VALUES
	(40, 'r1', 8, 'change', '{}', 'Ana', '2026-01-02T03:04:05Z');

INSERT INTO turn_queue(id, room_id, persona_id, participant_id, source, fresh, created_at, rerun_of) VALUES
	(41, 'r1', 8, 4, 'button', 0, '2026-01-02T03:04:05Z', 10);

INSERT INTO skills(id, name, profile_id, publisher_id, created_at, room_id) VALUES
	(42, 'sk', 3, NULL, '2026-01-02T03:04:05Z', NULL);

INSERT INTO skill_versions(id, skill_id, n, author, author_id, description, files, size, sha256, data, created_at) VALUES
	(43, 42, 1, 'Zoé', 3, 'd', 1, 2, 'abc', X'1F8B', '2026-01-02T03:04:05Z');

INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES (8, 42, 1);

INSERT INTO admin_sessions(id, token_hash, created_at, last_used_at, expires_at_s, user_agent, ip) VALUES
	(44, 'ah44', '2026-01-02T03:04:05Z', '2026-01-02T03:04:05Z', 1767323045, 'ua', '127.0.0.1');

INSERT INTO audit_log(id, at_ms, actor, action, target_kind, target_id, target_name, details) VALUES
	(45, 9223372036854775807, 'admin', 'room.delete', 'room', 'r9', 'Old', '{}');

INSERT INTO room_admins(room_id, profile_id, created_at) VALUES ('r1', 3, '2026-01-02T03:04:05Z');

INSERT INTO message_turns(message_id, persona_id, queue_id, turn_id, final_status, final_line_id) VALUES
	(20, 8, 41, 10, 'done', 21);

INSERT INTO push_subscriptions(id, participant_id, endpoint, p256dh, auth, all_messages, created_at) VALUES
	(46, 4, 'https://push.example.org/1', 'k', 'a', 1, '2026-01-02T03:04:05Z');

INSERT INTO document_renames(id, document_id, message_id, from_name, to_name, created_at) VALUES
	(47, 32, 21, 'a.md', 'notes.md', '2026-01-02T03:04:05Z');

INSERT INTO starters(id, builtin, profile_id, library, created_at) VALUES
	(48, 'fixture', NULL, 'builtin', '2026-01-02T03:04:05Z');

INSERT INTO starter_versions(id, starter_id, n, author, manifest, size, created_at) VALUES
	(49, 48, 1, 'server', '{}', 1, '2026-01-02T03:04:05Z');

INSERT INTO starter_files(version_id, name, data) VALUES (49, 'a', X'00');

INSERT INTO annotation_message_turns(message_id, persona_id, queue_id, turn_id, final_status, final_line_id) VALUES
	(35, 8, NULL, 10, 'failed', 20);

INSERT INTO room_plugins(id, room_id, name, source_url, ref, ref_name, private, created_by_name, created_at) VALUES
	(50, 'r1', 'pl', 'https://example.org/pl.git', 'abc', 'main', 1, 'Ana', '2026-01-02T03:04:05Z');

INSERT INTO plugin_versions(id, plugin_id, n, commit_sha, sha256, size, files, file_list, manifest, state, added_by_name, added_by_id, added_at, decided_by_name, decided_at) VALUES
	(51, 50, 1, 'abc', 'def', 1, 1, '[]', '{}', 'approved', 'Ana', 4, '2026-01-02T03:04:05Z', 'Ana', '2026-01-02T03:04:05Z');

INSERT INTO plugin_snapshots(version_id, data) VALUES (51, X'00');

INSERT INTO room_persona_plugins(persona_id, plugin_id) VALUES (8, 50);

INSERT INTO plugin_fetches(id, room_id, plugin_id, source_url, ref, state, reason, requested_by_name, requested_by_id, created_at) VALUES
	(52, 'r1', 50, 'https://example.org/pl.git', 'abc', 'failed', 'gone', 'Ana', 4, '2026-01-02T03:04:05Z');

INSERT INTO recovery_links(id, profile_id, token_hash, created_at, expires_at_ms, used_at) VALUES
	(53, 3, 'lh53', '2026-01-02T03:04:05Z', 1767323045000, '2026-01-02T03:04:06Z');

INSERT INTO api_tokens(id, profile_id, name, token_hash, scope, created_at) VALUES
	(54, 3, 'ci', 'th54', 'read', '2026-01-02T03:04:05Z');

INSERT INTO blocked_requests(message_id, room_id, persona, domain, count, decision) VALUES
	(21, 'r1', 'Critic', 'example.org', 2, 'room');

INSERT INTO room_blocks(room_id, profile_id, name, created_at, by_participant_id) VALUES
	('r1', 9, 'Troll', '2026-01-02T03:04:05Z', 4);

INSERT INTO tts_samples(voice, lang, sha256, data) VALUES ('v', 'fr', 'abc', X'52494646');

INSERT INTO pronunciations(id, term, style, fr_en, "by", at) VALUES (55, 'Kubernetes', 'en', 'kou-ber-né-tisse', '', '');

-- A deleted top message: SQLite never hands out its id again, nor must PostgreSQL.
UPDATE sqlite_sequence SET seq = 1000 WHERE name = 'messages';
