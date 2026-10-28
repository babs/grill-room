# 023: Room links and related rooms

**Status**: approved
**Requested by**: babs
**Date**: 2026-10-27
**Context**: builds on 021 (restricted instance: externals create no room), 003 (turns, workspace), 004 (room admins, audit log), 011 (shareable invite link 011-AC26), 012 (REST and MCP with API tokens), 016 (remove, leave, seats). Siblings: 024 (projects and tags), 025 (search).

## Problem

A topic often spans several rooms: a pricing review spawns a legal room, a spec room spawns one room per component. Each room's personas see only their own room. To use what was said or written next door, someone copies documents by hand and pastes chat excerpts, and the copy goes stale. A persona that sees a side question worth its own room can only say so; someone has to create the room and brief it by hand.

## Solution

A room admin who is also admin of another room links the two. A link has a direction: "A reads B" lets A's personas read B's documents and conversation during their turns. The link sheet offers both directions at once, both ticked. Both rooms show the link: A lists "Reads B", B lists "Read by A", each with the room's name, and any admin of either room removes it.

Linking gives every member of A read access to B, since anyone in A can ask a persona to quote B. So only someone who is admin in both rooms creates a link.

A persona may propose a related room. Its proposal appears in the chat as a card. A member opens it into a creation sheet: name, the brief the persona wrote, the current members of A to bring along (ticked or not), and the two link directions. Confirming creates the room with the brief as its first document. The person who confirms becomes its creator and room admin.

## Scope

- Directed links between two rooms of the instance: create (one or both directions), list, remove.
- Linked rooms in the turn workspace, read-only.
- Room settings › Links tab; a links line in the room header menu.
- A persona's related-room proposal, its chat card and creation sheet.
- REST routes, MCP tools, audit entries, chat lines, README and `docs/agent-workspace.md`.

## Out of scope

- Transitive reads: when A reads B and B reads C, A does not read C.
- Writing to a linked room: no document edit, annotation, reply or message there.
- Linked rooms' annotations and attachments in the workspace.
- A request and accept flow between admins of two rooms (Q1).
- Links in export and import: an export names no other room, an import creates none. Export leaves out `proposed_in_*` and `turns.linked*`; import sets them NULL.
- Links across instances.
- A persona creating a room without a human confirming it.
- Search across linked rooms (025 searches your own rooms).

## Acceptance criteria

### Links

- [ ] **AC1** Room settings has a "Links" tab, deep link `#links`, visible to every member. It lists "Reads" (rooms this room reads) and "Read by" (rooms that read this one) as kit list rows: room name, who linked it, date. A member who is not a room admin sees the list with no menu.
- [ ] **AC2** "Link a room" shows only for a room admin. It lists the other rooms where the caller's profile is a room admin, by name, and two checkboxes, both ticked: "<this room> reads <other>" and "<other> reads <this room>". Saving with at least one ticked creates the ticked links in one transaction. A room the caller is not admin of never appears; the route answers 403 "You must be a room admin of both rooms" when called for one.
- [ ] **AC3** A link that already exists is not created twice: the sheet shows it ticked and disabled, and the route answers 200 with the existing link.
- [ ] **AC4** A room reads at most 10 rooms. The 11th link answers 409 "This room already reads 10 rooms. Remove one first."
- [ ] **AC5** A room admin of either room removes a link from its row's ⋯ › "Remove link", with no confirm sheet. Each direction is its own row and is removed alone.
- [ ] **AC6** Creating and removing a link posts one system line in each of the two rooms ("Ana linked this room: it now reads Legal", "Ana linked Pricing: it now reads this room"; "… removed the link …") and writes one audit entry per link, actor the profile, action `room_link.create` or `room_link.delete`, details with both room ids and names, `api_token` when done with a token (012-AC10).
- [ ] **AC7** Deleting either room deletes its links in both directions, in the room delete's transaction: the other room's list no longer shows them and its chat gets "<room> was deleted; the link is gone". The room delete's audit entry lists the links it removed.
- [ ] **AC8** The room header's ⋯ menu shows "Linked rooms (N)" when N > 0, opening the Links tab. A linked room's name links to that room only for someone who is a member of it; for everyone else it is plain text.

### What a persona reads

- [ ] **AC9** When room A reads room B, each turn of a persona of A gets B under `linked/<b-slug>/` in its workspace, read-only: `docs/` (B's agent documents at their latest revision), `uploads/` (B's uploads and their Markdown copies, as in A's own workspace) and `transcript/` (B's messages up to the turn's creation, AC13, same chunking). `<b-slug>` is B's name made path-safe, with `-2`, `-3` … when two linked rooms give the same slug.
- [ ] **AC10** B's transcript under `linked/` leaves out deleted messages and shows edited messages at their current text, untagged: A's members are not B's participants, and a delete in B hides a line from them. A's own `transcript/` keeps its rules.
- [ ] **AC11** The prompt lists the linked rooms with their slug, B's name and its document names, and says the folder is read-only, its transcript as of the turn's creation and its documents as of the turn's first fetch. A room with no link gets no `linked/` folder and no prompt section.
- [ ] **AC12** Any file the agent creates, changes or deletes under `linked/` is dropped and listed in the turn's system message, as for `uploads/`.
- [ ] **AC13** A turn's creation pins its linked rooms on the turn, apart from `base_revisions`: each one's id, slug and last message id, the transcript cutoff AC9 means. A room linked after that is not in this turn. The first fetch of the turn's manifest or tar then pins those rooms' document revisions. Later fetches of that turn serve that snapshot. A link removed, or room B deleted, while the turn runs: requests for B's files are refused from then on as any path outside the turn is (400), the full-tar fallback leaves B out, the turn keeps what it already fetched, and its result is accepted as usual. The next turn has no `linked/<b-slug>/`. The daemon serves no linked file from its cache for a room the turn's manifest does not list.
- [ ] **AC14** The workspace routes serve B's files to A's host daemon only for a turn of A whose manifest lists B. Any other request for B's files from A's daemon is refused the same way.
- [ ] **AC15** Removing someone from B, or their leaving B, does not change what A's personas read. B's "Remove from room…" sheet says, when B is read by other rooms: "Rooms that read this one keep their access; removing someone here does not cut it." It names a reading room where the person still sits only to a remover who also sits in it.

### Related rooms

- [ ] **AC16** A turn may write `room-new.json` at the workspace root, which the daemon posts with the result as `room_new`: `{"name": "...", "brief": "..."}`, name 1 to 80 characters on one line, brief Markdown up to 64 KB. A valid file posts a card in the chat under the synthesis: "<persona> proposes a room: <name>", with "Create…" and "Dismiss". One proposal per turn; a second file, a bad JSON, an empty name or a brief past 64 KB is refused and listed in the turn's system message. A daemon from before this spec drops the file silently, as any root file: no card, no line.
- [ ] **AC17** "Create…" shows for every member with a profile. It opens a sheet: name (editable, prefilled), the brief (read-only preview), the current members of A with a profile as checkboxes, the caller ticked and disabled, others unticked, and, when the caller is a room admin of A, the two link checkboxes of AC2, both ticked. A member who is not an admin of A sees no link boxes and the line "A room admin of <A> can link the rooms later".
- [ ] **AC18** Confirming creates the room under the caller's right to create rooms: the room creation limits (004) and, on a restricted instance, internals only (021), with the caller as creator and room admin, the brief as its first agent document `brief.md`, and the ticked links. Each ticked member gets a seat through the caller's shareable invite of the new room (011-AC26), so the invite chain rules of 016 apply to them, and a push "<caller> added you to <name>". The card then reads "Created by <caller>: <name>", linking to the room for its members. Limits reached: 403 with the reason, nothing created, card unchanged.
- [ ] **AC19** "Dismiss" shows for the person who triggered the turn and for room admins; it greys the card out with "Dismissed by <name>". A proposal is acted on once: a second Create or Dismiss answers 409 "Already handled".
- [ ] **AC20** The new room keeps "Proposed in <A>" in Room settings › General, linking to A for its members, whether or not the rooms are linked. The room JSON carries it as `proposed_in` (`room`, `name`), apart from the starter provenance `created_from` (006-AC11); a room with both shows both lines.

### API and MCP

- [ ] **AC21** REST: `GET /api/rooms/{room}/links` (member), `POST /api/rooms/{room}/links` with `{"room": "<other>", "reads": bool, "read_by": bool}` (room admin of both), `DELETE /api/rooms/{room}/links/{id}` (room admin of either), `POST /api/rooms/{room}/proposals/{id}/create` and `.../dismiss`. A read-only token gets 403 on the writes (012-AC4).
- [ ] **AC22** MCP: `list_room_links` (read), `link_rooms` and `unlink_rooms` (full; `unlink_rooms` needs `confirm` equal to the other room's name, 012-AC20). Both map to the routes above and run with the token's rights. A token does not let an outside agent read a linked room: links widen what personas read, never what a profile reads.

## Phases

### Phase 1: links data and settings
- Work: table, store, routes, audit, chat lines, Links tab, link sheet with both directions, header menu entry, deletion cascade, cap.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC21 (link routes)
- Work also: `DeleteRoom` grows from one statement to a transaction that posts the other rooms' lines and the audit details before the delete.
- **Data model impact**: new `room_links` (`id`, `reader_room_id`, `read_room_id`, `created_by_profile_id` nullable `ON DELETE SET NULL`, `created_at`, both rooms `ON DELETE CASCADE`, `UNIQUE (reader_room_id, read_room_id)`, `CHECK (reader_room_id <> read_room_id)`); one migration.
- **DoD**: store tests for create, duplicate, cap, cascade, rights; Go route tests for 403 and 409; e2e: an admin of two rooms links both ways in one save and sees both rows in each room; a non-admin sees no button.

### Phase 2: linked rooms in the workspace
- Work: manifest and tar entries under `linked/`, the per-turn snapshot, transcript filter, prompt section, refusal of writes, daemon cache scoping, route check.
- **Covers**: AC9, AC10, AC11, AC12, AC13, AC14, AC15
- **Data model impact**: `turns.linked` (JSON: per linked room its id, slug and last message id, set at turn creation; its document revisions, added at the first manifest or tar fetch).
- **DoD**: workspace tests: a deleted message in B is absent from `linked/`, present in B's own `transcript/`; a write under `linked/` is listed and dropped; a request for B's file without a listing turn gets 404; e2e with a stub daemon: a turn of A reads a document of B.

### Phase 3: related rooms
- Work: daemon reads `room-new.json` and posts it as `room_new` with the result; server parsing, proposal store, chat card, creation sheet, seating through the shareable invite, push, "Proposed in".
- **Covers**: AC16, AC17, AC18, AC19, AC20, AC21 (proposal routes)
- **Data model impact**: new `room_proposals` (`id`, `room_id` cascade, `message_id ON DELETE SET NULL`, `turn_id ON DELETE SET NULL`, `name`, `brief`, `status` `open|created|dismissed`, `handled_by_profile_id` nullable `ON DELETE SET NULL`, `created_room_id ON DELETE SET NULL`); `rooms.proposed_in_room_id` (`ON DELETE SET NULL`) and `rooms.proposed_in_name` (kept when A is deleted); same migration as phase 1 if unreleased, else one more.
- **DoD**: daemon test: `room-new.json` posted as `room_new`; turn result tests for each refusal; route tests for limits, 409 on the second action, seats via the invite; e2e: a stub turn writes the file, a member creates the room with one other member and both links.

### Phase 4: MCP and docs
- Work: three tools, README (Rooms, Agent, API table, audit actions), `docs/agent-workspace.md` (`linked/`, `room-new.json`).
- **Covers**: AC22
- **Data model impact**: none
- **DoD**: MCP tests: catalogue, read-only token lists `list_room_links` only, confirm mismatch; `tools/list` sorted.

## Data model impact (summary)

- `room_links` (new), `room_proposals` (new), `turns.linked`, `rooms.proposed_in_room_id`, `rooms.proposed_in_name` (new columns).
- One migration, numbered at merge time after 56, last merged branch rebases.

## Open questions

- None blocking.

## Decisions

- Q1 (user, 2026-10-27): only a person who is room admin of both rooms links them. No request and accept flow.
- Q2 (user, 2026-10-27): a link is directed and shown on both sides. The link sheet creates both directions at once by default (user, same day); each stays its own row.
- Q3 (user, 2026-10-27): a related room is a persona proposal that a human confirms in a sheet listing A's current members.
- Personas read linked rooms through the workspace, not through an API call from the sandbox. The sandbox has no credential for the room server and turns already read their own room this way.
- A link outlives the admin who made it. Consent is the room's, through its admins; any admin of either side removes it.
- Linked transcripts hide deleted lines (AC10). A's members never had B's participant view.
- Members seated in a related room go through the creator's shareable invite. A new seat type would bypass 016's chain revocation.
- Cap of 10 read rooms per room. It bounds the workspace size and the prompt; there is no env setting until someone asks for one.
- No new env key and no manifest change.
