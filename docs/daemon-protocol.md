# Supervisor and worker protocol

`grill-room-daemon` with no subcommand runs the supervisor. The supervisor starts one worker per instance (room server) as `grill-room-daemon worker --instance <id>` and talks to it over the worker's stdin and stdout. Each line is one JSON object, at most 4 MiB. The worker's stderr goes to the supervisor's stderr and only carries crashes and startup errors.

The instance id is the first 12 hex digits of the sha256 of the server's scheme, lower-cased host and port, with the default port spelled out: `https://rooms.example` and `HTTPS://Rooms.example:443` are one instance.

## Versions

Both sides carry a protocol version, `Proto` in `internal/daemon/proto.go`, currently 3. Version 2 added `access`, version 3 the slots under the global cap. A worker that settles on version 2 asks for no slot: its turns run outside the cap. Peers at most one version apart (`ProtoSkew`) talk at the lower of the two. Further apart, the side that notices sends `refused` and the worker exits. The supervisor marks the instance `refused` with the reason and does not restart it.

A version bump must stay additive within that window: new fields and new message types only. Both sides ignore unknown fields and unknown message types. A supervisor must not send a request type its worker's agreed version lacks.

## Handshake

1. The worker sends `{"type":"hello","proto":3,"version":"v0.0.3","instance":"<id>"}`.
2. The supervisor answers either `{"type":"refused","proto":3,"error":"..."}`, or `{"type":"config","proto":3,"version":"v0.0.3","config":{...}}`.
3. The worker checks the supervisor's `proto` in turn, and may answer `refused` before it exits.

A worker that says nothing within 10 s is killed and restarted.

The `config` object:

| Field | |
|---|---|
| `instance` | the instance id; a worker refuses another instance's config |
| `config_dir`, `state_dir`, `cache_dir` | the instance's directories: `settings.json` and `allowlist.json`, `tokens.json`, `workspace-cache/` |
| `sandbox_root` | keys the sandbox names and the `grill-room-root` label, so that two supervisors never share a sandbox |
| `log_level`, `image`, `cache_mb` | as the supervisor's environment sets them |
| `settings` | the host cap and approval mode until the page saves the instance's own |
| `limits` | `cpus`, `memory`, `egress_cpus`, `egress_memory` for the sandbox and its egress proxy |
| `rooms` | the instance's room links, oldest first; the worker serves every one that holds a token |
| `pair` | a room to pair with now, beside the others |
| `access` | how the rooms reach Claude, below; sent to workers at version 2 or later |

The `access` object never holds a Claude token, an account name or a config dir (003-AC14, 003-AC17):

| Field | |
|---|---|
| `proxies` | by room link: `addr`, the `<gateway>:<port>` the sandbox relay dials for the room's account proxy (`host-gateway` under Docker Desktop), and `bearer`, the dummy token that proxy knows this worker by; or `error`, why the room cannot reach Claude now, in words the room may show |
| `cut_off` | the host cut this worker off: it ends its running turns with an error and starts none |

## Supervisor to worker

Requests carry an `id`; the worker answers each one with `{"type":"reply","id":N}`, plus `"error"` when it failed. The supervisor gives up on a reply after 30 s.

| Type | Fields | Does |
|---|---|---|
| `pair` | `room` | serves that room beside the others, pairing first if no token is stored for it |
| `settings` | `settings` | saves the host cap and approval mode, and tells the room |
| `allowlist` | `allowlist` (absent for an empty list) | saves the egress allowlist; the running sandbox takes it at once |
| `denied` | `n` | the reply's `denied` lists the last `n` refused egress requests |
| `recheck` | | re-runs the Docker check, and retries a failed sandbox image |
| `access` | `access` | replaces the access: a room whose proxy moved gets a new sandbox at its next turn, and a new `cut_off` ends the running turns. Sent whenever it changes: an account added, mapped or expired, a cut-off, Docker coming up |
| `update` | | refused with an error: only the supervisor updates, and it updates itself (003-AC5). Phase 2 workers replaced their binary here |
| `wait` | `slot` | no reply: the worker's turn waits in the supervisor's queue (version 3) |
| `grant` | `slot` | no reply: the turn may run (version 3) |
| `stop` | | no reply: the worker ends its rooms, removes their sandboxes and exits 0 |

A closed stdin means the same as `stop`, so a worker whose supervisor died stops on its own.

## Worker to supervisor

| Type | Fields | When |
|---|---|---|
| `status` | `status`, the page's view of the worker; `update.served` is the version its room server hands out; `rooms` lists each room's pairing, connection, and turns `running` and `queued` | whenever it changes, checked every 250 ms |
| `metrics` | `metrics`: `uptime_seconds`, `turns`, `failed_turns`, `heap_bytes`, `goroutines` | at start, then every 30 s |
| `turn` | `turn`: `room`, `id`, `phase` (`start` or `end`), `error`, `duration_ms`, and at the end `queue_ms`, the wait for a slot, `persona`, the session's `context_tokens` and `context_window`, `cause`, `asked_model` and `asked_effort`, `model` and `effort` as run, `model_note` and `model_mismatch` as in the turn result, `tokens_in`, `tokens_out`, `tokens_cache_read` and `tokens_cache_write` summed over every model claude called, `cost_usd` from claude's `total_cost_usd`, and `workspace_bytes`, the workspace tar's size. Absent fields are unknown | as a turn starts and ends |
| `acquire` | `slot`, a number the worker picks; `turn`: `room`, `id` | before each turn, version 3: the supervisor answers `grant`, or `wait` then `grant` |
| `release` | `slot` | once the turn ran, or when it gives up waiting |
| `log` | `log`, one zap JSON entry | for every log line. The supervisor re-encodes it before writing it to its stderr and to the page's log: it sets `instance` itself, and escapes control characters, C1 codes and Unicode format characters (bidi overrides, zero-width), so a worker binary cannot drive the terminal or fake a line. Entries over 16 KiB are dropped. The worker's stderr gets the same treatment, one entry per line with `"stream":"stderr"` |

## Global cap

The supervisor lets at most `max_turns` turns run at once, 2 by default, across every worker (003-AC18). A turn past it waits in one queue, first in first out, whatever its instance and room. A release, a worker's exit, or a higher cap grants the next ones. The page sets the cap, saved in `supervisor.json`, and shows the turns holding and waiting for a slot, with the slots granted since start, how many waited, and the average and longest wait.

## Monitoring

The supervisor records every `turn` end in `monitor.db`, a SQLite file in its state directory, owner-only (003-AC27). A row holds the instance, room, persona and the account the room used at that moment, the event's fields, and a status sorted from the error: `ok`, `cut off`, `conflict` (the room refused the result for a document conflict), `expired` (the turn ran out of time, or the Claude login expired) or `failed`. The page lists the latest 500 turns a filter matches, sums them per account and day, and exports every match as CSV. Text cells starting with `=`, `+`, `-`, `@`, a tab or a carriage return get a leading `'` so that a spreadsheet does not run them.

Rows older than the retention, 30 days unless the page sets 1 to 3650 in `supervisor.json`, are deleted at start, daily, and when the page shortens it (003-AC28).

The page shows four alerts (003-AC29): a Claude account's expiry warning, a running worker whose binary nobody signed, a persona whose latest recorded context is above 80% of its window in a room still served, and a turn waiting more than 10 minutes for a slot.

## Admin page

The page is written with Preact, htm and @preact/signals, served from `internal/uivendor/esm/`, shared with the room server's `/admin`, as unmodified ESM files with their licenses, and mapped by an import map that the CSP admits by its hash. No CDN, no build step. `tools/ui/package.json` pins the versions; `make ui-vendor` copies them again, and `make ui-check` (part of `make lint`) runs `tsc --checkJs` on the page's JSDoc types.

The room's stream tells each daemon whether it hosts the room's agent and whether the room runs personas in parallel: a `room` event, `{"host":true,"parallel_turns":false}`, sent after `hello` and whenever either changes. Daemons from before ignore it.

## Claude accounts

Only the supervisor reads Claude tokens. It runs one bearer-swap proxy per account and gives each worker its own dummy bearer for each proxy, so the proxy counts the worker's requests and a cut-off revokes them alone. Workers start with every `CLAUDE_*` and `ANTHROPIC_*` variable removed from their environment. A worker binary older than v0.0.3 is refused: it read the credentials itself.

## Supervision

A worker that exits without being told to is restarted after 1 s, then 2 s, 4 s and so on up to 30 s. A worker that ran for over a minute starts again from 1 s. Before the restart, the supervisor frees the worker's slots and removes the sandboxes a crash leaves behind: every Docker object labelled with its state root and the instance, and the first sandbox of each room by name, for workers that predate the labels. At start, before any worker, it removes every object labelled with its state root: a supervisor killed with its workers left them. A refused worker, by the handshake or by the binary checks, is not restarted until something changes: a dev key added or removed, or the instance's "accept unsigned" toggle. When a worker's status reports that its instance serves another version than the one it runs, the supervisor restarts it at once on that version.

When the supervisor stops, it sends `stop` to every worker, waits up to 45 s for each to exit, then kills it and removes its sandboxes.

## Single supervisor and handoff

The supervisor holds an exclusive lock on `supervisor.lock` in its state directory: `flock` on Unix, `LockFileEx` on Windows. The kernel drops it when the process dies.

A later launch that finds the lock taken connects to the local socket: `run/supervisor.sock` in the state directory, owner-only, or on Windows the named pipe `\\.\pipe\grill-room-<12 hex digits of the state dir's sha256>`, whose DACL admits only the current user. It sends one line, `{"room":"<room link>"}` or `{}`, and reads `{"link":"<admin page link>"}` or `{"error":"..."}`. With a room, the launch prints the link, opens it, and exits 0. Without one, it prints the link and exits 1: a second supervisor is refused. A launch opens a link only if it starts with `http://127.0.0.1:`.
