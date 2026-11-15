# 041 — Room switcher and unread counts

**Status**: in progress (v4, council consensus)
**Requested by**: babs
**Date**: 2026-11-15
**Context**: UX audit part B, direction S4 (journeys 3, 6, 12). Builds on 005 (room menu, `/profile` Rooms, the tab title's unread count 005-AC27), 019 (anonymous rooms), 020 (written and voice lines), 023 (linked rooms), 038 (room menu entries), 040 (room menu order). Council review (Codex, Fable) on 2026-11-15; unread counts added at the product owner's request.

## Problem

A PM in two or three rooms a day goes avatar › "My rooms and profile" › Rooms › the room (3 taps, a page load between) to switch. Creating a room is the same trip plus "+ New room". "Linked rooms" lists only the rooms this one reads or is read by. Nothing says which other room has news: the tab title counts unread lines (005-AC27) only for an open tab, in that browser, and forgets them on reload.

## Solution

- **Switch room:** the room menu's last entry, "Switch room…", opens a short list: the five rooms of the browser's profile with the latest activity, then "All my rooms" and "New room…". The list stays folded so a shared screen does not show other rooms' names each time the menu opens.
- **Unread counts:** each profile keeps a read marker per room. Rows in the switcher and in `/profile` › Rooms show how many lines the viewer has not seen, and "@" when one names them.

## Scope

- "Switch room…" in the room menu and its popover.
- `/profile#rooms/new` opens the new-room sheet.
- A read marker per profile and room, set from the room page; unread counts and the mention flag in `GET /api/profile`; shown in the switcher and on `/profile` › Rooms.

## Out of scope

- Search (025), a keyboard shortcut, changing "Linked rooms" (023, 038-AC8).
- Unread counts per thread or per document; push or email digests of unread lines.
- Keeping the switcher in an ended room: the avatar menu's "My rooms and profile" and the no-access card are the way out (005).
- Returning to `#rooms/new` after a sign-in: a signed-out `/profile` shows its sign-in card as today.

## Acceptance criteria

### Switch room

- [ ] **AC1** "Switch room…" is the room menu's last entry. It opens a popover in the room menu's place (same position, the menu closing), labelled "Your rooms": up to five rooms of the browser's profile (the profile cookie's, `own.profile`, even when the seat belongs to another profile), sorted by `last_activity` newest first, the current room left out; then "All my rooms" (`/profile#rooms`) and "New room…" (`/profile#rooms/new`). A browser with no profile sees no "Switch room…".
- [ ] **AC2** Each room row is a link to `/r/<id>`: the room's name on one line with an ellipsis, then "active <ago>" as `/profile` writes it, "· room admin" when the viewer is one, and the unread marker of AC12.
- [ ] **AC3** The list renders from the page's existing read of `GET /api/profile` (`loadOwn`); opening the popover calls a reload when the last read is older than a minute, one request at a time. Before the first read lands the popover says "Loading your rooms…"; a failed read (network, 5xx, 401) says "Your rooms could not load." with "All my rooms" still there. A read that answers for another profile than the last one replaces the list, never merges it.
- [ ] **AC4** A profile with only this room shows "No other rooms yet." above "All my rooms" and "New room…". "New room…" shows whatever `/profile`'s own "+ New room" shows for that profile (quota and restricted mode included).
- [ ] **AC5** `/profile#rooms/new` opens the new-room sheet (the existing `newRoom`, 038-AC5's picker) on the Rooms tab for a signed-in profile; closing it leaves `/profile#rooms`.
- [ ] **AC6** Keyboard: "Switch room…" opens the popover with Enter or Space and puts the focus on its first row; Tab and Shift+Tab move through rows; Escape closes it and returns the focus to the room's name. When rows replace "Loading your rooms…" while the focus is on it, the focus moves to the first row. Rows are direct children of the popover, 44 px targets on a phone. Docs and chat windows, which have no room menu, have no switcher.

### Unread counts

- [ ] **AC7** A read marker is kept per profile and room, in `room_reads(profile_id, room_id, read_through, PRIMARY KEY (profile_id, room_id))`, cascading from `profiles` and `rooms`, never per seat: with two seats in one room, reading through either moves the same marker. A seat with no profile keeps none. A profile merge moves the absorbed profile's rows to the kept profile before deleting it, keeping the greater `read_through` on conflict, as it does for `room_admins`.
- [ ] **AC8** The room page moves the marker when the chat is pinned to its end (`isPinned`) in a visible, focused tab: `read_through` takes the newest line id the stream delivered, any kind, so a voice line hidden by the chat filter does not stall it. The first move is sent at once, later ones at most every 5 seconds and only when it changed, a last one on `pagehide` with `keepalive`. Docs windows never move it; chat windows do.
- [ ] **AC9** `PUT /api/rooms/{room}/read` with `{"through": <id>}`: seat authentication; the marker of the seat's profile only; `read_through = GREATEST(stored, min(through, the room's newest id))` in one statement; an id of another room or a seat removed from the room is refused; a missing row is inserted (upsert); no room event is sent (it would reload every open page). The marker stays out of the room answer, the stream and the export.
- [ ] **AC10** Unread lines are the room's `text` and `agent` lines newer than the marker, not deleted, written by no seat of the viewer's profile in that room (`participant_id IS DISTINCT FROM`, so persona answers count). System and voice lines do not count, unlike the tab title, which counts a voice line that names the viewer (005-AC27).
- [ ] **AC11** `GET /api/profile` returns per room `unread` (0 to 100; 100 shows "99+") and `unread_mention`. They come from a separate store call used by that handler only, not from `profile()`. Per room it reads at most the newest 100 unread lines (`LIMIT 100` per room, existing `messages(room_id, id)` index). `unread_mention` runs the server's @name rule (`named()`, 005-AC27) on those of them whose body holds "@", for rooms with unread lines only; a mention older than the newest 100 unread lines is not flagged. A room with no `room_reads` row reads as nothing unread.
- [ ] **AC12** The switcher and `/profile` › Rooms show "4" or "4 · @" after the room's name, nothing at zero; the row's accessible name adds "4 unread" or "4 unread, mentions you". With Phase 2, opening "Switch room…" always reloads the profile (rows drawn from the last read meanwhile), one request per open.
- [ ] **AC13** The migration creates `room_reads` and fills it for every profile's live seats with each room's newest line id, so nothing is unread on upgrade. Joining a room (the three seat inserts and import) creates the row the same way, import after its lines are in, when the profile has none for that room; a room's first seat, inserted before any line exists, starts at 0.
- [ ] **AC14** A store test puts 50 rooms with 1,000 voice lines after the marker and 1,000 written lines, with the voice lines newest, and asserts from `EXPLAIN (ANALYZE, FORMAT JSON)` (rows times `Actual Loops`) that the count query visits at most 100 rows per room; a web test asserts `named()` runs on at most 100 bodies per room; add a partial index on `messages(room_id, id) WHERE kind IN ('text', 'agent')` only if that bound fails.

## Phases

### Phase 1 — Switch room
- **Covers**: AC1 to AC6
- **Data model impact**: none
- **DoD**: e2e (`--workers=1`): a profile in 7 rooms (5 shown, sorted by activity, current left out), one room only, a browser without profile (no entry), a seat of another profile than the browser's, a load failure, a profile replaced by a sign-in in another tab, `/profile#rooms/new` signed in; keyboard path of AC6; captures at 390 and 1440 px; `make lint` green.

### Phase 2 — Unread counts
- **Covers**: AC7 to AC14
- **Data model impact**: new table `room_reads(profile_id, room_id, read_through)` with its backfill, one positional migration, number taken at merge.
- **DoD**: `go test -race ./internal/store/... ./internal/web/...` with tests for AC10's kinds, persona answers, own lines of either seat, deleted lines, 99 against 100, the mention flag inside and beyond the 100 lines, two seats of one profile after a merge, a merge where only the absorbed profile was in the room (3 unread before, 3 after) and the conflict case both ways (the greater marker kept), AC9's clamp, refusals and atomic maximum under reordered writes, and AC14's bound; the migration on a fresh database and on one at the previous schema (backfill: nothing unread); e2e: Bob posts in room B while Alice is in room A, her switcher shows "1", opening B clears it, a mention shows "@", a second browser of Alice clears after reading on the first, a voice line last in a call does not stall the marker.

## Data model impact (summary)

- New table `room_reads(profile_id, room_id, read_through)`, backfilled at the newest line of each room.

## Open questions

- [ ] None blocking.

## Decisions

- The list sits behind "Switch room…". Because the room menu opens on shared screens during calls, and five other room names would show each time (Fable).
- The browser's profile, not the seat's, owns the list. Because `GET /api/profile` answers for the profile cookie, and the two can differ (Codex, Fable).
- No new fetch: the page already reads the profile on load and on return. Because a third cache of the same answer would drift (Codex, Fable).
- No switcher in an ended room, no sign-in return to `#rooms/new`. Because the way out exists already and the sign-in links drop the hash (both reviewers).
- Unread counts written lines and persona answers, not system or voice lines. Because voice lines would flood the count during a call (product owner, 2026-11-15).
- "@" marks a mention among unread lines (product owner, 2026-11-15); unlike the tab title, voice lines never count.
- One marker per profile and room, shared across devices (product owner, 2026-11-15); per profile, not per seat, so a merge or two seats cannot leave a count that never clears (both reviewers, second pass).
- The mention flag reads at most the newest 100 unread lines, prefiltered on "@", with the existing `named()` rule. Because storing every mention at write time is a larger change, and the flag only needs "is there one" (Fable over Codex: bounded and exact on what it reads, its limit stated).
- Upgrade and joins start the marker at the newest line. Because a null marker would show every room as "99+" on deploy day (Fable).
- Until the page's first read of the profile answers, "Switch room…" shows when the seat has a profile, so "Loading your rooms…" can show; after it, the browser's profile decides (implementation).
- A row is one menu line, the name ending in an ellipsis and its activity after it. Because the menus keep one row height (009-AC38) (implementation).
- One profile read at a time, a call meanwhile reading once more after it, each read bounded at 10 s. Because a dropped read left a return to the tab on stale state and a hung one held the text size save (implementation, review).
- `/profile#rooms/new` becomes `#rooms` as the sheet opens. Because Back from the room made there would open the sheet again (implementation, review).
- The partial index `messages_room_written` ships with the migration. Because AC14's bound fails on the existing index: over 100 rows per room with the voice lines newest (about 55,000 measured, the planner walking the primary key) (implementation).
- The marker write skips the global write lock, as hands and presence do. Because no id cursor reads markers and one upsert keeps the maximum on its own (implementation).
- `unread` and `unread_mention` are set by `GET /api/profile` only; the MCP `list_rooms`, which passes its rooms through, carries them too. A failed count leaves them out rather than failing the answer (implementation, review).
- The mention flag reads each room's personas and participants once per room holding an unread "@" line: at most two reads per such room, not batched. Because a profile sits in a few rooms and the cost stays linear in them (implementation, review).
- A line id of another room gets 400; a seat with no profile gets 204 and nothing is kept; a marker write that meets a profile merged away or a room deleted keeps nothing and answers 204 (implementation, review).
- Lines of any seat of the profile, removed ones included, never count as unread (implementation).
- The mention flag runs `named()` on every unread "@" body, at most 100 per room, with no cheaper prefilter. Because a raw-text prefilter drops mentions that emphasis or a link touches, which `named()` reads (review).
- The count's bound of 100 rows per room covers voice lines, not the viewer's own or deleted lines past the marker, which the scan still walks. Because the room page moves the marker as its own lines arrive; only a docs window or an API client posting at length grows the walk (review, trade-off kept).
