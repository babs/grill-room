# 004b — Admin lists and room sheets

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-16
**Context**: addendum to spec 004, with room-sheet items of 005 and 009. The panels review of v0.0.22 (`/admin` and room-sheet sections) and two leftovers of the daemon page rework (003b). Builds on the kit of v0.0.26.

## Problem

`/admin` shows its lists as tables of buttons. Profiles has 85 buttons, 14 of them primary, and its Merge and Delete fall past the page edge once a recovery link is open. On a phone the lists scroll sideways or turn into 300 px cards. On a 1920 px screen the tables fill half the width. Room settings stacks two headings above its tabs and puts Delete above Save as starter. The persona sheet shows its Save on every tab without saying what it covers. Notifications is a small card on a phone, with an unrelated tab setting first.

## Solution

Every `/admin` list becomes rows in columns (kit `.cols`): the name and a muted meta line, the values in columns, and a ⋯ with the row's actions. Under 40rem a row is a short card. Data views take the full width; forms keep 72rem. Room settings and `/admin` share one Admins list. The persona sheet gets one footer that counts unsaved changes across its tabs.

## Out of scope

- The Personas list sheet (RP1 to RP3) and the persona sheet's colour and badge fields (RQ3, RQ4).
- Native selects and date inputs on `/admin` (DX6 class): a kit `.select` is a separate change.
- Withdrawing a pairing request on the room server: a removed room's pairing is left to expire (AC14).

## Acceptance criteria

### `/admin` lists

- [x] **AC1** Rooms, Profiles, Libraries, Sessions, Audit and a room's Invites are `.cols` rows: the name with a meta line, one column per value under a header row, and a ⋯ holding the row's actions. Rooms still sorts by any column from its header, the sort shown by a chevron, not a glyph.
- [x] **AC2** A profile's ⋯ holds "Set the room limit…", "Generate a recovery link…", "Merge into another profile…" and "Delete…". The limit and the recovery link open in sheets: a link once made widens no row, and no action leaves the page.
- [x] **AC3** Each `/admin` view and each room tab has at most one enabled primary button. At 390, 1440 and 1920 px no control is cut by the page edge or by a scroll box, and the page never scrolls sideways.
- [x] **AC4** A room's Admins tab is one component on `/admin` and in Room settings: the name with where it was last seen ("last active 3 min ago · Firefox 133 on Linux", the IPs and raw user agents in the tip), the role, the admin button in a column of its own (one x on every row), and a ⋯ with Rename and, where offered, Make AI host.
- [x] **AC5** Under 40rem every list row is a card at most 120 px tall: the name and ⋯ on its first line, its values on one muted line, each after its label. Nothing scrolls sideways.
- [x] **AC6** At 1920 px the data views (`/admin` lists and room tabs) use `.page-wide` and their list spans at least 90 % of the content width. The Server view, a form, keeps `max-width: 72rem`.
- [x] **AC7** The audit log names each action in words ("Room renamed"), its code in the tip, and reads its details as `key: value` pairs. The filter lists the actions in words. Newer and Older sit above and below the list. A Go test fails for a server action without a label.
- [x] **AC8** The Instance view becomes Server: "AI host apps" connected, by version. Its settings are one `.settings-group` with one Save that counts unsaved changes; "Delete empty profiles" is a switch in the group that applies at once with a toast. Sessions name the browser in words ("Chrome 155 on Linux", raw in the tip); the current session's row has no action, the header signs out.

### Data-page width

- [x] **AC9** The daemon page's Rooms and Activity tabs use `.page-wide`; Accounts, Settings and Update keep 72rem.

### Room sheets

- [x] **AC10** The persona sheet has one footer for its four tabs: "N unsaved changes" or "No unsaved changes", and Save, disabled with none. Switching tabs keeps the count. The persona text grows with its content up to 60vh.
- [x] **AC11** Room settings has one heading, "Room settings · <room>", the tabs at most 80 px below the sheet's top edge. General ends with Delete, after Save as starter. Set quota is secondary, the storage a bar, "What takes the space" a chevron disclosure. The sheet takes its content's height.
- [x] **AC12** Notifications is full screen on a phone. The push explanation comes first, then "This tab" with the tab-title setting, then the primary at the bottom right.
- [x] **AC13** Diagnostics is 44rem wide: at 1440 px no label or value wraps.

### Daemon leftovers (003b)

- [x] **AC14** Once a room pairs, no status line stays under "Add a room": a toast says "Paired with <room>" and the row carries the state. While a room is being removed, Connect and a second removal are refused (409, "this room is being removed from this computer"). A room removed while its pairing waits for approval stores no token; an approval given later is revoked at once. A forced removal with no app running forgets the token, option values and cache, and drops the room.

### Flake

- [x] **AC15** `TestFetchDeltas` failed with `flate: corrupt input`: the pkt-line reader dropped a trailing `0x0a` from sideband pack data, so a pack packet ending in that byte lost it. Pack data passes through as sent; only text lines are trimmed. A test feeds packets ending in `0x0a`.

### Guards

- [x] **AC16** `e2e/layout-guards.spec.js` checks AC2 to AC6 and AC9 to AC13 at 390, 1440 and 1920 px. Each check fails on v0.0.27.

## Decisions

- 2026-10-16: lists use a kit `.cols` grid of list rows, not tables: the same markup is a row on a desktop and a card on a phone, and the ⋯ sits at the row's end at every width.
- 2026-10-16: the actions column is `var(--control-h)` wide in every template, the header row's included: an `auto` column sized by the ⋯ shifted the header off its values.
- 2026-10-16: a removed room's pending pairing keeps polling until answered or expired, out of the list, rather than withdrawing the request on the server, which would need a new endpoint.
- 2026-10-16: the persona sheet counts only named fields: the capability switches apply at once and wait for no Save.
