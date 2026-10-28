# 024: Room projects and personal tags

**Status**: approved
**Requested by**: babs
**Date**: 2026-10-27
**Context**: builds on 004 (room admins, export), 011 and 011b (`/profile` Rooms tab, profile merge), 012 (MCP `list_rooms`). Siblings: 023 (room links), 025 (search).

## Problem

`/profile` › Rooms lists every room in one flat list. Someone in thirty rooms across four projects scrolls and reads names to find the one they want. A room's name is the only thing that says what it belongs to, and everyone sorts their rooms differently.

## Solution

A project groups rooms under one name. A room admin puts their room in a project, a new one or one of the projects their rooms already use, from Room settings. Every member of the room sees its project. Renaming a project renames it for all its rooms at once. `/profile` › Rooms groups rooms by project, rooms without one last. Each person can also put their own tags on any of their rooms, seen by nobody else, and filter the list by tag.

## Scope

- Projects: create, rename, delete; a room in at most one project, set, moved or cleared by a room admin.
- The project shown in the room header and settings.
- Personal tags per profile and room.
- `/profile` › Rooms: grouped by project, tag chips to filter, a sort by last activity or name.
- Export and import carry the project, never tags.
- Profile merge moves tags. MCP `list_rooms` returns both.

## Out of scope

- A project page, project members, or a project role of its own: room admins manage it (Q5).
- Rooms of a project the caller has no seat in: `/profile` never lists them, nor counts them.
- Links implied by a shared project: rooms of one project read nothing of each other (023 does that).
- Tags shared with other members, or set by a room admin for everyone.
- A projects list or page in `/admin`; tags in `/admin`.
- Tag colours, tag renaming across rooms in one action.

## Acceptance criteria

- [ ] **AC1** Room settings › General has a "Project" picker, editable by the admin and the room's room admins, read-only for other members. It lists the projects of the rooms where the editor has a seat, by name with "(N rooms)" counting the rooms the editor sits in, plus "New project…" (name 1 to 60 characters, one line, trimmed) and "No project". Saving posts a system line in the room: "Ana moved this room to Pricing 2027" or "… took this room out of Pricing".
- [ ] **AC2** Project names are not unique. Two projects with the same name stay two groups; the picker shows each with its room count, and "New project…" with a name the editor already sees asks "Use the existing Pricing instead?".
- [ ] **AC3** The room header shows the project's name before the room name, small and muted, when the room has one.
- [ ] **AC4** `/profile` › Rooms groups rooms under their project, groups sorted by name, case-insensitive; rooms without a project come last under "No project". Inside a group, rooms sort by last activity, or by name when the "Sort" control says so; the choice persists in the browser.
- [ ] **AC5** Each room row's ⋯ › "Tags…" opens a sheet to add and remove the caller's tags on that room: 1 to 30 characters, one line, trimmed, case-insensitive unique per room, at most 20 per room. Suggestions are the caller's existing tags.
- [ ] **AC6** Tags show on the row as chips. Above the list, the caller's tags are chips; picking one or several filters to rooms carrying all of them; the filter persists in the URL hash (`/profile#rooms?tag=legal`).
- [ ] **AC7** Tags are the profile's only: no route returns another profile's tags, no room page, export or audit entry shows them. Any end of the profile's last seat in a room deletes its tags on that room in the same transaction: leaving, being removed (016), or losing the seat when an invite chain is cut (016-AC4). Joining again starts with none.
- [ ] **AC8** Export carries the room's project name. Import puts the room in a project with that name among those the importer sees (AC1), the one with most of the importer's rooms, else in a new project (004). An import by the admin (no importing profile) always creates a new project. Tags are not exported.
- [ ] **AC9** A profile merge (011-AC11) moves B's tags to A, merging duplicates per room; the merge preview counts them as `room_tags`.
- [ ] **AC10** MCP `list_rooms` returns `project` (`id`, `name`, or null) and the caller's `tags` per room; `create_room` accepts an optional `project` id among those the caller sees. `GET /api/profile` carries the same. New MCP tools: `rename_project` (full) and `delete_project` (full, `confirm` equal to the project's name, 012-AC20).
- [ ] **AC11** REST: `PATCH /api/rooms/{room}` accepts `project_id` (an id the caller sees, or null) or `new_project` (a name), with the rights it has for `name` (admin or room admin, 403 for another participant); `PATCH /api/projects/{id}` with `name` and `DELETE /api/projects/{id}`, for a room admin of one of its rooms or the admin (AC16), else 403 "Only a room admin of one of this project's rooms can do this"; `PUT /api/profile/rooms/{room}/tags` with `{"tags": [...]}` replaces the caller's tags on that room (403 for a room the profile is not in); `GET /api/profile/tags` lists the caller's tags with counts.
- [ ] **AC12** A room admin of any room in a project renames it from Room settings › General › Project › ⋯ › "Rename project…" (1 to 60 characters). The new name shows at once in every room of the project and every member's `/profile`. Each room of the project gets a system line "Ana renamed the project Pricing to Pricing 2027".
- [ ] **AC13** The same people delete a project from ⋯ › "Delete project…", with a sheet naming the number of rooms in it ("4 rooms, 2 you are not in, leave the project; nothing in them is deleted"). Its rooms then have no project, and each gets the line "Ana deleted the project Pricing".
- [ ] **AC14** A project with no room left, after its last room moved out or was deleted, is deleted in the same transaction.
- [ ] **AC15** Every project change writes one audit entry in the same transaction (004-AC6), actor the profile or the admin, `api_token` when done with a token: `project.attach` (a room put in a project, new one included) and `project.detach` (a room taken out, or moved, which writes a detach then an attach), target the room, details with the project id and name; `project.rename` (old and new name) and `project.delete` (name, room count, the rooms' ids), target the project. A project deleted by AC14 writes `project.delete` with actor `system` and the cause. A room deleted with the room delete is not a separate detach: the room delete entry carries the project id. The `/admin` audit view names the four actions and filters on a project id.
- [ ] **AC16** The admin (admin token or `/admin` session) acts on any room as a room admin does: in a room's admin view, the picker lists "No project", "New project…" and the room's current project, so the admin never files a room under a project it did not hold; renaming and deleting a project work from any of its rooms. Their audit entries carry actor `super-admin`.

## Phases

### Phase 1: projects
- Work: table, routes, picker, rename and delete sheets, header, system lines, audit, orphan cleanup, export and import, MCP and `GET /api/profile` fields, admin rights. `DeleteRoom` runs in a transaction that records the project id in the room delete's audit entry and deletes a project left empty (AC14, AC15). `projects` joins `serverWide` in the cascade test: no room owns it, AC14 deletes it.
- **Covers**: AC1, AC2, AC3, AC8, AC10 (project), AC11 (project), AC12, AC13, AC14, AC15, AC16
- **Data model impact**: new `projects` (`id`, `name`, `created_by_profile_id ON DELETE SET NULL`, `created_at`); `rooms.project_id REFERENCES projects(id) ON DELETE SET NULL` with an index; one migration.
- **DoD**: store and route tests: a non-admin gets 403 on move, rename and delete; a room admin of one room renames the project and every room shows the new name; the last room moving out deletes the project; the picker never lists a project the caller sits in no room of; export round-trip lands in the right project; e2e: two rooms in one project, rename from one, the other's header and `/profile` follow.

### Phase 2: tags and the grouped list
- Work: table, routes, tags sheet, chips, filter, grouping, sort, merge, purge on every seat end (`membership.go` leave and remove, the invite chain cut in `store.go`).
- **Covers**: AC4, AC5, AC6, AC7, AC9, AC10 (tags), AC11 (tags)
- **Data model impact**: new `room_tags` (`profile_id` cascade, `room_id` cascade, `tag`, unique index on `(profile_id, room_id, lower(tag))`); same migration as phase 1 if unreleased.
- **DoD**: store tests: another profile's tags never returned, cap 20, merge dedupe; e2e: tag two rooms, filter shows them, another member's `/profile` shows no tag; phone layout shows chips under the name.

## Data model impact (summary)

- `projects` (new table), `rooms.project_id` (new column), `room_tags` (new table). One migration, numbered at merge time after 56.

## Open questions

- None blocking.

## Decisions

- Q4 (user, 2026-10-27): the project is shared by the room; tags are personal per profile. "Creator or admin" maps to room admin: the creator is the first one, and a creator who stepped down lost the right.
- The room-wide label is a project (user, 2026-10-27), and a project is an entity, so one rename regroups every room (user, 2026-10-28).
- Q5 (user, 2026-10-28): any room admin of a room in the project renames or deletes it; there is no project role.
- Q6 (user, 2026-10-28): a room admin alone puts their room in a project they see. A project carries no access, so the worst case is a room filed under someone else's project name, visible only to that room's members.
- A project is "seen" through a seat in one of its rooms. Nobody browses projects; that is what keeps a name from leaking to people outside its rooms.
- A project links nothing: reading another room stays a 023 link, which needs its own consent.
- Tags drop when the profile leaves the room. Keeping tags on rooms the profile no longer sees would leak nothing but would clutter a list that hides those rooms.
- Q5 and Q6 together let a room admin of any room join a project they see, then rename or delete it for every room in it. Accepted: neither touches room content, delete only ungroups, and the system line in each room plus the audit entry name who did it.
