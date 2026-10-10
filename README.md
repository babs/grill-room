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
| `GET /api/rooms/{room}/events` | participant | SSE: history then live messages; resumes from `Last-Event-ID`; a `documents` event when a document or revision is added, an `annotations` event when an annotation is added or changes |
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

## Local dev with docker compose and traefik

The dev setup expects a traefik on the machine that serves `*.<dev-domain>` with a valid wildcard certificate and watches a file-provider rules directory. Copy `.env.example` to `.env` and fill in `DEV_DOMAIN` and `TRAEFIK_RULES_DIR`.

```sh
make dev-up     # renders deploy/local/traefik-grill-room.yaml into the rules dir, starts compose
make dev-down   # removes the rule, stops compose
```

`make dev-up` publishes the app port on the docker bridge gateway only, which is where traefik's `host.docker.internal` points, so the plain-HTTP port stays off the LAN. The room is then at `https://grill-room.<dev-domain>/`. Read the admin token with `docker compose logs grill-room | grep admin_token_generated`.

## Make targets

`build`, `run`, `test` (race detector), `e2e` (Playwright with the installed Chrome, needs pnpm and pandoc), `cover`, `lint` (golangci-lint), `docker-build` (also tests the office fixtures against the pandoc it ships), `fixtures` (regenerates them), `dev-up`, `dev-down`, `traefik-install`, `traefik-uninstall`.
