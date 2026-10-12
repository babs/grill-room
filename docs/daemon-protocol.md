# Supervisor and worker protocol

`grill-room-daemon` with no subcommand runs the supervisor. The supervisor starts one worker per instance (room server) as `grill-room-daemon worker --instance <id>` and talks to it over the worker's stdin and stdout. Each line is one JSON object, at most 4 MiB. The worker's stderr goes to the supervisor's stderr and only carries crashes and startup errors.

The instance id is the first 12 hex digits of the sha256 of the server's scheme, lower-cased host and port, with the default port spelled out: `https://rooms.example` and `HTTPS://Rooms.example:443` are one instance.

## Versions

Both sides carry a protocol version, `Proto` in `internal/daemon/proto.go`, currently 1. Peers at most one version apart (`ProtoSkew`) talk at the lower of the two. Further apart, the side that notices sends `refused` and the worker exits. The supervisor marks the instance `refused` with the reason and does not restart it.

A version bump must stay additive within that window: new fields and new message types only. Both sides ignore unknown fields and unknown message types. A supervisor must not send a request type its worker's agreed version lacks.

## Handshake

1. The worker sends `{"type":"hello","proto":1,"version":"v0.0.2","instance":"<id>"}`.
2. The supervisor answers either `{"type":"refused","proto":1,"error":"..."}`, or `{"type":"config","proto":1,"version":"v0.0.2","config":{...}}`.
3. The worker checks the supervisor's `proto` in turn, and may answer `refused` before it exits.

A worker that says nothing within 10 s is killed and restarted.

The `config` object:

| Field | |
|---|---|
| `instance` | the instance id; a worker refuses another instance's config |
| `host` | this machine's name, shown in the rooms |
| `config_dir`, `state_dir`, `cache_dir` | the instance's directories: `settings.json` and `allowlist.json`, `tokens.json`, `workspace-cache/` |
| `sandbox_root` | keys the sandbox names, so that two supervisors never share a sandbox |
| `log_level`, `model`, `image`, `cache_mb` | as the supervisor's environment sets them |
| `settings` | the host cap and approval mode until the page saves the instance's own |
| `limits` | `cpus`, `memory`, `egress_cpus`, `egress_memory` for the sandbox and its egress proxy |
| `rooms` | the instance's room links, oldest first |
| `pair` | a room to pair with now; without it the worker reconnects to the newest room in `rooms` that holds a token |

## Supervisor to worker

Requests carry an `id`; the worker answers each one with `{"type":"reply","id":N}`, plus `"error"` when it failed. The supervisor gives up on a reply after 30 s.

| Type | Fields | Does |
|---|---|---|
| `pair` | `room` | serves that room, pairing first if no token is stored for it |
| `settings` | `settings` | saves the host cap and approval mode, and tells the room |
| `allowlist` | `allowlist` (absent for an empty list) | saves the egress allowlist; the running sandbox takes it at once |
| `denied` | `n` | the reply's `denied` lists the last `n` refused egress requests |
| `recheck` | | re-runs the Docker and credentials checks, and retries a failed sandbox image |
| `update` | | refused with an error: only the supervisor updates, and it updates itself (003-AC5). Phase 2 workers replaced their binary here |
| `stop` | | no reply: the worker ends its rooms, removes their sandboxes and exits 0 |

A closed stdin means the same as `stop`, so a worker whose supervisor died stops on its own.

## Worker to supervisor

| Type | Fields | When |
|---|---|---|
| `status` | `status`, the page's view of the worker; `update.served` is the version its room server hands out | whenever it changes, checked every 250 ms |
| `metrics` | `metrics`: `uptime_seconds`, `turns`, `failed_turns`, `heap_bytes`, `goroutines` | at start, then every 30 s |
| `turn` | `turn`: `room`, `id`, `phase` (`start` or `end`), `error`, `duration_ms` | as a turn starts and ends |
| `log` | `log`, one zap JSON entry | for every log line. The supervisor re-encodes it before writing it to its stderr and to the page's log: it sets `instance` itself, and escapes control characters, C1 codes and Unicode format characters (bidi overrides, zero-width), so a worker binary cannot drive the terminal or fake a line. Entries over 16 KiB are dropped. The worker's stderr gets the same treatment, one entry per line with `"stream":"stderr"` |

## Supervision

A worker that exits without being told to is restarted after 1 s, then 2 s, 4 s and so on up to 30 s. A worker that ran for over a minute starts again from 1 s. Before the restart, the supervisor removes the sandboxes of the instance's rooms, which a crash leaves behind. A refused worker, by the handshake or by the binary checks, is not restarted until something changes: a dev key added or removed, or the instance's "accept unsigned" toggle. When a worker's status reports that its instance serves another version than the one it runs, the supervisor restarts it at once on that version.

When the supervisor stops, it sends `stop` to every worker, waits up to 45 s for each to exit, then kills it and removes its sandboxes.

## Single supervisor and handoff

The supervisor holds an exclusive lock on `supervisor.lock` in its state directory: `flock` on Unix, `LockFileEx` on Windows. The kernel drops it when the process dies.

A later launch that finds the lock taken connects to the local socket: `run/supervisor.sock` in the state directory, owner-only, or on Windows the named pipe `\\.\pipe\grill-room-<12 hex digits of the state dir's sha256>`, whose DACL admits only the current user. It sends one line, `{"room":"<room link>"}` or `{}`, and reads `{"link":"<admin page link>"}` or `{"error":"..."}`. With a room, the launch prints the link, opens it, and exits 0. Without one, it prints the link and exits 1: a second supervisor is refused. A launch opens a link only if it starts with `http://127.0.0.1:`.
