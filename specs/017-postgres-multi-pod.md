# 017: Room server on PostgreSQL, several server pods at once

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-17, questions answered 2026-10-18
**Context**: backlog "Server on PostgreSQL + multiple server pods" (2026-10-14); "Next step postgres" and a go for the spec (2026-10-17). Touches every spec that stores data. Builds on 003 (daemon stream, pairing, host election), 005 (presence, push 005-AC24, voice dedupe), 009 (notes watcher, plugin fetch), 011 (magic-link limits, purges), 013 (TTS pod stream, KEDA demand metric 013-AC34), 014 (egress holds), 016 (seats).

## Open questions

None blocking. The answers of 2026-10-18 are under Decisions as Q1 to Q12; the spec cites them by those numbers. Implementation loop: implement-loop, autonomous (user, 2026-10-18).

## Problem

The room server runs as one pod, `replicas: 1` with `strategy: Recreate`, on a ReadWriteOnce volume (`deploy/k8s/room.yaml:1`, `:40-42`). SQLite takes one writer. Every deploy and node drain takes the whole service down, a crash stops every room until the pod is back, and the server cannot spread load. A consistent backup needs the server scaled to 0.

## Solution

The server stores everything in PostgreSQL. Two or more server pods run behind one Service. A message, a turn result, a presence change or a TTS line handled by one pod reaches the browsers, apps and TTS pod streaming from any pod within a second. Background jobs run once, on one pod, and move to another pod when it dies. Deploys roll pod by pod. Backups run while the server runs. The lab's data moves over once, with a rollback path.

## Scope

- PG store: driver, schema, dialect fixes, write lock (Q2).
- One-shot copy of a SQLite database into an empty PG database, with verification.
- Cross-pod wake-ups for every stream (room, daemon, TTS pod, egress hold, push).
- Every item of the single-process inventory marked PG, NOTIFY, lock, leader or per-pod.
- Deploy: 2 replicas, `RollingUpdate`, PodDisruptionBudget, DSN secret, network policy to PG, scratch volume, models volume, backups by `pg_dump`.
- Go tests and e2e on PG in CI; a two-pod e2e.
- Docs: README config table, deploy and privacy sections, `docs/architecture.md`.

## Out of scope

- Redis or any broker (Q5).
- Native column types (Q3).
- Autoscaling the room server; replicas stay a fixed number.
- PostgreSQL HA. The spec consumes a DSN.
- Online cutover (Q9).
- Running or provisioning PostgreSQL. The deploy consumes a DSN to a shared cluster managed outside this repo (Q12).
- The daemon's local `monitor.db` (`internal/monitor/monitor.go:18`): stays SQLite.
- Connection poolers such as PgBouncer: session advisory locks and `LISTEN` need direct connections.
- Read replicas, multi-region.

## Analysis

### Store layer, measured 2026-10-17

`internal/store` non-test files: 13,946 lines. Tests: 11,830 lines, 217 `Test` functions. Counts are `grep -o` over non-test `.go` files of `internal/store`.

| Construct | Count | On PG | Where |
|---|---|---|---|
| `SELECT`, `INSERT INTO`, `UPDATE .. SET`, `DELETE FROM` | 653 | most run unchanged once placeholders are rebound | |
| `?` placeholders | 1,341 | `$n`, rebound once in the store's `execer` wrapper, skipping string literals | |
| `ON CONFLICT` / `excluded.` | 18 / 17 | same syntax | |
| `RETURNING` | 46 | same syntax | |
| `LastInsertId` | 11 | unsupported by pgx: `RETURNING id` | `store.go:1579`, `:1680`, `turns.go:162`, `:322`, `documents.go:126`, `:239`, `:253`, `annotations.go:134`, `:210`, `:219`, `plugins.go:226` |
| `PRAGMA` | 12 | 1 in a comment, 10 migration-only (`user_version`, `writable_schema`, `schema_version`), 1 at run time: `wal_checkpoint` (Q4) | `store.go:640`, `:754`, `:1010`, `notes.go:31`, `turns.go:523`, `merge.go:345` |
| DSN pragmas | 4 | `busy_timeout`, WAL, `foreign_keys`, `secure_delete`: none apply (Q4) | `store.go:974` |
| `json_extract` | 9 | `->>` on `::jsonb` | `admin.go:275`, `starters.go:148`, `plugins.go:44`, `:52`, `:587`, `:589`, `store.go:615-616` |
| `json_each` | 9 | `jsonb_array_elements`; the 4 `IN (SELECT value FROM json_each(?))` become `= ANY($n)` | `membership.go:70`, `:119`, `:122`, `:132`, `skills.go:454`, `plugins.go:44`, `:586` |
| `json_group_array` / `json_object` | 3 / 2 | `jsonb_agg` / `jsonb_build_object` | `overview.go:89`, `skills.go:436`, `plugins.go:512`, `store.go:1700` |
| `group_concat` | 1 | `string_agg` | `plugins.go:600` |
| `COLLATE NOCASE` | 3 | unique index on `lower(..)`, compare `lower(..)` | `store.go:408`, `:952`, `personas.go:422` |
| `unixepoch` | 1 | migration-only; gone with the baseline | `store.go:733` |
| `randomblob` UUID | 5 in 1 expression | `gen_random_uuid()` | `store.go:964`, `profiles.go:88` |
| `rowid` | 2 | `rooms` has a TEXT key: `ORDER BY created_at, id` | `store.go:1142`, `overview.go:90` |
| scalar `max(a, b)` | 1 | `GREATEST` | `notes.go:102` |
| `MAX(a = b)` on a boolean | 1 found | `bool_or(..)` | `merge.go:145` |
| `LIKE` (ASCII case-insensitive in SQLite) | 3 | `ILIKE` for the audit search | `admin.go:235` |
| `AUTOINCREMENT` / `INTEGER PRIMARY KEY` | 34 / 37 | `BIGINT GENERATED BY DEFAULT AS IDENTITY` | |
| `INTEGER` columns | all | `BIGINT`: PG `INTEGER` is 32-bit, `*_ms` values overflow it | |
| `BLOB` columns | 8 | `bytea` | `store.go:222`, `:277`, `:318`, `:434`, `:502`, `:721`, `:808`, `:944` |
| 0/1 flags (`CHECK .. IN (0, 1)`) | 19 | `SMALLINT` + same CHECK (Q3) | |
| `*_at TEXT` / `ts TEXT` | 52 / 1 | `TEXT`, written by `now()` as RFC 3339 nano (`store.go:1754`) | |
| Transactions | 97: 81 `withTx`, 16 `BeginTx` | each write takes the Q2 lock | `personas.go:477` |

Other findings:

- **The single connection is the lock.** `SetMaxOpenConns(1)` (`store.go:980`). Code relies on it: `MAX(change_seq) + 1` then write (`messages.go:102`, `documents.go:429`, `egress.go:126`); `overLimit` counts then inserts (`personas.go:35`, 7 callers, e.g. `personas.go:500`).
- **Commit order.** Every stream cursor reads "id greater than the last sent": room stream `Last-Event-ID` (`internal/web/server.go:1143-1147`, `:1217-1241`), edits by `change_seq` (`server.go:1250`), `documents`, `annotations` and `plugins` versions built from `MAX(id)` (`server.go:1287-1307`), the daemon stream's turn cursor (`daemons.go:411`), the push cursor (`push.go:182-190`). PG sequences hand out ids at insert, not at commit, so with concurrent writers id 101 can commit after 102 and a reader skips it for good. Q2 a keeps commit order; every write, single statements included, must take the lock.
- **Migrations.** 46 entries (`store.go:176`), tracked by `PRAGMA user_version` (`store.go:1010`). Two edit `sqlite_schema` (`store.go:640`, `:754`). Four run Go code (`store.go:968`). None applies to an empty PG. Baseline = schema v46 + the data `Open` ensures.
- **Boot writes.** `Open` runs `ensurePresets`, `ensureStarters` and two purges (`store.go:982-1001`). Two pods booting together race on them and on the schema: migrations and boot writes run under the Q2 lock.
- **Sort order.** SQLite compares bytes; PG sorts text by the database collation. Lists ordered by name keep today's order only with `C` collation.
- **Driver.** `modernc.org/sqlite` v1.60.1, pure Go (`store.go:16`). `github.com/jackc/pgx/v5/stdlib` keeps the `*sql.DB` and `*sql.Tx` code; `LISTEN` and the leader lock use raw `pgx.Conn`s. Pure Go too: `CGO_ENABLED=0` holds.
- **Test helpers.** `internal/sqlitetest` (fsync off, template file per test) and `internal/store/storetest/storetest.go` (migrated template). 7 test files outside the store open one, in `cmd/grill-room`, `cmd/grill-room-daemon`, `internal/web`, `internal/bus`, `internal/daemon`. 951 `Test` functions repo-wide.
- **The daemon keeps SQLite** (`internal/monitor/monitor.go:18`), so `modernc.org/sqlite` stays in `go.mod` whatever Q1 says.

### Single-process inventory

Comments already say it: "In memory: the room server runs one replica" (`internal/web/people.go:75-76`, `dedupe.go:25-26`, `profiles.go:527`). Paths below are under `internal/web/` unless stated. Treatments: **PG** (shared table, Q6), **NOTIFY** (cross-pod wake-up), **lock** (per-room `pg_advisory_xact_lock`), **leader** (Q7), **pod** (per-pod resource), **fine**.

Wake-ups and streams:

| # | Item | Where | With 2 pods | Needs |
|---|---|---|---|---|
| I1 | Bus subscribers | `internal/bus/bus.go:18-25`, `:42`, `:59`; 56 `Notify` sites; 4 subscribers: room stream `server.go:1181`, daemon stream `daemons.go:377`, egress hold `egress.go:381`, push `push.go:163` | a change on A wakes only A; B catches up at its 20 s heartbeat | NOTIFY |
| I2 | Room stream cursors (message id, `change_seq`, `MAX(id)` versions) | `server.go:1143-1174`, `:1217-1307` | out-of-order commits skip rows | Q2 lock |
| I3 | Daemon stream "sent" sets and turn cursor | `daemons.go:398-399`, `:411`, `:437`, `:460`, `:512` | per connection: fine; cursor as I2 | Q2 lock |
| I4 | Egress hold long-poll | `egress.go:336-411` | re-reads the DB every 1 s: works, up to 1 s late | fine (NOTIFY makes it instant) |

Daemons, turns, host:

| # | Item | Where | With 2 pods | Needs |
|---|---|---|---|---|
| I5 | Daemon presence registry `s.presence` | `daemons.go:75-92`, `:280-375`; readers `agent.go:113-137`, `:235-242`, `overview.go:48-61`, `:136-143`, `egress.go:22-30`, `chain.go:130`, `roomadmin.go:177` | only the pod holding the daemon's stream knows it; others answer 409 "agent is offline" (`turns.go:57-61`), fail fresh turns (`turns.go:197-199`), stop draining (`turns.go:237-240`) | PG `daemon_connections` with lease, NOTIFY |
| I6 | Host election `elect`, `electMu` | `agent.go:228-253`, `server.go:101`, `agent.go:395-396` | pods elect different hosts, flip back and forth, post a line each time | lock + I5 |
| I7 | `turnMu`: busy-check then start or enqueue, queue drain, auto-links | `server.go:107`, `turns.go:98-99`, `:231-232`, `autotriggers.go:25-44`, `chain.go:156-165`, `:235-249` | two pods both start, or both pop the queue | lock |
| I8 | Turn timeout backstop `time.AfterFunc` | `turns.go:200`, `:211-224`, `:449` | lost with its pod; a silent daemon's turn never times out | leader sweeper over `turns` |
| I9 | `FailRunningTurns` at boot | `cmd/grill-room/main.go:389-394` | each pod start kills turns running on the other pod's daemons | remove; leader sweeper fails turns whose daemon lease expired |
| I10 | Daemon-disconnect cleanup in a defer | `daemons.go:352-375` | a crashed pod runs no defer: turns stay running | lease expiry + sweeper |
| I11 | Pairing requests `s.pairings` | `daemons.go:32-46`, `:104-221` | created on A, polled on B: 404; caps per pod | PG `pairings` with expiry |
| I12 | `hostBy` banner | `server.go:104`, `agent.go:151`, `:293`, `:440-444` | shown only on the pod that moved the host | PG column on `rooms` |

People, voice, TTS:

| # | Item | Where | With 2 pods | Needs |
|---|---|---|---|---|
| I13 | Presence `s.people` (tabs, beats, audio, focus, device STT) | `people.go:75-152`, `:279-313`; `postPresence` `:333-371` | a tab's stream on A and its beats on B: each pod sees half; people flap offline | PG `presence` with expiry, NOTIFY |
| I14 | Presence readers | `egress.go:312-330`, `presence.go:258`, `autotriggers.go:74`, `push.go:204`, `:259`, `tts.go:240`, `:319`, `:372`, `:390`, `voice.go:209`, `:294` | egress says nobody can allow; timers skip rooms; pushes reach focused users; TTS drops lines `no_listener`; STT transcribes device-mode people twice | read I13 |
| I15 | Anonymous tab counter `stream-N` | `people.go:81`, `:100-101` | ids collide across pods | random id |
| I16 | STT worker registry `s.stt` | `voice.go:54-67`, `:102-210` | a heartbeat reaches one pod; the other shows STT unavailable | PG with expiry |
| I17 | Voice-line dedupe `voiceRecent`, `voiceMu` | `dedupe.go:25-60`, `voice.go:257-282` | device line on A and worker line on B both posted | lock + query recent voice messages |
| I18 | TTS line ring, seq from boot clock | `tts.go:27`, `:82-143`, `:163-192`, `:314-352`, `:423-432`; queued from `turns.go:834` | line queued on A, TTS pod streams from B: dropped `no_service`; `Last-Event-ID` meaningless across pods | PG `tts_lines` from one sequence, NOTIFY |
| I19 | TTS pod heartbeat and persona states | `tts.go:93-98`, `:195`, `:512-557` | pod shown down on the pod that got no beat | PG with expiry |
| I20 | TTS lexicon change counter | `tts.go:91`, `:146-159`, `lexicon.go:126-131` | an edit on A never reaches a TTS stream on B | version in PG, NOTIFY |
| I21 | Default-voice pick `tts.voiceMu` | `tts.go:103`, `:257-310` | two personas can get the same voice | lock |
| I22 | TTS demand and roster | `tts.go:368-413` | per-pod listeners: KEDA (`max(grill_room_tts_demand)`, `deploy/autoscale/tts.yaml:22`) undercounts | read I13; leader exports |

Limits, caches, files, secrets:

| # | Item | Where | With 2 pods | Needs |
|---|---|---|---|---|
| I23 | Magic-link limiter | `server.go:84`, `profiles.go:27-30`, `:274-311`, `:527-590` | limits times N: more mail per address | PG |
| I24 | Admin login limiter | `server.go:86`, `admin.go:24-25`, `:207-216` | N times the brute-force allowance on the admin token | PG |
| I25 | `mailFailedAt` | `server.go:89`, `profiles.go:56`, `:407`, `:412` | B offers mail while A knows the relay fails (011-AC28) | PG `settings` row |
| I26 | One plugin fetch per room `pluginRooms` | `server.go:119-120`, `plugins.go:187-210` | two fetches of one room race | lock |
| I27 | Client last-seen throttle | `client.go:89-121` | N times the writes | fine |
| I28 | Upload, archive, fetch, pandoc semaphores | `server.go:69-71`, `:118`, `archive.go:67-75`, `internal/convert/convert.go:116-125` | bounds become per pod | fine |
| I29 | VAPID key, instance id | `push.go:62-90`, `internal/store/roomadmin.go:130-135` | `ON CONFLICT DO NOTHING` then read: converge | fine |
| I30 | Admin token generated at boot | `cmd/grill-room/main.go:374-382` | race on first boot (Q11) | `SettingOrInit` |
| I31 | Archive staging beside the DB file; `cleanStaging` at boot | `archive.go:38-63`, `server.go:181`, `internal/store/export.go:283-286` | no DB file on PG; on a shared volume a booting pod deletes the other's staging | pod: `GRILL_ROOM_STAGING_DIR` on an `emptyDir` sized to `GRILL_ROOM_MAX_IMPORT_MB` |
| I32 | DB size in `/admin` instance view | `overview.go:119-130` | `os.Stat` of `db`, `-wal`, `-shm` | `pg_database_size()` |
| I33 | `Checkpoint` after purge and merge | `merge.go:97-109`, `server.go:729`, `profiles.go:522` | SQLite-only | removed, no PG step (Q4) |
| I34 | Daemon release dirs read at boot | `cmd/grill-room/main.go:396-411` | image skew during a rollout advertises two versions | fine, in the image |
| I35 | LiveKit tokens, kicks; MCP handler; session tokens (hashed in DB) | `voice.go:136-152`, `:335-360`, `mcp.go:647-676` | stateless | fine |

Loops (`cmd/grill-room/main.go:466-472`):

| # | Item | Where | With 2 pods | Needs |
|---|---|---|---|---|
| I36 | `PurgeAudit`, every 24 h | `admin.go:329-360` | duplicate deletes | leader |
| I37 | `PurgeProfiles`, every 1 h | `merge.go:112-128` | duplicate audit entries | leader |
| I38 | `RunTimers`: timer personas (15 s), notes watcher (2 s) | `autotriggers.go:63-203` | both pods evaluate; duplicates only stopped by `ErrAlreadyQueued` | leader + I13 |
| I39 | `PushLoop`: cursors, 5 s batches | `push.go:160-225` | without NOTIFY, pushes for B's messages never go; with it, every pod pushes: duplicates | leader; cursor in PG |

Metrics (`cmd/grill-room/main.go:492-551`):

| # | Item | With 2 pods | Needs |
|---|---|---|---|
| I40 | Counters: bus dropped, streams, voice lines, fallbacks, MCP calls, conflicts, TTS lines | additive | fine, `sum` |
| I41 | `grill_room_stt_workers`, `grill_room_device_stt_participants`, `grill_room_tts_pod_up`, `grill_room_tts_demand` | per-pod views of shared state | leader exports, others 0; dashboards `max` |

Health: `/healthz` writes `settings('healthz')` (`server.go:533-542`, `store.go:1040`). On PG every pod writes the same row each probe; harmless at a 10 s period, kept.

### Event fan-out design

- Each pod holds one `LISTEN grill_room` connection. `bus.Notify(room)` calls `pg_notify('grill_room', room)` and wakes local subscribers; the listener wakes local subscribers for other pods' notifications.
- Inside a write transaction, the notify goes out at commit, so a woken reader sees the row.
- On a lost `LISTEN` connection: reconnect, then wake every room once. Readers re-read the DB, so nothing is lost.
- Room stream events carry full rows (up to about 50 KB for an agent synthesis), but they are read from the DB, never sent through NOTIFY. Payloads are a room id or a short kind + id: far under the 8000-byte cap. Redis buys nothing here.

### Audio and several pods (Q8)

The user asked whether several pods cause a problem with audio. Media does not pass through the room server, so the calls themselves are unaffected. What the server does for audio moves to shared state in Phase 3.

| Part | Path | Through the room server? | With 2 pods |
|---|---|---|---|
| Call audio | browser to LiveKit over WebRTC | no; the server mints a 5 min token (`internal/web/voice.go:136-152`) and kicks over the LiveKit API (`voice.go:335-360`), both stateless | fine |
| STT worker media | worker joins LiveKit with its own token from the API key (`stt/grill_stt/worker.py:79`, `:146`) | no | fine |
| STT worker heartbeat and lines | `/api/stt/heartbeat`, `/voice/lines` (`voice.go:172-210`) | yes, state in memory (I16) | Phase 3: PG with expiry |
| Voice-line dedupe | `dedupe.go:25-60` | yes (I17) | Phase 3: lock + query |
| Who listens (gates TTS, the device-STT skip list) | `people.go:387`, `tts.go:240`, `voice.go:209` | yes, presence in memory (I13, I14) | Phase 3: shared presence |
| TTS line queue and pod stream | `GET /api/tts/events`, ring of 500 lines (`tts.go:27`), seq from the boot clock (`tts.go:107`) | yes (I18, I19) | Phase 3: `tts_lines` table, one sequence |
| Persona bots' media | TTS pod to LiveKit | no | fine |

Rolling deploy, the audio risk that remains:

- The TTS pod's stream ends when its server pod stops. The pod reconnects with backoff and sends `Last-Event-ID` (`tts/grill_tts/client.py:52`, `:72-83`), likely to the other pod.
- Today `Last-Event-ID` is a per-process number seeded from the boot clock; on another pod it either skips lines or resets and replays (`internal/web/tts.go:423-432`). With `tts_lines` and one sequence (Phase 3) it resumes right; a line queued during the gap waits in the table up to the room's queue maximum (013-AC14), then drops as `no_service`.
- The STT worker's heartbeat answer (tracks to skip) comes from whichever pod answers; with shared presence (Phase 3) both answer the same.
- Pods stay at 1 replica until Phase 4, so none of this applies before Phase 3 ships.

### Deploy, health, backups

| Item | Today | Multi-pod |
|---|---|---|
| Deployment | `replicas: 1`, `Recreate` (`deploy/k8s/room.yaml:40-42`) | `replicas: 2`, `RollingUpdate` with `maxUnavailable: 0`; PodDisruptionBudget `minAvailable: 1` |
| Data volume | RWO PVC `grill-room-data`, 2 Gi (`room.yaml:4-15`) | removed after the rollback window; PG holds the data |
| Models volume | RWO PVC `grill-room-models`, filled by an init container (`room.yaml:17-30`, `:67-81`) | RWO blocks a second pod on another node. `emptyDir` per pod filled by the same init container, or an RWX class (the fetch takes a flock, `internal/devicestt/lock_unix.go:13-20`) |
| Scratch | `/tmp` `emptyDir` 256 Mi (`room.yaml:153-155`); staging beside the DB | `emptyDir` for staging, sized to the import limit (I31) |
| Readiness / liveness | `/healthz` write check / TCP (`room.yaml:130-131`) | same, against PG |
| Metrics | VMPodScrape per pod (`deploy/monitoring/room.yaml`) | same; I41 |
| Backups | server at 0, copy `grill-room.db*`, sha256, `PRAGMA integrity_check` | `pg_dump -Fc` while running; restore test into a scratch database |
| Config | `GRILL_ROOM_DB`, default `grill-room.db` (`cmd/grill-room/main.go:159`) | `GRILL_ROOM_DATABASE_URL` from a secret; the copy command takes the SQLite file as `--from <file>` (Q1 a) |
| Network policy | no PG egress (`deploy/k8s/netpol.yaml`) | egress to PG 5432 |

### Tests and CI

- `make test` = `go test -race -cover -timeout 20m ./...` (`Makefile:75-76`), run in the `go` job (`.github/workflows/ci.yml:50`) and on Windows (`ci.yml:68`).
- e2e: Playwright starts one server on `test-results/e2e.db` (`e2e/playwright.config.js:53-59`); 56 spec files; flake gate (`ci.yml:174-209`).
- Needed: a PG 16 service container in the `go` and e2e jobs; a helper that gives each test its own database from a migrated template (`CREATE DATABASE .. TEMPLATE ..`), as `sqlitetest.Template` copies a file today; `GRILL_ROOM_TEST_DATABASE_URL` unset skips PG tests, which the Windows job does, listed in `docs/release.md` as the Docker tests are.
- New: a two-pod e2e, two server processes on one PG, one browser on each.
- SQLite as dev or single-node mode: dropped (Q1).

## Design options

| Decision | Options | Rec |
|---|---|---|
| Driver | `pgx/v5/stdlib`; `lib/pq` (maintenance mode); native `pgxpool` API | `pgx/v5/stdlib`: keeps every `*sql.Tx` signature |
| Dialect | rebind wrapper + about 40 rewritten sites; rewrite all 1,341 placeholders by hand; sqlc or a query builder | rebind wrapper in the store's `execer` |
| Schema | baseline = v46 translated, a `schema_version` table; replay the 46 migrations translated | baseline: the 46 include `sqlite_schema` edits and Go fixes that do not apply to an empty PG |
| Write serialization | Q2 | global advisory xact lock on every write |
| Fan-out | Q5 | NOTIFY, reconnect wakes all rooms |
| Shared short-lived state | Q6 | `UNLOGGED` tables with expiry |
| Singletons | Q7 | one leader, session advisory lock, followers retry every 5 s |
| Per-room critical sections (I6, I7, I17, I21, I26) | Go mutexes; `pg_advisory_xact_lock(hashtext(room))`; `SELECT .. FOR UPDATE` on the room row | advisory xact lock. Q2's global lock already serializes the writes; per-room locks cover check-then-act sequences that span reads and several store calls |
| Affinity | Q8 | none |
| Lab data | `grill-room db copy` subcommand; per-room export and import (`internal/store/export.go`, `import.go`) | subcommand: export and import skip server-wide tables (profiles, settings, audit, templates, skills) |

## Acceptance criteria

### PostgreSQL store

- [ ] **AC1** With `GRILL_ROOM_DATABASE_URL` set, the server starts on an empty PG 16 database, creates schema version 46 and the boot data (presets, starters, pronunciation seed) once, and serves. A second start changes nothing. Two pods starting at once give the same result. *Verify*: Go test: two concurrent opens, then `pg_dump --schema-only` and row counts compared with a single open.
- [ ] **AC2** Every store, web and server-side daemon test passes on PG. Store statement coverage on PG is at least the SQLite figure measured before Phase 1. *Verify*: `go test -cover ./internal/store/` before and after; both numbers in the PR.
- [ ] **AC3** No SQLite construct is left in server code. *Verify*: grep of non-test files of `internal/store` and `internal/web` for `PRAGMA`, `json_extract`, `json_each`, `json_group_array`, `group_concat`, `NOCASE`, `randomblob`, `rowid`, `LastInsertId`, `sqlite_schema`, `wal_checkpoint` returns 0; state the sweep.
- [ ] **AC4** Two stores on one database keep the invariants the single connection kept: 50 parallel message edits in one room give 50 distinct `change_seq` values; parallel `AddPersona` calls stop at `MaxRoomPersonas`; 200 parallel message inserts become visible to a reader in id order, with no id skipped by an "id greater than last" cursor. *Verify*: Go test with 2 `*Store`, under `-race`.
- [ ] **AC5** No column is PG `integer`; every former `BLOB` is `bytea`. *Verify*: a test over `information_schema.columns`.
- [ ] **AC6** The server refuses to start on a database whose collation is not `C`, with an error naming the fix. *Verify*: Go test.

### Data copy

- [ ] **AC7** `grill-room db copy --from <file> --to <dsn>` copies a v46 SQLite database into an empty PG database in one transaction, keeps every id, and sets each identity past its maximum. It refuses a non-empty target and a source at another version. *Verify*: Go test on a fixture with every table populated.
- [ ] **AC8** The copy prints, for each table on both sides, the row count and a sha256 over the rows in primary-key order, and exits non-zero on any difference. *Verify*: test with one target row altered.
- [ ] **AC9** The lab on PG serves the copied data: rooms, messages, documents and uploads open as before; a member's room cookie and profile session still work; the paired app reconnects without pairing again. *Verify*: the lab check script before and after cutover, same output.

### Several pods

- [ ] **AC10** With 2 pods, a line posted through pod A shows in a browser streaming from pod B within 1 s (p95 over 20 lines). Same for a turn result posted to A while the browser streams from B, and for a TTS line queued on A while the TTS pod streams from B. *Verify*: two-pod e2e; Go test for the TTS stream.
- [ ] **AC11** A turn starts, runs and ends whatever pod each request lands on: the app's stream on A, the "run" press on B, its result posted to A or B. No request answers "agent is offline" while the host app is connected to either pod. *Verify*: two-pod e2e with a fake app.
- [ ] **AC12** Host election gives one host per room with 2 pods: 20 connect and disconnect rounds of 2 fake apps spread over both pods post exactly one "holds the agent(s)" line per real change. *Verify*: Go test.
- [ ] **AC13** Pairing works when the app's request, the poll and the participant's answer land on different pods. *Verify*: Go test, each call on another server.
- [ ] **AC14** People, audio state, STT workers and the TTS pod show the same on both pods within 2 s: a tab streaming from A and beating to B is online on both. *Verify*: Go test on two servers.
- [ ] **AC15** A voice line from device transcription on A and the same line from the STT worker on B give one line in the room. *Verify*: Go test (005's dedupe test run across two servers).
- [ ] **AC16** Limits hold for the cluster: the 11th failed admin login from one IP in 15 min gets 429 whichever pod takes it; same for the magic-link caps (011). *Verify*: Go table test alternating pods.
- [ ] **AC17** Each loop (I36 to I39, I8's sweeper) runs on one pod only: one push per event, one purge per period, one timer turn per due trigger. Killing the leader's lock connection moves the loops within 15 s. `grill_room_leader` is 1 on one pod, 0 on the other. *Verify*: Go test with 2 servers.
- [ ] **AC18** A pod restart fails no turn running on the other pod's app. A turn whose app vanished with its pod fails within the turn timeout plus 30 s. *Verify*: Go test.
- [ ] **AC19** A rolling deploy of 2 pods drops nothing: one line per second posted through the ingress during the rollout all show, in order, in a browser that stays open; open streams resume with `Last-Event-ID`. *Verify*: lab run, log in the PR.
- [ ] **AC20** Metrics `grill_room_notify_sent_total`, `grill_room_notify_received_total`, `grill_room_listen_reconnects_total`, `grill_room_leader`, `grill_room_db_lock_wait_seconds`; the I41 gauges come from the leader only. Listed in `docs/architecture.md`. *Verify*: scrape of both pods.

### Deploy and operations

- [ ] **AC21** Manifests: 2 replicas, `RollingUpdate` with `maxUnavailable: 0`, a PodDisruptionBudget with `minAvailable: 1`, a DSN secret, a models volume a second node can mount, a staging `emptyDir`, a NetworkPolicy to PG. *Verify*: `make k8s-render`; draining one node keeps the room page live.
- [ ] **AC22** `/healthz` answers 503 when PG refuses a write, 200 otherwise. *Verify*: Go test with the database refusing writes (read-only).
- [ ] **AC23** A backup runs with the server up (`pg_dump -Fc`); a restore into a scratch database passes AC8's comparison against the source snapshot. *Verify*: lab run.
- [ ] **AC24** README (config, deploy) and `docs/architecture.md` describe PG and the pods. *Verify*: grep of README and `docs/` for "SQLite" returns only daemon and history lines.
- [ ] **AC27** README "Privacy" replaces the `secure_delete` line (README:321) with: removed rows leave the tables at once, the database files after PostgreSQL's autovacuum or a `VACUUM` reuses their space, and backups after their retention. No text promises zeroed pages, and the server runs no `VACUUM` of its own. *Verify*: grep of README for `secure_delete` and "overwrite" returns 0; grep of non-test Go files for `VACUUM` returns 0.

### Tests and CI

- [ ] **AC25** CI runs the Go tests and the e2e suite on PG 16, and the two-pod e2e. *Verify*: green run linked in the PR.
- [ ] **AC26** `make test` takes at most 30 % longer than on SQLite. *Verify*: both timings in the PR.

## Phases

### Phase 1: Store on PostgreSQL, one pod (L)
- Work: pgx driver; DSN config; rebind wrapper; baseline schema; the dialect sites above; `RETURNING id`; Q2 lock on every write, migrations and boot writes; `C` collation check; Q11; staging dir and DB size (I30 to I33); PG test helper and CI service; Q1 applied.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC22, AC25 (one pod), AC26
- **Data model impact**: PG baseline equal to SQLite v46; `schema_version` table.
- **DoD**: `make lint test` green on PG; AC3 sweep at 0, stated; AC2 numbers in the PR; e2e green on PG with one server.

### Phase 2: Lab copy and cutover (S)
- Work: database and login role created on the shared cluster (external, Q12); `grill-room db copy` with verification; runbook; cutover at `replicas: 1`; `pg_dump` backup; README and architecture docs.
- **Covers**: AC7, AC8, AC9, AC23, AC24, AC27
- **Data model impact**: none.
- **DoD**: AC7 and AC8 tests green; lab cutover done, check script identical before and after; rollback rehearsed once in a scratch namespace.

### Phase 3: Cross-pod events and shared state (L)
- Work: NOTIFY bridge under `bus.Notify` (I1); `UNLOGGED` tables for daemon connections, presence, STT workers, TTS pod beat, pairings (I5, I11, I13 to I16, I19); TTS line table (I18); lexicon version (I20); per-room locks (I6, I7, I17, I21, I26); `hostBy` column (I12); limiters and `mailFailedAt` in PG (I23 to I25).
- **Covers**: AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC20 (notify metrics)
- **Data model impact**: `daemon_connections`, `presence`, `stt_workers`, `tts_pod`, `pairings`, `tts_lines`, `rate_events` tables; `rooms.host_moved_line`; a lexicon version in `settings`.
- **DoD**: two-pod e2e of AC10 and AC11 green locally and in CI; Go tests of AC12 to AC16 green.

### Phase 4: Singletons and rollout (M)
- Work: leader lock; loops I36 to I39 under it; push cursor in PG; turn sweeper replacing I8, I9, I10; leader-only gauges (I22, I41); manifests; rolling deploy on the lab.
- **Covers**: AC17, AC18, AC19, AC20, AC21
- **Data model impact**: push cursor in `settings`.
- **DoD**: AC17 and AC18 tests green; lab rollout of AC19 with its log in the PR; node drain of AC21 done.

## Data model impact (summary)

- PG schema equal to SQLite v46: `BIGINT` integers, `bytea` blobs, `lower(..)` unique indexes for the `NOCASE` columns, identity columns for the 34 `AUTOINCREMENT` keys; `schema_version` replaces `PRAGMA user_version`.
- Phase 3: `UNLOGGED` tables `daemon_connections`, `presence`, `stt_workers`, `tts_pod`, `pairings` (each with `expires_at`); logged tables `tts_lines`, `rate_events`; `rooms.host_moved_line`.
- Phase 4: push cursor and lexicon version as `settings` rows.

## Rollback plan, lab data migration

1. Before: consistent SQLite backup (server at 0, copy, sha256, `integrity_check`). Note the image tag. Announce the 24 h window (Q10).
2. Cutover: server stays at 0; `grill-room db copy` from the backup file; AC8's verification must pass; deploy the PG image at `replicas: 1`; run the check script.
3. Trigger: the check script differs, or a blocking bug inside the window.
4. Rollback: scale to 0; redeploy the previous image on the kept `grill-room-data` PVC; run the check script. Writes made on PG since cutover are lost.
5. After the window: keep the backup file 30 days; delete the old PVC; go on with Phase 3 (pods stay at 1 until Phase 4).

## Risks

| Risk | Impact | Mitigation |
|---|---|---|
| A query no test runs fails on PG (strict types, `GROUP BY`, booleans) | 500 on one route after cutover | AC2 coverage floor; PG fails loudly; Phase 2 runs the lab on one pod before any second pod |
| A write outside the Q2 lock commits out of id order | a stream skips a line for good | all writes go through the locked path; AC4's visibility test |
| Phase 3 is the bulk: 20 inventory items change from memory to PG | schedule; subtle cross-pod bugs | one AC per item; two-pod e2e in CI from Phase 3 on |
| Presence beats and stream polls on PG (each stream reads presence every 1 s, `server.go:1334`) | DB load grows with open tabs | one cached read per room per pod per second; `UNLOGGED` tables; measure on the lab |
| Leader change: pushes during the gap are lost (cursor starts at the last message, `push.go:164-165`) | missed pushes | cursor in PG (Phase 4) |
| Weaker delete guarantee (Q4) | removed rows stay in PG files until autovacuum reuses the space, and in backups until their retention ends | README states it (AC27); backup retention is the cluster's, stated there |
| Pool exhaustion | stalls | streams hold no connection between reads; `grill_room_db_lock_wait_seconds` watched |
| Audio during a rolling deploy | the TTS pod's stream drops when its pod stops; with lines still in a pod's memory, speech is lost or replayed | Phase 3 moves lines to `tts_lines` with one sequence, so `Last-Event-ID` resumes on any pod; before Phase 3, pods stay at 1 |
| Models volume per pod | 1 Gi download per pod start | init container skips files with the right sha256; RWX if available |

## Decisions

User answers (user, 2026-10-18):

- **Q1 a**: PG only on the server; SQLite stays in the daemon's `monitor.db`. The user would have preferred b (both engines) and accepted a for the cost: every migration written twice, every store test run twice, a second implementation of each shared-state item.
- **Q2 a**: every write transaction, single statements included, takes one global `pg_advisory_xact_lock`: today's one-writer semantics, ids commit in id order. Marked `ponytail:` with its ceiling.
- **Q3 a**: same column encodings as today (RFC 3339 `TEXT`, `*_ms` integers, 0/1 `SMALLINT` flags); `INTEGER` to `BIGINT`, `BLOB` to `bytea`.
- **Q4 a**: the privacy promise is rewritten (AC27): removed rows leave the tables at once, the files after autovacuum or `VACUUM`, backups after their retention. No explicit `VACUUM` step; `Checkpoint` goes.
- **Q5 a**: PG `LISTEN/NOTIFY`, payload = room id. No Redis.
- **Q6 a**: short-lived shared state in `UNLOGGED` tables with an expiry column, plus NOTIFY on change.
- **Q7 a**: one leader through a session `pg_try_advisory_lock` on a dedicated connection.
- **Q8 a**: no session affinity. The user asked whether that is a problem for audio: see Analysis, "Audio and several pods".
- **Q9 a**: stop, copy, start on PG; minutes of downtime.
- **Q10 a**: rollback = previous image + the untouched SQLite volume; window 24 h, announced before cutover; writes made on PG since cutover are lost on rollback.
- **Q11 a**: the generated admin token goes through `SettingOrInit`; every pod uses the stored value.
- **Q12**: a shared PostgreSQL 16+ cluster managed outside this repo. The deploy needs a DSN secret, one database owned by one login role, `C` collation, and a direct (non-pooled) connection to the primary for `LISTEN/NOTIFY` and the advisory locks. Creating the role and database is an external step of Phase 2.

Made while drafting (overturnable):

- Number 017: 015 is reserved for the Codex provider (backlog).
- `GENERATED BY DEFAULT AS IDENTITY`, not `ALWAYS`: the copy inserts explicit ids. Room import inserts only the room's TEXT id (`internal/store/import.go:230`).
- `C` collation to keep today's sort order.
- No pooler in front of PG.
- The I41 gauges come from the leader only, so `max` and `sum` both read right.
