# 004 — Instance ops: admin UI, room admins, export/import, scenarios

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-11
**Context**: specs 001-003, grill notes (004)

## Problem

Running an instance means `curl` with a shared admin token: no overview of rooms, volumes, profiles or libraries, and no audit trail. Only the token holder can create a room, so nobody else can start a grill on their own. A room cannot leave its instance: there is no backup and no move. Its skills belong to people, not to the room, so they vanish when their owner leaves. Testing a whole session again means hand-made Playwright scripts with spike-specific audio.

## Solution

An instance gets an admin UI at `/admin`. One login with the admin token opens a revocable session, and the UI shows rooms with their volume and quota, profiles, libraries, instance health and an audit log. Any profile can create rooms within limits the super-admin sets, and becomes that room's admin. A room admin manages the room from the room page: rename, invites, other room admins, export, delete. Any room exports to a signed-off archive and imports on the same or another instance. On the same instance, participants are relinked to their profiles. Skills gain a room scope, so a room's skills travel with it. A scenario tool replays scripted sessions with synthetic voices, for demos and release checks.

## Scope

- Super-admin UI `/admin`:
  - admin-token login with a session cookie (HttpOnly, SameSite=Strict, 12 h, listed and revocable);
  - rooms (list, volume breakdown, quota, actions);
  - import with preview;
  - profiles (emails visible here only);
  - persona template and shared skill libraries;
  - instance health;
  - audit log (1 year).
- Room admins: creator plus delegates, tied to profiles. Rights: rename, all invites, appoint and remove room admins, export, delete, volume. Quota stays with the super-admin.
- Room creation by any profile, with instance guardrails: rooms per profile (default 10), an instance storage quota, and a "super-admin only" switch.
- Room skill scope: owned by the room and deleted with it. Members copy personal skills into the room, traced and held under "approve everything". Import lands embedded skills there.
- Export, a versioned tar.gz:
  - `room.json`;
  - messages as JSON plus readable Markdown, with versions and deletions;
  - documents, revisions and uploads (originals plus pandoc copies);
  - annotations, threads and attachments;
  - room personas;
  - room skills, plus the pinned shared and personal versions embedded;
  - agent sessions;
  - participants as display name plus profile uuid;
  - a sha256 manifest with the format and instance versions.

  An export warns that agent sessions hold everything Claude read.
- Import: a new room with new ids, and no invites ever revived.
  - Same instance: participants relinked to their profiles by uuid.
  - Another instance: the uuid is kept as metadata.
  - The manifest is verified, quotas are enforced, and too-large or newer-format archives are refused with a reason.
- Scenarios: a `scenarios/` tool (Python, uv) that reads YAML scenarios, generates Piper audio (not committed), runs Playwright against a room and asserts outcomes.
  - Library: the dark-mode grill, a two-persona chain, a long session, a host hand-over.
  - Uses: demos, lab non-regression before releases (the Claude request budget is shown before running), and tuning spec 003 thresholds.
  - Voice licenses: CC0, CC-BY or public domain, listed.
- New UIs in Preact + htm + signals (vendored, no build), typed via JSDoc and `tsc --checkJs` in CI.

## Out of scope

- Retention, archiving and scheduled exports.
- OIDC for admins.
- Federation of profiles across instances.
- Merging an import into an existing room.
- Migrating `room.js` beyond the panels this spec touches.

## Acceptance criteria

### Super-admin
- [ ] **AC1** `/admin` asks for the admin token once, then works on a session cookie (HttpOnly, SameSite=Strict, 12 h). The page lists the open admin sessions and revokes any of them; a revoked session gets a 401 on its next request.
- [ ] **AC2** The rooms view lists every room with:
  - name, creation date, last activity, participants and personas;
  - whether the host is online;
  - its volume broken down into uploads, revisions, attachments, agent sessions, transcripts and room skills, against its quota.

  The super-admin can rename, manage invites, change the quota, export and delete. Delete asks for the room name to be typed.
- [ ] **AC3** The profiles view lists display name, email, rooms, skill count and last activity. Deleting a profile detaches its participants. Emails appear nowhere else.
- [ ] **AC4** The libraries view lists persona templates and shared skills with their author and versions, and removes an entry.
- [ ] **AC5** The instance view shows version, disk used and free, STT status, connected daemons and SMTP status.
- [ ] **AC6** Every admin action, super-admin and room-admin alike, is written to an audit log: who, what, when, and which room or profile. The UI shows it read-only. Entries older than 1 year are purged.

### Room admins and creation
- [ ] **AC7** Any profile can create a room from its profile page and becomes its room admin, unless the instance is set to "super-admin only". It is refused beyond the rooms-per-profile limit (default 10) or the instance storage quota. The super-admin can override the limit for one profile (a number, unlimited, or back to the instance default).
- [ ] **AC8** A room admin can, from the room page:
  - rename the room;
  - manage every invite;
  - appoint and remove room admins;
  - export the room;
  - delete it, by typing its name;
  - see its volume.

  A plain member can do none of these, apart from the invite rights spec 002 already gives members.
- [ ] **AC9** The room quota can only be changed by the super-admin.

### Room skills
- [ ] **AC10** A room has its own skills: owned by the room, visible to its members, and deleted with it. A member can copy one of their personal skills into the room. The copy shows as a chat line, and waits for the host under "approve everything". The copy keeps working after its author leaves the room.

### Export and import
- [ ] **AC11** An export contains everything listed in Scope, with a manifest of sha256 per file plus the format version and instance version. Participants carry their display name and profile uuid, never an email or token. The export UI warns that agent sessions hold everything Claude read.
- [ ] **AC12** Importing an export on the same instance gives a new room with new ids whose content matches the original:
  - messages with versions and deletions;
  - documents and revisions;
  - annotations anchored as before;
  - personas;
  - skills;
  - the agent session resumable by the next turn.

  Its participants are relinked to their existing profiles, and no old invite works.
- [ ] **AC13** Importing on another instance (the lab from the local compose stack, or the reverse) gives the same content. Embedded skills land as room skills, and participants have no profile link but keep their uuid as metadata.
- [ ] **AC14** An import with a sha256 mismatch, a format newer than the instance supports, or a size above the target quota is refused with a reason, and nothing is created.
- [ ] **AC15** The import UI previews an archive (room name, counts, size, format and instance versions) before creating anything.

### Scenarios
- [ ] **AC16** `scenarios/` runs a YAML scenario against a room URL. It generates per-speaker Piper audio, plays it through Playwright fake microphones, performs the scripted actions, and asserts:
  - voice lines and their speakers;
  - syntheses and the chain count;
  - latency bounds.

  The output is a report.
- [ ] **AC17** The repo ships four scenarios: the dark-mode grill, a two-persona chain, a long session (context and transcript chunks) and a host hand-over. Each runs green against the lab, and prints its Claude request budget before starting.
- [ ] **AC18** Every Piper voice used is listed with its license, and only CC0, CC-BY or public-domain voices are allowed.

### UI stack
- [ ] **AC19** `/admin` and the room-admin panels are written in Preact + htm + @preact/signals, vendored as ESM with no build step, and CI runs `tsc --checkJs` on them. They stay usable at phone width.

## Phases

### Phase 1 — UI stack and admin sessions
- Work: vendor Preact, htm and signals; add `tsc --checkJs` to CI; admin login, sessions and revocation; audit log store and view.
- **Covers**: AC1, AC6, AC19
- **Data model impact**: `admin_sessions`, `audit_log`
- **DoD**: Playwright login, revoke and audit view; `tsc --checkJs` green in CI.

### Phase 2 — Super-admin views
- Work: rooms (volume breakdown, quota, actions), profiles, libraries, instance health.
- **Covers**: AC2, AC3, AC4, AC5, AC9
- **Data model impact**: `rooms.quota_bytes` (if not already per room)
- **DoD**: e2e for each view and action, at phone width; volume numbers checked against the store.

### Phase 3 — Room creation, room admins, room skills
- Work: create from the profile page, guardrails, room admin role and delegation, room skill scope and copy-in.
- **Covers**: AC7, AC8, AC10
- **Data model impact**: `room_admins`; `skills.scope` gains `room` (with `room_id`); instance settings for limits
- **DoD**: e2e for creation limits, delegation, and a room skill that survives its author leaving; cascade and purge invariants extended.

### Phase 4 — Export and import
- Work: export writer, manifest, warning; import with preview, verification, quotas, relinking by uuid, embedded skills as room skills.
- **Covers**: AC11, AC12, AC13, AC14, AC15
- **Data model impact**: `participants.profile_uuid_hint` (metadata on cross-instance imports)
- **DoD**: round-trip test on the same instance (content diff empty apart from ids); cross-instance import between compose and the lab; refusal tests (tampered, newer format, oversize); the next turn resumes the imported session (fake claude locally, one haiku on the lab).

### Phase 5 — Scenarios
- Work: `scenarios/` tool, YAML schema, the four scenarios, voice license list, a lab run.
- **Covers**: AC16, AC17, AC18
- **Data model impact**: none
- **DoD**: all four scenarios green on the lab, within at most 30 real Claude requests in total (haiku, apart from one Opus run of the chain scenario).

## Data model impact (summary)

- New tables: `admin_sessions`, `audit_log`, `room_admins`.
- `skills` scope `room` with `room_id`.
- `rooms.quota_bytes` if absent.
- `participants.profile_uuid_hint`.
- Instance settings: rooms per profile, storage quota, creation mode.

## Personal data

- The audit log records admin actions with the acting profile or "super-admin". It is kept 1 year.
- Exports carry profile uuids and display names, not emails. The uuid lets a same-instance import relink people; across instances it is only metadata.
- An export contains agent sessions: whoever holds the archive can read everything the agents read. The UI warns before export.

## Open questions

- (none)

## Decisions

- Two admin levels: super-admin by admin token, and room admins tied to profiles (grill Q1).
- Export is complete, sessions included (decided in 002, confirmed); participant identity uses profile uuids (grill Q2).
- Skills gain a room scope (grill Q3).
- Any profile creates rooms, with instance guardrails (grill Q6).
- Preact + htm + signals for new UIs, no build (grill 004 UI).
- The import format accepts the current version and older ones it knows; newer formats are refused. Routine call, overturnable.
