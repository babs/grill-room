# 006 — Room starters

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-13
**Context**: specs 002, 004, 005 and 008 (Room settings tabs, document rename, the design rules and their `⋯` menus, sheets and icon buttons), grill notes (006)

## Problem

Every new room starts the same way: one Default persona, no agenda, no documents. Setting up a retro or a doc review means adding personas by hand, picking models, triggers and skills, and explaining the goal in chat. A room that worked well cannot be reused as the base of the next one.

## Solution

"New room" opens a picker of room starters: blank, four built-ins (Spec grill, Retro, Doc review, Incident), the user's own starters and the instance's shared ones. Picking one opens an editor prefilled with its agenda, personas, documents and skills; the user adjusts, fills the starter's fields, and creates the room. The room gets a copy, with no link back. A room admin can save a room as a starter, review what goes in, keep it private or publish it to the instance.

## Scope

- Starter content: title, description, default room name, `agenda.md`, fields, personas, room skills, documents, chain settings, STT glossary.
- Three libraries: built-in (ships with the binary), personal (private to a profile), instance (shared, versioned).
- Built-ins: Spec grill, Retro, Doc review, Incident.
- New room flow: picker, preview, prefilled editor, fields, "Create room".
- Pinned `agenda.md` that every persona aligns on.
- "Save room as starter", with an editable review step.
- Starters in `/admin` Libraries, audit entries, storage quota.

## Out of scope

- Live link or sync between a room and its starter.
- Sharing starters across instances (a room export already carries everything).
- Fields in persona prompts.
- Messages, transcripts, agent sessions, participants, invites or annotations in a starter.
- Human editing of `agenda.md` text (agents revise it, humans annotate, as for any agent document).

## Acceptance criteria

### Libraries
- [x] **AC1** Starters live in three libraries: built-in, personal (visible only to its profile) and instance (visible to every profile). Each entry shows its title, author and version.
- [x] **AC2** Built-ins ship with the binary and are read-only. Any profile can duplicate one into its personal library. A binary that ships a changed built-in adds a new version at start; existing rooms are untouched.
- [x] **AC3** Every save of a starter, personal or instance, is a new immutable version with its author. A profile publishes a personal starter to the instance library. Its owner deletes a personal starter; the author or the super-admin deletes an instance starter. Rooms created from a deleted starter are untouched.
- [x] **AC4** A starter's size (documents and skills) counts against the instance storage quota (004-AC7). A save past the quota, or past 50 starters or 100 versions per profile, is refused with the reason.
- [x] **AC5** Publishing to and deleting from the instance library are written to the audit log (who, which starter, which version). The `/admin` Libraries tab lists instance starters with author, versions and size, and removes one.

### Content and built-ins
- [x] **AC6** A starter holds full copies, never references: title, description, default room name, `agenda.md`, fields, personas (name, badge, colour, text, model, effort, triggers, self-call, autocompact, active skills), room skills at a frozen version, documents at one revision (uploads with their original), chain settings (budget, suspended, parallel turns) and the room's words to recognise (STT glossary), carried as data only.
- [x] **AC7** The built-ins are:
  - Spec grill: Grill (sonnet / high, on demand) + Secretary (sonnet / medium, every 20 voice lines) + Rodin (sonnet / high, on demand);
  - Retro: Facilitator (sonnet / medium, on demand) + Secretary;
  - Doc review: Reviewer (sonnet / high, on demand, annotates the documents) + Secretary;
  - Incident: Incident-lead (sonnet / high, on demand, keeps `docs/timeline.md`) + Secretary.

  Prompts and agendas are in English; personas reply in the room's language. No built-in defaults to opus, and none carries skills.

### New room
- [x] **AC8** "New room" opens a picker: Blank, Built-in, Yours (personal) and Shared (instance), with a search on title, description and author. Selecting a starter previews its agenda, personas with model and effort, documents, skills, author and version.
- [x] **AC9** Picking any entry, Blank included, opens the editor prefilled; there is no direct create. The editor changes the name, agenda and personas, and leaves out documents and skills, for this room only, never the starter; documents are added in the room once it exists. "Create room" creates it.
- [x] **AC10** A starter's fields show at the top of the editor, required ones starred. "Create room" stays disabled until every required field is filled. Values replace `{{key}}` in `agenda.md` and the default room name; an empty optional field becomes an empty string. Persona prompts are never substituted.
- [x] **AC11** The new room holds copies. It records "created from <title> vN", shown in Room settings › General and in `/admin`, and carried in the export's `room.json`. A later version or the deletion of the starter changes nothing in the room.
- [x] **AC12** Room creation keeps the 004-AC7 guardrails, and the creator becomes room admin.
- [x] **AC13** Starter skills land as room skills. Those from personal or instance starters wait for the host under "approve everything", as a 004-AC10 copy-in does; built-in skills are pre-approved. A pending skill never blocks or fails room creation.

### Agenda
- [x] **AC14** The room document named `agenda.md` is pinned first in the document list, versioned and annotable like any agent document. The presence preamble names it as the room's agenda and tells every persona to align on it; a room without one gets no such line. The built-in Secretary ticks done items (`- [x]`).

### Triggers
- [x] **AC18** A persona's automatic trigger is one of:
  - none;
  - every N rounds (002);
  - every N voice lines;
  - every T minutes.

  A human can still run any persona on demand. The timer fires only when at least one human is present (005-AC6) and the room had new voice or text activity since that persona's last turn; otherwise it waits for the next tick. Voice-line and timer turns count against the room's turn limit like any automatic turn, and stop when it is reached or suspended. The persona panel shows the next trigger ("in 7 voice lines", "in 4 min"). The Personas sheet words the choice as "Speaks up": only when asked, every N answers, every N voice lines, every N minutes.

### Save as starter
- [x] **AC15** A room admin's "Save as starter" (Room settings › General) opens the same editor prefilled with the room's personas, their active skills and the room skills, `agenda.md`, chain settings and words to recognise. Documents are listed unchecked and added one by one, at their current revision. Everything is editable before saving. The result lands in the saver's personal library. A plain member sees no button and gets a 403 on the route.
- [x] **AC16** The editor's Fields section declares fields: label, key derived from the label, required checkbox, optional hint. "Insert field" puts a `{{key}}` chip at the cursor in `agenda.md`; typing `{{key}}` works too. The default room name may use fields. Saving is blocked by an undeclared `{{x}}` in the agenda or the name, naming it; a declared field used nowhere shows a warning only.
- [x] **AC17** A starter saved from a room never contains its messages, transcripts, agent sessions, participants, invites, annotations or older revisions.

## Phases

### Phase 1 — Starter store, libraries, built-ins
- Work: starter and version storage, the three libraries, built-ins upserted at start (like persona presets), duplicate, publish, delete, quota and limits, audit entries, `/admin` Libraries tab, profile-page list.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7
- **Data model impact**: `starters` (builtin key, owning profile, library); `starter_versions` (n, author, manifest JSON, size); `starter_files` (document and skill blobs per version)
- **DoD**: store tests for versioning, a changed built-in giving v2, deletion leaving rooms intact, quota and limit refusals; cascade and purge invariants extended to the new tables; e2e for duplicate, publish, delete and the admin tab with its audit lines.

### Phase 2 — New room from a starter
- Work: picker with search and preview, prefilled editor, fields at creation, copy into the room, provenance, pinned agenda and preamble line, pending starter skills, the voice-line and timer triggers.
- **Covers**: AC8, AC9, AC10, AC11, AC12, AC13, AC14, AC18
- **Data model impact**: `rooms.created_from`; `room_personas` trigger kind (rounds, voice lines, minutes) with its N or T, and a last-turn activity mark
- **DoD**: e2e creating a room from each built-in and from Blank, at phone and desktop width; required field disables "Create room"; a room unchanged after its starter gets v2 and is deleted; a pending skill under "approve everything"; golden preamble test with and without `agenda.md`; export of a created room carries `created_from`; trigger unit tests with a fake clock: voice-line count, timer firing only with presence and new activity, timer silent in an empty or idle room, both stopped by the turn limit.

### Phase 3 — Save room as starter
- Work: Manage action, review editor with document opt-in, Fields section, `{{key}}` validation, save to personal library.
- **Covers**: AC15, AC16, AC17
- **Data model impact**: none
- **DoD**: e2e save → publish → create a room from it, content equal to the source minus excluded items; validation blocks an undeclared key in the agenda and in the name, warns on an unused field; a source room with messages, sessions, invites and annotations yields a starter holding none of them; plain member gets 403.

## Data model impact (summary)

- New: `starters`, `starter_versions`, `starter_files`.
- `rooms.created_from`.
- `room_personas` trigger kind and value (rounds, voice lines, minutes).
- Starter tables cascade from `profiles` (personal and published starters), like persona templates.

## Personal data

- A starter carries its author's display name and owning profile, nothing about the room's participants.
- Saved documents are chosen one by one by the room admin; the review step shows what leaves the room.

## Open questions

- (none)

## Decisions

Reconciliations with specs 005 and 008, which shipped after this draft (2026-10-13):

- The words to recognise (STT glossary) are off by default (005 Decisions) and spec 007 hides them. A starter still copies the room's text into the new room, and nothing in 006 reads or tests its use.
- Room settings, formerly Manage, keeps its tabs (General, Invites, Admins, Personas, Skills, Documents, Export, Words to recognise). "Save as starter" and "created from" go in General; no new tab.
- UI words follow `docs/design.md` §8: the picker's groups read Built-in, Yours and Shared, not Personal and Instance; "Speaks up" names the triggers. Lists use the 008 `⋯` row menu, the editor and the picker are sheets, and confirmations use the confirm sheet.
- Blank is not stored: it is the Default template's persona, as a room was before 006.
- Publishing moves a personal starter, with its versions, to the instance library; its owner stays its author. A profile duplicates any starter it sees into its personal library. Its owner edits a personal or published starter from the profile page, which saves the next version.
- The editor at room creation removes documents and skills but does not add them: uploads need a room. Documents are added in the room afterwards.
- `agenda.md` is pinned and named in the preamble when it is an agent document: an upload named so stays read-only and unpinned.
- The 100-version limit counts per starter, as for templates.
- A starter skill stays pending until a host connects: with "approve everything" on, the host decides as for a 004-AC10 copy-in; otherwise it turns on when the host connects. A pending skill never loads in a turn.
- Voice-line and timer turns queue as "every N" chain links, with their own cause; the queue keeps its sources.

Decisions taken in the loop (2026-10-13):

- "Insert field" puts the text `{{key}}` at the agenda's cursor: the agenda is a plain textarea, so the chip is that text.
- A voice-line or timer trigger counts from its persona's latest turn, or from when it was turned on; an imported room's lines are history and count for nothing.
- A starter's documents and skills count against the instance storage quota when a room is created from it, as when it is saved.
- The timer is checked every 15 s; marks are stamped with the server's clock, as turns are.


- Starters are copied into the room at creation; no live link, no sync (user, Q1).
- Personal and instance libraries plus read-only, duplicable built-ins (user, Q2).
- Save as starter captures personas, room skills, agenda, chain settings, glossary; documents opt-in at current revision; never messages, transcripts, sessions, participants, invites; editable review; personal library first (user, Q3).
- Goal and agenda live in a pinned `agenda.md`; personas align on it, the Secretary ticks items (user, Q4).
- Starter skills become room skills, pending under "approve everything", built-ins pre-approved, creation never blocked (user, Q5).
- Built-ins as in AC7: English, reply in room language, no opus default, no skills (user, Q6).
- Single path: pick, prefilled editor, "Create room" (user, Q7 revised).
- Fields in `agenda.md` and room name only, with save validation (user, Q8). Keep it light.
- No silent notes (user).
- A starter embeds full persona copies, not template references. Routine call, overturnable.
- No cross-instance starter sharing; room export covers it. Routine call, overturnable.
- A changed built-in becomes a new version; existing rooms are untouched. Routine call, overturnable.
- Starter size counts against the instance storage quota. Routine call, overturnable.
- The audit log records publishing and deleting instance starters. Routine call, overturnable.
- Personal starters are versioned like instance ones, one model for both. Routine call, overturnable.
- Limits mirror persona templates: 50 starters, 100 versions per profile. Routine call, overturnable.
- Facilitator, Reviewer and the agenda-ticking Secretary exist only inside the built-ins, not in the persona template library. Routine call, overturnable.
- `agenda.md` is pinned by name, with no new column; renaming it unpins it. Routine call, overturnable.
- Starter tables cascade from the author's profile, like persona templates. Routine call, overturnable.
- `created_from` is an optional `room.json` field; the export format does not change. Routine call, overturnable.
- Triggers: on demand always, plus one automatic mode among N rounds, N voice lines, T minutes; the timer needs presence and new activity (user, 2026-10-13). One automatic mode per persona and the Secretary's 20 voice lines are routine calls, overturnable.
- Non-built-in starter skills land pending; the first host approves them under "approve everything", otherwise they activate when it connects (user, 2026-10-13).
- Starter personas are the room's initial setup, not held for approval; the host's model cap still clamps them (user, 2026-10-13).
- Spec grill gains Rodin, who asks whether the idea should exist before Grill tests the spec branch by branch. Rodin is also a template in the persona library. Only rooms created from the new version get it (user, 2026-10-18).
- A fourth built-in, Incident, for code, security and infra incidents: Incident-lead puts mitigation before root cause and keeps a UTC timeline, the Secretary keeps the minutes (user, 2026-10-21). The persona is "Incident-lead": names take no spaces. Its optional Scope field leads the agenda title, so an empty one leaves no stray brackets: routine call, overturnable.
