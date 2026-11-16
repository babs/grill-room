# 040 — Room settings and AI in this room, membership in one place

**Status**: shipped (v2 after council review)
**Requested by**: babs
**Date**: 2026-11-14
**Context**: UX audit part B, directions S3 (split Room settings) and S10 (membership in one place); journeys 4, 5, 6, 7, 10. Builds on 005 (Room settings, people), 011 (invites, admins), 016 (revoke and cut, blocking), 023 (links), 035 (Personas sheet), 038 (tab names, Leave, vocabulary test), 039 (`#ai` panel, merged first), and part A's tab fold (A10). Council review on 2026-11-15: v2 folds in its findings.

## Problem

Room settings holds 12 tabs for a room admin. Half are about the room (General, Invites, Members and admins, Links, Deleted documents, Export), half about its AI (Personas, Skills, Plugins, Websites and connectors, Words to recognise, Call voices). At 820 px the bar clips. The Personas tab is a hint with a button to another sheet; room skills live in two places (the Personas sheet copies them in, Room settings › Skills removes them). Plain members get a subset in a mixed order.

Membership is spread over two places with different actions for the same person:
- the People list ⋯: Rename, Can ask the AI, Make / Remove room admin, Make AI host, Lower hand, Volume, Remove from room… (with a block box), Leave room…;
- Room settings › Members and admins: the member list again, with Step down on one's own row, and Blocked from rejoining under it.

Invite links have no name ("Alice · link 8"). "Revoke" reads as cutting access, while it only stops new joins; removing who joined is a box inside its confirm (016).

## Solution

Two sheets from the room menu:
- **Room settings:** General, Members, Invites, Links, Deleted documents, Export.
- **AI in this room** (039's `#ai` sheet, now with tabs): Status, Personas, Plugins, Websites and connectors, Words to recognise, Call voices.

AI › Personas is the whole Personas sheet, room skills included, so Room settings › Skills goes. Members and the People list share one definition of the membership actions. Invites get an optional name, and "Revoke" becomes "Stop new joins" on 016's sheet.

## Scope

- Split the tabs between the two sheets; old deep links keep working; tab ids stay.
- AI › Personas hosts the 035 Personas sheet, room skills with their copy form and Remove included.
- Members (room admins): every seat with badges and the invite it joined with; row ⋯ from the shared membership actions; Blocked under it.
- Invites: optional name, rename, "Stop new joins" wording.
- Visible texts that name a moved place say its new place.

## Out of scope

- Settings by owner across `/profile`, host app and `/admin` (audit direction S5).
- The voice sheet (042).
- New permissions: who may do what stays as today. A plain member gets no Members tab and sees no invite provenance.
- `/admin`'s room panel keeps its own tabs; it gets invite names and the new wording through the shared components (AC17).
- Renaming the API routes, the MCP `revoke_invite` tool, the `invite.revoke` audit action, server error texts and seat messages ("Your access may have been revoked"): they keep "revoke".

## Acceptance criteria

### Two sheets

- [x] **AC1** The room menu shows "Room settings", "AI in this room" and "Personas", in that order. Room settings tabs: General, Members, Invites, Links, Deleted documents, Export (`#manage/<tab>`, ids `general`, `admins`, `invites`, `links`, `documents`, `export` unchanged). AI in this room tabs: Status, Personas, Plugins, Websites and connectors, Words to recognise, Call voices (`#ai/<tab>`, ids `status`, `personas`, `plugins`, `network`, `glossary`, `voices`; `#ai` opens Status). "Personas" in the room menu opens `#ai/personas`.
- [x] **AC2** `#manage/personas`, `#manage/plugins`, `#manage/network`, `#manage/glossary` and `#manage/voices` open the matching `#ai/<tab>`, replacing the history entry. `#manage/skills` opens `#ai/personas` scrolled to Room skills. `#links` still opens Links. A deep link to a tab the viewer lacks opens that sheet's first tab they have. In an ended room `#ai/<tab>` opens nothing and the hash goes, as `#ai` does (039).
- [x] **AC3** A plain member's Room settings shows Invites (their own) and Links. Their AI in this room shows Status, Personas, Plugins, Websites and connectors, and Words to recognise (038-AC12's "Not available on this server" when the engine is missing). A room admin gets every tab of both sheets.
- [x] **AC4** Engine-dependent tabs (Words to recognise, Call voices) keep 038-AC12: present with "Not available on this server" when the engine is missing, with 038's distinct texts for a worker loading or down.
- [x] **AC5** Each sheet's tab bar folds into its menu (A10) whenever it overflows its own width, at any viewport, not only below 40rem. Captures at 390, 640, 820 and 1440 px, at default and XL text size, show no clipped tab.
- [x] **AC6** One sheet owns the AI tab bar and mounts each tab: Status (039's panel), Personas (the existing vanilla Personas content), and the Preact tabs (Plugins, Websites and connectors, Words to recognise, Call voices). Closing it, by Escape, ✕ or a click outside, returns the focus as 039 does.

### Personas in the AI sheet

- [x] **AC7** AI › Personas holds the whole 035-AC17 Personas sheet: persona rows with ⋯, the one primary "Add a persona", the auto-turn limit, "Personas answer at the same time", and Room skills with its copy form. Each room skill row gains Remove for room admins (moved from Room settings › Skills, same confirm). There is one Personas list and one room skills list in the product.
- [x] **AC8** A persona's sheet opened from AI › Personas, closed by Escape, ✕ or Back, returns to `#ai/personas` with the focus on that persona's row. The picker opened from "Add a persona", cancelled, returns the focus to "Add a persona". A persona removed while its row holds the focus hands it to "Add a persona" (035-AC17).
- [x] **AC9** Visible texts that name a moved place say its new place: "AI in this room › Plugins" (038-AC4's Capabilities hint, 039-AC6's "Open Plugins"), "AI in this room › Websites and connectors" (039-AC7), "Room settings › Members" (038-AC10's Step down link), and Room settings › Skills' empty-state link (038-AC4) goes with its tab. README and docs follow. 038's vocabulary test fails on "Room settings › Plugins", "Room settings › Skills", "Room settings › Websites" and "Members and admins".

### Members

- [x] **AC10** Room settings › Members (room admins) lists every seat, one row each, never grouped by profile (003): name, "room admin" and "AI host" badges, "can ask the AI" when the host granted it, and the name (or "Link <id>") of the invite it joined with. The invite comes from the admin-only room overview, never from the room payload.
- [x] **AC11** The membership actions are one definition used by the People list ⋯ and the Members row ⋯: Rename, Make room admin / Remove room admin, Step down (own row, room admin; new in the People ⋯), Can ask the AI (the AI host), Make AI host, Remove from room… (016's confirm with its block box), Leave room… (own row). The People ⋯ also keeps Lower hand and Volume, which Members does not show. A test compares the membership actions of both menus, checked and disabled states included, for a room admin on another row and on their own, the last room admin, an AI host who is not a room admin, and a plain member (People only).
- [x] **AC12** Step down for the last room admin is disabled with "Make someone else room admin first." as text. Leave room… for a room admin keeps 038-AC10's two reasons (any room admin; the last one).
- [x] **AC13** "Blocked from rejoining" sits under the member list for room admins, with Unblock, and reads "Nobody is blocked." when empty. `/admin`'s room panel shows the same.

### Invites

- [x] **AC14** An invite has an optional name: one line, trimmed, at most 60 characters; empty clears it. It is set in the new-invite form and changed from the row ⋯ "Rename…" by its creator or a room admin, stopped rows included, through `PATCH /api/rooms/{room}/invites/{invite}` with `{"name": …}`; the instance admin renames from `/admin` as it revokes today. A member's shareable link (011-AC26) takes a name the same way. The name shows to room admins and the invite's creator only, never on the join page, and is not exported (archives carry no invites).
- [x] **AC15** Rows read "<name or Link <id>> · created by <member> · N in the room · Active" or "… · Stopped", where N counts the seats that joined through it and are still in the room. `invite.create` audit details carry the name; a rename by a room admin or the instance admin is audited as `invite.rename`, as creation is. MCP invite tools return `name`; no MCP rename tool.
- [x] **AC16** Row ⋯ "Stop new joins…" replaces "Revoke…". The 016 confirm sheet is kept (who loses access, who keeps it through another invite, chained invites, the box unticked by default, admin-only); its title reads "Stop new joins through this link?", its chain line "it also stops N invites made by people who joined through it", and its button "Stop new joins", or "Stop and remove N people" while the box is ticked (N from the preview's `lose`). The toast reports the actual result, 016's wording with "Stopped" for "Revoked".
- [x] **AC17** The invite sheet opened from "Invite people" links to "Room settings › Invites". `/admin`'s room panel shows invite names and the new wording through the shared components; its tab keeps the label "Members and admins".

## Phases

### Prerequisite
039 is merged.

### Phase 1 — Two sheets
- Work: the AI sheet's tab bar and mounting, redirects, plain member and admin tab sets, fold on overflow, reworded paths.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC9
- **Data model impact**: none
- **DoD**: e2e (`--workers=1`) on every old `#manage/<tab>` link, both tab sets, an ended room; captures per AC5; the existing suites that open these tabs (`layout-guards`, `approvals`, `admin-views`, `room-links`, `ai-readiness`, `plugins`) updated with their assertions kept, ids unchanged; vocabulary test red on each old path put back; `make lint` green.

### Phase 2 — Personas in the AI sheet
- Work: the Personas content hosted in AI › Personas, room skill Remove, focus returns, Room settings › Skills removed.
- **Covers**: AC7, AC8
- **Data model impact**: none
- **DoD**: e2e for AC8's three returns, Back and Forward, a live persona change while the tab is open, phone full screen; the `personas`, `persona-library`, `persona-sheet` and `skills` suites green.

### Phase 3 — Members and invites
- Work: Members tab, shared membership actions, Blocked always shown, invite names, Stop new joins wording.
- **Covers**: AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC17
- **Data model impact**: `invites.name TEXT NULL`, one positional migration, number taken at merge.
- **DoD**: `go test -race ./internal/store/... ./internal/web/...` green, with the migration applied on a fresh database and on one at the previous schema; tests for the name rules, rename permission (creator, room admin, instance admin, unrelated member, other room, read-only API token), audit entries without tokens in details; e2e of AC11's comparison, the stop sheet ticked and unticked, and seat privacy (two seats of one profile seen from a third session); `invites`, `admin-views` and `room-ux` suites green.

## Data model impact (summary)

- `invites.name TEXT NULL`. No backfill.

## Open questions

- [ ] None blocking.

## Decisions

- The AI sheet is 039's `#ai` sheet with tabs. Because one entry for "the AI in this room" covers status and settings, and keeps each bar short.
- Room settings › Skills goes; room skills live in AI › Personas, Remove included. Because two places for one list is what the audit found, and copying a skill in is open to every member there, while the Skills tab is admin-only.
- A plain member gets no Members tab and no invite provenance. Because the People list already shows them everyone, and who invited whom is room admin information today (011-AC27). The council review flagged the exposure.
- Membership actions share one definition; Lower hand and Volume stay People only. Because they are about the call, not membership. Step down joins the People ⋯ so the two menus match.
- Blocking stays the box in Remove's confirm. Because 016-AC6 shipped it and a second entry would duplicate it.
- A stopped row does not say how many it removed; the toast does. Because the invite stores no cut result and later removals or rejoins would make a count from current seats wrong (council review), and storing it is scope this spec does not need.
- Tab ids stay (`admins`, `documents`): only labels change. Because renaming ids buys the user nothing and costs redirects and e2e selectors (council review).
- 016's revoke sheet stays; only its words change. Because it already previews who loses access and chained invites.
- room.js owns the AI sheet: its dialog, its hash (`#ai`, `#ai/<tab>`), the ended-room guard, the tab bar, Status and Personas; room-admin.js mounts the Preact tabs into it and follows the tab room.js reports (`grill:ai-tab`). Because 039's sheet, its focus return and the Personas content are vanilla already, and one owner of the shown tab leaves no hash race between the two scripts.
- Phase 1 keeps the old Personas and Skills lists, Remove included, as an interim AI › Personas tab until phase 2. Because every commit keeps the `skills` suite green.
- A bar folds as soon as it overflows by more than half a pixel, so the whole-pixel scroll of a narrowed bar goes. Because AC5 folds at any width and a scrolled bar hides tabs.
- AI in this room is as wide as Room settings, 64rem: a room admin's six tabs show whole on a desktop and fold once the sheet is narrower than them (at 820 px in the captures). Because at 44rem they folded at every viewport, behind one more click (council review). Shorter labels, "Websites" and "Words", were the other option; the names stay, as 038's paths, the overview links and the vocabulary test carry them.
- AC5's "XL text size" is 028's XL plus a 20 px root font. Because 028's XL scales the chat and transcription only, not the sheets' bars.
- A persona's sheet and the picker open over AI in this room, which stays open under them, and closing the sheet replaces its hash with the tab it came from. Because the old Personas sheet worked that way, and going back through history instead leaves the room when the sheet was reached by Back.
- `#manage/skills` is redirected by room.js, the other moved tabs by room-admin.js. Because only room.js knows when the Personas tab is drawn, which the scroll to Room skills waits for.
- 042's hand-over to the Personas sheet goes: a persona's sheet opened from AI in this room › Personas stacks over it, so closing it returns there and then to the overview or Help through 042-AC7's stamp.
- Migration 67 adds `invites.name`. `invite.rename` details are `{"invite", "from", "to"}`, as `participant.rename`; a PATCH without the `name` key is refused, so a typo never clears a name. Invite names refuse the Unicode line and paragraph separators too; the shared text check stays as it is, so older archives keep importing.
- Invite creation and the shareable link take an optional JSON body: an empty one, or one not sent as JSON, means no name, as the routes took no body before.
- A room admin's own Members row has no Rename, as their own People row; they rename themselves from the avatar menu. Because AC11 makes the two menus one definition.
- The shareable link is named through its row's Rename. Because the server makes that link on first ask, so no form creates it.
- The join page's "invalid or was revoked" and /admin's "Invite revoked" audit label keep "revoked". Because they are a seat message and the label of the `invite.revoke` action, both kept by Out of scope.
