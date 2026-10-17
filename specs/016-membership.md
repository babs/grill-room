# 016: Membership: remove a person, invites apart from seats, mentions on edit

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-17, from the notes of 2026-10-16 (Q1 validated the same day)
**Context**: backlog "Membership model"; reverses 005's "no kick" (005 Decisions, "Moderation: self-rename plus admin rename; no kick", 2026-10-12); builds on 003 (host election), 004 (room admins, audit log, 004-AC6, 004-AC8), 005 (mention notices and chips, 005-AC3, 005-AC4, 005-AC27), 009 (host transfer, 009-AC33), 011 (revoke preview, shareable link, 011-AC19, 011-AC20, 011-AC26), 012 (personal API tokens, MCP), 014 (host-change resets, 014-AC12) and PR #28 (@ autocomplete in the edit box)

## Problem

A seat is valid only while the invite it joined through is unrevoked. Every access check joins `participants` to `invites` and tests `revoked_at IS NULL`, about 24 queries across the store. A room admin has no way to remove one person: they revoke an invite, which cuts everyone who joined through it and through the invites those people made down the chain. Revoking a leaked link to stop new joins has the same effect.

Editing a message recomputes its chips (`editMessage` calls `nameMentions`), but a persona the edit adds never runs: only `postMessage` calls `mentionTurns`.

## Solution

A seat gets its own validity. A room admin removes one person from the room from their row's ⋯ menu, and can block that profile from coming back. Anyone can leave a room. Revoking an invite stops new joins only, unless the revoker ticks "Also remove the N people who joined through it". An edit that adds an `@Persona` runs that persona, as a send would.

## Scope

- Seat validity on the seat (`participants.removed_at`), migrated from the invite chain; every access check reads it.
- Invite revoke: stops new joins by default; cutting the joiners is an explicit checkbox in the confirm sheet, with the count.
- "Remove from room…" for room admins, with "Block from rejoining"; a blocked list in Manage › Invites with "Unblock".
- "Leave room…" for oneself.
- Host failover when the removed or leaving person's app hosts the AI.
- Chat line and audit entry for removal and unblock.
- What a removed person's personal API tokens (012) can still do.
- New `@Persona` mentions in an edit queue that persona.

## Out of scope

- Deleting a removed person's messages, uploads, notes or documents. They stay.
- A block that survives a new profile: someone without a profile cookie who joins through a valid invite gets a new profile (Risks).
- New MCP tools. `revoke_invite` keeps its arguments and stops new joins only.
- Exporting blocks.
- Temporary bans, mute, or a reason field on removal.
- 016's sibling backlog item "Chat links & mentions" (persona names in push, document links, agent `@Name` prompting).

## Acceptance criteria

### Seats apart from invites

- [x] **AC1** A migration adds `participants.removed_at` (NULL while the seat is valid) and `participants.end_reason` (`revoked`, `removed` or `left`). A seat whose invite is revoked gets `removed_at` = the invite's `revoked_at` and `end_reason = 'revoked'`; every other seat stays valid. On a database migrated from the previous version, `Participants`, `profileRooms`, the `/admin` overview counts and `PushSubscriptions` answer the same rows before and after. *Verify*: Go migration test from the previous schema with a revoked chain, a valid seat and a profile with two seats.
- [x] **AC2** Every access check reads the seat alone: room cookie, profile session, API token, daemon token, invite creation and sharing, push subscriptions, room admin and rename targets, profile rooms, the overview, merge, purge, skills, export and import. No query tests seat validity through `invites.revoked_at`. *Verify*: grep of the store for `revoked_at IS NULL` joined from `participants` returns 0 (state the sweep); Go test: a seat with `removed_at` set gets `ErrNotFound` from each of `ParticipantByToken`, `ProfileParticipant`, `APITokenParticipant`, `DaemonByToken`, and is absent from `Participants` and `PushSubscriptions`.
- [x] **AC3** Revoking an invite with the box unticked revokes that invite only. A join through it gets 401 "invalid or revoked invite". Everyone who joined through it keeps their seat, and the invites they made stay valid. *Verify*: Go test on `RevokeInvite`; e2e in `invites.spec.js`: revoke, the joiner's page stays live, a new join through the link fails.
- [x] **AC4** The revoke sheet (011-AC19) has "Also remove the N people who joined through it", unticked. N is the preview's "lose" count; the lose and keep lists show under the box. A room admin and the super-admin see the box; a plain member revoking their own invite does not. Ticked, the revoke does what it does today (011-AC19, 011-AC20): the chain's invites are revoked and the "lose" people are removed as in AC8 and AC9, with `end_reason = 'removed'`. *Verify*: e2e at 390 and 1440 px: admin sees the box with the count, member does not; ticked, the cut member's page shows "Your access to this room has ended."
- [x] **AC5** `DELETE /api/rooms/{room}/invites/{id}` takes `?cut=1`. A plain member sending it gets 403 "Only a room admin can remove people". The `invite.revoke` audit entry gains `cut` (bool) and `removed` (seat ids). The MCP `revoke_invite` tool sends no `cut`. *Verify*: Go table test over member, room admin, super-admin × cut on/off.

### Remove from room

- [x] **AC6** A room admin sees "Remove from room…" in another person's ⋯ menu (`personActions`). On a room admin's row it is disabled with "Remove their room admin role first". It opens a confirm sheet: "Remove <name> from the room?", "Their messages stay.", a "Block from rejoining" box (disabled with "No profile, so no block" for a seat without one), Remove as a danger button. *Verify*: e2e in `room-admins.spec.js`.
- [x] **AC7** `DELETE /api/rooms/{room}/participants/{id}` with `{"block": bool}`. One transaction checks: the caller is a room admin (403 otherwise); the target has a valid seat in the room (404); the target's profile is not a room admin (409 "Remove their room admin role first"). The caller's own id is a leave (AC12). Removal ends every valid seat of the target's profile in the room, or the one seat when it has no profile. *Verify*: Go table test: admin removes member 200, member removes member 403, admin removes admin 409, unknown seat 404, a profile with two seats loses both.
- [x] **AC8** Removal takes effect at once. The removed person's room cookie and profile session get 401 on the room's routes from the next request, as a revoked seat always has (001-AC3), and their API token 403 (012-AC3). Their open event streams get `revoked` and close; they leave the audio (`kickFromVoice`); their app's daemon stream gets `revoked`; the room's pushes stop reaching them. Turns they queued and that have not started are dropped. *Verify*: Go test on the seat queries and the turn queue; e2e: the removed browser shows "Your access to this room has ended." within one stream wake-up, and its next `GET /api/rooms/{room}` is 401.
- [x] **AC9** When a removed or leaving person's app is the AI host, its running turn fails with "the AI host left the room" and the host election runs as on a disconnect (003 D7): the first other eligible connected app becomes host, with its "<name> holds the agent(s)." line. With none eligible, the room has no host until an app connects. The host change resets 014's delegation and pending approvals. *Verify*: Go test on election after removal, with and without a second eligible daemon; e2e in `host-transfer.spec.js` with two fake apps.
- [x] **AC10** Removal posts "<admin> removed <name> from the room", plus " and blocked them from rejoining" when blocked, and writes `participant.remove` (seat ids, name, blocked) in the same transaction (004-AC6). *Verify*: Go test reads the line and the entry from one commit.
- [x] **AC11** The removed person's lines stay. A line whose seat ended and whose person holds no valid seat in the room now shows its sender as "<name> (removed)", or "<name> (left)" after a leave. Rejoining clears the mark. `@name` of someone no longer in the room stops being a chip (005-AC27 already reads the participant list). *Verify*: e2e: remove, the line reads "(removed)"; rejoin, the mark goes.
- [x] **AC12** "Leave room…" sits in your own row's ⋯ menu, with a confirm sheet. A room admin sees it disabled with "Step down as room admin first"; the last admin cannot step down (`ErrLastAdmin`), so cannot leave. The server answers the same route with your own id, ends your seats in the room with `end_reason = 'left'`, posts "<name> left the room" and applies AC8 and AC9 to you. No audit entry: a member's own act, as their invites are. *Verify*: Go test (admin 409, member 200, last admin 409); e2e: leave, the page shows the end screen, the room is gone from `/profile`.
- [x] **AC13** A block is a `(room, profile)` row. A join whose profile cookie names a blocked profile gets 403 "A room admin blocked you from rejoining this room." An unblocked removed or left person joins through any valid invite and gets a new seat. *Verify*: Go test on `Join`: blocked 403, unblocked 200 with a new seat id.
- [x] **AC14** Manage › Invites lists "Blocked from rejoining": name at removal, when, by whom, and "Unblock" for room admins. `/admin` › room › Invites shows the same (shared `room-panel.js`). Unblock posts "<admin> unblocked <name>" and writes `participant.unblock`. *Verify*: e2e in `room-admins.spec.js` and `admin-views.spec.js`.
- [x] **AC15** Blocks go with the room and the profile (`TestEveryTableCascadesFromAnOwner`). A profile merge (011-AC11) moves B's blocks to A, one per room. *Verify*: Go tests on delete cascade and merge.
- [x] **AC16** Removal and leave revoke the invites the person created in the room, their shareable link included; seats that joined through those invites stay (Q1). *Verify*: Go test.

### API tokens (012)

- [x] **AC17** A removed or leaving person's personal tokens stay valid for their other rooms. On this room they get 403 "Your profile is not in this room" (012-AC3), and `list_rooms` omits it. Rejoining gives the same token access again. A full token of a room admin may call the remove and unblock routes, and the audit entry carries `api_token` (012-AC10); a read token gets 403. *Verify*: Go auth table test.
- [x] **AC18** Export writes a participant's `active` from `removed_at`. Import sets `removed_at` on inactive ones, with `end_reason = 'revoked'`. *Verify*: export/import round-trip test with one removed seat.

### Mentions on edit

- [x] **AC19** Editing your own line so it names a persona that its previous body did not name, and that the line has not queued before, queues that persona's turn as a send does: same rights check, notices for no right, no host online and pending approval (005-AC3), chain reset (002-AC26), the line's "→ @Persona" chip (005-AC4). The edit answer carries `mention_notices` as the send answer does. *Verify*: Go test on `editMessage`.
- [x] **AC20** An edit never re-runs a persona: a persona named in the previous body, or already linked to the line's turns (named, edited out, edited back), queues nothing. An edit with no new persona queues nothing. Unknown-name notices cover new names only. *Verify*: Go table test: add, keep, remove then re-add, two new personas at once, no right.
- [x] **AC21** The edit box (autocomplete since PR #28) shows the answer's notices under the line, as after a send. *Verify*: e2e in `mentions.spec.js`: edit adds `@Grill`, the chip shows "queued"; a member without the right sees the notice.

## Phases

### Phase 1: Seats apart from invites
- Work: migration; one seat-validity predicate swapped into every query of AC2; `RevokeInvite` without the chain by default and with it on `cut`; revoke sheet box; export/import `active`.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC18
- **Data model impact**: `participants.removed_at TEXT`, `participants.end_reason TEXT CHECK (end_reason IN ('revoked', 'removed', 'left'))`, a partial index on `participants(room_id) WHERE removed_at IS NULL`.
- **DoD**: AC1 migration test green; the AC2 grep at 0, stated; Go revoke tests for both modes; 011's revoke e2e still green with the box ticked; `make lint test` green.

### Phase 2: Remove, leave, block
- Work: the remove/leave route and its store call; seat end with voice kick, stream wake-up, queued-turn drop, invite revoke; election on removal; blocks table, join check, Manage and `/admin` lists, unblock route; merge; the "(removed)" and "(left)" marks.
- **Covers**: AC6 to AC17
- **Data model impact**: `room_blocks(room_id NOT NULL → rooms ON DELETE CASCADE, profile_id NOT NULL → profiles ON DELETE CASCADE, name, created_at, by_participant_id → participants ON DELETE SET NULL, PRIMARY KEY (room_id, profile_id))`; audit actions `participant.remove`, `participant.unblock`.
- **DoD**: Go table tests of AC7, AC12, AC13, AC17; election test of AC9; cascade and merge tests; e2e: remove with block, removed page ends, rejoin refused, unblock, rejoin works and the mark goes; leave; host removed with a standby app taking over; `make lint test` green, changed e2e specs green.

### Phase 3: Mentions on edit
- Independent of Phases 1 and 2; may ship first.
- Work: `editMessage` diffs the persona set (`mentioned`) of the old and new body, drops personas already linked through `message_turns`, and runs the rest through `callMentioned`; `room.js` shows the notices on the edited line.
- **Covers**: AC19, AC20, AC21
- **Data model impact**: none. `EditMessage` returns the previous body.
- **DoD**: Go table test of AC20; e2e of AC21; `make lint test` green.

## Migration notes

- One append-only migration (Phase 1) and one for `room_blocks` (Phase 2). Both are additive; the Phase 1 backfill is one `UPDATE` from `invites`.
- No seat changes state: a seat valid before stays valid, a revoked one is `removed_at`.
- `participants.invite_id` stays, as the record of how someone joined; the revoke preview's chain still uses it.
- Rollback: the previous binary ignores the new columns, but would read seats removed after the upgrade as valid while their invite is valid. Rolling back after Phase 2 shipped means re-revoking by hand. Say so in the release notes.

## Risks

- One missed query in AC2 leaves a removed person a door (push, a token, a daemon). Mitigation: the grep at 0, and AC2's per-surface test.
- A block keys on the profile. A removed person who clears cookies and joins through a valid invite gets a new profile. Revoking the invite they used is the fallback; the confirm sheet does not promise more than "this profile".
- A removed host's daemon may post a turn result in flight. Its token no longer authenticates (AC8), so the room refuses the result, as for a removed daemon today (003b-AC9).
- Old messages of seats revoked before 016 show "(removed)" after the migration. That is accurate, and visible.

## Decisions

From the notes (user, 2026-10-16):

- Go: fix the membership model.
- Removed person's messages stay, marked "(removed)"; seat revoked at once; can rejoin through a valid invite unless "Block from rejoining" was ticked; block per profile, lifted by admins; admins must be demoted first; last admin cannot be removed; a removed host fails over to another app; chat line plus audit entry (Q1).
- Revoking an invite stops new joins by default; cutting the joiners is an explicit option with the count.
- An edit behaves like a send for mentions: new `@Persona` mentions run, existing ones never re-run, same rights checks and notices.

Made while drafting (overturnable):

- Removal is per person: every valid seat of the profile in the room ends. Access follows the profile's newest valid seat (`ProfileParticipant`, `APITokenParticipant`), so ending one seat of two would remove nobody.
- Unticked revoke cuts that invite alone, not the chain: the chain existed so a cut member could not walk back in through their own invite, and an unticked revoke cuts nobody. Ticked keeps today's chain behaviour, so 011's preview code stays.
- Only room admins and the super-admin get the box: cutting people is removal, an admin act.
- With no eligible app after a host's removal, the room has no host rather than an offline removed one: approvals and delegation must not wait on someone outside the room.
- Turns a removed person queued and that have not started are dropped; a running turn on someone else's app finishes.
- Leave writes no audit entry, as a member's own invites write none (004-AC6 audits admin actions).
- A left person's lines read "(left)", not "(removed)".
- Seats keep their rows: messages, versions and turns reference them with `ON DELETE CASCADE`, so deleting a seat would delete the messages Q1 keeps.

Made while implementing (2026-10-17, overturnable):

- AC8: an ended seat's room cookie and profile session answer 401, not 403: 401 is what the room page already reads as "Your access to this room has ended", and what 001-AC3 asserts for a revoked seat. API tokens answer 403 as 012-AC3 says.
- A block asked for a seat without a profile is ignored; the sheet disables the box for such a seat.
- Room settings › Admins and `/admin` › room › Admins also offer "Remove from room…" in their row ⋯ (Q2's UI); a room admin's row does not.
- The `/admin` e2e of AC14 lives in `host-transfer.spec.js`, next to the super-admin's host move.

User defaults, 2026-10-17, overturnable (the drafted Open questions, answered with their recommendation):

- Q1 yes: removal and leave revoke the invites the person created, their shareable link included; people who joined through them keep their seats (AC16).
- Q2 yes: the super-admin removes a person from `/admin` › room through a `/api/admin/rooms/{room}/participants/{id}` mirror; the line reads "The admin removed <name> from the room".
- Q3 yes: an edit of a voice line that adds `@Persona` runs it, and the voice line shows the "→ @Persona" chip as a text line does.
- Q4 yes: a ticked revoke posts one line, "<who> revoked an invite; <names> were removed".
