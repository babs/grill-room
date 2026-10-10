# grill-room

Self-hosted rooms for grill sessions. Guests join from an invite link with a display name, no account. The room shows who is in and a live message stream.

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
| `GET /api/rooms/{room}/events` | participant | SSE: history then live messages; resumes from `Last-Event-ID` |

Request bodies must be JSON with `Content-Type: application/json` (415 otherwise). Participants authenticate with `Authorization: Bearer <token>` or the cookie set at join. The server stores tokens as SHA-256 hashes.

## Local dev with docker compose and traefik

The dev setup expects a traefik on the machine that serves `*.<dev-domain>` with a valid wildcard certificate and watches a file-provider rules directory. Copy `.env.example` to `.env` and fill in `DEV_DOMAIN` and `TRAEFIK_RULES_DIR`.

```sh
make dev-up     # renders deploy/local/traefik-grill-room.yaml into the rules dir, starts compose
make dev-down   # removes the rule, stops compose
```

`make dev-up` publishes the app port on the docker bridge gateway only, which is where traefik's `host.docker.internal` points, so the plain-HTTP port stays off the LAN. The room is then at `https://grill-room.<dev-domain>/`. Read the admin token with `docker compose logs grill-room | grep admin_token_generated`.

## Make targets

`build`, `run`, `test` (race detector), `e2e` (Playwright with the installed Chrome, needs pnpm), `cover`, `lint` (golangci-lint), `docker-build`, `dev-up`, `dev-down`, `traefik-install`, `traefik-uninstall`.
