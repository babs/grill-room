# 038 — Quick wins: dead ends, wording, places

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-14
**Context**: UX audit part B, quick wins Q1, Q3-Q5, Q8-Q17 (Q2, Q6 and Q7 are in 035). Part A's quick wins (layout and accessibility) ship apart; this spec does not touch the Personas sheet (035) or the announcement and layout specs (036, 037). Builds on 005 (room menu), 006 (starters), 009 (skills, plugins), 011 (invites, admins), 012 (API tokens), 020 (chat filter), 023 (links, proposals), 028 (text size).

## Problem

The audit walked 12 journeys and found dead ends that cost no new concept to fix:
- A toast in the AI host app says the opposite of what the setting does.
- Hints name a place with no link to it.
- A member brought to a proposed room cannot get back to the room it came from.
- A sole admin on a phone cannot find how to leave.
- A phone offers a Linux download and a link to a page on the phone's own loopback.
- A PM who speaks on a phone sees nothing in the chat, because the filter hides voice lines without saying so.

One concept goes by up to eight names (the AI host). Some settings sit far from their effect: text size and notifications are stored on the profile but absent from `/profile`.

## Solution

Small changes, each in place, with no new screen:
- Correct the toast and the label.
- Turn every hint that names a place into a link.
- Add the missing actions where the user already looks: start a room from a starter, open the proposed room, leave from the avatar menu, link a room from the room menu.
- Tell a phone user what running the AI needs and give them a message to send.
- Say when the chat filter hides lines.
- Make one wording pass so each concept has one name.
- List text size and notifications in `/profile`.

## Scope

- AI host app: the approval label and toast.
- Room: linked hints (Capabilities, Room settings › Skills, starter saved toast), chat filter hint, room menu entries (Linked rooms at zero, Leave room in the avatar menu), host sheet on a phone, offline host hand-off message, proposal follow-through.
- Room settings: tab names and the place of "Blocked from rejoining".
- `/profile`: Starters row action, API tokens wording, text size and notifications in Account.
- One vocabulary across the room, `/profile`, `/admin`, the AI host app, README and docs.

## Out of scope

- Persona provenance, "Save to the library", the "+" on the persona bar: 035.
- Screen reader announcements (036), phone and tablet layout (037), part A's accessibility batch.
- Splitting Room settings, AI readiness panel, room switcher, voice sheet, help: later specs (039-042).
- Search (025).
- Renaming API routes, MCP tool names, CSS ids or e2e selectors beyond what a visible text change forces.

## Acceptance criteria

### AI host

- [ ] **AC1** The AI host app's General setting reads "Hold new AI personas and changes for my approval". Ticking it toasts "New AI personas and changes wait for your approval"; unticking toasts "New AI personas and changes apply at once". A test pins the toast to the stored `approve_all` value.
- [ ] **AC2** On a phone or tablet, meaning Android, iOS or iPadOS (an iPad that reports a Mac platform with touch points counts), the host sheet reads "Running the AI needs a computer with Docker." It offers no download and no "Open my AI host app", and shows the hand-off of AC3. The room menu hides "Open my AI host app" there.
- [ ] **AC3** Wherever the room says no one runs the AI (offline pill tip, host sheet, ▶ on an offline pill), an "Ask someone to run it" action copies a message: "Can you run the AI for our grill-room room? Open <room link>, then click the room's name › Run the AI on my computer." The action answers "Message copied.".

### Links that were text

- [ ] **AC4** Empty Capabilities reads "Add a skill on your profile page" with "your profile page" linking to `/profile#skills`; its plugin hint links "Room settings › Plugins" to `#manage/plugins`. Empty Room settings › Skills links to the Personas sheet. The "Saved "<title>" to your starters" toast carries an "Open" link to `/profile#starters`.
- [ ] **AC5** A starter row's ⋯ on `/profile` › Starters offers "Create a room from this" first. It opens the new-room sheet with that starter picked.

### Proposals and links

- [ ] **AC6** After Create on a proposal card, the sheet shows "Room <name> created" with "Open the room" and "Stay here". The card then links to the room, as today.
- [ ] **AC7** Every member of a room created from a proposal sees "Proposed in <room>" in the room menu, linked when they are a member of that room and plain text otherwise. It no longer appears only in Room settings › General.
- [ ] **AC8** The room menu always shows "Linked rooms": with links, "Linked rooms (N)" as today; with none, room admins see "Link a room…" (opens `#manage/links`) and plain members see nothing. "Link a room" with no other room to link reads "You are a room admin of no other room. Create one first." with a link to `/profile#rooms`.

### Membership

- [ ] **AC9** Room settings tabs: "Admins" becomes "Members and admins", "Documents" becomes "Deleted documents". "Blocked from rejoining" moves from Invites to Members and admins, under the member list. `#manage/admins` and `#manage/documents` keep working.
- [ ] **AC10** The avatar menu has "Leave room…". For a room admin it is disabled with the reason as visible text, "Step down as room admin first", and a "Step down" link to `#manage/admins`. For the last room admin the reason reads "Make someone else room admin, then step down." The People ⋯ entry stays.

### Chat filter

- [ ] **AC11** When the chat filter hides lines that exist (voice lines under Written, written under Voice), a one-line hint under the filter says "N voice lines hidden" with "Show all" (a view for this visit, 020's viewFilter). The hint goes once nothing is hidden.
- [ ] **AC12** Room settings tabs and settings that depend on a server engine (Words to recognise, Call voices, a persona's voice) show "Not available on this server" in place of vanishing, at the place they would be.

### Profile

- [ ] **AC13** `/profile` › "API tokens" becomes "API tokens and Claude Code". The New token form says what each scope allows: "Read only: Claude Code can read your rooms, documents and transcripts." and "Full: it can also create rooms, add personas, link rooms and change settings." The creation result says "This command shows once. Lost it? Make a new token and revoke this one."
- [ ] **AC14** `/profile` › Account lists "Text size" (chat, Transcription, editable there, same values as the room's sheet) and "Notifications" (state per this browser, with the room's turn-on flow). A change on either side shows on the other after a reload.

### Words

- [ ] **AC15** One vocabulary, applied to visible text, `aria-label`s, tips, README and docs:

  | Concept | Word | Replaces |
  |---|---|---|
  | Who runs the AI | AI host; its app is "the AI host app" | daemon, agent daemon, grill-room-daemon (except the download filename), "holds the agent(s)", "its page" |
  | Document notes | note | annotation, Annotate, Comment on the document |
  | The audio room | call | Join audio, Leave audio (become Join the call, Leave the call) |
  | Stop hearing others | Stop hearing the call | Mute the room, Room muted, deafened |
  | Who sees a shared thing | Shared with everyone on this server | Everyone on this server, Shared with everyone (alone) |

  A grep test over `internal/web/ui`, `internal/daemon/ui`, README and `docs/` fails on any replaced word in visible text.
- [ ] **AC16** README's room menu section names the room name as the menu, not "`⋯` menu". The AI host app's Activity hint says "for the days set on General".

## Phases

### Phase 1 — Dead ends and links
- Work: approval label and toast; phone host sheet and hand-off; linked hints; starter row action; proposal follow-through; Linked rooms entry; Leave in the avatar menu; chat filter hint; "not available" placeholders.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC10, AC11, AC12
- **Data model impact**: none. AC7 reads `proposed_in`, which the room payload already carries.
- **DoD**: `go test -race ./internal/web/... ./internal/daemon/...` green; e2e (`--workers=1`) for the toast value, the phone host sheet at 390 px, the proposal "Open the room", a member seeing "Proposed in", a sole admin's Leave reason, the filter hint; `make lint` green.

### Phase 2 — Names and places
- Work: Room settings tab names and Blocked section; API tokens wording; text size and notifications in `/profile`; vocabulary pass with its grep test; README and docs.
- **Covers**: AC9, AC13, AC14, AC15, AC16
- **Data model impact**: none
- **DoD**: the vocabulary grep test goes red on each replaced word put back in one UI file, one daemon file and README; e2e selectors updated; old deep links tested; captures at 390 and 1440 px of Room settings and `/profile` › Account.

## Data model impact (summary)

None.

## Open questions

- [ ] None blocking.

## Decisions

- Phone detection is by OS (Android, iOS, iPadOS), not by pointer. Because a touch laptop can run the AI host and a pointer test would hide the download from it.
- The hand-off message carries the room link, not an invite. Because whoever runs the AI pairs through the room link and a member approves them (005), and a plain member may not make invites.
- "Stop hearing the call", not "Deafen": the audit found "Mute the room" read as silencing everyone.
- The longer tab names depend on part A's tab fold on overflow (A10) at 820 px. If A10 has not shipped, phase 2 waits for it.
- AC2: the room link field and its Copy button hide on a phone with the "its page" section: the hand-off message carries the link. Diagnostics' "Open my AI host app" hides there too. Known limit: an Android tablet in Chrome's desktop mode sends a Linux user agent, on UA-CH too, and sees the computer sheet.
- AC3: the pill's tip is a plain tooltip and holds no button, so "Ask someone to run it" sits in the pill's card. The card and the ▶ dialog offer it only when the room has no AI host at all; a known host whose app is away is named, and members ask that host. The host sheet always offers it. A failed copy shows the message to copy by hand.
- AC4: a toast holds one action, a button ("Undo") or a link ("Open").
- AC5: "Create a room from this" opens the new-room sheet in that starter's editor (006-AC9); Back returns to the picker.
