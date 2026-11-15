# 035 — Persona library

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-14
**Context**: UX audit part B (journeys 1 and 2, direction S1). Builds on 002 (personas, templates), 006 (starters and their libraries), 009 (persona sheet, skills, plugins), 013 (voices), 032/033 (time keeper). Folds in part A finding A19 (Personas sheet rows).

## Problem

A PM tunes a persona in one room (prompt, trigger, voice, skills) and cannot take it anywhere else whole. The "library" is a `<select>` of templates that keep 6 of a persona's ~15 settings: name, badge, colour, text, model, effort. Reusing a persona costs 11 taps and the copy loses its trigger, follow-up, notes watcher, memory size, voice, time keeper, skills and plugins. "Save to the library" publishes to every profile on the instance with no confirm; its author can neither unpublish nor delete it; a second press creates a duplicate. No page outside a room shows a template. Every persona that came with a starter says "Its template was removed from the library", which is false. Starters, next door, already have Yours / Shared with everyone / Built-in and keep every setting, but only apply to new rooms.

## Solution

Personas get a library like starters. `/profile` gains a Personas tab with three sections: Yours, Shared with everyone, Built-in. A library persona holds the whole persona: every setting the persona sheet has, plus frozen copies of its skills and the plugins it uses. From the tab, a PM reads a persona before using it, edits their own (each save is a new version), duplicates any into Yours, shares or unshares their own, deletes their own, and adds one to any room where they can add personas. In a room, "Save to my personas" puts the persona in Yours; sharing is a separate, confirmed step. "Add a persona" opens a picker that shows what each persona does. A room persona says truthfully where it came from.

## Scope

- A library persona carries every persona setting: name, badge, colour, text, model, effort, memory size, trigger (every N answers, voice lines or minutes), follow-up, notes watcher, speaks in the call, voice, rate, time keeper, speak-over wait, and a one-line description.
- It carries frozen copies of the skills it has on, at their version, and the plugins it has on as git references, as a starter does (006, 009-AC28).
- Three libraries: Yours (private to the profile), Shared with everyone (every profile on the instance), Built-in (shipped presets).
- `/profile` › Personas: list, read, edit (new version), duplicate to Yours, share, unshare, delete, add to a room.
- Room: "Save to my personas" from the persona sheet ⋯; a picker for "Add a persona" with sections, description and a preview; a "+" at the end of the persona bar that opens the picker.
- Adding a library persona to an existing room brings its skills in as room skills and its plugins as room plugins, under the AI host's approval as today.
- Truthful provenance on the persona sheet.
- The Personas sheet lists personas as kit rows with a ⋯ and one primary action (part A, A19).
- Existing user templates move to Yours; presets become Built-in.
- `/admin` › Libraries › Persona templates lists Shared and Built-in, as it does for starters.
- MCP `list_templates` returns the library section and the description of each persona.

## Out of scope

- Starters referencing library personas: a starter keeps its own copy of each persona.
- Sharing with one person, a group or a room's members: Shared means the whole instance, as for starters and skills.
- Updating a room persona automatically when its library persona changes: the room keeps "Update to vN" (002), offered only to a viewer who can see that library persona.
- New MCP write tools for the library (create, edit, share). `add_persona` keeps working with a library persona id.
- A catalogue of skills and plugins (audit direction S8).
- Moving a persona's session or memory between rooms: a library persona is configuration, never conversation.

## Acceptance criteria

### The library

- [x] **AC1** `/profile` has a "Personas" tab after "Starters", with a count, and a deep link `/profile#personas`. It lists three sections in this order: "Yours", "Shared with everyone", "Built-in". Each row shows badge, name, version, author, description and the row ⋯. Empty Yours reads "No personas of yours yet. Save one from a room, or duplicate one below."; empty Shared reads "Nobody has shared a persona yet."
- [x] **AC2** A row's name opens the persona's page: every setting in read-only form (identity, model, triggers, call, capabilities with skill names and versions and plugin names), its version history (number, author, date), and actions by role. On a phone the page is a full-screen sheet.
- [x] **AC3** The author edits their own library persona on its page with the same four tabs as the room persona sheet (Identity, Model, Triggers, Capabilities), Description included. Save makes version N+1; nothing in any room changes. A non-author has no Edit.
- [x] **AC4** Row ⋯ for the author: "Add to a room…", "Duplicate", "Share with everyone" or "Stop sharing", "Delete…". For anyone else: "Add to a room…", "Duplicate to yours". Duplicate makes a new library persona in Yours at version 1, titled "<name> (copy)" when the name is taken in Yours.
- [x] **AC5** "Share with everyone" asks: "Everyone on this server will see <name> and can add it to their rooms. Rooms keep their copies if you stop sharing." Confirming moves it to Shared. "Stop sharing" moves it back to Yours without a confirm; rooms that added it keep their copies, and other viewers lose its "Update to vN" offer.
- [x] **AC6** "Delete…" confirms "Rooms keep the personas they added from it." and removes the library persona with its versions and frozen files. The instance admin removes a Shared one from `/admin`; Built-in ones cannot be deleted.
- [x] **AC7** A profile holds at most 50 library personas of its own and 100 versions each (today's limits); past them, the action answers with the limit in words and changes nothing.

### Whole persona

- [x] **AC8** A library persona version stores every setting listed in Scope. Adding it to a room gives the room persona exactly those settings; a test round-trips each field room persona → library → another room and compares them.
- [x] **AC9** Saving a persona that has skills on stores a frozen copy of each skill at the version the persona uses; a skill the saver may not read (another profile's personal skill copied into the room) is stored as the room copy it is. Plugins are stored as their git URL, ref and entry, as a starter stores them.
- [x] **AC10** Adding a library persona to a room brings each frozen skill in as a room skill (pending under the host's "hold for approval", as starter skills are, 006-AC13) and each plugin as a room plugin awaiting the host's review (009). A skill or plugin of the same name already in the room is reused, not duplicated. The add answers with what waits for the host.
- [x] **AC11** The time keeper role is copied only when the room has no time keeper yet (one per room, 032); otherwise the persona arrives without it and the status line says so.

### In the room

- [x] **AC12** The persona sheet ⋯ shows "Save to my personas" for a room persona not linked to one of the caller's library personas, and "Save to my personas as v<N+1>" for one that is (the linked one when it is the caller's, else the one the caller first saved from this persona). Saving uses the saved state of the persona; with unsaved changes the item reads "Save your changes first" and is disabled with that reason as text, not a tip.
- [x] **AC13** After a first save, the room persona is linked to the new library persona: the next save makes version 2, never a second library persona. The status line reads "Saved <name> v<N> to your personas." with a link to `/profile#personas`.
- [x] **AC14** "Add a persona" (Personas sheet, and a "+" button at the end of the persona bar labelled "Add a persona") opens a picker with the three sections. The "+" sits after the scrolling pills, outside the bar's scroll area, so it never mixes with 034's edge "+N" buttons; on a phone it keeps its 44 px target. Selecting a row shows a preview: description, model and effort, trigger in words, voice when the server has speech, skill and plugin names. "Add" adds it; the picker closes and the new pill shows.
- [x] **AC15** At 12 personas in the room, the Personas sheet's "Add a persona" and the picker's "Add" are disabled with the text "A room holds 12 personas at most."; the bar's "+" opens the picker, which says so.
- [x] **AC16** The persona sheet's Identity tab names the source: "From <name> v<N>, in your personas", "…, shared by <author>", "…, built in", "Came with the starter <title>" (a persona from a starter), "Made in this room" (none), or "Its library persona was deleted." only when it was added from one that no longer exists.
- [x] **AC17** The Personas sheet lists the room's personas as kit rows with a ⋯ (Edit, Save to my personas, Remove); "Add a persona" is its one primary button and comes before the auto-turn limit and room skills.

### Migration, admin, API

- [x] **AC18** On upgrade, every existing template published by a profile becomes a library persona in that profile's Yours at its current versions; presets become Built-in. Rooms keep their personas; a room persona added from another profile's template keeps its settings and stops offering updates to viewers who cannot see it. Its source line reads "From <name> v<N>, by <author>".
- [x] **AC19** `/admin` › Libraries › "Persona templates" becomes "Personas" and lists Shared and Built-in only, with Remove on Shared ones.
- [x] **AC20** `GET /api/rooms/{room}/templates` and MCP `list_templates` return, per library persona the caller can see, its section (`personal`, `instance`, `builtin`) and description. `add_persona` with such an id behaves as AC10.
- [x] **AC21** A profile merge (admin) moves the merged profile's library personas into the kept profile's Yours; a profile deletion deletes its library personas and leaves rooms' copies.

## Phases

### Phase 1 — Whole-persona versions and libraries
- Work: library personas get a section (personal, instance, builtin) and a description; a version stores every persona setting and frozen skill and plugin copies; migration of existing templates; saving from a room links the room persona (no duplicates); truthful provenance.
- **Covers**: AC7, AC8, AC9, AC12, AC13, AC16, AC18, AC21
- **Data model impact**: `persona_templates` gains a section column (personal, instance, builtin) replacing the `preset`/`profile_id` pairing rule; `persona_template_versions` gains a description and the persona settings beyond the six fields (or one manifest column in the starter persona shape); a files table for frozen skills, as `starter_files`. One positional migration (number taken at merge, last to merge rebases).
- **DoD**: migration applies on a fresh database and on a copy holding user templates; `go test -race ./internal/store/... ./internal/web/...` green, with a round-trip test per field (AC8) and a duplicate-save test (AC13).

### Phase 2 — Profile Personas tab
- Work: the tab, the read page, the editor, duplicate, share, unshare, delete, add to a room; `/admin` Libraries renamed and scoped.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC19
- **Data model impact**: none
- **DoD**: e2e (`--workers=1`): save a tuned persona in room A, find it on `/profile#personas`, edit it to v2, share it, see it from a second profile, unshare, delete; captures at 390 and 1440 px.

### Phase 3 — Room picker and adding a whole persona
- Work: "+" on the persona bar, the picker with preview, adding with skills and plugins, the time keeper rule, Personas sheet on kit rows; the API and MCP fields.
- **Covers**: AC10, AC11, AC14, AC15, AC17, AC20
- **Data model impact**: none
- **DoD**: e2e with the fake AI host: add a library persona with one skill and one plugin to a room, the skill and plugin wait for approval and land once approved; a 12-persona room shows the disabled state; `make lint` green.

## Data model impact (summary)

- `persona_templates`: section (personal, instance, builtin) and its constraint; migration of existing rows (profile rows to personal, presets to builtin).
- `persona_template_versions`: description and every persona setting beyond the six fields.
- New table for frozen skill files per version, modelled on `starter_files`.
- `room_personas`: none beyond what it has; provenance reads `template_id`, `template_version` (0 = not from the library) and the room's starter provenance.

## Open questions

- [x] None blocking. Answered on 2026-11-14: existing templates go to Yours (private); skills and plugins are frozen copies; saving goes to Yours and sharing is separate.

## Decisions

- The UI word is "personas" / "your personas"; "template" stays in the code and the API route names, for compatibility. Because the audit found three names (library, template, Persona templates) for one list.
- Unshare exists for personas, unlike starters today. Because the audit found sharing one-way, and rooms keep their copies either way.
- Saving from a room uses the saved state, not the form's unsaved edits. Because a library version must match a state the room actually ran.
- A skill the saver cannot read as a personal skill is frozen as the room copy. Because the room copy is what the persona runs with.
- The time keeper role is copied only into a room without one. Because 032 allows one per room.
- The "+" on the persona bar ships here, not in the quick-wins spec, because it opens this picker.
- `/admin` does not list private personas. Because they are private, as personal starters are.
- A library persona version keeps the six persona columns and holds every other setting, with its frozen skills and plugins, in one manifest in the starter persona shape, its blobs in `persona_template_files`. Because the starter shape already carries every setting and its readers exist.
- Built-in versions saved before 035 get their description and settings in place, not as a new version. Because a new version would offer every room an "Update to vN" on upgrade.
- "Update to vN" keeps 002's scope (text, model, effort). Because skills and plugins on update would need the add flow's host approvals for a change nobody asked for.
- Adding a library persona gives the notes watcher role, like the time keeper, only to a room without one and never to a copy held for the host. Because taking the role from another persona silently, or before the host approves, would leave the room without a watcher on a refusal.
- Adding or updating from a library persona the caller cannot see answers "not found". Because a private persona's id must not let another profile copy it.
- "Save to my personas" is refused while the persona or a change to it waits for the host. Because the saved state must be one the room ran.
- Saving finds the caller's library persona saved from this room persona when the link points to someone else's. Because two profiles saving the same room persona in turn must not give either a second library persona (AC13).
- The library editor shows a persona's frozen skills and plugins read-only. Because they are copies of a room's, and saving from a room is where they change.
- A library persona's title follows its name whenever a version renames it; a duplicate takes the persona's name as its title, or "<name> (copy)", then "<name> (copy) 2"…, while Yours has one of that title. Because the list must tell copies apart and still read like the persona.
- Library edits carry the version they started from, and a stale one is refused. Because two tabs must not silently overwrite each other.
- Adding a library persona brings its plugins only when a room admin adds it; a member's copy arrives without them, and the chat says so. Because 009 keeps adding plugins a room admin's call.
- A same-name room plugin is reused only while it is waiting, inspected or approved. Because a refused one would never pass the host.
- The host's approval line no longer says the skill came with a starter. Because a library persona brings skills the same way, and telling them apart needs a column.
- Skills a library persona brings skip the skills disclaimer (002-AC32), as a starter's do. Because the AI host approves them before they run, and the add is one action for many skills.
- At 12 personas the bar's "+" still opens the picker, where "Add" is disabled with "A room holds 12 personas at most." Because the bar has no room for the reason as text, and a control announced as unavailable must not act.
