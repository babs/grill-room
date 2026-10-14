# grill-room

Self-hosted rooms for grill sessions. Guests join from an invite link with a display name, no account. The first join gives the browser a lightweight profile that follows its person from room to room. The room shows who is in, a live message stream and the room's documents.

This is an early PoC. `INTENT.md` has the design and `specs/001-grill-room-poc.md` the plan.

## Run

Go 1.26 or newer.

```sh
make build
./bin/grill-room
```

On first start the server logs an `admin_token_generated` line holding the admin token. It is shown once: the database keeps only its hash. Set `GRILL_ROOM_ADMIN_TOKEN` to pin a token instead. To get a new one, delete the `admin_token_sha256` row from `settings` and restart.

| Variable | Default | |
|---|---|---|
| `GRILL_ROOM_ADDR` | `127.0.0.1:7680` | UI, API, `/healthz` |
| `GRILL_ROOM_METRICS_ADDR` | `127.0.0.1:7681` | Prometheus `/metrics` |
| `GRILL_ROOM_DB` | `grill-room.db` | SQLite file |
| `GRILL_ROOM_URL` | `http://<GRILL_ROOM_ADDR>` | public base URL used in invite links |
| `GRILL_ROOM_ADMIN_TOKEN` | generated | admin token |
| `GRILL_ROOM_MAX_UPLOAD_MB` | `20` | largest upload, in MiB (413 above) |
| `GRILL_ROOM_MAX_ROOM_MB` | `200` | default room quota: all uploads and annotation images of one room, in MiB (413 above); the admin sets other quotas room by room |
| `GRILL_ROOM_MAX_ATTACHMENT_MB` | `5` | largest image on an annotation message, in MiB (413 above) |
| `GRILL_ROOM_MAX_IMPORT_MB` | `1024` | largest room archive an import takes, compressed and once uncompressed, in MiB (at least `GRILL_ROOM_MAX_ROOM_MB`) |
| `GRILL_ROOM_PANDOC` | `pandoc` | pandoc binary for DOCX, ODT and PPTX (3.8+ for PPTX) |
| `GRILL_ROOM_PANDOC_TIMEOUT` | `60s` | one conversion's time budget |
| `GRILL_ROOM_LIVEKIT_URL` | none | LiveKit signalling URL for browsers (`wss://...`); unset turns voice off |
| `GRILL_ROOM_LIVEKIT_API_URL` | `GRILL_ROOM_LIVEKIT_URL` as `https://` | LiveKit's HTTP API as the server reaches it |
| `GRILL_ROOM_LIVEKIT_API_KEY`, `GRILL_ROOM_LIVEKIT_API_SECRET` | none | LiveKit API key pair; the secret needs 32+ characters |
| `GRILL_ROOM_STT_TOKEN` | none | the STT worker's service token; unset refuses every worker |
| `GRILL_ROOM_TURN_TIMEOUT` | `15m` | one agent turn's time budget (1m to 24h) |
| `GRILL_ROOM_NOTES_WATCH_DELAY` | `20s` | how long the persona that watches the notes waits after the last new note message (1s to 1h) |
| `GRILL_ROOM_DAEMON_DIR` | none (`/daemon` in the image) | daemon release written by `make release` or `make dev-release`, or downloaded from a GitHub release; unset offers no download |
| `GRILL_ROOM_SANDBOX_IMAGE` | none | sandbox image advertised to daemons, pinned by digest (`<repo>[:tag]@sha256:<hex>`); unset lets each daemon build it |
| `GRILL_ROOM_SMTP_ADDR` | none | SMTP relay (`host:port`) for magic links; unset turns email sign-in off |
| `GRILL_ROOM_SMTP_FROM` | none | sender address, needed with `GRILL_ROOM_SMTP_ADDR` |
| `GRILL_ROOM_SMTP_USERNAME`, `GRILL_ROOM_SMTP_PASSWORD` | none | SMTP login, both or neither; sent only after STARTTLS, unless the relay is on localhost |
| `GRILL_ROOM_MAGIC_LINK_TTL` | `15m` | how long an emailed link works, in whole seconds (1s to 1h) |
| `GRILL_ROOM_AUDIT_RETENTION` | `8760h` | how long the audit log keeps an entry (24h or more) |
| `GRILL_ROOM_CLIENT_RETENTION` | `720h` | how long a participant's IP and user agent are kept (24h or more) |
| `GRILL_ROOM_PUSH_HOSTS` | Chrome, Firefox, Safari and Edge push services | comma-separated host suffixes a Web Push subscription may post to; any other is refused, as the server posts to it |
| `GRILL_ROOM_TRUSTED_PROXIES` | none | comma-separated CIDRs or addresses of the reverse proxies whose `X-Forwarded-For` names the client; unset uses the peer address |
| `LOG_LEVEL` | `info` | |
| `LOG_FORMAT` | `auto` | `console` on a TTY, `json` otherwise |

`/healthz` returns 200 when SQLite accepts a write and 503 otherwise.

## Admin page

`/admin` asks for the admin token once and then works on a session cookie: HttpOnly, SameSite=Strict, Secure over https, sent only to `/api/admin/`, valid 12 hours. The database keeps only the session token's hash. The page lists the open sessions with their IP and browser, and revokes any of them; a revoked browser gets a 401 on its next request and lands back on the login form. A client IP gets 10 login attempts per 15 minutes, and a success resets its count. The bearer token keeps working for `curl` and scripts, on every admin route.

The page has six views, in a tab bar: Rooms, Profiles, Libraries, Instance, Sessions and Audit. Each is a deep link (`/admin#profiles`), and so is each room and each of its tabs (`/admin#rooms/<room>/invites`).

- Rooms: name, creation, last line, participants, personas and whether the host's daemon is online, sortable by any column. "Manage" opens a room in its own view with four tabs. General shows its volume by kind (uploads, revisions, attachments, agent sessions, transcripts, room skills, plugins) and its quota; there the admin renames it, sets its quota or returns it to the default, and deletes it after typing its name. Invites creates and revokes invites. Admins appoints and removes room admins among the participants with a profile, and renames any participant. Export exports it. Above the list, "Import a room" previews an archive and imports it as a new room (see Export and import).
- Profiles: the name a profile chose in a room, else the one it last joined with, its email, rooms, skill count (personal ones plus the shared ones it published) and last activity. Deleting a profile leaves its participants in their rooms, detached. Each row also shows how many rooms the profile created, and sets its own limit on them: a number, unlimited, or back to the instance's. This is the only place an email shows, besides its owner's profile page.
- Libraries: persona templates and shared skills, with author and latest version. The admin removes any but the shipped templates.
- Instance: server version, database size (with its write-ahead log), free and total space on its disk, speech to text, connected daemons by version, and whether SMTP is set. Below, the room creation limits: who creates rooms (anyone with a profile, or the admin only), how many rooms a profile may have created (10 by default), and an optional storage quota. Past that quota, which counts the database pages in use, profiles create no room.
- Sessions and the audit log, below.

A room's quota counts its uploads and annotation images, the blobs people add. Revisions, transcripts and agent sessions show in its volume but do not count. Only the admin changes a quota; a participant asking through the room's API gets a 403. The volume comes from SQL sums over lengths, so the view reads no blob.

The audit log records every admin action: room create, rename, quota change, export, import and delete, invite create and revoke, room admin appointment and removal, renames of participants by a room admin or the admin, document deletion and restoration, room skill removal, profile delete and rooms limit, template and skill removal, room creation limits, and admin logins, logouts and session revocations. Each entry holds who (`super-admin`, or `profile:<uuid>` for a room admin, with the name they acted as), the action, the target's kind, id and name at the time, details as JSON (4 KiB at most) and the time. An entry is written in the same transaction as its change: if the entry cannot be written, the change is rolled back and the request gets a 500. Deleting a room or a profile keeps the entries about it, with the old name. The server purges entries older than `GRILL_ROOM_AUDIT_RETENTION`, and expired sessions, at start and then daily. `/admin` shows the log read-only, newest first, filtered by action and by target id or name.

The page is Preact, htm and @preact/signals, served from `internal/uivendor/esm/` like the daemon's page, and so is the room page's Room settings sheet. `make ui-check` runs `tsc --checkJs` on both.

## Rooms and invites

The admin token creates rooms and the first invites:

```sh
curl -H "Authorization: Bearer $ADMIN" --json '{"name":"Pricing review"}' localhost:7680/api/rooms
curl -H "Authorization: Bearer $ADMIN" -X POST localhost:7680/api/rooms/$ROOM/invites
```

The second call returns a `url` such as `https://<host>/join#<token>`. Whoever opens it picks a display name and lands in the room. One invite can be shared with several people.

Once in, any member can create invites from the room page ("Invite people") and revoke the ones they created. The admin revokes any. Revoking an invite also revokes the invites created by those who joined through it, and so on down the chain, so a member cut off cannot come back through an invite they made.

A profile can also create a room from `/profile` ("New room"), within the limits set in `/admin`. It joins the room under the name it picks, through an invite of its own, and becomes the room's first room admin. A room admin opens "Room settings" from the room page's `⋯` menu, a wide sheet (full screen on a phone) with tabs: General (rename the room, copy its id, see its storage against its limit, delete it after typing its name), Invites (list and revoke every invite), Admins (appoint other members as room admins or remove them, rename any participant), Personas, Skills, Plugins, Export and Words to recognise (the glossary). Each tab is a deep link, `#manage/invites`. A plain member's Room settings hold the Plugins tab, read only, and the glossary tab while an STT worker reads the glossary: with parakeet, none does, so that tab is hidden. The room keeps at least one room admin: the last one cannot step down. Room admin rights belong to profiles, so a member without one cannot be appointed. Each of these actions is a line in the chat and an audit log entry. The quota stays the admin's. A room the admin created has no room admin until the admin appoints one from `/admin`.

```sh
# Revoke an invite: everyone who joined through it gets a 401 and their live stream closes.
curl -H "Authorization: Bearer $ADMIN" -X DELETE localhost:7680/api/rooms/$ROOM/invites/$INVITE_ID
# Delete a room and everything stored for it.
curl -H "Authorization: Bearer $ADMIN" -X DELETE localhost:7680/api/rooms/$ROOM
```

| Route | Auth | |
|---|---|---|
| `GET /api/rooms`, `POST /api/rooms` | admin | list, create |
| `DELETE /api/rooms/{room}` | admin or room admin | delete with all its data; 403 for another participant |
| `PATCH /api/rooms/{room}` | admin or room admin | rename: `name`; 403 for another participant |
| `GET /api/rooms/{room}/invites` | admin or participant | every invite for the admin and room admins, a participant's own for anyone else; `created_by` is the participant who created it, absent for the admin |
| `POST /api/rooms/{room}/invites` | admin or participant | new invite link |
| `DELETE /api/rooms/{room}/invites/{id}` | admin, room admin, or the participant who created it | revoke, with the invites its joiners created; 403 for another participant's invite |
| `GET /api/rooms/{room}/overview` | room admin | the room as `GET /api/admin/rooms` shows it: volume by kind, `quota`, `used` |
| `PUT /api/rooms/{room}/admins/{participant}`, `DELETE` | admin or room admin | appoint the participant's profile as a room admin, or remove it; 404 for someone not in the room, 409 for a participant without a profile or for the last room admin stepping down (the admin may remove the last one) |
| `GET /api/admin/rooms/{room}/participants` | admin | the room's participants with `admin` and `has_profile`, for appointing room admins, and `host` and `can_host` (a connected daemon of theirs can host) |
| `GET /api/rooms/{room}/export`, `GET /api/admin/rooms/{room}/export` | admin or room admin | the room as a `.tar.gz` (docs/export-format.md); 503 while two exports or imports run |
| `POST /api/rooms/import`, `POST /api/admin/rooms/import` | admin, or a profile cookie within the room creation limits | an `application/gzip` body holding an export; `?preview=1` runs every check and creates nothing; `?display_name=` seats a profile that is not one of the room's participants. 201 with the new `room`, or 200 for a preview, with `counts`, `bytes`, `warnings`, `format`, `instance_version`, `same_instance`; 413 past `GRILL_ROOM_MAX_IMPORT_MB` compressed, 422 with the reason for a refused archive, 403 past the creation limits |
| `POST /api/profile/rooms` | profile cookie | create a room: `name`, and `display_name` to join it as; the profile becomes its room admin; 403 with the reason when creation is the admin's only, the profile has created as many rooms as allowed (its own limit, or the instance's), or the instance is at its storage quota. From a starter (the editor): `starter` (`id`, `version`; absent for Blank), `fields` (key to value), and what the editor changed: `agenda`, `personas`, and the names of the `documents`, `skills` and `plugins` to keep; `{{key}}` in the name and the agenda becomes the field's value; 400 with the reason for a required field left empty, a persona the room API would refuse or a skill a kept persona uses but the room leaves out, 404 for a starter the profile does not see |
| `GET /api/admin/settings`, `PUT /api/admin/settings` | admin | room creation limits: `rooms_per_profile` (1 to 1000), `storage_quota_bytes` (0 for none), `room_creation` (`anyone` or `super-admin`), `plugin_max_mb` (1 to 200, 20 by default: the plugin snapshot cap); a field left out keeps its value |
| `GET /api/profiles` | admin | every profile with its `email`, `rooms`, `uuid`, `name` (the one it last joined with), `skills` and `last_activity` |
| `DELETE /api/profiles/{id}` | admin | delete a profile, see Personal data |
| `PUT /api/profiles/{id}/max_rooms`, `PUT /api/admin/profiles/{id}/max_rooms` | admin | the profile's own limit on the rooms it creates: `max_rooms` 1 to 1000, -1 for unlimited, or null for the instance's; the profiles list shows it as `max_rooms`, with `rooms_created` |
| `POST /api/admin/login` | admin token in the body (`token`) | opens a 12 h session and sets its cookie; 401 for a wrong token, 429 past 10 attempts per client IP in 15 minutes |
| `POST /api/admin/logout` | admin | ends the caller's session and clears its cookie |
| `GET /api/admin/sessions`, `DELETE /api/admin/sessions/{id}` | admin | the open sessions (`current` marks the caller's); revoke one |
| `GET /api/admin/rooms` | admin | every room with `participants`, `personas`, `last_activity`, `host_online`, `volume` by kind in bytes, `quota` and `used` (what counts against it), and `quota_bytes` (0 on the default); plus `default_quota` |
| `PUT /api/rooms/{room}/quota`, `PUT /api/admin/rooms/{room}/quota` | admin; a participant gets 403 on the first, 401 on the second | `quota_bytes`: 1 byte to 1 TiB, or 0 for the default |
| `GET /api/admin/instance` | admin | `version`, `db_bytes`, `disk_free` and `disk_total` (-1 when unknown), `stt` (`voice`, `configured`, `workers`), `daemons` (`connected`, `versions`), `smtp` |
| `/api/admin/rooms/{room}`, `.../invites`, `/api/admin/profiles`, `/api/admin/templates`, `/api/admin/skills` | admin | the same routes as without `/admin`, where the page's cookie reaches them |
| `GET /api/admin/audit` | admin | the audit log, newest first: `entries`, `next` (the `before` of the next page, 0 on the last), `actions`; filters `action`, `target` (an id, or part of a name), `before`, `limit` (1 to 200, default 50) |
| `POST /api/join` | invite token in the body | returns a participant token and sets the room cookie; without a valid profile cookie, also creates a profile, sets its cookie and returns `recovery_url` once |
| `GET /api/profile` | profile cookie, optional | `email_enabled`, `magic_link_ttl_s` with SMTP, and `profile` (`id`, own `email`, `rooms`, `pending_email` while its newest bind link is unused and unexpired, `email_nudge_until` once snoozed) or null |
| `POST /api/profile/recover` | recovery token in the body | signs this browser into the profile, with its permanent recovery token or a one-time recovery link from the admin; 401 "this recovery link is invalid" otherwise |
| `POST /api/profile/nudge` | profile cookie | hides the add-an-email banner for 14 days on every device of the profile; 204 |
| `POST /api/admin/profiles/{id}/recovery-link` | admin | a one-time recovery link for the profile, valid 24 h: `url`, `expires_at`, shown once; voids the profile's earlier unused one; audited without the token; 404 for an unknown profile |
| `POST /api/profile/email` | profile cookie | mails a link that binds `email` to the profile; 404 without SMTP |
| `POST /api/profile/signin` | none | mails a sign-in link if a profile holds `email`; 202 either way; 404 without SMTP |
| `GET /api/profile/skills` | profile cookie | the profile's personal skills and the shared ones it published, each at its latest version |
| `POST /api/profile/skills` | profile cookie | upload a skill folder as an `application/zip` body, personal, or shared with `?scope=shared`; a name the profile already uploaded in that scope becomes its next version; 400 with the reason for an invalid zip, 409 for the same files as the latest version, a shared name another profile published, or past 20 skills per profile or 20 versions per skill |
| `DELETE /api/profile/skills/{id}` | profile cookie | remove one of the profile's personal skills; 404 for a shared one |
| `GET /api/profile/starters` | profile cookie | the room starters the profile sees: the built-ins, its own (`personal`) and the shared ones (`instance`), each at its latest version, with its author, version count and size |
| `GET /api/profile/starters/{starter}` | profile cookie | one starter, `0` for Blank, with the models and efforts the editor offers and the `manifest` of its version `?v=`, the latest by default: title, room name, agenda, fields, personas, skills, plugins, documents, auto-turn settings; 404 for another profile's personal starter |
| `POST /api/profile/starters/{starter}/duplicate` | profile cookie | copy a starter the profile sees into its personal library as version 1; 409 past 50 starters per profile or the instance storage quota |
| `POST /api/profile/starters/{starter}/publish` | profile cookie | move one of the profile's personal starters, with its versions, to the shared library, audited; 403 for a built-in or someone else's |
| `DELETE /api/profile/starters/{starter}` | profile cookie | delete one of the profile's starters, personal or shared (audited); rooms created from it keep their copy |
| `GET /api/admin/starters`, `DELETE /api/admin/starters/{starter}` | admin | list the shared starters, and remove one, audited; 403 for a built-in or a personal starter |
| `POST /api/profile/starters/{starter}/versions` | profile cookie | save the editor's changes to one of the profile's starters as its next version: `version` it was opened on, then `title`, `description`, `room_name`, `agenda`, `fields` (`label`, `key`, `required`, `hint`), `personas`, and the names of the `documents`, `skills` and `plugins` of that version to keep, a plugin left out leaving its personas; 400 naming an undeclared `{{key}}` in the agenda or the room name; 404 for someone else's personal starter, 403 for a built-in or someone else's shared one, 409 when a newer version exists than the one opened |
| `GET /api/rooms/{room}/starter-draft` | room admin | the room as "Save as starter" opens it: its personas with their triggers, active skills and plugins, those skills and the room's own, its plugins at the version in use, `agenda.md` as the agenda, its other documents at their current revision, auto-turn settings and words to recognise; never messages, annotations, sessions, participants or invites; 403 for a plain member |
| `POST /api/rooms/{room}/starters` | room admin | save the room as a new starter in the admin's personal library, with the same body as a new version, the documents named taken at their current revision; 403 for a plain member or without a profile |
| `GET /api/skills`, `DELETE /api/skills/{id}` | admin | every skill; remove one with its versions, and from every persona that had it |
| `POST /api/profile/magic` | link token in the body | spends an emailed link: a sign-in link signs this browser in, a bind link binds the address to the profile this browser holds; from any other browser a bind link answers 409 `reason: confirm` with `profile_name` and `email`, then binds with `confirm: true` (`bound`, `email`, `profile_name`, this browser unchanged); 401 `reason: invalid`, 410 `reason: expired` or `used`, 409 `reason: taken` when another profile holds the address; 404 without SMTP |
| `GET /api/rooms/{room}` | participant | room, me, participants; `admin` marks the room admins, `previous_name` is a participant's name before their latest rename |
| `POST /api/rooms/{room}/messages` | participant | post a text message (Markdown); messages carry `body` (source) and `html` (sanitised render) |
| `PATCH /api/rooms/{room}/messages/{id}` | author | edit your text or voice line: `body`; 403 for anyone else, 409 once deleted |
| `DELETE /api/rooms/{room}/messages/{id}` | author | soft delete: participants see `deleted_at` and no body; the agent keeps the text |
| `GET /api/rooms/{room}/messages/{id}/versions` | participant | earlier bodies of an edited message, oldest first |
| `GET /api/rooms/{room}/events` | participant | SSE: history then live messages, each with what it `named` (the personas and members its `@name`s name, as `at`, `persona_id` or `participant_id`) and the `mentions` its `@name`s queued, with their turn's `status` and `line_id`; resumes after `?since=<id>` or `Last-Event-ID`; a `message_edited` event with the message for each edit or delete past `?edits=<change_seq>` (all of them on a resume without it);  a `documents` event when a document or revision is added, an `annotations` event when an annotation is added or changes, a `plugins` event when a plugin is added or removed, a version is added or changes state, a persona's plugin is turned on or off, a plugin starts or stops needing host setup, or a request to the AI host is made, fails or is done (the page then reads `GET .../plugins`); an `agent` event with the agent state (`host_participant_id`, `online`, `reason`, last `turn`, the `running` turns (each with `host_queued`, `rerun_of` and `cause`), `parallel_turns`, `can_trigger`, connected `daemons`, the room's `personas`, the `queue`, the `models` and `efforts` a persona may ask for, the host daemon's `cap`, the `cap_requests` of personas asking above it, the persona `changes` waiting for the host, the `room_skills`, those waiting for the host included, the `note_mentions` (the turn states of the latest note messages' `@name`s, by `message_id`), `notes_watch_delay` in seconds, and `host_by`, the chat line of the latest move of the AI host role when the previous host did not hand over) at open and on every change; with voice on, an `stt` event (`available`, `listening`, `transcribed`, the live workers' `engines`, and `glossary`, true while one reads the glossary) at open and on every change |
| `GET /api/rooms/{room}/agent` | participant | the agent state, as in the `agent` event |
| `PATCH /api/rooms/{room}/agent` | host | `parallel_turns`: personas run at the same time, or one at a time |
| `POST /api/rooms/{room}/turns` | host, or a participant the host allowed | a persona's button: `persona_id` (the room's first persona when absent), `fresh` (host only) starts a new session for it; 201 with the turn, or 202 with the queued press while another turn runs or others wait (in parallel mode, while this persona runs or waits); 409 when that persona is queued already, waits for the host's approval, or no host daemon is connected |
| `DELETE /api/rooms/{room}/queue/{id}` | who queued it | cancel a queued press |
| `GET /api/rooms/{room}/templates` | participant | the persona template library at each template's latest version; `mine` marks the ones this participant's profile published |
| `POST /api/rooms/{room}/templates` | participant with a profile | publish `name`, `badge`, `colour`, `text`, `model`, `effort` as a new template (`title` optional), or with `template_id` as the next version of one this profile published (403 otherwise); signed with the participant's name; 409 past 50 templates per profile or 100 versions per template |
| `GET /api/templates`, `DELETE /api/templates/{id}` | admin | the library; remove a published template (409 for a shipped one); rooms keep their copies |
| `POST /api/rooms/{room}/personas` | participant | add a copy of `template_id`'s latest version; a name the room uses gets a number; `pending` while the host approves everything; 409 past 12 personas |
| `PATCH /api/rooms/{room}/personas/{id}` | participant | change `name` (one word), `badge`, `colour` (`#rrggbb`), `text`, `model` (`haiku`, `sonnet`, `opus`, `fable`), `effort` (`low`, `medium`, `high`, `xhigh`, `max`), one automatic trigger among `every_n` (answers), `every_voice` (voice lines) and `every_minutes` (with someone here and new activity), the other two turning off, `self_call`, `watches_notes` (true makes it the room's notes watcher, in place of any other); applies from the persona's next turn; 202 with `held` when it waits for the host's approval; 409 for a name taken |
| `POST /api/rooms/{room}/personas/{id}/update` | participant | take the text, model and effort of the template's newer version, keeping the room's name, badge and colour; 202 with `held` as for a change |
| `POST /api/rooms/{room}/personas/{id}/cap` | host | answer a persona asking above the host cap: `approve`, and the `model` and `effort` the banner showed (409 when the persona asks for something else by now, or runs as it asks already) |
| `POST /api/rooms/{room}/changes/{id}` | host | `approve` or refuse a held persona change or new persona |
| `GET /api/rooms/{room}/skills` | participant | the skills this participant can turn on: the shared library, their profile's personal skills and the room's own skills the host approved; each has a `scope` (`shared`, `personal`, `room`); `disclaimer` is true until their profile accepts the skills disclaimer, `no_profile` when they have none |
| `POST /api/rooms/{room}/skills` | participant with a profile | copy `skill_id`, one of their personal skills, into the room at its latest version: 201, or 202 with `pending` while the host approves everything; 404 for a skill that is not their personal one, 409 for a name the room has or past 20 room skills |
| `POST /api/rooms/{room}/skills/{id}/decision` | host | `approve` or refuse a pending room skill; refused, it is removed |
| `DELETE /api/rooms/{room}/skills/{id}` | room admin | remove a room skill, and from every persona that had it |
| `PUT /api/rooms/{room}/personas/{id}/skills/{skill}` | participant with a profile | turn a skill on at its latest version, or move it there from an older pin; `accept_disclaimer` the first time (428 without it); a persona change, 202 with `held` as for one; 404 for a skill not offered to this participant, 409 for a second skill of the same name or past 10 per persona |
| `DELETE /api/rooms/{room}/personas/{id}/skills/{skill}` | participant | turn a skill off |
| `GET /api/rooms/{room}/plugins` | participant | the room's plugins: `name`, `url`, `ref` as typed, `ref_name` as shown, `entry` (its marketplace entry), `private`, the `personas` it is on for, `running` (the version turns load, absent for none), `needs_setup` (the last turn left it out for an option the AI host has not set), `why` it does not load, and its `versions`, each with `n`, `commit`, `short_commit`, `commit_url` (GitHub and GitLab), `size` (as fetched; a version older than the running one drops its files once no turn loads it), `files`, `state` (`inspecting`, `pending`, `approved`, `refused`, `invalid`), `reason`, `manifest` (what the server read), `report` (what the host's daemon found), who added and decided it and when; a version newer than the running one has its `change` against it and a `compare_url`. Also `fetches`, the room's requests to the AI host: `url`, `ref`, `entries`, `personas`, `plugin_id` and `plugin_name` for an update, `state` (`waiting` or `failed`), the failure's `reason`, `requested_by`, `created_at` |
| `POST /api/rooms/{room}/plugins/check` | room admin | fetch `url` at `ref` (empty for the default branch) and list its `plugins`: a marketplace's entries, or the repo as one plugin, each `pickable` or with the `reason` it is not, with its `manifest`, `size` and `files`; also `commit`, `ref_name`, `marketplace`; 400 for a link with credentials or not https, 422 with `host_fetch` for a repo the server cannot read anonymously or an ssh link, 422 with the reason when the fetch or the snapshot fails, 409 while another fetch runs in the room, 503 past two at once on the server |
| `POST /api/rooms/{room}/plugins` | room admin | add the `entries` picked from the marketplace at `url` and `ref` (a plain repo needs none), pinned at the commit the ref resolves to now, on for the `personas` listed; each waits for the host's daemon to inspect it; answers as the check does when the fetch fails; 409 past 10 plugins per room or 5 per persona, or for a name the room or the persona has; 413 past the room's quota |
| `POST /api/rooms/{room}/plugins/host-fetch` | room admin | ask the AI host to fetch `url` (https, `ssh://` or `user@host:path`) at `ref`, adding the marketplace `entries` named, or the repo as one plugin when none is, on for the `personas` listed: 202 with the request, which waits until the host presses Fetch on its daemon page; 400 for a link with credentials or another scheme, 409 past 10 requests in the room |
| `DELETE /api/rooms/{room}/plugin-fetches/{id}` | room admin | cancel a request still waiting for the AI host, or dismiss a failed one |
| `POST /api/rooms/{room}/plugins/{id}/update` | room admin | re-resolve the plugin's ref: `{"up_to_date": true}` for the same commit, else 201 with the plugin and its new version, waiting for inspection while the approved one keeps running; for a private plugin, 202 with `host_fetch`, a request to the AI host, and 409 while one waits already |
| `DELETE /api/rooms/{room}/plugins/{id}` | room admin | remove a plugin: no persona loads it from its next turn; its snapshots go once no running turn loads them |
| `POST /api/rooms/{room}/plugins/{id}/versions/{n}/decision` | host | `approve` or refuse a `pending` version; 409 for one in another state or older than the running one |
| `PUT /api/rooms/{room}/personas/{id}/plugins/{plugin}`, `DELETE /api/rooms/{room}/personas/{id}/plugins/{plugin}` | room admin | turn a plugin on or off for a persona; 409 past 5 per persona |
| `DELETE /api/rooms/{room}/personas/{id}` | participant | remove a persona with its session and queued press; 409 during its turn |
| `PUT /api/rooms/{room}/host`, `PUT /api/admin/rooms/{room}/host` | host, a room admin or admin | make `participant_id` the AI host; their daemon must be connected and able to host (409 otherwise). The chat says "Alice made Bob the AI host", "Alice took the AI host role" when a room admin takes it for their own app, or "Bob holds the agent(s)." when the host hands over; audited unless a host who is no room admin does it. A running turn finishes on the previous host's daemon; queued and new turns go to the new host. 403 for a plain member who does not host |
| `PATCH /api/rooms/{room}/participants/{id}` | host | `can_trigger`: grant or revoke the agent button |
| `PUT /api/rooms/{room}/participants/{id}/name`, `PUT /api/admin/rooms/{room}/participants/{id}/name` | the participant, a room admin or admin | `name`, validated as at join; `profile` true also makes it the name of the caller's own profile. A line in the chat; audited when it renames someone else. 403 for a plain member renaming another, 404 for someone not in the room |
| `POST /api/rooms/{room}/presence` | participant | a tab's heartbeat, every 10 s and on each focus change: `tab` id, `audio`, `mic`, `muted`, `deaf`, `focused`, and `gone` as the page closes; 429 past 20 tabs |
| `GET /api/rooms/{room}/push` | participant | the instance's VAPID public `key`, for `pushManager.subscribe` |
| `PUT /api/rooms/{room}/push`, `DELETE` | participant | store this browser's Web Push `subscription` (as `PushSubscription.toJSON()` gives it) for this room, with `all_messages` to get every line; DELETE with `subscription.endpoint` removes it. 400 for a malformed subscription or a push service outside `GRILL_ROOM_PUSH_HOSTS`. Ten browsers per participant at most: past that the oldest goes |
| `PATCH /api/rooms/{room}/chain` | host, or a participant the host allowed | `budget` (1 to 1000), `rearm`, or `suspended`: the agent chain's limit; returns the chain |
| `POST /api/rooms/{room}/turns/{id}/stop` | who started the turn, the host or a room admin | stop a running turn: nothing of it is applied; 409 when it no longer runs |
| `GET /api/rooms/{room}/glossary`, `PUT` | participant | the room's transcription `glossary`, the `prompt` it makes and the terms it fits (`used`, `total`, `capacity`); PUT sets `glossary`, 400 past 2000 characters or 60 per term |
| `GET /api/rooms/{room}/voice` | participant | LiveKit `url` and a 5 min `token` for this room's audio; 404 when voice is off |
| `GET /api/rooms/{room}/documents` | participant | list |
| `POST /api/rooms/{room}/documents` | participant | upload, multipart field `file` |
| `GET /api/rooms/{room}/documents/{id}` | participant | document and its revisions |
| `PATCH /api/rooms/{room}/documents/{id}` | participant | rename: `name`, checked as an upload's, with the same extension and no other document of the room holding it; the id, revisions and annotations stay. 409 for a name taken, or for an agent document while a turn runs |
| `DELETE /api/rooms/{room}/documents/{id}`, `POST /api/rooms/{room}/documents/{id}/restore` | admin or room admin | delete a document softly, or restore it: its data stays and counts in the volume. 409 for an agent document while a turn runs, or a restore whose name another document took |
| `GET /api/rooms/{room}/documents/deleted` | admin or room admin | the deleted documents, to restore |
| `GET /api/rooms/{room}/documents/{id}/view` | participant | rendered page; `?rev=N` picks a revision, `&from=M` shows the word diff from M |
| `GET /api/rooms/{room}/documents/{id}/original` | participant | the uploaded bytes |
| `GET /api/rooms/{room}/documents/{id}/annotations` | participant | annotations with their threads; each message has the `mentions` its `@name`s queued, with their turn's `status` and `line_id` |
| `POST /api/rooms/{room}/documents/{id}/annotations` | participant | new annotation: `anchor` (null for the whole document), `revision`, `body`, `attachments`; the answer's `notices` say, to the author only, why a persona named will not run |
| `POST /api/rooms/{room}/annotations/{id}/messages` | participant | reply: `body`, `attachments`; `notices` as for a new annotation |
| `PATCH /api/rooms/{room}/annotations/{id}` | participant | `status`: `done` or `reopened` |
| `GET /api/rooms/{room}/attachments/{id}` | participant | an annotation image |
| `POST /api/rooms/{room}/pairings` | none | a daemon asks to pair: `name`; returns `id`, `secret`, `code`, `approve_url` |
| `GET /api/pairings/{id}` | pairing secret | `pending`, `denied`, or `approved` with the daemon token, handed once |
| `GET /api/rooms/{room}/pairings/{id}` | participant | the request's daemon name and code, for the approval page |
| `POST /api/rooms/{room}/pairings/{id}` | participant | `approve`: true pairs the daemon to this participant, false denies |
| `GET /api/rooms/{room}/daemon/events` | daemon | SSE: `hello` (`daemon`, `heartbeat_seconds`, `room_name`; servers before 005-AC42 send no `room_name`), pings, `turn` (`id`) when a turn starts on this daemon, `inspect` (`id`) to the host daemon for each plugin version waiting for its inspection, `revoked` |
| `GET /api/rooms/{room}/daemon/turns/{id}` | daemon running the turn | the job: `prompt`, `resume` session id, its `session_size` and `session_sha256`, `timeout_seconds` |
| `GET /api/rooms/{room}/daemon/turns/{id}/workspace` | daemon running the turn | the workspace as a tar, see [docs/agent-workspace.md](docs/agent-workspace.md) |
| `GET /api/rooms/{room}/daemon/turns/{id}/manifest` | daemon running the turn | the workspace files with their sha256 and size, for a daemon with a cache |
| `GET /api/rooms/{room}/daemon/turns/{id}/skills` | daemon running the turn | the turn's pinned skills as a tar, the plugin directory `claude -p` loads |
| `GET /api/rooms/{room}/daemon/turns/{id}/plugins/{sha256}` | daemon running the turn | the snapshot of a plugin version the job lists, as a tar; 404 for one it does not list |
| `GET /api/rooms/{room}/daemon/plugins/{version}/snapshot` | host daemon | a plugin version's snapshot to inspect |
| `POST /api/rooms/{room}/daemon/plugins/{version}/report` | host daemon | the inspection of a version waiting for it (docs/agent-workspace.md); 409 once inspected |
| `GET /api/rooms/{room}/daemon/plugins` | host daemon | the room's plugins as the daemon page needs them, each with its `version`, its `options` (never a value) and the `domains` its MCP servers name, the `fetches` waiting for the host, and `max_bytes`, the snapshot cap |
| `POST /api/rooms/{room}/daemon/plugin-fetches/{id}` | host daemon | what the host fetched for a waiting request, as `multipart/form-data`: `commit`, `ref_name` and one `snapshot` tar per plugin named after its marketplace entry (`.` for the repo); the server checks each tar again, marks the plugins private and adds them, or the new version for an update, waiting for inspection. A JSON `{"error"}` or `{"declined": true}` fails the request with that reason; 422 for an upload it refuses, which fails the request too |
| `GET /api/rooms/{room}/daemon/sessions/{id}` | daemon running the turn | the stored session jsonl of the session the turn resumes |
| `POST /api/rooms/{room}/daemon/turns/{id}` | daemon running the turn | the result: `error`, or `synthesis`, `changes`, and the session bytes from `session_offset`; applied all at once or not at all; 409 when an edited document changed during the turn |
| `POST /api/rooms/{room}/daemon/turns/{id}/state` | daemon running the turn | `host_queued`: true while the turn waits for a slot on the host's machine, false once it runs |
| `POST /api/rooms/{room}/daemon/status` | daemon | heartbeat: `can_host`, `reason`, `version` |
| `POST /api/stt/heartbeat` | STT token | `worker` name, the `rooms` whose audio it is in, `voices`, by room, the participant ids whose track it is subscribed to, its `engine` and whether it reads the `glossary`; a worker silent for 15 s counts as gone |
| `POST /api/rooms/{room}/voice/lines` | STT token | one transcribed utterance: `participant_id`, `body`; stored as a `voice` message from that participant |
| `POST /api/rooms/{room}/voice/dropped` | STT token | `participant_id`: the worker's full queue lost one of their utterances; posts the system line "Transcription overloaded: one line lost for <name>." |

Request bodies must be JSON with `Content-Type: application/json` (415 otherwise), uploads excepted. A body over its route's limit gets a 413. Participants authenticate with `Authorization: Bearer <token>`, the room cookie set at join, or the profile cookie. A browser's room cookie is tried first, then its profile cookie, which opens every room its profile has a participant in, as the newest of them. The server stores tokens as SHA-256 hashes.

## Profiles

The first join from a browser creates a profile and sets its cookie: HttpOnly, SameSite=Strict, valid for the whole site for 400 days. Every later join from that browser attaches the new participant to the same profile. `/profile` lists the profile's rooms, and the name in the room header links there.

After that first join, the room page's user menu (your initials) offers the profile's recovery link under "Use on another device", with a copy button. Without SMTP, a notice points there after the first message; with SMTP, a banner suggesting an email takes its place. The tab keeps it until "I saved it"; after that, the menu points to email sign-in instead. Opening it in another browser and confirming signs that browser into the profile, and from there into all its rooms. The page asks first, and warns when the browser already holds a profile: a link someone else sent would otherwise put the browser, and every room it joins next, in their profile. The link stays valid, so whoever holds it holds the profile. The server keeps only its hash.

With SMTP configured, `/profile` offers to bind an email. The server mails a link that binds the address once opened in the same browser. Opened in another browser, the page names the profile the address goes to and binds it only once you confirm; that browser keeps its own profile and nobody gets signed in. Later, "sign in with email" on `/profile` mails a sign-in link to a bound address, which signs a browser in after the same confirmation as a recovery link. Each link works once, for 15 minutes unless `GRILL_ROOM_MAGIC_LINK_TTL` says otherwise.

The join page offers "Recover my profile by email" while the browser holds no profile, and "Your profile" with its name once it does. A profile without an email shows a small silhouette next to its own avatar and name, to its owner only, and after its first message a banner suggests adding one; "Later" hides it for 14 days on every device. When someone has lost every device, the admin generates a one-time recovery link in `/admin` › Profiles: it works once, for 24 hours, and the audit log records both its creation and its use, never the link.

Within 15 minutes, a client IP may ask for 10 links, and for 3 to the same address; an address gets at most 10 whoever asks. That cap stops mail floods, at a cost: someone asking from 4 IPs can block an address's email sign-in for 15 minutes. The recovery link still works meanwhile. The client IP is the peer address. When the peer is in `GRILL_ROOM_TRUSTED_PROXIES`, it is the right-most `X-Forwarded-For` hop not in that list; a header from any other peer is ignored, so a client cannot spoof it. The admin login limit counts the same IP. A sign-in request gets the same answer, as fast, whether or not a profile holds the address: the lookup and the mail come after it. Without SMTP, `/profile` shows no email option and the email routes answer 404.

The server uses STARTTLS whenever the relay offers it. A relay that offers none gets the links in clear, so keep it on a trusted network, as an in-cluster relay is.

## Personal data

| Data | Who sees it | Removed by |
|---|---|---|
| display names, messages, voice transcripts, documents, annotations | the room's participants | deleting the room |
| a profile's rooms | the profile's own browsers, the admin | deleting the room, or the profile (the link only) |
| a profile's email | the profile's own browsers, the admin's profiles view; never other participants nor other admin views | deleting the profile |
| persona templates a profile published, signed with a display name | every participant of every room | the admin removing the template, or deleting the profile; room copies stay |
| personal skills, signed with a display name | the profile's own browsers; the agent and anyone in a room where a persona has one on | the profile removing it, the admin, or deleting the profile |
| shared skills, signed with a display name | every participant of every room | the admin removing it; deleting the profile only detaches it |
| room skills, copies of a personal skill signed with its author's display name | the room's participants and its agent | a room admin or the admin removing it, or deleting the room; not leaving the room or deleting the profile |
| room plugins: the display names of who added each plugin and version and who decided on it (`created_by`, `added_by`, `decided_by`), and the repo link | the room's participants | removing the plugin, or deleting the room; not leaving the room or deleting the profile |
| requests to the AI host to fetch a plugin: the repo link and the requester's display name | the room's participants and the host's daemon page | being done, cancelled or dismissed, or deleting the room |
| plugin option values the host sets, secrets included | the host's daemon page (whether a sensitive one is set, never its value) and the agent during a turn of that room | the host clearing them on the daemon page; they stay on the host's machine and never reach the room server |
| room admins: which profiles administer a room | the room's participants | removing them, deleting the room, or deleting the profile |
| audit log entries: the name of a room or profile an admin action targeted, the admin's IP at login, a room admin's profile uuid and display name | the admin | the purge after `GRILL_ROOM_AUDIT_RETENTION` (1 year), not deleting the room or profile |
| participants' client IP and user agent, at join and at last activity | the admin, in a room's view | the purge after `GRILL_ROOM_CLIENT_RETENTION` (30 days), or deleting the room |
| admin sessions: IP and browser user agent | the admin | revoking or signing out, or the daily purge once expired (12 h) |
| room exports: everything the room holds, agent sessions included, and its participants' display names and profile uuids | whoever holds the file | nothing on the server: the file leaves it |
| an imported participant's profile uuid that matches no profile here (`profile_uuid_hint`) | nobody but the next export | deleting the room |

- Access is by invite link. Anyone holding a valid one can read the room, and any member can make more.
- No audio is stored. There is no retention timer: rooms and profiles live until deleted.
- Deleting a room removes everything stored for it. A profile outlives its rooms and stops listing them.
- Deleting a profile (`DELETE /api/profiles/{id}`) removes its sessions, its pending and used links, its email, the persona templates it published and its personal skills. Shared skills it published stay in the library under the display name that signed them. Its participants stay in their rooms under their display names, detached from it. Every browser signed into it is signed out.
- Deletes overwrite the freed pages (`secure_delete`) and checkpoint the WAL, so the removed rows do not linger in the database files.
- Logs carry profile ids, never emails.

## Export and import

A room admin, from the room page's Room settings, or the admin, from `/admin`, exports a room as a `.tar.gz`: the room's settings and personas, every line with its earlier versions and deletions plus a readable `transcript.md`, documents with all their revisions, uploads (originals and their Markdown copies), annotation threads with their images, the room's skills and the shared and personal skill versions its personas pin, the room's plugins with their versions and snapshots (see Room plugins), the agent sessions, and the participants as display name and profile uuid. No email, token or invite leaves. `manifest.json` lists every file with its sha256, the format version and the exporting instance's version. docs/export-format.md describes the files.

The agent sessions hold everything Claude read in the room. The export panel says so, and so does the import preview.

`/admin` imports an archive, as does a profile allowed to create rooms through the API. The import first shows a preview: room name, counts per kind, sizes, the format and the exporting version, and warnings. Nothing exists until the admin confirms; the preview runs the whole import and rolls it back. The import makes a new room with new ids:

- Participants whose profile uuid matches a profile here are relinked to it, and a relinked room admin stays one. The others keep their uuid as `profile_uuid_hint`, unlinked. A profile importing relinks itself, and the other participants only if it administers the exported room on this instance: an archive cannot seat anyone in a room its importer controls. The importing profile becomes room admin, and gets a seat under `display_name` unless it is among the participants.
- No old invite or token opens the new room: they are not in the archive.
- Each persona resumes its agent session from where its last turn left it.
- On the instance that exported it, the pinned shared and personal skills link back to the library when it still holds the same versions. Elsewhere they become room skills, renamed `<name>-2` if the room has one of that name.
- Plugins land pinned at their exported versions, each waiting for this instance's AI host to check and approve it.
- Personas waiting for the host's approval, held persona changes, requests to the AI host to fetch a plugin and the turn history are not exported.

An import is refused, with the reason and with nothing created, for a file whose sha256 differs from the manifest, a format newer than the instance reads, a path that is absolute or holds `..`, an entry that is not a regular file (symlink, hard link, device, FIFO, directory), more than 50 000 files or more than `GRILL_ROOM_MAX_IMPORT_MB` uncompressed, an upload or image whose content is not what the room accepts, a plugin snapshot a daemon's upload would fail (not a normalised snapshot, over the instance's plugin size limit, or not matching its sha256), a plugin link with credentials, or a room too large for this instance. "Too large" counts two things. The room quota counts the uploads, annotation images and plugin snapshots, as for a live room, against the instance's default room quota. The instance storage quota, when set, counts the whole uncompressed archive against what the database has left.

Exports and imports stage their files in a directory beside the database, removed after the request and at the next start, so the volume needs room for the archive besides the database. At most two run at once; a third gets a 503, and each one gets 30 minutes to upload or download. Each one holds the database's single connection while it reads or writes the room: seconds for a room at its quota, more for long agent sessions, which no quota bounds. An import renders its lines before it takes the connection.

## Documents

Any participant can add Markdown, HTML, PDF, PNG, JPEG, GIF, WebP, SVG, DOCX, ODT or PPTX, several at once: the Documents header's `+` button, or a drop on the documents list (anywhere in the docs window). Each file shows its progress, then ✓, or ✗ with the reason and Retry; a done row clears after 5 s. The page refuses an unlisted extension before sending it. The server reads the content to decide the type. The extension only picks between text formats, so a binary file named `.md` gets a 415. Uploads are read-only. The room keeps the original, and pandoc turns DOCX, ODT and PPTX into a Markdown copy that the room shows. Documents the agent writes take revisions instead.

Markdown renders GitHub-style: tables, highlighted code, mermaid diagrams, math, alerts, footnotes. Every rendered document, uploaded HTML included, loads in an iframe sandboxed without `allow-same-origin`, and its response carries a `sandbox` CSP. A script in it runs in an opaque origin: it cannot read the room cookie, and the CSP blocks fetch, XHR and form posts.

Any participant renames a document from the viewer's `⋯` menu, which also holds Download (uploads) and Open in a new window: it keeps its id, revisions, annotations and links, the chat says "Alice renamed `a.md` → `b.md`", and a persona resuming its session reads the old and new workspace paths in its next turn's prompt. A room admin deletes one from the viewer, softly and at once, with Undo in a toast for 5 s: it leaves the list, the workspace and what the agents read, but its data stays, counts in the room's volume and goes only with the room. Room settings, Documents tab, restores it, unless another document took its name. An agent document is neither renamed nor deleted while a turn runs, as the turn would write it back under the name it read. Exports carry deleted documents, flagged.

The Documents header's external-link button, or `⋯` › "Open documents in a new window", puts the document manager in a window of its own, `/r/<room>/docs`: the list, uploads, the viewer, annotations, revisions and diffs, kept live by the room's event stream like the room page. Its address follows the document shown, so `/r/<room>/docs/<doc>` opens that document. A second click reuses the same window, and the room's name in its header leads back to the room.

From 64rem up, the documents list (the side panel on the room page) and Notes keep the width you drag their edge to, also with ← and →; the document keeps at least 24rem in the docs window and with the chat folded. Above 40rem, each drawer of the side panel folds on its own, Documents and People, with the triangle at the start of its header or a click on the header, and the panel narrows to a rail once all of them are folded. Markdown reads at Narrow, Large or Full width (the ↔ button). These choices stay in the browser, for every room. On a phone the docs view shows one pane at a time, the list, the document, then Notes, and Back steps through them.

The Docker image ships pandoc's official static build. The image grew from 16 MB to 174 MB, 147 MB of it pandoc (7 MB to 49 MB gzipped). Math renders with fallback glyphs for now: MathJax fetches its fonts from a CDN, and the CSP blocks that.

## Annotations

Select text in a Markdown or HTML document, or in the Markdown copy of an office file, and press Annotate. In an HTML document, "Annotate an element" picks an element instead. "Comment on the document" works on every document, PDF and images included. Each annotation holds a thread. Every message can carry up to 4 PNG, JPEG, GIF or WebP images; the server reads the type from the bytes and refuses SVG. Anyone can mark a thread done, and a participant's reply reopens it.

A text anchor is a quote with 32 characters of context on each side, matched the way grip-live-diff does. An element anchor is an `nth-of-type` path from `body` plus the first 80 characters of the element's text. When a script rebuilds the page, the mark moves to the element whose text matches. When the agent writes a new revision, the server re-anchors every annotation on it. A reworded quote follows its context. An annotation whose text or element is gone is flagged orphaned and stays in the list with the revision it was last seen in.

The document frame has no room credential, so it cannot call the API. It tells the room page what was selected or clicked through `postMessage`. The room page drops any message that is not from the frame on show, not from an opaque origin, or not of the expected shape and size. A participant still has to type and save the comment. The frame receives anchors and statuses only, never thread text or names.

`store.AnnotationsSince` is the read for the agent's turn: every thread with a message past a cursor, with its image bytes.

An `@name` in a note calls a persona as in the chat: same rights, same notices for its author, and a "→ @Persona" chip on the note message that follows the turn. A thread whose participants named personas belongs to them. Another persona's prompt leaves it out of the new threads, and its reply or close there is refused and listed in its turn's report. A persona naming another in its own reply claims nothing.

One persona per room may watch the notes ("Watches the notes", in its sheet's Triggers tab). It answers the threads that name no persona once `GRILL_ROOM_NOTES_WATCH_DELAY` (20 s) passes with no new note message; each new message restarts the wait. Its own messages and notes never wake it, another persona's do. Its turns are chain links, like every N answers, with one difference: at the limit it waits quietly, with no line in the chat, and runs once the limit lifts. The persona row, its pill and the Notes pane show who watches.

## Voice

With `GRILL_ROOM_LIVEKIT_URL` set, the room page shows a Join audio button; once joined, a mic toggle and an audio menu. Each grill room maps to one LiveKit room named after the room id. A participant joins it under their participant id with their display name, and may publish their microphone only. "Mute the room" (deafen) stops hearing the others and mutes the mic, unless "Keep my mic on while the room is muted" is checked, for people who share a physical room; "Hear the room again" restores the mic as it was. The state outlives a reconnect within the browser session.

## Presence

The participant list shows "In the room", then "Not here", collapsed, with when each was last seen. On a phone the 👥 button opens it as a sheet over the chat, closed by its ✕, a tap outside, Escape or picking a document. The server keeps presence from each page's event stream and a heartbeat every 10 seconds: a participant stays here while one of their pages holds its stream open, and for 20 seconds after their last sign of life, so a reload, a background tab or a phone switching apps does not flap. A closing page says so, and the audio state it held goes at once. Each page reports its audio state, and every participant sees it, in the audio or not: mic live, muted, listening only, or not in the audio, with an icon when deafened. Icon badges, each with its meaning in a tip on hover, mark you, the AI host (whose computer runs the personas) and room admins, who can ask the AI, and each voice the STT worker hears ("transcribed"). Those in the audio also see the speaking dot, and a volume control per remote participant in their `⋯` menu.

Each person is one row: a status dot, the name, then badges in aligned columns (you, AI host, admin, can ask the AI, transcribed, audio), each with its meaning in a tip on hover. Settings about a person sit in their `⋯` menu, not in the row: a room admin renames them and makes them a room admin or removes it, the AI host lets them ask the AI, the AI host or a room admin makes them the AI host when their AI host app is connected, and anyone in the audio sets their volume. A room admin who takes the role from someone else confirms first; the previous host's page then says who did it and that their app stays connected as a standby. The super-admin does the same from the room's Admins tab in `/admin`.

Hovering a name in People or on a chat line for 300 ms, or focusing it, shows a details card; on a phone a tap on the row opens it. It lists the name and the one before the latest rename, the roles (you, room admin, AI host, standby host when their app is connected but not hosting), "Can ask the AI", the audio state, whether the transcription hears them, here since or last seen, and when they joined. Escape closes it. It shows no IP address or user agent: those stay in `/admin`.

A participant changes their own name from the user menu, "Change my name", with the same rules as at join, and may make it their profile's name too. A room admin, from Room settings, or the admin, from `/admin`, renames anyone, and the renamed person sees a notice. The chat says who renamed whom. Earlier lines show the new name, with the one they were written under on hover. The agents' presence preamble keeps the same short id after the name, as it comes from the profile.

A participant turns on browser notifications for a room under Notifications. A sheet says what they are for before the browser asks for permission, and the room offers it once on its own, after the participant's first line in a page load; "Not now" keeps it quiet for 14 days in that browser, in every room. They come through Web Push, so they reach a background tab and a closed browser, but only while none of that participant's tabs in the room is focused: each page tells the server when it gains or loses the focus. By default a notification comes for a mention of the participant, a persona's synthesis, and the end of a turn they started; "Also every other line" adds the rest. Events of the same room within 5 seconds make one push, and the phone shows one notification per room, counting the events. Clicking it opens the room's chat scrolled to the line that triggered it, `/r/<room>#m<line id>`, in the room's open tab if there is one. The payload holds the room's name, the kind of event and a link, never a line's text: it passes through the browser vendor's push service. The VAPID key pair is generated on first use and kept in the database; a room page holding a subscription made with another key, as after the key changed, deletes it and subscribes again, for every room turned on in that browser. When the browser renews a subscription on its own, the service worker hands the new one to the server for every room turned on in that browser. A subscription the push service reports gone (404, 410) or refuses (a 403, or a 401 for another VAPID key) is dropped; any other 401, or a 403 naming the JWT as Safari's push service sends, points at the server's own VAPID setup, so the subscription stays and the server logs `push_unauthorized` as an error. Subscriptions also go with their participant and their room. On iPhone and iPad notifications need the room added to the home screen; the page says so, and the web manifest opens it standalone for that.

In the chat, `@Grill` shows as a chip in the persona's colour with its badge, and `@Ana_María` as a chip for that participant; an `@name` that names nobody in the room stays text. The server decides what a line names, by the same rule that calls personas, and sends it with each line (`named`); the page only draws it. A line that names you is highlighted, with an accent bar. A copy of the line still reads `@Grill`.

While the room's tab is not focused, its title counts new lines from others and syntheses, "(3) Pricing review · grill-room", and resets once the tab is focused again. "(4 @)" says one of them names you. Voice lines count only when "Count voice lines in the tab title" is ticked, under Notifications.

The server hands out LiveKit tokens valid for 5 minutes, and LiveKit refreshes them for connected clients. LiveKit has no way to revoke a token. So when an invite is revoked, the server also removes its participants from the LiveKit room, and deleting a room closes its LiveKit room. A revoked participant could still rejoin with a token minted before the revoke, until it expires.

Media needs no published UDP port (spike S4): ICE runs over TCP 7881, or TURN over TLS on 443, where traefik terminates TLS and routes `turn.<dev-domain>` by SNI to LiveKit's plain TURN port.

While an STT worker is in its audio, the audio menu counts the voices it transcribes, "2 voices are being written down": the participants whose track a worker is subscribed to. A participant whose own voice is one of them also reads "yours is one of them", and their mic button carries a dot; one who left the audio, or never joined it, reads nothing about their own. "Voice-to-text is off" shows in the audio menu, so only to people in the audio, when no worker has sent a heartbeat for 15 s. Audio and text keep working meanwhile.

Each room has a transcription glossary: terms people will say, one per line or comma-separated, 60 characters each, 2000 in all. Any member edits it in Room settings, and the room shows who changed it. The default engine, parakeet, has no input for it, so Room settings hide the tab while no worker reads the glossary; the glossaries stay stored, exported and imported. S10 and S10d found parakeet-ultra writes French with English jargon right without a list, and Claude reads the transcript afterwards anyway. With `APP_ENGINE=whisper` and `APP_GLOSSARY_PROMPT=on`, before each utterance, the worker reads the room's prompt (`GET /api/stt/rooms/{room}/prompt`, with its service token) and passes it to faster-whisper as `initial_prompt`, never as `hotwords` (spike S10). It is off by default: on real speech the prompt made `small` worse, as it loops on the prompt and drops text (spike S10b). The glossary is kept for an engine that takes a word list. The prompt is one French sentence: the glossary, then the participants' and personas' names, then the documents' titles, taken in that order while an estimate stays under Whisper's limit of 223 tokens. Room settings show how many terms fit, "12 / ~70 terms used". The sentence puts the glossary last, so if Whisper cuts an overlong prompt from its start, the titles go first.

### STT worker

`stt/` is a Python service. It polls LiveKit for rooms with participants and joins each one as a hidden participant that cannot publish. Then it listens to every participant's audio track, and closes that listener when the track is unsubscribed or unpublished, when its participant leaves, or when the worker leaves the room. LiveKit reuses a freed transceiver for the next track it sends the worker, so a listener left open would hear the newcomer and post them under the old name (spike S5). Silero VAD cuts each track into utterances after 800 ms of silence, or at 15 s. parakeet-ultra then transcribes them through transcribe.cpp, Q8_0, greedy, with no language argument: it writes French and English as spoken (spec 007, spike S10d). On the lab's real French speech it had a WER of 0.232 on meetings and 0.121 on a talk, against 0.326 and 0.257 for faster-whisper `small`, at half the latency.

`APP_ENGINE=whisper` brings back faster-whisper `small` int8 for one release, the rollback path. It detects the language per utterance, so French and English mix freely (spike S1-bis). Detection and decoding share one encoder pass: faster-whisper's own `transcribe(language=None)` runs two and throws the first away, which doubled the cost per line. Each line goes to `/voice/lines` under the speaker's participant id. The worker sends a heartbeat every 5 s, and at once when it joins or leaves a room or a track.

Before posting, the worker drops a transcript that fails one of these checks, and counts it in `grill_stt_lines_implausible_total{reason}`. Each threshold is a setting, in brackets. Parakeet gives no confidence and no language, so with it only `repetition`, computed on the text, and `script`, against Latin letters, apply. Parakeet turns a TTS glitch into Latin-letter babble that no check catches (`stt/tests/fixtures/voices.json`).

| Reason | Dropped when |
|---|---|
| `avg_logprob` | the lowest segment's mean token log-probability is under -1.0 (`APP_MIN_AVG_LOGPROB`), Whisper's own failure threshold |
| `repetition` | a segment's gzip ratio is over 2.4 (`APP_MAX_COMPRESSION_RATIO`), Whisper's sign of a looping decoder |
| `no_speech` | a segment's no-speech probability is over 0.6 (`APP_MAX_NO_SPEECH_PROB`) and the language is unsure. Whisper itself keeps a likely-silent segment it decoded confidently |
| `language_probability` | the language is detected at under 0.5 (`APP_MIN_LANGUAGE_PROBABILITY`) on an utterance of 2 s or more (`APP_SHORT_UTTERANCE_S`). Shorter ones pass: Whisper often hears a French "oui" as English at 0.3 |
| `script` | under 90% of the letters (`APP_MIN_SCRIPT_SHARE`) are in the script of the detected language, like Chinese characters for `fr` |

Audio stays in memory. Samples go from the LiveKit stream to the VAD, then to a bounded queue, then to an engine process over a pipe, and are released once transcribed. Nothing is written to disk. The worker runs `APP_ENGINE_PROCESSES` engine processes; each takes the oldest queued utterance whose speaker has none in an engine, so a speaker's lines post in order while other speakers pass. A process that dies, or hangs past `APP_ENGINE_TIMEOUT_S`, loses its utterance and is restarted. When the queue is full, a new utterance is dropped, counted, and announced in the room, "Transcription overloaded: one line lost for <name>.", once per speaker per 30 s. [docs/architecture.md](docs/architecture.md) has the capacity, the metrics and the alert. The worker logs the size of each line and a language guess, `fr`, `en` or `unknown`, from a count of function words, never its words. The guess also feeds `grill_stt_lines_language_total{engine,guess}`: English lines in a French room show an English leak. It never reaches the room server.

The image bakes both models in and runs with `HF_HUB_OFFLINE=1`, because a pod behind a default-deny egress policy could not fetch them at start. Whisper comes at a pinned Hugging Face revision. The parakeet GGUF comes from `handy-computer/parakeet-ultra-gguf` at a pinned revision, and the build fails if its sha256 differs. transcribe.cpp is compiled in the image from a commit pinned by full SHA, for the AVX2 baseline, never `GGML_NATIVE`: an image built on a newer CPU must run on the lab's AVX2 nodes. The worker loads it through its ctypes binding and `TRANSCRIBE_LIBRARY`. Compose gives it 4 CPUs and 3 GiB: two engine processes of 2 threads and 1.2 GB each (spike S10d).

| Variable | Default | |
|---|---|---|
| `APP_ROOM_URL` | none | the room server's API |
| `APP_ROOM_TOKEN` | none | the service token, `GRILL_ROOM_STT_TOKEN` on the server |
| `APP_LIVEKIT_URL` | none | LiveKit signalling URL; the API is the same host over HTTP |
| `APP_LIVEKIT_API_KEY`, `APP_LIVEKIT_API_SECRET` | none | LiveKit API key pair |
| `APP_ENGINE` | `parakeet` | `parakeet` or `whisper`; anything else stops the worker at start |
| `APP_PARAKEET_MODEL` | `/models/parakeet/parakeet-ultra-Q8_0.gguf` | the parakeet GGUF |
| `APP_MODEL` | `small` (`/models/whisper-small` in the image) | Whisper model size or directory |
| `APP_PARAKEET_REVISION`, `APP_WHISPER_REVISION` | set by the image | the revisions `grill_stt_engine_info` reports |
| `APP_ENGINE_PROCESSES` | `2` | engine processes, each with its own model |
| `APP_CPU_THREADS` | `2` | threads per engine process |
| `APP_ENGINE_TIMEOUT_S` | `60` | an engine silent that long on one utterance is killed and restarted |
| `APP_QUEUE_SIZE` | `64` | utterances waiting for an engine |
| `APP_GLOSSARY_PROMPT` | `off` | pass the room's glossary prompt to Whisper as `initial_prompt` (spike S10b: worse on real speech); parakeet ignores it |
| `APP_VAD_MIN_SILENCE_MS`, `APP_VAD_MAX_SPEECH_S` | `800`, `15` | silence that ends an utterance, longest utterance before a cut |
| `APP_POLL_SECONDS`, `APP_HEARTBEAT_SECONDS` | `2`, `5` | room discovery and heartbeat periods |
| `APP_PORT` | `9100` | `/healthz` (503 while the models load, and once no engine process has been up for 60 s) and `/metrics` |
| `APP_WORKER_NAME` | hostname | name in heartbeats; LiveKit identity `stt-<name>` |
| `APP_LOG_LEVEL` | `info` | |

Metrics: those in [docs/architecture.md](docs/architecture.md), plus `grill_stt_rooms` and `grill_stt_tracks`.

`python -m grill_stt.replay` plays WAV tracks in real time, one or several speakers at once, through the worker's own segmenter, engine processes and filter, and writes each line with its latency: the lab A/B run of spec 007 (`spikes/s007-ab/`) uses it in the image.

`make stt-image-test` runs the worker's e2e tests inside the built image with no network, for both engines: each must load its baked-in model offline.

The voice e2e tests play `e2e/voice/fixtures/fr-en.wav` as a fake microphone. It holds four read sentences from Google FLEURS (CC-BY-4.0), alternating French and English. `fr-en.json` gives their source files, text and timings. `uv run ./scripts/make_fixture.py ../e2e/voice/fixtures` (from `stt/`) rebuilds them.

## Agent

The agent runs on a participant's machine, through their host daemon (below). The first participant whose daemon connects and can host becomes the host. When the host's daemon drops or can no longer host, the room moves the role to the daemon that connected earliest among those that can. The host can hand the role to anyone whose daemon is connected. Each persona's LED in the header is green while the host's daemon is connected, amber during its turn, blue while it waits in the queue, and red with the reason when no host can run the agents. Transcripts keep recording either way.

A room holds personas: agents with a name, a badge, a colour, a text that sets their role, and the model and effort they ask for. Every persona names both; there is no "default" that only the host could read. Personas and templates from before that ask for `sonnet` and effort `medium`. Every room starts with Grill, from the Default template, which challenges the plan. The Secretary template takes notes. In the Personas sheet (`⋯` › Personas) any member adds a persona from the template library. Each persona is one row there: badge, name, model and effort, trigger and state. A row opens that persona's sheet, with an Identity, Model, Triggers and Capabilities tab (each a link, `#personas/<id>/<tab>`), one Save for all four, and Remove in its `⋯` menu. A right-click or a long press on a persona's pill opens the same sheet. Each change is a line in the chat and applies from that persona's next turn. A template's newer version is offered to the room, never applied by itself. A profile can publish a persona as a template, and new versions of its own templates.

Each persona has its own button. The host can press them and can let other participants press too, with "Can ask the AI" in their `⋯` menu in People. A pill someone may not press still opens, to say why and who can change it. One press runs one turn of that persona: it reads what was said and annotated since its own last successful turn, other personas' syntheses included, edits or writes documents, answers threads and may close them, opens annotations of its own on any document, uploads included, and posts a synthesis. The synthesis shows the persona's badge and name and the model and effort the turn ran with. A new agent document never takes an upload's name. One turn runs at a time in a room: a press during a turn waits in a queue shown under the header, which the person who pressed can cancel. A turn the room handed over can still wait on the host's machine, behind turns of the host's other rooms: its LED stays blue and the header says "waiting (the AI host is busy)".

The host can tick "Personas answer at the same time" in the Personas sheet. Two personas then run at once, each in its own sandbox; a press waits only while that persona runs or waits already. A turn's workspace holds each agent document at the revision it had when the daemon first fetched it. When a turn writes a document another turn revised since, its whole result is set aside and the turn runs again, ahead of the queue, on the new version; the chat says so, and the re-run is no chain link. If the re-run conflicts too, the turn fails with "conflict on <document>". Replies, new annotations, syntheses and new documents never conflict. The server counts both outcomes in `grill_room_turn_conflicts_total{outcome}`. "Start fresh" (host only, in the persona sheet's `⋯` menu) starts a persona without its memory. It then rereads the room history from the transcript files. "Start fresh, keep a summary" (host only) first runs a last turn that writes `docs/_handoff-<persona>.md`, then starts a new session told to read that note and the transcripts; if the last turn fails, the persona keeps its session. The persona sheet's Model tab shows how much of its context window the persona's session holds, and above 80% the room offers the host that handoff. "Memory size", on the same tab, sets the autocompact context size, in thousands of tokens from 100 to 1000, at which Claude Code compacts the persona's session; empty leaves it to Claude Code. The room keeps each persona's Claude session after every turn, so when another daemon takes over, each persona's next turn resumes its own session. [docs/agent-workspace.md](docs/agent-workspace.md) has the details.

### Skills

A skill is a Claude Code skill folder: a `SKILL.md` whose YAML frontmatter has a `name` (lowercase letters, digits and hyphens) and a `description`, plus any files it needs, scripts included. The profile page uploads one as a zip, with `SKILL.md` at the root of the zip or of its single folder. The upload is personal, kept in the profile and offered only in rooms that profile is in, or shared, in the library every room sees. Uploading a name again publishes its next version; versions never change. The server refuses a zip with symlinks, absolute or `..` paths, more than 200 files, or more than 8 MB packed or unpacked, and says why. The admin can remove any skill. Skills never come from the host's disk.

A member can also copy one of their personal skills into the room from the Personas sheet (`⋯` › Personas). The copy is the room's: frozen at the version copied, offered to every member, and kept when its author leaves the room or deletes the original, unlike a personal skill, which rides only while its owner is a member. The copy is a line in the chat. While the host approves everything, it waits for the host before any persona can use it. A room admin removes room skills from Room settings; deleting the room deletes them.

On a persona sheet's Capabilities tab any member with a profile turns a skill on for that persona, from the shared library, their own personal skills or the room's skills, and anyone turns one off. Each change is a persona change: a line in the chat, held while the host approves everything. The persona keeps the version it was turned on at. A newer upload shows as "Update to vN", applied only when someone takes it. The first time a profile turns any skill on, the sheet shows a disclaimer once:

- a skill's scripts run in the host's sandbox, on the host's machine;
- anyone in the room can ask the agent to print a skill's content, so a skill must hold no secret;
- the sandbox reaches the Internet only through the host's egress allowlist, which does not inspect traffic, so a script can send room data to any allowed domain that accepts uploads, GitHub included when the script carries a token.

The turn hands the skills to `claude -p` as a plugin directory with `--plugin-dir`. [docs/agent-workspace.md](docs/agent-workspace.md) has the layout.

### Room plugins

A room admin adds Claude Code plugins from Room settings › Plugins: "Add a plugin" takes a repository link and a git ref (empty for the default branch), and Check lists what the repo holds. A marketplace lists its plugins, none checked, and the ones stored elsewhere are greyed out with the reason. The admin also picks the personas to turn it on for, none checked by default. The list shows each plugin as "rich, main @ 3c23934", the version in use, and where a newer one stands. Its `⋯` holds Update, which says "Up to date" or adds a version, and Remove. Every member reads the list; only room admins see these actions.

A new version waits for the AI host's app to check it. While the host approves everything, it then waits for the host: the status strip and the plugin's row offer Review, which opens the approval sheet. That sheet shows the repo, ref, commit, size, the components with their names, each hook's command and each MCP or LSP server's command or URL highlighted, the websites MCP servers name, what grill-room could not read, Claude Code's warnings and the token cost per answer. For an update it adds the change against the version in use: components and files added, removed and changed, and a compare link on GitHub and GitLab, both commits elsewhere. A persona's Capabilities tab lists the room's plugins: a room admin turns each on or off there. Each row gives the version, its token cost or why it does not load, and the tab totals the cost of those that load.

The server fetches public https repos only. When it cannot read one, because the repo asks for a login, is not found, or is an ssh link (`git@host:owner/repo`), the add form offers "Ask the AI host to fetch it", with an optional list of plugin names for a marketplace repo and the personas to turn them on for. The request waits in Room settings › Plugins. The host's daemon page lists it on the Rooms tab with who asked, the link and the ref, and nothing is fetched until the host presses Fetch, whatever the approval mode: the fetch runs the host's own git, with their ssh keys, `gh` login and credential helpers, outside every sandbox. Git runs with no prompt and no askpass program, https and ssh only, no submodule, no hook and no LFS download, within the 60 s and size limits of a server fetch; at the deadline the daemon kills git and what it started (git only on Windows). The daemon reads the commit's files without a checkout, builds the snapshots and uploads them; the server checks them again and marks the plugin private. Update on a private plugin makes the same request. A host whose git cannot reach the repo answers "This AI host cannot reach the repo", and the version in use keeps running.

A plugin may declare options (`userConfig`). The host sets them on the daemon page, per room: the Rooms tab shows a form per plugin, each option with its title, description and type. A sensitive option is a password field, and its value is never shown again, only whether it is set. The form warns: "The AI can read these values during a turn. Anyone in the room can ask it to show them." The values stay in the instance's state dir, in `plugin-options.json` (owner-only), kept per plugin: a plugin removed and added again starts with none. They never reach the room server, a room or an export. Each turn writes them where Claude Code reads a `--plugin-dir` plugin's options: sensitive ones in the sandbox's `.credentials.json` as `pluginSecrets`, the others in `settings.json` as `pluginConfigs`, both rewritten every turn. A plugin with a required option unset and no default does not load: the turn runs without it, and the persona's Capabilities tab and the daemon page say "needs host setup". Before posting a turn, the daemon replaces each sensitive text value of the room, 4 characters or more, with `[redacted]` in the synthesis, the error, the documents, replies and notes, and the session, escaped forms included, and rewrites its local copy of the session the same way, so the next turn resumes without fetching it. JSON stays valid: the daemon redacts it string by string.

The approval sheet marks the websites of MCP servers that the host's egress allowlist leaves out, as the daemon saw it at the inspection. The daemon page lists them per plugin with an "Add to allowlist" button each, under the allowlist's own rules: no IP, no wildcard, no Anthropic host. Nothing is added without that press.

An export carries the room's plugins: link, ref, every version with its commit and inspection report, the snapshots the room still holds, and which personas have each one on. Option values stay on the host. The Export tab names the plugins from private repos, since their files go in the archive. An import pins each plugin at the same versions and loads none of them: the new instance's AI host checks each one, then approves it, even when it approves everything without asking. A version whose files the room had dropped, as a newer one replaced it, arrives as a record that never loads, and so does a refused or invalid one. The approval sheet says an imported version's files came from an archive, not from the repo, and the room links no commit for it. Update fetches the repo again, even at the same commit, once the new server can read it, or for a private repo, once the new host's git can.

"Save as starter" lists the room's plugins at the version in use, none checked, as for documents. A checked plugin goes in with its snapshot and stays on the personas that had it; one left out leaves them too. A room created from the starter gets it as version 1, which the AI host checks first; then the host approves it when it approves everything, and it loads at once otherwise, as for a starter's skills. A plugin the new room cannot take (past the instance's plugin size limit, the 10 plugins of a room or the room's quota) is left out, off its personas, with a line in the chat: the room is created anyway. The snapshots count in the starter's size against the storage quota, and in the new room's quota. Built-in starters carry no plugin.

### Triggers and chains

A persona runs on three triggers. Its button is the first. With "Speaks up every N answers" set in the sheet, it also runs after N completed turns of any persona since its own last turn; a failed turn of its own counts as that last turn, so a persona that keeps failing waits N answers before it tries again. The third is `@name`: a text message naming a persona, in any case and outside code, queues its turn as a press by the author, so only the host and the people they allow can call one that way, and only while the agent is online. A synthesis naming another persona queues that persona too. A persona naming itself runs again only with "Can follow up on its own" ticked. Each persona is queued once however many times it is named, and queued turns run in order like presses.

Turns that agents start, every N answers, the notes watcher or `@name` in a synthesis, are links of a chain. Above 0, the header shows the count against the room's budget, "auto 3/25" (25 by default), which the host and the people they allow set in the Personas sheet as the "Auto-turn limit". At the budget the next link is dropped, not queued (the notes watcher only waits, quietly): a line in the chat names who did not run, and a banner offers "Resume auto turns" and "Remove the limit". Dropping keeps a paused room from replaying a backlog of calls nobody saw once someone resumes it. A press, "Resume auto turns" or a text message that calls a persona starts the count over. Voice lines and other text do not. "Remove the limit" lets agents call each other without end, behind a red warning until someone restores the limit; the AI host's own cap still applies.

### Host cap and approvals

The host pays for every turn, so the host's daemon caps the model and effort personas run with. The cap comes from `GRILL_ROOM_MAX_MODEL` and `GRILL_ROOM_MAX_EFFORT`, or from the "Limit on models" form on the daemon page, which saves it in the instance's config dir and wins over the environment from then on. Delete that `settings.json` to go back to the environment's.

- Models rank `haiku` < `sonnet` < `opus` < `fable`. A full model id ranks with the family it names, so `claude-sonnet-5-5` is `sonnet` and a Mythos model is `fable`. An id naming no family ranks above all of them, so any cap lowers it. Efforts rank in `--effort` order: `low` < `medium` < `high` < `xhigh` < `max`.
- Model and effort clamp separately. A persona asking for `opus` and `max` under a `sonnet`, `high` cap runs at `sonnet`, `high`, and its synthesis says "requested opus · effort max → ran … (host cap)".
- A banner under the header lists every persona asking above the cap, with what it runs at. Everyone sees it; only the host answers. Approving lets that persona run as asked from its next turn, on the host's daemon only: the daemon's cap does not change. Refusing keeps it at the cap. Either answer is a line in the chat. An answer covers the exact model and effort it was given for: a persona asking for anything else above the cap shows in the banner again, and until then an approval still raises its cap up to what was approved.
- Answers belong to the daemon that hosted when they were given. After a hand-over, the new host's cap applies, the banner asks the new host again, and the old answers apply again if the old host takes the role back.
- With "Approve every new persona and persona change" ticked on the daemon page, or `GRILL_ROOM_APPROVE_ALL=true`, a member's new persona waits, greyed and unable to run, and any change of theirs to a persona waits too, in the same banner. Later changes to the same persona join its waiting request. The host's own changes apply at once, and removing a persona is never held, since it runs nothing. The mode follows the host's daemon even while it is offline. Waiting requests survive a hand-over and the new host decides them, whatever its own mode.

## Host daemon

`grill-room-daemon` runs on a participant's machine: Linux, macOS, or Windows inside WSL with the Linux build. It runs the agent with that participant's own Claude Code login, in a Docker sandbox.

```sh
make build
./bin/grill-room-daemon https://<host>/r/<room>
```

Or install it from the room page (see Distribution below).

The command runs the supervisor, one per machine: it holds a lock on its state dir. For each room server it serves (an instance), it starts a worker, `grill-room-daemon worker --instance <id>`, which holds the rooms' connections and sandboxes. The supervisor restarts a worker that crashes, with backoff, and its room reconnects with the stored token. Stopping the supervisor stops the workers, which remove their sandboxes. [docs/daemon-protocol.md](docs/daemon-protocol.md) has the protocol between the two. Running the command again with a room link while a supervisor runs hands the room to it over a local socket, then opens its page; running it again with no room is refused. A worker serves every room of its instance at once, each with its own connection and sandboxes.

The supervisor caps the turns running at once across all its instances and rooms: 2 by default, set under "Turns at once" on the page and saved in `supervisor.json`. Before a turn, its worker asks the supervisor for a slot. Turns past the cap wait in one queue, first in first out, and the room shows "queued (host busy)" until theirs starts. The page lists the rooms with their turns running and waiting, and the queue waits since start, to tune the cap.

The supervisor opens a page on `127.0.0.1` in the default browser; with none to open, as when it runs detached, it prints the link with the reason. `grill-room-daemon admin` gets a fresh link from the running supervisor, opens it and prints it; a plain second `grill-room-daemon` does the same. A tab bar, pinned at the top, splits the page into Rooms, Servers, Accounts, Settings, Activity and Update; each tab is a deep link (`#accounts`) and shows its alerts as a badge. The page lists the Claude accounts, the instances and their rooms with the account each uses, and shows for the last room handed over: the pairing, Docker, whether this machine can host the agent, the host cap, the egress allowlist with the requests it refused, the logs, and a stop button. The Update tab holds the supervisor's signature, its update channel, the trusted dev keys and any update waiting. A "Start at login" box installs a systemd user unit (Linux and WSL), a LaunchAgent (macOS) or a scheduled task (Windows), and removes it again. The Rooms tab's "Add a room" takes a room link at any time, also while another room is connected, and hands it over as a second launch does. For each room this machine hosts, the Rooms tab also lists the room's requests to fetch a plugin, with Fetch and Decline, the plugins' options to set, and the websites their MCP servers reach with "Add to allowlist" (Room plugins, above). Any link inside a room works there, on the command line and in `install.sh`: the room page, its docs window or a document, with any query or fragment; a link with no `/r/<room>` is refused and the page says why. The room page's `⋯` › "Run the AI on my computer" gives the room's link to paste there, and the install when the server hands out a release. To pair, follow the page's link to the room, check that both pages show the same code, and press Approve: the room gives the daemon a token, which it keeps in the instance's `tokens.json`. The daemon only opens outbound connections: an SSE stream from the room plus a status POST every 15 s, and it reconnects with backoff.

State lives in one directory per instance, `instances/<id>/`, under three roots:

| | Config (`instance.json`, `settings.json`, `allowlist.json`; `supervisor.json`, `accounts.json` and `dev-keys/` at the root) | State (`tokens.json`, `plugin-options.json`, `plugin-cache/`, `plugin-fetch/`, lock, socket; `bin/`, `trust/` and `accounts/` at the root) | Cache (`workspace-cache/`) |
|---|---|---|---|
| Linux, WSL | `$XDG_CONFIG_HOME/grill-room` (`~/.config`) | `$XDG_STATE_HOME/grill-room` (`~/.local/state`) | `$XDG_CACHE_HOME/grill-room` (`~/.cache`) |
| macOS | `~/Library/Application Support/grill-room` | same | `~/Library/Caches/grill-room` |
| Windows | `%APPDATA%\grill-room` | `%LOCALAPPDATA%\grill-room` | `%LOCALAPPDATA%\grill-room` |

`GRILL_ROOM_DAEMON_STATE_DIR` puts all three in one directory. On first start, the supervisor migrates a single-room daemon's state dir (`GRILL_ROOM_DAEMON_STATE_DIR`, or `grill-room-daemon` beside the config root): each paired room moves to its server's instance with its token and workspace cache, the settings and allowlist are copied to each instance, `CLAUDE_CONFIG_DIR` is recorded as account `default` in `accounts.json`, and the old files stay in `legacy-backup-<date>` under the config root. Paired rooms keep working without pairing again.

The page link carries a one-time code. The page trades it for a session token kept in the tab's `sessionStorage`, and the daemon prints a fresh link in its terminal after each use. The page refuses requests whose `Host` is not `127.0.0.1:<port>` or, for writes, whose `Origin` is not the page's own.

The supervisor holds the Claude accounts, listed in `accounts.json`. Without that file, the only account is `default`: `$CLAUDE_CONFIG_DIR`, or `~/.claude`. The page adds and removes accounts of two kinds:

- A Claude Code config dir, read and never written. The page takes it as `~/…` or as an absolute path, stores it absolute and shows it as `~/…` under your home. It reads the macOS Keychain first (`security find-generic-password`, 3 s timeout, so a locked Keychain fails instead of hanging), then `<config dir>/.credentials.json`. The host's Claude Code keeps refreshing the token; the supervisor re-reads it when the file changes, when it expires and after an upstream 401. The page shows its expiry and warns 30 minutes before it.
- A `claude setup-token` token, pasted once and never shown again. It goes to the OS keyring (Keychain, Secret Service, Windows Credential Manager), or, when the keyring fails or takes over 5 s, to `accounts/<name>.token` in the state root: mode 0600, or on Windows a DACL that admits the current user alone. The token carries no expiry, so the page assumes a year from the day it was added and warns two weeks before. Its status comes from `POST /v1/messages/count_tokens`, which spends no inference: when added, every 15 minutes, a minute after upstream refuses a proxied request, and on "Check now". Only that probe marks the token refused.

Each instance has an account, `default` unless the page picks another, and each room may use its own. Each account has its own bearer-swap proxy, in the supervisor. A worker gets each room's proxy address and a dummy bearer of its own, never a token, and its environment loses every `CLAUDE_*` and `ANTHROPIC_*` variable. A room moved to another account loses its bearer for the old one. The proxy counts each worker's requests, shown on the page. "Cut it off from Claude" revokes the worker's bearers at every proxy and ends its running turns with "the host cut this agent off from Claude for now" in the room; the room sees the agent offline until "Let it use Claude again". The room never sees an account name, a config dir or this machine's name: it shows "Alice holds the agent(s)".

The sandbox holds no credential. It sits on an `--internal` Docker network whose only other members are a socat relay to its account's proxy and the egress proxy, below. The proxy replaces the sandbox's dummy bearer with the account's token. It listens on the default bridge gateway, or on `127.0.0.1` under Docker Desktop. The daemon writes a dummy `.credentials.json` and an `oauthAccount` stub before every turn and sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `ENABLE_CLAUDEAI_MCP_SERVERS=false` (spike S3). Containers, networks and volumes carry the `grill-room-daemon` label, plus `grill-room-root` (a hash of the supervisor's state dir) and `grill-room-instance`, and are removed when the sandbox closes. A supervisor killed with its workers leaves them behind: the next one removes everything labelled with its own state dir at start, and never touches another's. To check a new room plugin version, the host's daemon also runs a throwaway container from the sandbox image, with no network and a read-only root, for at most 60 s ([docs/agent-workspace.md](docs/agent-workspace.md), Plugin inspection).

The image has `python3` with a venv on `PATH` (`/home/agent/.venv`, layered on Debian's openpyxl, pandas, python-docx, pypdf and matplotlib, plus python-pptx from PyPI), `uv`, `pip`, `node` 24 with `npm`, `pandoc`, `jq`, `yq`, `rg`, `git` and `curl`. No LibreOffice. The base, pandoc and uv images are pinned by digest; Node, yq and python-pptx are checked against sha256 sums the Dockerfile pins; the Claude Code installer checks its download against its release manifest. It runs as the non-root `agent` user on a read-only root filesystem; the home and `/workspace` are volumes, so `uv pip install`, `pip install` and `npm install` work there. It weighs 1.26 GB, up from 455 MB before the tools: Claude Code 257 MB, the Python stack about 300 MB, pandoc 147 MB, Node 138 MB.

Tools reach the Internet through an egress proxy, a squid container per sandbox started from the same image. Squid sits on the sandbox's internal network as `egress` and on a bridge of its own that routes out. The sandbox gets `HTTP_PROXY` and `HTTPS_PROXY` (and lowercase) pointing at it, which uv, pip, npm, git and curl follow, and `NO_PROXY=proxy,localhost,127.0.0.1`, so Claude Code's own traffic still goes to the bearer-swap proxy and never through squid. Squid denies `.anthropic.com`, `.claude.ai` and `platform.claude.com` first, then anything but `CONNECT` to port 443, then any host off the allowlist. It does not intercept TLS, so an allowed host gets the whole tunnel. The default allowlist is `pypi.org`, `files.pythonhosted.org`, `github.com`, `codeload.github.com`, `raw.githubusercontent.com`, `objects.githubusercontent.com` and `registry.npmjs.org`.

The host edits the list on the daemon page: one domain per entry, or a leading dot (`.example.com`) for a domain and its subdomains. The page refuses wildcards, IP addresses, ports, URLs and Anthropic hosts, and drops an entry a leading-dot entry already covers, which squid would refuse. The list is saved in the instance's `allowlist.json` and the running squid reloads it at once. Below it the page lists the last 50 requests squid refused in the instance's sandboxes, with host and time, read from squid's access log; the list empties when the sandbox goes.

The sandbox gets 2 CPUs and 4 GiB and squid 0.5 CPU and 256 MiB by default, swap included, so a runaway script is killed instead of paging the host. The four `GRILL_ROOM_*_CPUS` and `*_MEMORY` variables below change that; `0` lifts a limit.

When someone presses a persona's button, the room hands the turn to the host's daemon. The daemon fetches the job and the workspace tar, clears the sandbox's `/workspace` and unpacks the tar there. If the room's copy of the session differs from the local one, it fetches the room's copy. Then it runs `claude -p` with the prompt on stdin, the persona's text as an appended system prompt file, its `--model` and `--effort`, `--resume` when the room has a session for that persona, and one `--plugin-dir` for its skills and each room plugin it has on, and posts back the changed files, the final answer and the session bytes added. [docs/agent-workspace.md](docs/agent-workspace.md) has the workspace layout and the prompt contract. A turn is cancelled after the room's turn timeout, counted from when it leaves the host's queue. A room's first sandbox is named from the supervisor's state dir and the room, so a restarted worker replaces what a crash left; a turn running beside another gets a sandbox of its own, `<name>-1` and on. They share the room's workspace cache.

The sandbox image comes from the room server. When the server advertises a digest-pinned image (`GRILL_ROOM_SANDBOX_IMAGE` on the server), the daemon pulls it by digest. When it advertises none, or the pull fails, for instance on a registry that wants a login, the daemon builds the Dockerfile embedded in its binary and tags the result `grill-room-sandbox:local-<hash of the Dockerfile>`, so it builds once per Dockerfile change. The page shows the pull or build progress. Until the image is ready, the daemon tells the room it cannot host.

#### Worker binaries and signatures

Each instance's worker runs the daemon version its room server hands out. When that is the supervisor's own version, or the server hands out none, the worker runs the supervisor's binary. Otherwise the supervisor reads `/api/daemon/version`, downloads the binary for its OS and architecture and its sigstore bundle, and runs it only if all of these hold:

- the download matches the sha256 the server publishes;
- sigstore-go verifies the bundle: a certificate for this repository's `release.yml` on a semver tag, issued by GitHub Actions, logged in Rekor ([docs/release.md](docs/release.md#signing-identity)), whose tag is the version served; or a signature by one of the trusted dev keys;
- the file is an executable for this OS and architecture;
- the version is v0.0.2 or later, the first release that runs as a worker.

Otherwise the worker stays stopped and the instance list shows why, as does the log. The binary and its bundle are kept in `bin/<version>/` under the state root, one copy per version, and verified again before every start, offline included. A version no instance runs any more is deleted at start and whenever an instance changes version. When a worker reports that its room server now hands out another version, the supervisor restarts it on that version.

Verification works offline with the sigstore trusted root embedded in the binary. Online, the supervisor refreshes it through TUF when it verifies a binary, at most once a day (cache in `trust/tuf` under the state root) and keeps the last good copy in `trust/trusted_root.json`. When TUF is unreachable it uses that copy, else the embedded one, and tries TUF again an hour later. The page says which root is in use.

Dev builds signed by `make dev-release` run once their cosign public key is trusted: paste it under "Trusted dev keys" on the page, or drop it in `dev-keys/<name>.pub` under the config root. Adding a key restarts the refused workers; removing one restarts the workers running a binary it signed.

Each instance also has an "accept unsigned binaries" toggle, off by default, for testing builds nobody signed. Switching it on takes the instance's host typed exactly as in its URL. While it is on, a red banner and a badge stay on the page and every worker start logs `worker_accepts_unsigned`. The binary must still match the published sha256 and this platform.

Worker log lines and stderr reach the supervisor's terminal re-encoded: the supervisor sets their `instance` field and escapes control and Unicode format characters, so a worker binary cannot drive the terminal.

#### Supervisor updates

The supervisor updates itself from GitHub releases by default: it reads `releases/latest/download/VERSION` at start and every 6 hours, and the page offers "Update and restart" for a newer version. The other channel, "the version an instance serves", follows one instance and offers whatever version it hands out, older ones included, under that instance's rules (dev keys, accept unsigned). It checks again as soon as that instance moves to another version. Nothing updates without that click. The supervisor downloads the binary next to its own and checks it like a worker binary. Only then does it rename it over the running binary and keep the bundle beside it as `<binary>.sigstore.json`. It then stops its workers, which remove their sandboxes, and execs the new version, which reconnects to the same rooms. The new version binds the page's port again and keeps its sessions, handed over in an owner-only file of the state dir, so the open tab says "Updating…" and reconnects by itself. It prints a fresh link either way, and opens it only if that port was taken meanwhile. A binary named after its version, `grill-room-daemon-v0.0.3`, becomes `grill-room-daemon` at the update, and a login item follows it. On a failed check it keeps the current binary and shows the reason. The binary's directory must be writable by the user, as `~/.local/bin` is.

Next to its version, the page says whether the running supervisor is project-signed. It checks its own binary against `<binary>.sigstore.json`, or against the release's bundle on GitHub. A version that is not a release, or that GitHub does not have, shows "dev build"; a binary its bundle does not match shows "NOT project-signed".

`grill-room-daemon check` checks Docker and account `default`, builds the embedded sandbox image unless one is pinned, runs `claude -p --model haiku "say ok"` in a throwaway sandbox, checks that only the two proxies are reachable and that the egress proxy refuses `api.anthropic.com`, and counts the real token's occurrences in the container, its volumes and its inspect output. It spends one Claude request.

| Variable | Default | |
|---|---|---|
| `GRILL_ROOM_DAEMON_STATE_DIR` | the platform's directories, above | one directory for the config, state and cache roots |
| `GRILL_ROOM_DAEMON_CACHE_MB` | `1024` | size cap of each instance's workspace cache, least recently used files dropped first; `0` fetches every workspace whole |
| `GRILL_ROOM_DAEMON_BROWSER` | `auto` | `none` prints the page link instead of opening it |
| `GRILL_ROOM_GITHUB_URL` | `https://github.com/babs/grill-room` | the repository the GitHub update channel reads releases from |
| `GRILL_ROOM_SANDBOX_IMAGE` | none | pins the sandbox image, e.g. `grill-room-sandbox:local` from `make sandbox-image`; skips the pull and the build |
| `GRILL_ROOM_MAX_MODEL` | no cap | host cap on the model: `haiku`, `sonnet`, `opus`, `fable`, or a model id naming one |
| `GRILL_ROOM_MAX_EFFORT` | no cap | host cap on the effort: `low`, `medium`, `high`, `xhigh`, `max` |
| `GRILL_ROOM_APPROVE_ALL` | `false` | hold every new persona and persona change until the host approves it |
| `GRILL_ROOM_SANDBOX_CPUS`, `GRILL_ROOM_SANDBOX_MEMORY` | `2`, `4g` | the sandbox's `docker run --cpus` and `--memory` (swap included); `0` for no limit |
| `GRILL_ROOM_EGRESS_CPUS`, `GRILL_ROOM_EGRESS_MEMORY` | `0.5`, `256m` | the same for its squid egress proxy |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | the config dir of account `default` until `accounts.json` exists |
| `LOG_LEVEL` | `info` | |

`docs/daemon-os-checklist.md` lists the manual checks on macOS and WSL.

### Distribution

The room server hands out the daemon itself. `make release` cross-compiles it for linux, darwin and windows, amd64 and arm64 (static, `-trimpath`, version stamped; the windows builds end in `.exe`), and writes a sha256 file per binary plus `VERSION`. The server image builds the same release into `/daemon`, unsigned, or ships a signed one with its bundles (`docs/release.md`). These routes need no token:

| Route | |
|---|---|
| `GET /install.sh` | the installer |
| `GET /download/grill-room-daemon-<os>-<arch>[.exe][.sha256]` | a binary, or its `sha256sum` line |
| `GET /download/grill-room-daemon-<os>-<arch>[.exe].sigstore.json` | the binary's cosign bundle, when the release is signed |
| `GET /api/daemon/version` | `{"version", "sha256": {"<os>-<arch>": "<hex>"}, "bundles": {"<os>-<arch>": "/download/<binary>.sigstore.json"}, "sandbox_image"}`; `bundles` lists only signed targets |

The room page's `⋯` menu has "Run the AI on my computer". It guesses the visitor's OS and CPU (Chromium reports the CPU, Safari and Firefox do not, so both stay editable), links the matching binary with its sha256, and shows the one-liner:

```sh
curl -fsSL https://<host>/install.sh | sh -s -- https://<host>/r/<room>
```

`install.sh` is POSIX sh. It picks the build from `uname` (WSL gets the Linux one, a Rosetta shell the arm64 one), downloads it from the host in the room URL, and checks it against the published sha256. With `cosign` on the PATH, it also checks the binary's sigstore bundle against the project's release workflow identity (docs/release.md), and refuses a build whose bundle is missing or does not verify; without cosign it says the signature was not checked. Then it moves it into `~/.local/bin` and starts it with the room URL, which opens the pairing page. On a mismatch it installs nothing. It accepts plain `http` for `localhost` and `127.0.0.1` only.

## Local dev with docker compose and traefik

The dev setup expects a traefik on the machine that serves `*.<dev-domain>` with a valid wildcard certificate and watches a file-provider rules directory. Copy `.env.example` to `.env` and fill in `DEV_DOMAIN` and `TRAEFIK_RULES_DIR`.

```sh
make dev-up     # renders deploy/local/traefik-grill-room.yaml into the rules dir, starts compose
make dev-down   # removes the rule, stops compose
```

Compose also runs mailpit, which catches the magic link mails; its inbox is at `http://127.0.0.1:8025` (`MAILPIT_PORT`). `make dev-up` publishes the app port and LiveKit's 7880 and 5349 on the docker bridge gateway only, which is where traefik's `host.docker.internal` points, so the plain-HTTP ports stay off the LAN. The room is then at `https://grill-room.<dev-domain>/`, LiveKit signalling at `livekit.<dev-domain>` and TURN at `turn.<dev-domain>`. Compose needs the LiveKit key pair and the STT token in `.env`. With `GRILL_ROOM_ADMIN_TOKEN` set there, that is the admin token; otherwise read the generated one with `docker compose logs grill-room | grep admin_token_generated`.

## Lab deployment on Kubernetes

`deploy/k8s/` holds plain manifests for the room server, LiveKit and the STT worker, applied with kubectl. There is no GitOps and no Helm. The templates carry no domain, registry, digest or secret. `make k8s-render` fills them from `.env` into `deploy/k8s/out/`, which is untracked and readable by its owner only.

The cluster needs:
- traefik as the `traefik` IngressClass, with its CRDs (`IngressRouteTCP`) and a `websecure` entrypoint on 443;
- a certificate for the three hostnames in traefik's default TLS store, since the routes name no secret;
- a ReadWriteOnce StorageClass for the SQLite volume;
- amd64 nodes for the room server and the worker, one of them with 2 free CPUs and 3 GiB for the worker;
- a namespace you create beforehand, with pull access to your registry for its default service account.

```sh
make k8s-push VERSION=v0.8.0     # builds and pushes server and stt, prints GRILL_ROOM_IMAGE= and GRILL_STT_IMAGE= for .env
export KUBECONFIG=<lab kubeconfig>
make k8s-apply                   # renders, applies to K8S_NAMESPACE (default grill-room), waits for the rollouts
make k8s-delete                  # removes everything k8s-apply created, the data volume included
```

The targets refuse to run without `KUBECONFIG` set, so the default kubeconfig is never used by accident. The namespace is left alone: delete it yourself.

What gets deployed, with `<domain>` standing for `K8S_DOMAIN`:

| Host | Route | Backend |
|---|---|---|
| `grill-room.<domain>` | Ingress, HTTPS | room server, port 7680 |
| `grill-room-livekit.<domain>` | Ingress, HTTPS | LiveKit signalling, port 7880 |
| `grill-room-turn.<domain>` | IngressRouteTCP, `HostSNI`, TLS terminated by traefik | LiveKit TURN, plain on 5349 (`turn.external_tls`) |

- The room server runs one replica with a `Recreate` strategy. SQLite has one writer, on a 2 GiB volume. Probes hit `/healthz`, and `/metrics` is on 7681.
- LiveKit exposes no UDP. Browsers relay through TURN/TLS on 443. `allow_restricted_peer_cidrs` lists only the pod's own IP, where the relay hands media to the SFU.
- The STT worker requests 2 CPUs and may burst to 4, with 3 GiB as request and limit: two parakeet engine processes of 2 threads (spike S10d). One pod serves every room, about four people talking at once. It updates with `Recreate`: no lab node has 2 CPUs free for a second worker beside the old one, so transcription stops for the minute a new pod takes to load the models.
- Switching the STT engine back to faster-whisper is one command, in [docs/lab-checklist.md](docs/lab-checklist.md) (STT engine rollback).
- `make k8s-monitoring` applies the STT queue alert and its scrape (`deploy/monitoring/stt.yaml`) for a cluster running the VictoriaMetrics operator; `make k8s-monitoring-delete` removes them. `K8S_MONITORING_NAMESPACE` (default `monitoring`) is where vmagent runs.
- Every pod runs as non-root with a read-only root filesystem, no service account token and no service links.
- A default-deny NetworkPolicy blocks everything, then one policy per flow opens DNS, traefik to the room server and LiveKit, the worker to both, the room server to LiveKit's API and, for Web Push, to TCP 443 anywhere (the server itself posts only to `GRILL_ROOM_PUSH_HOSTS`), media between LiveKit and the worker, and the worker's traces to the cluster's OpenTelemetry collector (a Kyverno policy there injects `OTEL_EXPORTER_OTLP_ENDPOINT` into every pod).
- The Secret `grill-room` comes from the same `.env` keys compose uses.
- With `GRILL_ROOM_SMTP_ADDR` and `GRILL_ROOM_SMTP_FROM` in `.env`, the room server mails magic links; the `room-smtp` NetworkPolicy opens TCP `K8S_SMTP_PORT` (default 1025) to the namespace `K8S_SMTP_NAMESPACE` (default `mailpit`), where the relay runs.

`make e2e-lab` runs a whole session against the deployment through its public hostnames, with two participants, an upload, an annotation, audio and a voice line. Audio runs twice, once forced through TURN/TLS and once with default ICE. One agent turn runs with a real daemon on this machine and haiku, at most six Claude requests. [docs/lab-checklist.md](docs/lab-checklist.md) is the manual check with a participant outside the LAN.

`scenarios/` replays scripted sessions against the deployment, or any instance, with synthetic voices and real daemons, and reports on each assertion: a dark-mode grill, a two-persona chain, a long session and a host hand-over. It prints the Claude request budget before it starts. [docs/scenarios.md](docs/scenarios.md) has the file format and the voice licenses.

## Make targets

`build` (server and daemon), `release` (the daemon release into `DIST`, default `dist/`), `run`, `test` (race detector), `test-docker` (sandbox and agent turns against real Docker, a fake Anthropic upstream and a fake `claude`, no Claude request; the egress test needs Internet access to PyPI, npm and GitHub), `e2e` (Playwright with the installed Chrome, needs pnpm, pandoc and Docker for the agent turns, which use the fake `claude`; then the magic link tests against a second server and a throwaway mailpit container), `e2e-voice` (Playwright against the compose stack, with fake microphones; it kills and restarts the `stt` container), `e2e-agent-live` (two real daemons against the compose stack with `claude --model haiku`: a turn, a daemon killed mid-turn, a takeover that recalls a code word through `--resume`; about six Claude requests), `e2e-install` (a clean Debian container runs the room's one-liner against the compose stack, pairs, takes a self-update and refuses a tampered release; it copies releases into the `grill-room` container and recreates it at the end; no Claude request), `stt-test` (ruff, mypy and pytest for the worker), `stt-image-test` (the worker's e2e tests in the built image, offline, both engines; `stt-image-e2e` runs them on an image already built), `scenarios-test` (ruff, mypy and pytest for the scenario tool; no audio, no Claude request), `stt-image`, `cover`, `lint` (golangci-lint, shellcheck), `docker-build` (also tests the office fixtures against the pandoc it ships), `sandbox-image`, `sandbox-push` (pushes it to `REGISTRY` and prints its digest-pinned reference), `sandbox-fake-image` (the sandbox with `internal/sandbox/fakeclaude` as `claude`, for tests), `fixtures` (regenerates them), `icons` (renders the favicon, PNG icons, apple-touch-icon and notification badge from the logo with Inkscape and ImageMagick), `dev-up`, `dev-down`, `traefik-install`, `traefik-uninstall`, `k8s-push`, `k8s-render`, `k8s-check`, `k8s-apply`, `k8s-delete`, `k8s-monitoring`, `k8s-monitoring-delete` (lab deployment, above), `e2e-lab` (a session against it; up to six Claude requests), `e2e-lab-mail` (magic links against it, read through the lab mailpit at `K8S_MAILPIT_URL`; no Claude request), `e2e-push` (a mention arrives as a real Web Push notification, through the browser vendor's push service, on the lab or on `PUSH_BASE` with `PUSH_ADMIN_TOKEN`; a headed Chrome under xvfb in the Playwright image; no Claude request), `e2e-supervisor` (one supervisor serving a room on the compose stack and one on the lab at once, on this machine's Claude Code login and the setup token in `SETUP_TOKEN_FILE`; the compose stack must serve a release signed with the dev key `DEV_KEY_PUB`; two haiku turns, at most six Claude requests), `e2e-crossinstance` (a room exported from the compose stack and imported on the lab, where a haiku turn resumes its session, then moved back; both must run this build; at most two Claude requests).

CI also runs a flake gate, `tools/flake-gate.sh`: on Linux and Windows, the Go tests in every test file a change adds or edits run five times with the race detector, and the e2e specs it adds or edits run three times each (`make e2e-repeat SPECS=…`), against the PR's base. Running the `ci` workflow by hand (`gh workflow run ci.yml --ref <branch>`) runs the full gate: every Go package five times with the race detector, on Linux and Windows, and every e2e spec three times.

### Writing stable tests

- Assert on state, never on elapsed time. In Go, inject the clock. In Playwright, wait on a locator or `expect.poll`.
- A `time.Sleep`, `waitForTimeout` or fixed `setTimeout` in a test carries a comment saying why: a poll bounded by a deadline, or a wait on a process with no clock to move.
- Every network or IPC call in a test has a deadline.
- Names are unique per test (rooms, pipes, sockets, containers), and the test removes what it created.
- Before acting, wait for the state the action needs. A page that shows "connected" may be ahead of the server that will accept the action.
- A test that fails once in CI is a bug. Find the cause, fix it in the test or the product, and write the cause in the commit body. Rerunning until green is not a fix.
- Repeat runs happen in CI, never on a workstation.
- A spec whose daemon runs agent turns goes in `sandboxSpecs` in `e2e/playwright.config.js`. Chrome fails the requests it is opening whenever the host's interfaces change, and each sandbox adds and removes Docker networks, so those specs run one at a time, after the others.

## Third-party notices

The STT worker image ships these under `/licenses`:

- parakeet-ultra, by Moondream, a post-train of NVIDIA's Parakeet TDT 0.6B v3 ([model card](https://huggingface.co/moondream/parakeet-ultra)). It runs as the Q8_0 GGUF that the transcribe.cpp authors converted ([handy-computer/parakeet-ultra-gguf](https://huggingface.co/handy-computer/parakeet-ultra-gguf)), unmodified. License: [CC-BY-4.0](https://creativecommons.org/licenses/by/4.0/). The room's audio menu gives the same credit while a worker runs it.
- [transcribe.cpp](https://github.com/handy-computer/transcribe.cpp) and the ggml, miniz and Silero VAD code it vendors: MIT.
