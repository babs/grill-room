# 003b — Daemon page rework

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-16
**Context**: addendum to spec 003. The panels review of v0.0.22 (daemon section, 30 defects) and the user's own requests of 2026-10-15 on the Rooms and Settings tabs. Builds on the kit components of v0.0.26 (`.settings-group`, `.list-row`, `.status`, `.value`, sticky tabs, one tip).

## Problem

The page makes the host hunt. Rooms repeats the last paired room above the list, and shows every room as a tall card with a monospace link. A host who wants a room off this computer has no way to do it short of stopping the app. Settings mixes machine-wide and per-server settings, has five Save buttons and two save models, and wraps a digest over six lines. Activity on a phone is 5,300 px of 13-line cards.

## Solution

Rooms becomes one section per server: the server's host name, its state and a `⋯` menu, then one row per room with its state, AI account and actions. A room can be paused, removed or reconnected from its row. Settings splits into "This machine" and "Per server", one Save each, toggles applied at once. Accounts becomes "AI accounts". The page drops the words "instance", "worker", "supervisor" and "daemon" from what it shows.

## Out of scope

- Hosting providers other than Claude. The provider badge reads "Claude" for every account.
- Per-room sandbox limits. The limits stay per server in storage. The page edits them once for the machine.

## Acceptance criteria

### Rooms

- [x] **AC1** Rooms lists one section per server, headed by the server's host name, a status pill (connected, connecting, offline, stopped) and a `⋯` menu holding the app details, "Accept unsigned binaries…" and "Cut off from the AI". The Servers tab is gone; `#instances` opens Rooms.
- [x] **AC2** Each room is one `.list-row` of at most 64 px at 1440 px: the room's name as a link to the room, its full URL only in the tip, a status pill, the AI account select inline, then its actions. At 390 px the name, pill and `⋯` share the first line and the account select and Pause take the second; nothing wraps further.
- [x] **AC3** The status pill says one of: runs the AI (`ok`), standby (`idle`, connected while another computer runs the AI), paused (`off`), waiting for approval or connecting (`busy`), disconnected or can't run the AI (`failed`, the reason in its tip).
- [x] **AC4** The block that repeated the last paired room is gone. "Add a room" is one inline form; its status line says how the room just added pairs, with the code and the approve link while pending.
- [x] **AC5** "Pause hosting" keeps the room paired and connected and reports `can_host=false` with the reason "paused on the host's machine". The room shows that reason and, if another participant's app can host, moves the AI host role to it. "Resume" reports `can_host=true`. The pause survives a restart of the app.
- [x] **AC6** "Remove this room" asks first, then revokes this computer's token on the room server, deletes the room's token, workspace cache and sandboxes here, and drops the room from the list. If the server does not answer, the page says so and offers "Remove anyway", which removes it here only.
- [x] **AC7** A disconnected room shows "Connect". It reconnects at once instead of waiting for the backoff. If the room revoked the token, Connect starts a new pairing, with the code and approve link in the row.
- [x] **AC8** A room's plugin requests (fetches) show in its row. Its plugin options sit behind a "Plugins" disclosure, closed by default.

### Room server

- [x] **AC9** `DELETE /api/rooms/{room}/daemon`, authenticated by the daemon token, deletes the daemon: its token stops working at once (401), its open stream ends with `revoked`, its running turn fails with "the AI host removed this room from their computer", and the room re-elects its AI host.
- [x] **AC10** A room whose AI host's app was removed says "The AI host removed this room from their computer." until someone else hosts.
- [x] **AC11** A paused host's daemon (`can_host=false`) loses the AI host role to a connected daemon that can host; the room shows the paused reason while no other one can.

### AI accounts

- [x] **AC12** The tab and its heading read "AI accounts". Each account row shows a provider badge ("Claude"), its state pill, its kind and dates as relative times with the full date in the tip. The add form puts labels above fields and its hint under the whole form.

### Settings

- [x] **AC13** A status strip at the top: Docker, the sandbox image (digest cut to one line with copy) and whether this computer can run the AI, each a `.status` pill, plus "Check again".
- [x] **AC14** "This machine" is one `.settings-group`: answers at once, days of history, start at login (applies at once, with a toast), sandbox CPU and memory. One Save, which counts the unsaved changes; saving limits applies them to every server and restarts its connection, asking first while an answer runs.
- [x] **AC15** "Per server" is one `.settings-group` with the server picker at its top: the server's default AI account, max model, max effort (one Save), and "Approve every new AI persona and change" (applies at once, with a toast).
- [x] **AC16** "Websites the AI may reach" is a table: domain, hits (requests the proxy let through, from the running sandboxes' recent log), and an icon button to remove. Adding and removing apply at once. "Blocked websites" is its own table after it.
- [x] **AC17** Both groups use the full page column at 1440 px and one column at 390 px. No horizontal overflow at either width.

### Activity, Update and the whole page

- [x] **AC18** Activity's turn table shows Ended, Room, AI persona, Model, Status, Took and Cost; a row opens a sheet with every field. Under 40rem each turn is one line. Logs have a level filter (all, warnings, errors).
- [x] **AC19** "Accept unsigned binaries" sits behind the server's `⋯` and is an outlined danger button, never a primary. The app's binary is stated once, in the server's details. On Update the signer identity is a `.value` and the add-key form puts its labels above its fields.
- [x] **AC20** The page's text drops "instance", "supervisor", "worker" and "daemon" (docs/design.md §8): "server", "this computer", "AI host app". A test greps the page's scripts for them in user-facing strings.
- [x] **AC21** On a phone the header is one row: the logo, "AI host", the state and Stop; the version and signature show on Update.

### Autostart

- [x] **AC22** The systemd user unit sets `MemoryMax=` (default `4G`, `GRILL_ROOM_DAEMON_MEMORY_MAX` to change it, `infinity` to lift it). An existing unit takes it the next time autostart is switched on or the app updates itself.

### Guards

- [x] **AC23** e2e on a test daemon at 1440 and 390 px: one Save per settings group, no wrapping Remove buttons, rooms as rows not cards, the pill colours per state, Pause fails the room over to a second test daemon and shows the reason, Remove revokes the token, Connect reconnects.

## Decisions

- 2026-10-15 (user): per-room Pause, Remove and Connect; compact room rows; "AI accounts" with a provider badge; Settings split in two groups with one save each; the allowlist as a table with hits; a memory cap on the autostart unit.
- 2026-10-16: Pause rides on the per-room access the app already sends: a paused room gets the error "paused on the host's machine" instead of a proxy, so its readiness, and the room's view of it, follow without a protocol change. A running answer finishes.
- 2026-10-16: Remove is a new request, `remove_room`, protocol version 5. A worker below 5 says "update the AI host app". Connect reuses `pair`, which already restarts an ended room and now also cuts a running backoff short.
- 2026-10-16: removing a room deletes its daemon row on the server. The cap approvals tied to it go with it (`ON DELETE SET NULL`), as a new pairing would.
- 2026-10-16: CPU and memory limits stay stored per server. "This machine" shows the first server's and saves to all of them.
- 2026-10-16: "Accept unsigned binaries" stays reachable for a server whose app runs this app's own binary: the e2e and the dev flow test it there. Behind `⋯`, outlined, it no longer reads as the tab's main action.
- 2026-10-16: the server's pill sums up its rooms: connected if one is, connecting while one pairs or connects, offline otherwise.
- 2026-10-16: hits come from the last 2,000 lines of each running egress proxy's log. A server restart resets them. No new storage.

## Phases

### Phase 1 — Server side

- **Covers**: AC9 to AC11
- **DoD**: `go test ./internal/web/ ./internal/store/` green, tests citing each AC.

### Phase 2 — App side

- **Covers**: AC5 to AC7 (backend), AC16 (hits), AC22
- **DoD**: `go test ./internal/daemon/ ./internal/sandbox/` green.

### Phase 3 — Page

- **Covers**: AC1 to AC4, AC8, AC12 to AC21
- **DoD**: `make lint` (with `ui-check`) green; the daemon e2e specs updated and green in CI.

### Phase 4 — Guards and ship

- **Covers**: AC23
- **DoD**: `e2e/daemon-page.spec.js` green in CI; before and after screenshots at 1440 and 390 px.
