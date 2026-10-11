# 003 — Daemon: one supervisor, many instances, rooms and agents

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-11
**Context**: specs 001 and 002 (shipped), `docs/architecture.md`, `docs/agent-workspace.md`, grill notes

## Problem

The host daemon serves one room at a time. Hosting a second room means a second process: a second admin page, the same state directory and the same sandbox name, with nothing preventing a clash. A person who hosts rooms on two room servers (work and personal) has nowhere to say which Claude account each one should use, because the account is read once from the environment at start. The daemon's version follows the room server it was installed from, so one machine cannot follow two servers at different versions. Credentials, cap, allowlist and monitoring live inside a process that also runs code shipped by a remote server.

Inside a turn, an agent does not know why it was started. The day's transcript is re-sent whole on every turn, and long resumed sessions fill the context window unnoticed. The room shows the host's machine name.

## Solution

One program runs on the host's machine as a supervisor. It has a single local admin page, holds the Claude accounts and their proxies, enforces the host's global limits and keeps a local monitoring history. For each room server the host works with (an "instance"), it starts a worker running the exact daemon version that instance serves. It checks that the worker binary was signed by the project's release workflow before running it. A worker hosts any number of rooms on its instance and runs turns in parallel across rooms, and optionally within a room, under the supervisor's global cap. Workers never see a Claude token; they only get a proxy address for the account picked for each room.

The host picks which Claude account serves each instance or room: a Claude Code config dir, or a long-lived token. That choice is visible only on the local admin page. Agents learn why they are running, transcripts travel in small immutable chunks, context use is measured, and a host can hand a long session over to a fresh one with a written summary. The room shows "Alice holds the agent(s)", not her machine.

Releases are built and signed keyless by GitHub Actions, and the sandbox image is published publicly.

## Scope

- Release pipeline:
  - GitHub Actions: tests, lint and build on PRs.
  - On a semver tag: linux/darwin/windows × amd64/arm64 binaries, sha256 files, cosign keyless bundles, a GitHub release, and the sandbox image on ghcr.io, public and pinned by digest.
  - Instances serve binaries together with their bundles.
- Supervisor:
  - single binary, default mode;
  - one per machine, held by a lock;
  - a local socket so a second launch hands its room over to the running supervisor;
  - platform paths: XDG on Linux and WSL, `~/Library` on macOS, `%APPDATA%`/`%LOCALAPPDATA%` on Windows;
  - migration of the current daemon state;
  - optional autostart per platform.
- Workers:
  - one per instance, started as `grill-room-daemon worker --instance <id>`, supervised with backoff;
  - JSON over stdio with a protocol version handshake;
  - the binary at the instance's version, verified with sigstore-go (identity check), deduplicated by version, garbage-collected;
  - trust exceptions: extra trusted dev keys, and a per-instance unsigned "relax" mode.
- Supervisor update channel: GitHub (default) or follow the instance. The page shows whether the supervisor is project-signed.
- Claude accounts:
  - two kinds: a config dir (read-only, refreshed by Claude Code) and a long-lived setup token (OS keyring, or a 0600 file);
  - a default account per instance, overridable per room;
  - one bearer-swap proxy per account, inside the supervisor;
  - per-worker request counts, and a cut-off;
  - status, expiry and warnings.
- Concurrency:
  - a global cap on concurrent turns (default 2), with a local FIFO across instances and rooms, shown in the room as "queued (host busy)";
  - an optional per-room parallel mode, off by default and switched by the host, with one sandbox per parallel turn;
  - document conflict: reject, re-run once, then fail.
- Turn contract:
  - a "Why you are running" line in the preamble;
  - transcript chunks `transcript/<day>/<NNN>.md`;
  - a per-persona `autocompact` setting;
  - context-use measurement;
  - a "new session with handoff" button, with a banner above 80%.
- Room label "<name> holds the agent(s)", never the hostname.
- Single local admin page: instances, rooms, accounts, settings (global caps, per-instance cap, approval mode, allowlist, limits) and monitoring.
- Monitoring:
  - SQLite `monitor.db`, local only;
  - per turn: instance, room, persona, account, model and effort requested versus effective, why, duration, queue wait, status, tokens and cost, context %, workspace bytes;
  - continuous state;
  - views and CSV export;
  - 30-day retention;
  - alerts.

## Out of scope

- Room admin UI, export/import, Piper scenarios: spec 004.
- Apple notarization and Windows Authenticode signing (SmartScreen may warn).
- A host per persona.
- Auto-update without approval for the supervisor.
- Merging conflicting document edits: a conflicting result is rejected and re-run.

## Acceptance criteria

### Release and trust
- [ ] **AC1** A PR runs tests, lint and builds on GitHub Actions. A semver tag publishes a GitHub release with six binaries (linux, darwin, windows × amd64, arm64), their sha256 and cosign keyless bundles, plus a public sandbox image on ghcr.io whose digest is recorded in the release.
- [ ] **AC2** The supervisor runs a worker binary only after sigstore-go verifies its bundle against the expected identity: this repository's release workflow on a semver tag. A tampered binary, a wrong identity or a missing bundle is refused, with the reason shown on the admin page.
- [ ] **AC3** Bundle verification works offline with the embedded trusted root. When online, the supervisor refreshes the trusted root through TUF.
- [ ] **AC4** A trusted dev key added on the admin page lets binaries signed with it run. The per-instance "accept unsigned" toggle is off by default, needs the instance host typed to switch on, and shows a red badge on the page and in the logs while on.
- [ ] **AC5** The admin page shows whether the supervisor binary itself is project-signed. The supervisor updates from GitHub by default, or from its instance when the channel is set to "follow instance", always after approval on the page.

### Supervisor and workers
- [ ] **AC6** A second launch while a supervisor runs starts no second process. It hands its room URL to the running supervisor over the local socket and opens the existing admin page. A second supervisor started directly exits with a clear message.
- [ ] **AC7** Rooms from two instances are served by two workers, each running the binary version its own instance serves. When both instances serve the same version as the supervisor, no extra binary is stored. A version no instance uses any more is removed.
- [ ] **AC8** A worker that crashes is restarted with backoff, and its rooms reconnect. Stopping the supervisor stops every worker and removes its sandboxes.
- [ ] **AC9** The supervisor and a worker one protocol version apart still work together. Beyond the supported range, the worker is refused with a clear message.
- [ ] **AC10** On first start, an existing single-room daemon state is migrated: tokens, settings, allowlist and cache move into `instances/<id>/`, the old directory is kept as `legacy-backup-<date>`, and its `CLAUDE_CONFIG_DIR` becomes account "default". An already paired room keeps working without re-pairing.
- [ ] **AC11** Paths follow the platform:
  - XDG on Linux and WSL;
  - `~/Library/Application Support` and `~/Library/Caches` on macOS;
  - `%APPDATA%\grill-room` and `%LOCALAPPDATA%\grill-room` on Windows.

  Windows runs natively (supervisor and workers) with Docker Desktop. WSL stays supported as a plain Linux.

  Autostart is opt-in from the page: a systemd user unit, a LaunchAgent, or a Windows scheduled task.

### Accounts and credentials
- [ ] **AC12** The host can add accounts of two kinds: a config dir, read-only and refreshed by Claude Code, or a long-lived setup token stored in the OS keyring (Keychain, Secret Service, Windows Credential Manager) or a 0600 file. Each account shows its status, expiry and last use.
- [ ] **AC13** Each instance has a default account, and a room can override it. A turn uses the room's account, through that account's proxy in the supervisor.
- [ ] **AC14** No worker process, environment, file or sandbox ever holds a real token. A scan of worker memory maps, environments, state dirs and sandbox exports finds none.
- [ ] **AC15** The supervisor counts requests per worker and per account. The host can cut a worker off, which ends its turns with a clear error in the room.
- [ ] **AC16** The page warns about 30 minutes before an access token expires, and two weeks before a setup token expires.
- [ ] **AC17** The account name, the config dir, the hostname and the cap never appear in the room UI, in prompts or in the presence preamble. The room shows "<name> holds the agent(s)".

### Concurrency
- [ ] **AC18** With a global cap of N (default 2), at most N turns run at once across all instances and rooms. Further turns wait in a local FIFO, and the room shows "queued (host busy)".
- [ ] **AC19** Turns of two rooms on the same machine run in parallel when the cap allows.
- [ ] **AC20** With a room's parallel mode on (host only, off by default), two personas of that room run at once in separate sandboxes.
- [ ] **AC21** When a parallel turn edits a document that changed since its turn started, its result is rejected and the turn re-runs once, at the head of the queue, on the new version. A second conflict fails the turn with "conflict on <doc>". Thread replies, new annotations and syntheses never conflict, and the re-run does not count as a chain link.

### Turn contract
- [ ] **AC22** Every prompt's preamble has a "Why you are running" line naming the cause: pressed by a person, an @mention with its start, a call from a persona, every N rounds, or a re-run after a conflict on a document.
- [ ] **AC32** The presence preamble shows a short profile uuid next to each human name ("Alice ·a1b2"), so two people with the same display name stay distinct.
- [ ] **AC23** Transcripts are split into chunks `transcript/<day>/<NNN>.md` of about 200 lines, and a full chunk never changes. On a turn after new messages, only the last chunk, plus any chunk with an edited message, is transferred.
- [ ] **AC24** Each persona has an `autocompact` setting (auto, or a token count) passed to `claude -p`. The persona panel and the monitoring show each session's context use as a percentage of the window.
- [ ] **AC25** "New session with handoff" (host) runs a last turn that writes `docs/_handoff-<persona>.md`, then starts a new session that reads it plus the transcripts. Above 80% context use, a banner offers it.

### Admin page and monitoring
- [ ] **AC26** The page lists instances with their version and signature state, their rooms with status (connected, host or not, running or queued turns), and the accounts. It edits global and per-instance settings: global cap, per-instance model and effort cap, approval mode, allowlist and resource limits.
- [ ] **AC27** Every turn is recorded in local `monitor.db` with:
  - instance, room, persona and account;
  - model and effort requested and effective;
  - why the turn ran;
  - duration, queue wait and status;
  - tokens and estimated cost, context %, and workspace bytes.

  The page filters turns by instance, room, persona and account, shows daily per-account totals, and exports CSV.
- [ ] **AC28** History older than the retention (default 30 days) is purged at start and daily.
- [ ] **AC29** Alerts on the page: a token near expiry, an unsigned worker, context above 80%, and a queue stuck for more than 10 minutes.

### Platforms and end to end
- [ ] **AC30** CI cross-compiles linux, darwin and windows, amd64 and arm64, and runs the unit tests on a `windows-latest` runner. `docs/daemon-os-checklist.md` covers:
  - macOS: Keychain account, Docker Desktop, LaunchAgent;
  - native Windows: Claude Code credentials location, Credential Manager, Docker Desktop, scheduled task, local socket;
  - WSL as Linux.
- [ ] **AC31** End to end, on this machine:
  - one supervisor serves a room on the local compose instance and a room on the lab instance at the same time, each with its own account;
  - two turns run in parallel across the two rooms;
  - the monitoring shows both turns with their accounts;
  - the lab room shows "<name> holds the agent(s)".

## Phases

### Phase 0 — Spikes
- Work:
  - S7: sigstore-go in the daemon binary: size added, offline verification of a cosign keyless bundle with an embedded trusted root, and TUF refresh.
  - S8: a `claude setup-token` token through the bearer-swap proxy: a turn works, and it holds over a day.
- **Covers**: none
- **Data model impact**: none
- **DoD**: `spikes/S7-sigstore.md` and `spikes/S8-setup-token.md` with a verdict. If S7 is a no-go, switch to minisign: the spec's signing ACs keep their meaning with a minisign key.

### Phase 1 — Release pipeline
- Work: GitHub Actions workflows for PRs and tags, keyless signing, release assets, the public ghcr sandbox image, the instance serving bundles, and `make dev-release` with a cosign key pair.
- **Covers**: AC1
- **Data model impact**: none
- **DoD**: a test tag on the repo produces a signed release and image; `cosign verify-blob` passes on each asset; the PR workflow is green.

### Phase 2 — Supervisor, workers and migration
- Work: supervisor mode, lock, socket handoff, worker spawning and supervision, stdio protocol with version handshake, platform paths, migration, autostart.
- **Covers**: AC6, AC8, AC9, AC10, AC11
- **Data model impact**: none on the server; local state layout changes
- **DoD**: process tests (second launch, crash and restart, protocol skew); migration test on a copy of the real `.scratchpad/daemon-run/state`; cross-compile.

### Phase 3 — Worker binaries and trust
- Work: download per instance version, sigstore-go verification with identity, dedup and GC, dev keys, relax toggle, supervisor update channel and signature indicator.
- **Covers**: AC2, AC3, AC4, AC5, AC7
- **Data model impact**: none
- **DoD**: refusal tests for tampered, wrong-identity and missing bundles; offline verification test; two instances at different versions on one supervisor.

### Phase 4 — Accounts and proxies
- Work: accounts of both kinds, keyring storage, a proxy per account in the supervisor, account mapping per instance and room, request counting and cut-off, expiry warnings, privacy.
- **Covers**: AC12, AC13, AC14, AC15, AC16, AC17
- **Data model impact**: server: the room label uses the holder's display name only; the hostname is removed from room messages and state
- **DoD**: a token scan over workers and sandboxes finds nothing; two rooms on two accounts each run a real haiku turn; a privacy grep over prompts and room UI finds no account or host data.

### Phase 5 — Concurrency
- Work: global cap and local FIFO, server state "queued (host busy)", parallel turns across rooms, per-room parallel mode, a sandbox per parallel turn, conflict detection on base revisions with one re-run, queue-wait and conflict metrics.
- **Covers**: AC18, AC19, AC20, AC21
- **Data model impact**: `rooms.parallel_turns`; `turns.base_revisions` (or a per-document base table); `turns.rerun_of`
- **DoD**: tests with the fake claude covering the cap, the FIFO, two rooms in parallel, and an in-room parallel conflict re-run then failure.

### Phase 6 — Turn contract
- Work: the "why" line, transcript chunks, autocompact and context measurement, handoff session and banner.
- **Covers**: AC22, AC23, AC24, AC25, AC32
- **Data model impact**: `room_personas.autocompact`; `turns.cause`; `agent_sessions.context_tokens`
- **DoD**: preamble golden updated; bytes test on chunks; handoff e2e with the fake claude; a live haiku turn shows context %.

### Phase 7 — Admin page and monitoring
- Work: the single page (instances, rooms, accounts, settings, monitoring), `monitor.db`, views, CSV, retention and alerts. The page is written in Preact + htm + @preact/signals, vendored as ESM with no build step, and typed through JSDoc checked by `tsc --checkJs` in CI.
- **Covers**: AC26, AC27, AC28, AC29
- **Data model impact**: local `monitor.db` only
- **DoD**: Playwright on the page, mobile width included; retention and alert tests.

### Phase 8 — Platforms and end to end
- Work: the OS checklist extended, then the AC31 run with the local compose instance and the lab instance on one supervisor, and a lab deploy of the server changes.
- **Covers**: AC30, AC31
- **Data model impact**: none
- **DoD**: AC31 passes, with at most 6 real Claude requests (haiku); lab updated with a DB backup first.

## Data model impact (summary)

Server:
- `rooms.parallel_turns`
- `turns.base_revisions` (or a per-document base table), `turns.rerun_of`, `turns.cause`
- `room_personas.autocompact`
- `agent_sessions.context_tokens`
- the hostname is removed from room-visible state

Daemon (local):
- new state layout under `instances/<id>/`
- `monitor.db`
- the accounts store, with secrets in the OS keyring

## Personal data

- The account names, config dirs, hostnames and caps stay on the host's machine; nothing new is sent to the room server.
- `monitor.db` holds turn metadata (room names, persona names, costs), locally, for 30 days by default.
- Setup tokens are secrets: kept in the OS keyring, or in a 0600 file when no keyring exists. They are never logged, never sent to workers, and never shown after entry.

## Open questions

- [ ] 1. Native Windows: where does Claude Code keep its credentials (expected `%USERPROFILE%\.claude\.credentials.json`), and is a non-default config dir honoured the same way? This needs checking on a real machine; it only changes the account picker and docs.

## Decisions

- "Instance" names a room server (grill Q1 naming).
- Windows is native (supervisor and workers, Docker Desktop), replacing spec 001's D25 "Windows via WSL"; WSL remains a Linux option.
- One binary with two modes, because a single artifact is simpler to ship (grill Q2).
- Credentials live only in the supervisor, and workers are signed by the project, because workers run code an instance serves (grill Q3).
- Default global cap is 2, to be tuned after the first sessions using the queue-wait metrics (grill Q5).
- The prompt stays built at turn start, so messages between a call-out and the start are included (grill Q6 check).
- The install script downloads from the instance and checks the signature when cosign or minisign is present. After that, the GitHub channel brings project-signed updates (grill Q9).
- Signing is cosign keyless: spike S7 found it works offline and adds about 12 MB per binary (grill Q10).
- New UIs use Preact + htm + signals, vendored with no build step; `room.js` migrates panel by panel when touched. A build step stays possible later through a multi-stage build that runs before `go build` (grill 004).
