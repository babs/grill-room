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
| `GRILL_ROOM_MAX_ROOM_MB` | `200` | all uploads and annotation images of one room, in MiB (413 above) |
| `GRILL_ROOM_MAX_ATTACHMENT_MB` | `5` | largest image on an annotation message, in MiB (413 above) |
| `GRILL_ROOM_PANDOC` | `pandoc` | pandoc binary for DOCX, ODT and PPTX (3.8+ for PPTX) |
| `GRILL_ROOM_PANDOC_TIMEOUT` | `60s` | one conversion's time budget |
| `GRILL_ROOM_LIVEKIT_URL` | none | LiveKit signalling URL for browsers (`wss://...`); unset turns voice off |
| `GRILL_ROOM_LIVEKIT_API_URL` | `GRILL_ROOM_LIVEKIT_URL` as `https://` | LiveKit's HTTP API as the server reaches it |
| `GRILL_ROOM_LIVEKIT_API_KEY`, `GRILL_ROOM_LIVEKIT_API_SECRET` | none | LiveKit API key pair; the secret needs 32+ characters |
| `GRILL_ROOM_STT_TOKEN` | none | the STT worker's service token; unset refuses every worker |
| `GRILL_ROOM_TURN_TIMEOUT` | `15m` | one agent turn's time budget (1m to 24h) |
| `GRILL_ROOM_DAEMON_DIR` | none (`/daemon` in the image) | daemon release written by `make release`; unset offers no download |
| `GRILL_ROOM_SANDBOX_IMAGE` | none | sandbox image advertised to daemons, pinned by digest (`<repo>[:tag]@sha256:<hex>`); unset lets each daemon build it |
| `GRILL_ROOM_SMTP_ADDR` | none | SMTP relay (`host:port`) for magic links; unset turns email sign-in off |
| `GRILL_ROOM_SMTP_FROM` | none | sender address, needed with `GRILL_ROOM_SMTP_ADDR` |
| `GRILL_ROOM_SMTP_USERNAME`, `GRILL_ROOM_SMTP_PASSWORD` | none | SMTP login, both or neither; sent only after STARTTLS, unless the relay is on localhost |
| `GRILL_ROOM_MAGIC_LINK_TTL` | `15m` | how long an emailed link works (1s to 1h) |
| `LOG_LEVEL` | `info` | |
| `LOG_FORMAT` | `auto` | `console` on a TTY, `json` otherwise |

`/healthz` returns 200 when SQLite accepts a write and 503 otherwise.

## Rooms and invites

The admin token creates rooms and the first invites:

```sh
curl -H "Authorization: Bearer $ADMIN" --json '{"name":"Pricing review"}' localhost:7680/api/rooms
curl -H "Authorization: Bearer $ADMIN" -X POST localhost:7680/api/rooms/$ROOM/invites
```

The second call returns a `url` such as `https://<host>/join#<token>`. Whoever opens it picks a display name and lands in the room. One invite can be shared with several people.

Once in, any member can create invites from the room page ("Invite people") and revoke the ones they created. The admin revokes any. Revoking an invite also revokes the invites created by those who joined through it, and so on down the chain, so a member cut off cannot come back through an invite they made. Any member can rename the room, and a system line in the chat says who renamed it, from what, to what.

```sh
# Revoke an invite: everyone who joined through it gets a 401 and their live stream closes.
curl -H "Authorization: Bearer $ADMIN" -X DELETE localhost:7680/api/rooms/$ROOM/invites/$INVITE_ID
# Delete a room and everything stored for it.
curl -H "Authorization: Bearer $ADMIN" -X DELETE localhost:7680/api/rooms/$ROOM
```

| Route | Auth | |
|---|---|---|
| `GET /api/rooms`, `POST /api/rooms` | admin | list, create |
| `DELETE /api/rooms/{room}` | admin | delete with all its data |
| `PATCH /api/rooms/{room}` | admin or participant | rename: `name` |
| `GET /api/rooms/{room}/invites` | admin or participant | every invite for the admin, a participant's own for a participant; `created_by` is the participant who created it, absent for the admin |
| `POST /api/rooms/{room}/invites` | admin or participant | new invite link |
| `DELETE /api/rooms/{room}/invites/{id}` | admin, or the participant who created it | revoke, with the invites its joiners created; 403 for another participant's invite |
| `GET /api/profiles` | admin | every profile with its email and rooms |
| `DELETE /api/profiles/{id}` | admin | delete a profile, see Personal data |
| `POST /api/join` | invite token in the body | returns a participant token and sets the room cookie; without a valid profile cookie, also creates a profile, sets its cookie and returns `recovery_url` once |
| `GET /api/profile` | profile cookie, optional | `email_enabled`, and `profile` (`id`, own `email`, `rooms`) or null |
| `POST /api/profile/recover` | recovery token in the body | signs this browser into the profile |
| `POST /api/profile/email` | profile cookie | mails a link that binds `email` to the profile; 404 without SMTP |
| `POST /api/profile/signin` | none | mails a sign-in link if a profile holds `email`; 202 either way; 404 without SMTP |
| `POST /api/profile/magic` | link token in the body | spends an emailed link: a sign-in link signs this browser in, a bind link binds the address to the profile this browser holds (403 from any other browser); 401 when unknown, used or expired, 409 when another profile holds the address; 404 without SMTP |
| `GET /api/rooms/{room}` | participant | room, me, participants |
| `POST /api/rooms/{room}/messages` | participant | post a text message (Markdown); messages carry `body` (source) and `html` (sanitised render) |
| `PATCH /api/rooms/{room}/messages/{id}` | author | edit your text or voice line: `body`; 403 for anyone else, 409 once deleted |
| `DELETE /api/rooms/{room}/messages/{id}` | author | soft delete: participants see `deleted_at` and no body; the agent keeps the text |
| `GET /api/rooms/{room}/messages/{id}/versions` | participant | earlier bodies of an edited message, oldest first |
| `GET /api/rooms/{room}/events` | participant | SSE: history then live messages; resumes after `?since=<id>` or `Last-Event-ID`; a `message_edited` event with the message for each edit or delete past `?edits=<change_seq>` (all of them on a resume without it);  a `documents` event when a document or revision is added, an `annotations` event when an annotation is added or changes; an `agent` event with the agent state (`host_participant_id`, `online`, `reason`, last `turn`, `can_trigger`, connected `daemons`) at open and on every change; with voice on, an `stt` event (`available`, `listening`) at open and on every change |
| `GET /api/rooms/{room}/agent` | participant | the agent state, as in the `agent` event |
| `POST /api/rooms/{room}/turns` | host, or a participant the host allowed | the agent button: `fresh` (host only) starts a new session; 409 `turn in progress` while one runs, 409 when no host daemon is connected |
| `PUT /api/rooms/{room}/host` | host | hand over: `participant_id` of a participant whose daemon is connected and can host |
| `PATCH /api/rooms/{room}/participants/{id}` | host | `can_trigger`: grant or revoke the agent button |
| `GET /api/rooms/{room}/voice` | participant | LiveKit `url` and a 5 min `token` for this room's audio; 404 when voice is off |
| `GET /api/rooms/{room}/documents` | participant | list |
| `POST /api/rooms/{room}/documents` | participant | upload, multipart field `file` |
| `GET /api/rooms/{room}/documents/{id}` | participant | document and its revisions |
| `GET /api/rooms/{room}/documents/{id}/view` | participant | rendered page; `?rev=N` picks a revision, `&from=M` shows the word diff from M |
| `GET /api/rooms/{room}/documents/{id}/original` | participant | the uploaded bytes |
| `GET /api/rooms/{room}/documents/{id}/annotations` | participant | annotations with their threads |
| `POST /api/rooms/{room}/documents/{id}/annotations` | participant | new annotation: `anchor` (null for the whole document), `revision`, `body`, `attachments` |
| `POST /api/rooms/{room}/annotations/{id}/messages` | participant | reply: `body`, `attachments` |
| `PATCH /api/rooms/{room}/annotations/{id}` | participant | `status`: `done` or `reopened` |
| `GET /api/rooms/{room}/attachments/{id}` | participant | an annotation image |
| `POST /api/rooms/{room}/pairings` | none | a daemon asks to pair: `name`; returns `id`, `secret`, `code`, `approve_url` |
| `GET /api/pairings/{id}` | pairing secret | `pending`, `denied`, or `approved` with the daemon token, handed once |
| `GET /api/rooms/{room}/pairings/{id}` | participant | the request's daemon name and code, for the approval page |
| `POST /api/rooms/{room}/pairings/{id}` | participant | `approve`: true pairs the daemon to this participant, false denies |
| `GET /api/rooms/{room}/daemon/events` | daemon | SSE: `hello`, pings, `turn` (`id`) when a turn starts on this daemon, `revoked` |
| `GET /api/rooms/{room}/daemon/turns/{id}` | daemon running the turn | the job: `prompt`, `resume` session id, its `session_size` and `session_sha256`, `timeout_seconds` |
| `GET /api/rooms/{room}/daemon/turns/{id}/workspace` | daemon running the turn | the workspace as a tar, see [docs/agent-workspace.md](docs/agent-workspace.md) |
| `GET /api/rooms/{room}/daemon/sessions/{id}` | daemon running the turn | the stored session jsonl of the session the turn resumes |
| `POST /api/rooms/{room}/daemon/turns/{id}` | daemon running the turn | the result: `error`, or `synthesis`, `changes`, and the session bytes from `session_offset`; applied all at once or not at all |
| `POST /api/rooms/{room}/daemon/status` | daemon | heartbeat: `can_host`, `reason`, `version` |
| `POST /api/stt/heartbeat` | STT token | `worker` name and the `rooms` whose audio it is in; a worker silent for 15 s counts as gone |
| `POST /api/rooms/{room}/voice/lines` | STT token | one transcribed utterance: `participant_id`, `body`; stored as a `voice` message from that participant |

Request bodies must be JSON with `Content-Type: application/json` (415 otherwise), uploads excepted. A body over its route's limit gets a 413. Participants authenticate with `Authorization: Bearer <token>`, the room cookie set at join, or the profile cookie. A browser's room cookie is tried first, then its profile cookie, which opens every room its profile has a participant in, as the newest of them. The server stores tokens as SHA-256 hashes.

## Profiles

The first join from a browser creates a profile and sets its cookie: HttpOnly, SameSite=Strict, valid for the whole site for 400 days. Every later join from that browser attaches the new participant to the same profile. `/profile` lists the profile's rooms, and the name in the room header links there.

Right after that first join, the room page shows the profile's recovery link once, with a copy button. Opening it in another browser and confirming signs that browser into the profile, and from there into all its rooms. The page asks first, and warns when the browser already holds a profile: a link someone else sent would otherwise put the browser, and every room it joins next, in their profile. The link stays valid, so whoever holds it holds the profile. The server keeps only its hash.

With SMTP configured, `/profile` offers to bind an email. The server mails a link that binds the address once opened in the same browser; anywhere else it is refused and signs nobody in. Later, "sign in with email" on `/profile` mails a sign-in link to a bound address, which signs a browser in after the same confirmation as a recovery link. Each link works once, for 15 minutes unless `GRILL_ROOM_MAGIC_LINK_TTL` says otherwise.

Within 15 minutes, a client IP may ask for 10 links, and for 3 to the same address; an address gets at most 10 whoever asks. That cap stops mail floods, at a cost: someone asking from 4 IPs can block an address's email sign-in for 15 minutes. The recovery link still works meanwhile. The client IP is the peer address, or the last `X-Forwarded-For` hop when the peer is on a private or loopback address, as a proxy is. A sign-in request gets the same answer, as fast, whether or not a profile holds the address: the lookup and the mail come after it. Without SMTP, `/profile` shows no email option and the email routes answer 404.

The server uses STARTTLS whenever the relay offers it. A relay that offers none gets the links in clear, so keep it on a trusted network, as an in-cluster relay is.

## Personal data

| Data | Who sees it | Removed by |
|---|---|---|
| display names, messages, voice transcripts, documents, annotations | the room's participants | deleting the room |
| a profile's rooms | the profile's own browsers, the admin | deleting the room, or the profile (the link only) |
| a profile's email | the profile's own browsers, the admin; never other participants | deleting the profile |

- Access is by invite link. Anyone holding a valid one can read the room, and any member can make more.
- No audio is stored. There is no retention timer: rooms and profiles live until deleted.
- Deleting a room removes everything stored for it. A profile outlives its rooms and stops listing them.
- Deleting a profile (`DELETE /api/profiles/{id}`) removes its sessions, its pending and used links, and its email. Its participants stay in their rooms under their display names, detached from it. Every browser signed into it is signed out.
- Deletes overwrite the freed pages (`secure_delete`) and checkpoint the WAL, so the removed rows do not linger in the database files.
- Logs carry profile ids, never emails.

## Documents

Any participant can upload Markdown, HTML, PDF, PNG, JPEG, GIF, WebP, SVG, DOCX, ODT or PPTX. The server reads the content to decide the type. The extension only picks between text formats, so a binary file named `.md` gets a 415. Uploads are read-only. The room keeps the original, and pandoc turns DOCX, ODT and PPTX into a Markdown copy that the room shows. Documents the agent writes take revisions instead.

Markdown renders GitHub-style: tables, highlighted code, mermaid diagrams, math, alerts, footnotes. Every rendered document, uploaded HTML included, loads in an iframe sandboxed without `allow-same-origin`, and its response carries a `sandbox` CSP. A script in it runs in an opaque origin: it cannot read the room cookie, and the CSP blocks fetch, XHR and form posts.

The Docker image ships pandoc's official static build. The image grew from 16 MB to 174 MB, 147 MB of it pandoc (7 MB to 49 MB gzipped). Math renders with fallback glyphs for now: MathJax fetches its fonts from a CDN, and the CSP blocks that.

## Annotations

Select text in a Markdown or HTML document, or in the Markdown copy of an office file, and press Annotate. In an HTML document, "Annotate an element" picks an element instead. "Comment on the document" works on every document, PDF and images included. Each annotation holds a thread. Every message can carry up to 4 PNG, JPEG, GIF or WebP images; the server reads the type from the bytes and refuses SVG. Anyone can mark a thread done, and a participant's reply reopens it.

A text anchor is a quote with 32 characters of context on each side, matched the way grip-live-diff does. An element anchor is an `nth-of-type` path from `body` plus the first 80 characters of the element's text. When a script rebuilds the page, the mark moves to the element whose text matches. When the agent writes a new revision, the server re-anchors every annotation on it. A reworded quote follows its context. An annotation whose text or element is gone is flagged orphaned and stays in the list with the revision it was last seen in.

The document frame has no room credential, so it cannot call the API. It tells the room page what was selected or clicked through `postMessage`. The room page drops any message that is not from the frame on show, not from an opaque origin, or not of the expected shape and size. A participant still has to type and save the comment. The frame receives anchors and statuses only, never thread text or names.

`store.AnnotationsSince` is the read for the agent's turn: every thread with a message past a cursor, with its image bytes.

## Voice

With `GRILL_ROOM_LIVEKIT_URL` set, the room page shows a Join audio button. Each grill room maps to one LiveKit room named after the room id. A participant joins it under their participant id with their display name, and may publish their microphone only. The participant list shows who is in the audio, a dot that lights while LiveKit reports them speaking, red when muted, and a volume slider per remote participant.

The server hands out LiveKit tokens valid for 5 minutes, and LiveKit refreshes them for connected clients. LiveKit has no way to revoke a token. So when an invite is revoked, the server also removes its participants from the LiveKit room, and deleting a room closes its LiveKit room. A revoked participant could still rejoin with a token minted before the revoke, until it expires.

Media needs no published UDP port (spike S4): ICE runs over TCP 7881, or TURN over TLS on 443, where traefik terminates TLS and routes `turn.<dev-domain>` by SNI to LiveKit's plain TURN port.

The room page shows "Transcribing" while an STT worker is in its audio, and "Transcription unavailable" when no worker has sent a heartbeat for 15 s. Audio and text keep working meanwhile.

### STT worker

`stt/` is a Python service. It polls LiveKit for rooms with participants and joins each one as a hidden participant that cannot publish. Then it listens to every participant's audio track, and closes that listener when the track is unsubscribed or unpublished, when its participant leaves, or when the worker leaves the room. LiveKit reuses a freed transceiver for the next track it sends the worker, so a listener left open would hear the newcomer and post them under the old name (spike S5). Silero VAD cuts each track into utterances after 800 ms of silence, or at 15 s. faster-whisper `small` int8 then transcribes them, detecting the language per utterance, so French and English mix freely (spike S1-bis). Detection and decoding share one encoder pass: faster-whisper's own `transcribe(language=None)` runs two and throws the first away, which doubled the cost per line. Each line goes to `/voice/lines` under the speaker's participant id. The worker sends a heartbeat every 5 s, and at once when it joins or leaves a room.

Before posting, the worker drops a transcript that fails one of these checks, and counts it in `grill_stt_lines_implausible_total{reason}`. Each threshold is a setting, in brackets.

| Reason | Dropped when |
|---|---|
| `avg_logprob` | the lowest segment's mean token log-probability is under -1.0 (`APP_MIN_AVG_LOGPROB`), Whisper's own failure threshold |
| `repetition` | a segment's gzip ratio is over 2.4 (`APP_MAX_COMPRESSION_RATIO`), Whisper's sign of a looping decoder |
| `no_speech` | a segment's no-speech probability is over 0.6 (`APP_MAX_NO_SPEECH_PROB`) and the language is unsure. Whisper itself keeps a likely-silent segment it decoded confidently |
| `language_probability` | the language is detected at under 0.5 (`APP_MIN_LANGUAGE_PROBABILITY`) on an utterance of 2 s or more (`APP_SHORT_UTTERANCE_S`). Shorter ones pass: Whisper often hears a French "oui" as English at 0.3 |
| `script` | under 90% of the letters (`APP_MIN_SCRIPT_SHARE`) are in the script of the detected language, like Chinese characters for `fr` |

Audio stays in memory. Samples go from the LiveKit stream to the VAD, then to a bounded queue, then to the model, and are released once transcribed. Nothing is written to disk. When the queue is full, new utterances are dropped and counted. The worker logs the size and language of each line, never its words.

The image bakes the model in at a pinned Hugging Face revision and runs with `HF_HUB_OFFLINE=1`, because a pod behind a default-deny egress policy could not fetch it at start. Compose gives it 4 CPUs and 1.5 GiB, the S1-bis starting point, and the model 3 threads: the fourth CPU is left to the event loop, the VAD and LiveKit's threads. On this host a line lands about 2 to 3 s after the speaker pauses.

| Variable | Default | |
|---|---|---|
| `APP_ROOM_URL` | none | the room server's API |
| `APP_ROOM_TOKEN` | none | the service token, `GRILL_ROOM_STT_TOKEN` on the server |
| `APP_LIVEKIT_URL` | none | LiveKit signalling URL; the API is the same host over HTTP |
| `APP_LIVEKIT_API_KEY`, `APP_LIVEKIT_API_SECRET` | none | LiveKit API key pair |
| `APP_MODEL` | `small` (`/models/whisper` in the image) | model size or directory |
| `APP_CPU_THREADS` | `3` | model threads |
| `APP_QUEUE_SIZE` | `8` | utterances waiting for the model |
| `APP_VAD_MIN_SILENCE_MS`, `APP_VAD_MAX_SPEECH_S` | `800`, `15` | silence that ends an utterance, longest utterance before a cut |
| `APP_POLL_SECONDS`, `APP_HEARTBEAT_SECONDS` | `2`, `5` | room discovery and heartbeat periods |
| `APP_PORT` | `9100` | `/healthz` (503 while the model loads) and `/metrics` |
| `APP_WORKER_NAME` | hostname | name in heartbeats; LiveKit identity `stt-<name>` |
| `APP_LOG_LEVEL` | `info` | |

Metrics: `grill_stt_queue_depth`, `grill_stt_utterances_dropped_total`, `grill_stt_lines_total{result}` (`posted`, `failed`, `empty`, `implausible`, `error`), `grill_stt_lines_implausible_total{reason}`, `grill_stt_transcribe_seconds`, `grill_stt_line_latency_seconds`, `grill_stt_rooms`, `grill_stt_tracks`.

The voice e2e tests play `e2e/voice/fixtures/fr-en.wav` as a fake microphone. It holds four read sentences from Google FLEURS (CC-BY-4.0), alternating French and English. `fr-en.json` gives their source files, text and timings. `uv run ./scripts/make_fixture.py ../e2e/voice/fixtures` (from `stt/`) rebuilds them.

## Agent

The agent runs on a participant's machine, through their host daemon (below). The first participant whose daemon connects and can host becomes the host. When the host's daemon drops or can no longer host, the room moves the role to the daemon that connected earliest among those that can. The host can hand the role to anyone whose daemon is connected. The LED in the header is green while the host's daemon is connected, amber during a turn, and red with the reason when no host can run the agent. Transcripts keep recording either way.

The host can press "Ask the agent" and can tick "can ask" next to other participants to let them press it too. One press runs one turn: the agent reads what was said and annotated since its last successful turn, edits or writes documents, answers threads and may close them, opens annotations of its own on any document, uploads included, and posts a synthesis. The synthesis shows the Claude model the turn ran on, as the daemon reported it. A new agent document never takes an upload's name. A second press during a turn gets "turn in progress". "New session" (host only) starts the agent without its memory. It then rereads the room history from the transcript files. The room keeps the Claude session after every turn, so when another daemon takes over, the next turn resumes the same session. [docs/agent-workspace.md](docs/agent-workspace.md) has the details.

## Host daemon

`grill-room-daemon` runs on a participant's machine: Linux, macOS, or Windows inside WSL with the Linux build. It runs the agent with that participant's own Claude Code login, in a Docker sandbox.

```sh
make build
./bin/grill-room-daemon https://<host>/r/<room>
```

Or install it from the room page (see Distribution below).

It opens a page on `127.0.0.1` in the default browser. The page shows the pairing, Docker, the Claude credentials, whether this machine can host the agent, the logs, and a stop button. Without a room URL on the command line, paste the room link there. To pair, follow the page's link to the room, check that both pages show the same code, and press Approve: the room gives the daemon a token, which it keeps in `tokens.json` under its state dir. The daemon only opens outbound connections: an SSE stream from the room plus a status POST every 15 s, and it reconnects with backoff.

The page link carries a one-time code. The page trades it for a session token kept in the tab's `sessionStorage`, and the daemon prints a fresh link in its terminal after each use. The page refuses requests whose `Host` is not `127.0.0.1:<port>` or, for writes, whose `Origin` is not the page's own.

Credentials are read, never written: the macOS Keychain first (`security find-generic-password`, 3 s timeout, so a locked Keychain fails instead of hanging), then `<config dir>/.credentials.json`, where the config dir is `$CLAUDE_CONFIG_DIR` or `~/.claude`. The host's Claude Code keeps refreshing the token; the daemon re-reads it when the file changes, when it expires and after an upstream 401.

The sandbox holds no credential. It sits on an `--internal` Docker network whose only other member is a socat relay to the daemon's bearer-swap proxy. The proxy replaces the sandbox's dummy bearer with the real token. It listens on the default bridge gateway, or on `127.0.0.1` under Docker Desktop. The daemon writes a dummy `.credentials.json` and an `oauthAccount` stub before every turn and sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `ENABLE_CLAUDEAI_MCP_SERVERS=false` (spike S3). Containers, network and volumes carry the `grill-room-daemon` label and are removed when the sandbox closes.

When someone presses the agent button, the room hands the turn to the host's daemon. The daemon fetches the job and the workspace tar, clears the sandbox's `/workspace` and unpacks the tar there. If the room's copy of the session differs from the local one, it fetches the room's copy. Then it runs `claude -p` with the prompt on stdin and `--resume` when the room has a session, and posts back the changed files, the final answer and the session bytes added. [docs/agent-workspace.md](docs/agent-workspace.md) has the workspace layout and the prompt contract. A turn is cancelled after the room's turn timeout. Each daemon keeps one sandbox per room, named from its state dir and the room, so a restart replaces what a crash left.

The sandbox image comes from the room server. When the server advertises a digest-pinned image (`GRILL_ROOM_SANDBOX_IMAGE` on the server), the daemon pulls it by digest. When it advertises none, or the pull fails, for instance on a registry that wants a login, the daemon builds the Dockerfile embedded in its binary and tags the result `grill-room-sandbox:local-<hash of the Dockerfile>`, so it builds once per Dockerfile change. The page shows the pull or build progress. Until the image is ready, the daemon tells the room it cannot host.

At start, and again each time it reconnects, the daemon asks the room server for its release (`/api/daemon/version`). When the server hands out a newer version built for this platform, the page offers "Update and restart". Nothing updates without that click. The daemon downloads the binary next to its own and checks it against the sha256 the server publishes. Only then does it rename it over the running binary, stop its sandboxes and exec the new version with the same room. On a mismatch it keeps the current binary and shows the error. The binary's directory must be writable by the user, as `~/.local/bin` is.

`grill-room-daemon check` checks Docker and the credentials, builds the embedded sandbox image unless one is pinned, runs `claude -p --model haiku "say ok"` in a throwaway sandbox, checks that only the proxy is reachable, and counts the real token's occurrences in the container, its volumes and its inspect output. It spends one Claude request.

| Variable | Default | |
|---|---|---|
| `GRILL_ROOM_DAEMON_STATE_DIR` | `<user config dir>/grill-room-daemon` | daemon tokens |
| `GRILL_ROOM_DAEMON_BROWSER` | `auto` | `none` prints the page link instead of opening it |
| `GRILL_ROOM_SANDBOX_IMAGE` | none | pins the sandbox image, e.g. `grill-room-sandbox:local` from `make sandbox-image`; skips the pull and the build |
| `GRILL_ROOM_AGENT_MODEL` | Claude Code's default | `--model` for agent turns, e.g. `haiku` |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | where Claude Code keeps its credentials |
| `LOG_LEVEL` | `info` | |

`docs/daemon-os-checklist.md` lists the manual checks on macOS and WSL.

### Distribution

The room server hands out the daemon itself. `make release` cross-compiles it for linux and darwin, amd64 and arm64 (static, `-trimpath`, version stamped), and writes a sha256 file per binary plus `VERSION`. The server image builds the same release into `/daemon`. These routes need no token:

| Route | |
|---|---|
| `GET /install.sh` | the installer |
| `GET /download/grill-room-daemon-<os>-<arch>[.sha256]` | a binary, or its `sha256sum` line |
| `GET /api/daemon/version` | `{"version", "sha256": {"<os>-<arch>": "<hex>"}, "sandbox_image"}` |

The room page has a "Host the agent" panel. It guesses the visitor's OS and CPU (Chromium reports the CPU, Safari and Firefox do not, so both stay editable), links the matching binary with its sha256, and shows the one-liner:

```sh
curl -fsSL https://<host>/install.sh | sh -s -- https://<host>/r/<room>
```

`install.sh` is POSIX sh. It picks the build from `uname` (WSL gets the Linux one, a Rosetta shell the arm64 one), downloads it from the host in the room URL, and checks it against the published sha256. Then it moves it into `~/.local/bin` and starts it with the room URL, which opens the pairing page. On a mismatch it installs nothing. It accepts plain `http` for `localhost` and `127.0.0.1` only.

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
- amd64 nodes for the room server and the worker, one of them with 2 free CPUs and 1.5 GiB for the worker;
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
- The STT worker requests 2 CPUs and may burst to 4, with 1.5 GiB. The model runs on 3 threads, leaving a CPU to the VAD and LiveKit. It updates with `Recreate`: no lab node has 2 CPUs free for a second worker beside the old one, so transcription stops for the minute a new pod takes to load the model.
- Every pod runs as non-root with a read-only root filesystem, no service account token and no service links.
- A default-deny NetworkPolicy blocks everything, then one policy per flow opens DNS, traefik to the room server and LiveKit, the worker to both, the room server to LiveKit's API, media between LiveKit and the worker, and the worker's traces to the cluster's OpenTelemetry collector (a Kyverno policy there injects `OTEL_EXPORTER_OTLP_ENDPOINT` into every pod).
- The Secret `grill-room` comes from the same `.env` keys compose uses.
- With `GRILL_ROOM_SMTP_ADDR` and `GRILL_ROOM_SMTP_FROM` in `.env`, the room server mails magic links; the `room-smtp` NetworkPolicy opens TCP `K8S_SMTP_PORT` (default 1025) to the namespace `K8S_SMTP_NAMESPACE` (default `mailpit`), where the relay runs.

`make e2e-lab` runs a whole session against the deployment through its public hostnames, with two participants, an upload, an annotation, audio and a voice line. Audio runs twice, once forced through TURN/TLS and once with default ICE. One agent turn runs with a real daemon on this machine and haiku, at most six Claude requests. [docs/lab-checklist.md](docs/lab-checklist.md) is the manual check with a participant outside the LAN.

## Make targets

`build` (server and daemon), `release` (the daemon release into `DIST`, default `dist/`), `run`, `test` (race detector), `test-docker` (sandbox and agent turns against real Docker, a fake Anthropic upstream and a fake `claude`, no Claude request), `e2e` (Playwright with the installed Chrome, needs pnpm, pandoc and Docker for the agent turns, which use the fake `claude`; then the magic link tests against a second server and a throwaway mailpit container), `e2e-voice` (Playwright against the compose stack, with fake microphones; it kills and restarts the `stt` container), `e2e-agent-live` (two real daemons against the compose stack with `claude --model haiku`: a turn, a daemon killed mid-turn, a takeover that recalls a code word through `--resume`; about six Claude requests), `e2e-install` (a clean Debian container runs the room's one-liner against the compose stack, pairs, takes a self-update and refuses a tampered release; it copies releases into the `grill-room` container and recreates it at the end; no Claude request), `stt-test` (ruff, mypy and pytest for the worker), `stt-image`, `cover`, `lint` (golangci-lint, shellcheck), `docker-build` (also tests the office fixtures against the pandoc it ships), `sandbox-image`, `sandbox-push` (pushes it to `REGISTRY` and prints its digest-pinned reference), `sandbox-fake-image` (the sandbox with `internal/sandbox/fakeclaude` as `claude`, for tests), `fixtures` (regenerates them), `icons` (renders the favicon, PNG icons and apple-touch-icon from the logo with Inkscape and ImageMagick), `dev-up`, `dev-down`, `traefik-install`, `traefik-uninstall`, `k8s-push`, `k8s-render`, `k8s-check`, `k8s-apply`, `k8s-delete` (lab deployment, above), `e2e-lab` (a session against it; up to six Claude requests), `e2e-lab-mail` (magic links against it, read through the lab mailpit at `K8S_MAILPIT_URL`; no Claude request).
