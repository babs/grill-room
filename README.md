# grill-room

Self-hosted rooms for grill sessions. Guests join from an invite link with a display name, no account. The room shows who is in, a live message stream and the room's documents.

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
| `LOG_LEVEL` | `info` | |
| `LOG_FORMAT` | `auto` | `console` on a TTY, `json` otherwise |

`/healthz` returns 200 when SQLite accepts a write and 503 otherwise.

## Rooms and invites

The admin token creates rooms and invites:

```sh
curl -H "Authorization: Bearer $ADMIN" --json '{"name":"Pricing review"}' localhost:7680/api/rooms
curl -H "Authorization: Bearer $ADMIN" -X POST localhost:7680/api/rooms/$ROOM/invites
```

The second call returns a `url` such as `https://<host>/join#<token>`. Whoever opens it picks a display name and lands in the room. One invite can be shared with several people.

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
| `POST /api/rooms/{room}/invites` | admin | new invite link |
| `DELETE /api/rooms/{room}/invites/{id}` | admin | revoke |
| `POST /api/join` | invite token in the body | returns a participant token and sets a cookie |
| `GET /api/rooms/{room}` | participant | room, me, participants |
| `POST /api/rooms/{room}/messages` | participant | post a text message |
| `GET /api/rooms/{room}/events` | participant | SSE: history then live messages; resumes from `Last-Event-ID`; a `documents` event when a document or revision is added, an `annotations` event when an annotation is added or changes; with voice on, an `stt` event (`available`, `listening`) at open and on every change |
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
| `GET /api/rooms/{room}/daemon/events` | daemon | SSE: `hello`, pings, `revoked` |
| `POST /api/rooms/{room}/daemon/status` | daemon | heartbeat: `can_host`, `reason`, `version` |
| `POST /api/stt/heartbeat` | STT token | `worker` name and the `rooms` whose audio it is in; a worker silent for 15 s counts as gone |
| `POST /api/rooms/{room}/voice/lines` | STT token | one transcribed utterance: `participant_id`, `body`; stored as a `voice` message from that participant |

Request bodies must be JSON with `Content-Type: application/json` (415 otherwise), uploads excepted. A body over its route's limit gets a 413. Participants authenticate with `Authorization: Bearer <token>` or the cookie set at join. The server stores tokens as SHA-256 hashes.

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

## Host daemon

`grill-room-daemon` runs on a participant's machine: Linux, macOS, or Windows inside WSL with the Linux build. It runs the agent with that participant's own Claude Code login, in a Docker sandbox.

```sh
make build sandbox-image
./bin/grill-room-daemon https://<host>/r/<room>
```

It opens a page on `127.0.0.1` in the default browser. The page shows the pairing, Docker, the Claude credentials, whether this machine can host the agent, the logs, and a stop button. Without a room URL on the command line, paste the room link there. To pair, follow the page's link to the room, check that both pages show the same code, and press Approve: the room gives the daemon a token, which it keeps in `tokens.json` under its state dir. The daemon only opens outbound connections: an SSE stream from the room plus a status POST every 15 s, and it reconnects with backoff.

The page link carries a one-time code. The page trades it for a session token kept in the tab's `sessionStorage`, and the daemon prints a fresh link in its terminal after each use. The page refuses requests whose `Host` is not `127.0.0.1:<port>` or, for writes, whose `Origin` is not the page's own.

Credentials are read, never written: the macOS Keychain first (`security find-generic-password`, 3 s timeout, so a locked Keychain fails instead of hanging), then `<config dir>/.credentials.json`, where the config dir is `$CLAUDE_CONFIG_DIR` or `~/.claude`. The host's Claude Code keeps refreshing the token; the daemon re-reads it when the file changes, when it expires and after an upstream 401.

The sandbox holds no credential. It sits on an `--internal` Docker network whose only other member is a socat relay to the daemon's bearer-swap proxy. The proxy replaces the sandbox's dummy bearer with the real token. It listens on the default bridge gateway, or on `127.0.0.1` under Docker Desktop. The daemon writes a dummy `.credentials.json` and an `oauthAccount` stub before every turn and sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `ENABLE_CLAUDEAI_MCP_SERVERS=false` (spike S3). Containers, network and volumes carry the `grill-room-daemon` label and are removed when the sandbox closes.

`grill-room-daemon check` checks Docker and the credentials, runs `claude -p --model haiku "say ok"` in a throwaway sandbox, checks that only the proxy is reachable, and counts the real token's occurrences in the container, its volumes and its inspect output. It spends one Claude request.

| Variable | Default | |
|---|---|---|
| `GRILL_ROOM_DAEMON_STATE_DIR` | `<user config dir>/grill-room-daemon` | daemon tokens |
| `GRILL_ROOM_DAEMON_BROWSER` | `auto` | `none` prints the page link instead of opening it |
| `GRILL_ROOM_SANDBOX_IMAGE` | `grill-room-sandbox:local` | sandbox image, built by `make sandbox-image` |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | where Claude Code keeps its credentials |
| `LOG_LEVEL` | `info` | |

`docs/daemon-os-checklist.md` lists the manual checks on macOS and WSL.

## Local dev with docker compose and traefik

The dev setup expects a traefik on the machine that serves `*.<dev-domain>` with a valid wildcard certificate and watches a file-provider rules directory. Copy `.env.example` to `.env` and fill in `DEV_DOMAIN` and `TRAEFIK_RULES_DIR`.

```sh
make dev-up     # renders deploy/local/traefik-grill-room.yaml into the rules dir, starts compose
make dev-down   # removes the rule, stops compose
```

`make dev-up` publishes the app port and LiveKit's 7880 and 5349 on the docker bridge gateway only, which is where traefik's `host.docker.internal` points, so the plain-HTTP ports stay off the LAN. The room is then at `https://grill-room.<dev-domain>/`, LiveKit signalling at `livekit.<dev-domain>` and TURN at `turn.<dev-domain>`. Compose needs the LiveKit key pair and the STT token in `.env`. With `GRILL_ROOM_ADMIN_TOKEN` set there, that is the admin token; otherwise read the generated one with `docker compose logs grill-room | grep admin_token_generated`.

## Make targets

`build` (server and daemon), `build-daemons` (daemon for linux and darwin, amd64 and arm64), `run`, `test` (race detector), `test-docker` (sandbox against real Docker and a fake Anthropic upstream, no Claude request), `e2e` (Playwright with the installed Chrome, needs pnpm and pandoc), `e2e-voice` (Playwright against the compose stack, with fake microphones), `cover`, `lint` (golangci-lint), `docker-build` (also tests the office fixtures against the pandoc it ships), `sandbox-image`, `fixtures` (regenerates them), `dev-up`, `dev-down`, `traefik-install`, `traefik-uninstall`.
