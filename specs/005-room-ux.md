# 005 — Room UX from live testing

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-12
**Context**: live tests of specs 001-004 on the lab, from a phone and a desktop

## Problem

Live testing on the lab showed a room that leaves people guessing:
- A name chosen at join cannot be changed.
- A `@persona` mention that triggers nothing says nothing, and a mention that does trigger shows no sign of it.
- A running turn cannot be stopped.
- Nobody can tell who is in the room, who is in the audio, or whose voice is transcribed. The header says "Transcribing" to someone who left the audio.
- A synthesis header can name the wrong model after a model change, and "default" model and effort mean nothing to anyone but the host.
- The documents can only be read inside the room page.
- A background tab gives no sign of new messages.
- Modals ignore clicks outside them.
- A UX audit (2026-10-13, 29 findings F01 to F29) found the six UIs share no control scale, no colours and no wording: 7 button heights in the room header alone, a native file box as the only way to upload, white on teal at 3.30:1, and words like "Chain 0/25" or "holds the agent" a product manager cannot read.

## Solution

People rename themselves in the room. `@` autocompletes personas and people. A mention that triggers a persona shows its turn's state on the message; one that cannot trigger tells its author why. Whoever started a turn, the host or a room admin can stop a running turn, and nothing of it is applied. The participant list shows who is here, who is in the audio, who is speaking and whose voice is transcribed, live and reliably. The room label counts transcribed voices instead of a misleading "Transcribing". Personas always carry an explicit model and effort, shown in full, and the synthesis header names the model the turn really ran on. The document manager opens in its own window. A background tab shows an unread count in its title. Modals close on a click outside or Escape.

## Scope

- Self-rename in the room (no moderation by others in this spec).
- `@` autocompletion in the chat.
- Mention feedback and trigger state.
- Stopping a running turn.
- Presence: server-side state, events, heartbeat with grace.
- Participant list sections and audio and capture indicators.
- Room transcription label.
- Explicit model and effort on personas, plus the correct model label.
- Document manager in its own window.
- Unread count in the tab title, and Web Push notifications when no tab is focused.
- A per-room STT glossary.
- Modal dismissal.
- Renaming documents, and soft-deleting them for room admins.
- A design system shared by the six UIs (`docs/design.md`), and the audit's fixes: upload, header, wording, touch targets, phone layout, profile, `/admin` and the daemon page.

## Out of scope

- Removing (kicking) a single participant.
- WebSockets: SSE stays.
- Typing indicators.

## Acceptance criteria

### Identity
- [x] **AC1** A participant can change their own display name from the room page, with the same validation as at join. A system line says "<old> is now <new>". Past messages show the new name with the old one on hover. An option also applies it to their profile. The presence preamble keeps the same short uuid across the rename.

### Mentions and turns
- [x] **AC2** Typing `@` in the message box suggests the room's personas first, then participants, filtered as you type. It works with keyboard and touch.
- [x] **AC3** A message mentioning a persona that cannot run gets a notice visible only to its author, with the reason: no right to trigger agents, no host online, or unknown persona. A human mention resets the chain (002-AC26), so it never meets a paused chain.
- [x] **AC4** A message whose mention queued a turn shows "→ @Persona" with that turn's live state (queued, running, done, failed, stopped), linked to the synthesis. The link between the message and its turn is stored, so every message keeps its chip after a reload, however old, and an export carries it (the export format moves to 2; format 1 still imports).
- [x] **AC27** In the chat, `@Persona` shows as a chip with the persona's badge and colour, and `@Person` as a chip with that person's name. Only a persona or member of the room makes a chip; any other `@name` stays plain text. The match is case-insensitive and is the server's (`mentionRe`): the server names the mentions of each line, and the page only draws them. A line mentioning the viewer gets a highlighted background and an accent bar, and counts as a mention for the unread title (AC12) and for push (AC24). Chips are reachable by keyboard and read by screen readers, and the text still copies as `@Name`. Chips are DOM nodes; the line's text stays escaped.
- [x] **AC5** Whoever started a turn, the host or a room admin can stop it while it runs. The daemon kills `claude` in the sandbox. The turn ends as "stopped" with nothing applied: no revision, no synthesis, no replies. The persona's next turn resumes from the server's session copy, and a stopped turn is not a chain link.

### Presence and audio
- [x] **AC6** The server keeps each participant's presence from their open streams and a heartbeat, with a grace period of about 20 s, so reloads, background tabs and several tabs do not flap. It sends `presence` events (here, gone, audio on or off, mic on or off) and replays the full state when a stream opens. Tests cover a flapping connection, a backgrounded phone, and two tabs with one closed.
- [x] **AC7** The participant list shows "In the room", then "Not here" collapsed, with "seen <time> ago". Each person shows their audio state: mic live, muted, listening only, not in audio. It also shows the speaking pulse, and a "transcribed" badge when the STT worker is subscribed to that person's track.
- [x] **AC14** The participant list marks roles with a badge, and each badge has a tooltip: room admin, host (holds the agents), and "you".
- [x] **AC18** When LiveKit closes an empty room while the STT worker is in it, the worker drops that room and joins again when people come back to the audio, including when the new LiveKit room has the same name. Lines resume without restarting the worker.
- [x] **AC19** Every event line in the chat shows its time, as messages already do: joins, leaves, renames, persona changes, and turns started, stopped, done or failed.
- [x] **AC20** `/admin` shows, for each participant, the client IP and user agent at join and at last activity. Only the super-admin sees them; they are kept 30 days by default (configurable) and purged with the room.

  The client IP comes from `X-Forwarded-For` ONLY when the direct peer is in a configured trusted-proxy list (`GRILL_ROOM_TRUSTED_PROXIES`, CIDRs). The address taken is the right-most one not in that list. With no list configured, the peer address is used. A header sent by an untrusted peer is ignored, so it cannot spoof the IP. The same rule replaces 004's "any private peer is trusted" for the admin login rate limit.
- [x] **AC21** A room admin (in the room's Manage panel) and the super-admin (in `/admin`) can rename any participant. A system line says who renamed whom, the audit log records it, and the renamed person sees a notice.
- [x] **AC22** Each participant can turn on "deafen", which stops receiving the other participants' audio. As in Discord, deafen also mutes their mic by default. An option "keep my mic on while deafened" (useful when people share a physical room) keeps the voice published, and so transcribed. Undeafening restores the mic state from before. The list shows a deafened icon, and the state survives a reconnect within the session.
- [x] **AC23** Each room has an STT glossary, edited by any member in Manage, with a line in the journal. Every change applies from the next utterance. The worker passes it to faster-whisper as a natural sentence in `initial_prompt`, never as `hotwords` (spike S10), only when `APP_GLOSSARY_PROMPT=on`; it is off by default (spike S10b, Decisions). Manage says the glossary is used by the transcription engine when supported. The prompt stays within Whisper's ~224-token limit. When the terms do not all fit, they are taken in this order: the edited glossary, then participant and persona names, then document titles. Manage shows "N / ~70 terms used". A Piper scenario with technical terms checks term accuracy against `small` without a glossary.
- [x] **AC8** The room no longer shows "Transcribing" as if it applied to the viewer. It shows how many voices are transcribed ("2 voices transcribed"). A viewer whose voice is captured sees "your voice is being transcribed", and one who is not sees nothing implying capture.

### Models
- [x] **AC9** Every persona has an explicit model (haiku, sonnet, opus, fable) and an explicit effort. "Default" is never offered or shown. Existing personas without a value migrate to sonnet / medium. Effort shows in full words ("effort medium").
- [x] **AC10** The synthesis header names the model the turn ran on: the requested one, checked against `claude -p`'s `modelUsage`. Any other model in `modelUsage` shows as secondary with its cost share, for example on a resumed session started with another model.

### Documents and tabs
- [x] **AC11** "Open in a new window" opens the whole document manager at `/r/<room>/docs`: list, upload, viewer, annotations, revisions and diff, all live. Deep links `/r/<room>/docs/<doc>` open one document. The room page and the docs window stay in sync.
- [x] **AC12** When the room tab is not focused, its title reads "(N) <room name>", where N counts new text messages from others and persona syntheses. Voice lines are counted only if the user opts in. The count resets when the tab regains focus.
- [ ] **AC24** A participant can turn on browser notifications for a room. They arrive through Web Push (VAPID; keys generated once and kept in the instance data), so they work in a background tab and with the tab closed. They are sent only when none of that participant's tabs is focused: presence (AC6) tells the server. The default triggers are:
  - a mention of the participant;
  - a persona synthesis;
  - a turn they started that ends.

  Other messages are opt-in. The payload carries only the room name, the kind of event and a link, never message text, because it passes through the browser vendor's push service. Repeated events are grouped into one notification per room. Clicking a notification opens the room's chat scrolled to the message that triggered it: the link is a deep link to that message. Expired subscriptions (410) are dropped, and subscriptions are purged with the participant or the room. On iOS, this works only once the room is added to the home screen; the UI says so.
- [x] **AC15** Room management (the "Manage" panel) opens as a wide sheet with tabs: General (name, volume), Invites, Admins, Personas, Skills, Documents (deleted ones, to restore: AC26), Export, Glossary. It is full screen on a phone. Every member opens Manage and sees only its Glossary tab (AC23); the other tabs are for room admins. Each tab has a deep link (`#manage/invites`, …), and the tabs are keyboard reachable.
- [x] **AC16** `/admin` uses the full page width with a tab bar: Rooms, Profiles, Libraries, Instance, Sessions, Audit. A room opens its own detail view with tabs (General, Invites, Admins, Export), following the AC15 layout. Every tab and room has a deep link, and phone width stays usable.
- [x] **AC17** The daemon's local admin page uses the full width with a tab bar that stays pinned at the top while scrolling: Instances, Rooms, Accounts, Settings, Monitoring, Update. Alerts show as a badge on their tab. Each tab has a deep link (`#accounts`, …).
- [x] **AC25** Any member can rename a document from the document manager. The document keeps its id, so its revisions, annotations and deep links survive. Its workspace path changes; the daemon's cache is keyed by sha256, so nothing is transferred again. The new name passes the same validation as an upload, keeps the extension, and no other document of the room may hold it, so paths stay unique. A timestamped chat line says "X renamed `a.md` → `b.md`". The personas' next turn tells them the old and new paths, so a resumed session does not keep the old one. An agent document is not renamed while a turn runs, as its result would recreate the old name.
- [x] **AC26** A room admin can delete a document, softly. It leaves the list, the workspace and the agents' view, and a chat line records it. Its data (revisions, annotations, original) stays and counts in the room's volume. A room admin restores it from Manage, unless another document took its name meanwhile. Only deleting the room purges it. An export carries deleted documents, flagged. As for a rename, an agent document is not deleted while a turn runs.
- [x] **AC13** Every modal (persona panel, the "Manage" room-admin panel, and the others) closes on a click outside it and on Escape, and keeps focus correctly for keyboard users.

### Design system and polish (UX audit, 2026-10-13)

Each AC names the audit findings it covers. `docs/design.md` holds the rules these ACs refer to.

- [ ] **AC28** One `tokens.css` holds the colour, type, spacing, size, radius, shadow and layer tokens and the component classes. Both binaries serve it at `/static/kit/tokens.css` from one Go package, and the room page, the docs window, join, profile, `/admin` and the daemon page link it before their own stylesheet. A page stylesheet holds no colour literal: every colour is a token, persona colours (user data) excepted. Dark tokens follow `prefers-color-scheme`, as the document frame already does, so a document matches the page around it. The six pages share one header: the chrome background, the logo and the title, 56 px high. (F21, F22)
- [ ] **AC29** Colours are harmonized from the mapping table in `docs/design.md`, and only the pairs that fail WCAG AA change: primary buttons and teal text move to `#0f7d6e`, input and button outlines get a 3:1 border, warning text moves to `#8a5a00`, the "reopened" badge gets ink text, and status dots get a ring. A test computes every text pair at 4.5:1 or more and every non-text pair at 3:1 or more, light and dark, from the tokens file. (F29, F28)
- [ ] **AC30** Every button, input, select, tab and summary uses one control height: 36 px on a desktop, 44 px under 40rem, with one control font size per scale. At 1440 px the room header's visible controls share one height. At 390 px every visible control of the room (as it opens, with a document open, and with Room settings open), the docs window, join, profile, `/admin` and the daemon page is at least 44 × 44 px. Links and chips inside a message's text are exempt, as WCAG 2.5.8 exempts inline targets. (F11)
- [ ] **AC31** The Documents header has a `+` button, "Add documents", that picks several files at once; the native file box is never shown. The whole documents pane, and the docs window, take dropped files, with an overlay that reads "Drop to add to the room". Each file gets a row with its progress, then ✓, or ✗ with a short reason (the server's, or "This file type isn't supported…") and Retry. A finished row clears after 5 s; a failed one stays until retried or dismissed. (F01, F02, F03)
- [ ] **AC32** The room header has three zones: the room's name, cut with an ellipsis; the personas, with the auto-turn counter shown only above 0 ("auto 3/25"); then the audio control, the user menu and a `⋯` menu, all at the control height, 8 px apart inside a group and 24 px between groups.
  - The user menu (initials) holds: Change my name, Notifications, Use on another device, My rooms and profile.
  - `⋯` holds: Invite people, Room settings, Personas, Open documents in a new window, Run the AI on my computer.
  - The audio control is "Join audio"; once joined, a mic toggle and a menu with Mute the room (deafen), Keep my mic on, the transcribed voices and Leave audio. "Voice-to-text is off" shows only there, so only to people in the audio.
  - A persona pill shows its state as a glyph with a ring (● ready, ◐ working, ○ offline, ⧗ queued, 🔒 not allowed) and, on a desktop, as a word.
  - The recovery link no longer opens as a panel on the first join. It stays in the user menu while this tab holds it, and one notice offers it after the first message. Once the tab no longer holds it, the menu says how to sign in elsewhere: an email bound from the profile page. (F04, F08, F10, F28)
- [ ] **AC33** The room's header, menus, sheets and side panel, Room settings and the docs window use the plain words of the wording table in `docs/design.md`: no "Chain", "host" alone, "holds the agent", "Manage", "Transcribing", "Deafen", "Upload a document", "Download original", "Volume". AC8's label reads "2 voices are being written down · yours is one of them". The event lines the server writes in the chat keep their words: past lines would otherwise read differently from new ones. The composer placeholder is "Message the room · @ to mention", and the Enter and Shift+Enter hint shows on the first focus. Room settings › General hides the room id behind "Copy room id", sums the volume as one line, "Storage: 37 B of 200 MB", and shows "Delete the room" as a danger outline before the name is typed. (F09, F15, F26)
- [ ] **AC34** No screen starts blank. An empty chat says "Invite people to start" with a "Copy invite link" button for anyone who may create one. An empty Documents list says "No documents yet. Drop files here or press +." and names the accepted types. A room just created from the profile opens with its invite card. (F07)
- [ ] **AC35** The small fixes:
  - A participant who may not ask a persona can still tap its pill: a sheet says who can let them ("Only Alice can let you ask Grill").
  - On a phone, the People list is a real sheet, with a handle, Close, a backdrop, Escape, and it closes when a document is picked.
  - The document viewer has one toolbar: ✕ first, then the title and its meta, then icon buttons (download, new window, rename, delete), all at the control height.
  - A participant's name never breaks mid-word: it ends with an ellipsis, and roles beyond two show as icons with a tooltip.
  - Under 40rem, Room settings' tables become stacked cards, and every tab bar scrolls sideways instead of wrapping.
  - A message's Edit and Delete sit in one `⋯` per message. On a desktop it shows on hover and focus; on a phone it always shows. (F06, F12, F13, F16, F19, F27)
- [ ] **AC36** On a phone (under 40rem), the room is one 56 px header row, a 44 px persona strip that scrolls sideways, the content, the composer, and a 56 px bottom tab bar: Chat, Docs (count), People (count). It leaves room for the safe-area insets. Docs holds the list and the viewer. A tab keeps its scroll position when you come back to it. The People button and its sheet go. (F05, F12)
- [ ] **AC37** With a document open on a desktop, annotations are a panel toggled from the viewer's toolbar, with their count. It is closed by default below 100rem and remembers its last state. The chat folds into a 56 px rail and opens again from it, so the document can take the width. (F14)
- [ ] **AC38** One status strip under the header shows the most urgent state (reconnecting, then waiting for approval, then auto turns paused, then a running answer, then the queue) and "+N more", which opens the rest. Bars no longer stack. Action feedback goes to toasts: bottom right on a desktop, above the tab bar on a phone, `role=status`, 4 s, at most one action, worded after the button ("Copied", "Renamed", "Saved"). (F20)
- [ ] **AC39** No page calls `confirm()` or `alert()`; a test checks the six UIs' scripts. A destructive action asks in one confirm sheet: a question as its title, one line on the consequence, then Cancel and a danger button. Deleting a message or a document acts at once and offers Undo in a toast for 5 s: the message is only deleted when the toast ends, and the document is restored by Undo. Deleting a room keeps the type-the-name form. On the daemon page, cutting an instance off from Claude, removing an account or a key, accepting unsigned binaries and stopping the daemon ask first. (F16, F17)
- [ ] **AC40** The Personas sheet:
  - Save is its only primary button. Remove is a danger outline. Start fresh and its handoff variant sit under "More".
  - The memory size, "Speaks up every N answers" and "Can follow up on its own" sit under "Advanced".
  - It opens from `⋯` › Personas, from Room settings › Personas, and from a right-click or long-press on a pill, which scrolls to that persona. (F18)
- [ ] **AC41** The profile page is a home page. It has the room's header, "Your rooms" as cards (room name, your name there, how many people, last activity as "2 min ago" with the date in a tooltip, an admin badge), and "+ New room" as a sheet asking for the room's name and your name, prefilled. A new room opens with its invite card (AC34). A "This profile" section holds your name, your email and "Use on another device". Skills fold under "Skills (for advanced users)". (F23)
- [ ] **AC42** The daemon page is titled "grill-room AI host", with its version, its state and Stop in the header. Its tabs keep their deep links and read Rooms, Servers, Accounts, Settings, Activity, Update. Rooms show their names, which the room server sends in the stream's hello; a room server from before shows the link. Settings is one card per topic, each with one Save. Errors are in sentence case and say what to do. (F24, F22)
- [ ] **AC43** `/admin` has the room's header and tab bar. The Rooms list has a search box, relative dates with the full date in a tooltip, the import in a sheet ("Import a room"), and becomes cards under 40rem. A room's detail view looks exactly like Room settings, from the same component and the same classes. (F22, F26)
- [ ] **AC44** The join page says "Join the room" in its title and heading, and an empty or invalid name shows an inline line under the field, never the browser's tooltip. Naming the room and the inviter waits for the user's decision on an invite preview API (Open questions). (F25)

## Phases

### Phase 1 — Models and labels
- Work: explicit model and effort with migration, the correct model label from `modelUsage`, and the room transcription label.
- **Covers**: AC8, AC9, AC10
- **Data model impact**: `room_personas` and templates' model and effort become NOT NULL after migration
- **DoD**: unit tests with recorded `modelUsage` shapes, including the resumed-session case with two models; migration on a copy of the lab DB; daemon release `v0.0.4` and lab redeploy (DB backup first), so the host's worker picks up the fix.

### Phase 2 — Presence and audio indicators
- Work: server presence state and events, heartbeat, participant list sections, audio and capture states (from LiveKit and the STT worker's subscriptions).
- **Covers**: AC6, AC7, AC14, AC18, AC19, AC20, AC22, AC23
- **Data model impact**: `participants.last_seen_at` (if not present); `rooms.stt_glossary`
- **DoD**: presence tests (flap, background, multi-tab); e2e for the list at phone width; the glossary prompt builder unit-tested (order, token cap); a lab scenario run with the glossary shows better term accuracy than without.

### Phase 3 — Mentions, autocomplete, stop
- Work: `@` autocompletion, mention feedback, trigger state on messages, stopping a running turn end to end (server, protocol, daemon, sandbox).
- **Covers**: AC2, AC3, AC4, AC5
- **Data model impact**: `messages.turn_id` link (or a mentions table); turn status `stopped`
- **DoD**: e2e for each mention outcome; a stop test with the fake claude (nothing applied, next turn resumes); one live haiku turn stopped mid-run.

### Phase 4 — Self-rename, docs window, unread, modals
- Work: self-rename, the document manager in its own window, the unread title count, modal dismissal, document rename and soft delete, the stored mention chips.
- **Covers**: AC1, AC3 (text), AC4 (stored link), AC11, AC12, AC13, AC15, AC16, AC17, AC21, AC24, AC25, AC26, AC27
- **Data model impact**: the message author snapshot gives the name history; `profiles.name`; `push_subscriptions`; VAPID keys in instance settings; `documents.deleted_at` and a change counter; `document_renames`; `message_turns` keeps the final state of an imported mention; export format 2
- **DoD**: e2e for each, at phone and desktop width; push encryption (RFC 8291) and VAPID JWT tested against a local test push endpoint; a real notification received on an Android phone from the lab.

### Phase 5 — Design system and polish
- Work: `tokens.css` and its Go package, applied to the room, the docs window, join, profile, `/admin` and the daemon page; the colour mapping; upload with `+`, drag and drop, progress and errors; the room header and its menus; the wording table; empty states; touch targets; the small fixes.
- **Covers**: AC28, AC29, AC30, AC31, AC32, AC33, AC34, AC35
- **Data model impact**: none
- **DoD**: Playwright bounding boxes: the room header's controls share one height at 1440 px, and every control of the AC30 screens is at least 44 × 44 px at 390 px; `TestTokenContrast` and `TestPageCSSHasNoColourLiterals` green; e2e for drag-and-drop upload (several files, one refused, Retry) in the room and the docs window; before and after screenshots listed in the tracking file; daemon release `v0.0.9` and lab redeploy (DB backup first).

### Phase 6 — Redesigns
- Work: the phone tab bar; the viewer's annotation panel and chat rail; the status strip and toasts; confirm sheets and Undo; the Personas sheet; the profile home page; the daemon page; the `/admin` layout; the join page.
- **Covers**: AC36, AC37, AC38, AC39, AC40, AC41, AC42, AC43, AC44
- **Data model impact**: none; the profile's room list gains each room's people count, last activity and the viewer's admin flag; the daemon stream's hello gains the room's name
- **DoD**: e2e at 390 px for the tab bar (switching keeps each tab's scroll), at 1440 px for the annotation toggle and the chat rail; a test that no UI script calls `confirm(` or `alert(`; e2e for message Undo and document Undo; the AC30 bounding-box spec extended to the new screens; before and after screenshots in the tracking file; daemon release `v0.0.10` and lab redeploy (DB backup first).

## Data model impact (summary)

- `room_personas` and template model and effort become explicit and NOT NULL.
- `participants.last_seen_at`.
- A mention to turn link, stored (`message_turns`), with the final state of an imported mention; export format 2.
- Turn status `stopped`.
- A name history for renames: the author snapshot each line keeps; `profiles.name`.
- `rooms.stt_glossary`.
- `push_subscriptions` and the VAPID keys.
- `documents.deleted_at` and a change counter; `document_renames`.
- Phases 5 and 6 add no table; the profile's room list and the daemon hello gain fields.

## Open questions

- F25: naming the room and the inviter on the join page needs a public invite preview API (`GET` by invite token, answering the room's name and the inviter's). The audit asks to check with the user first; AC44 leaves it out until then.
- F08: a lost recovery link cannot be shown again: the server keeps only its hash. Issuing a new one needs a new endpoint that rotates it. AC32 points to the email sign-in instead.

## Decisions

- Moderation: self-rename plus admin rename; no kick (user, 2026-10-12).
- SSE stays; presence uses SSE plus a heartbeat POST (user agreed after the SSE vs WebSocket comparison).
- Migrated personas get sonnet / medium.
- STT stays on faster-whisper `small`; the glossary is passed as `initial_prompt` only (spike S10, user, 2026-10-12).
- The glossary prompt is off by default, opt-in with `APP_GLOSSARY_PROMPT=on` (2026-10-13). Spike S10b measured it on real speech: `small` with the room prompt went from WER 0.327 to 0.398 on real meetings (gold reference) and from 0.252 to 0.552 on a real talk, as it loops on the prompt and drops text. It only helped on synthetic Piper lines, and in production the prompt is never empty: names and titles always feed it. The glossary, its editor and the prompt builder stay, to feed a word-list engine later.
- Web Push with VAPID for background notifications (user, 2026-10-12); a notification opens the chat at the message that triggered it (user, 2026-10-13).
- Manage opens for every member, who sees only the Glossary tab; the admin tabs stay admin-only (user, 2026-10-13).
- AC3 drops "chain paused": a human mention resets the chain, so it never meets a paused one (user, 2026-10-13).
- The mention to turn link is stored, not kept as the latest 200 in memory, and travels in exports (user, 2026-10-13).
- Mentions show as chips, resolved by the server's rule only, and a line mentioning the viewer is highlighted (user, 2026-10-13).
- Documents: any member renames, room admins soft-delete and restore; only the room's deletion purges (user, 2026-10-13).
- Plain members see only the Glossary tab in Room settings (user decision C, 2026-10-13).
- `profiles.name`, set by "Also use it for my profile", is accepted as routine (user, 2026-10-13).
- The web manifest is `standalone`, which iOS needs for push (user, 2026-10-13).
- The UX audit's findings become AC28 to AC44, grouped by theme, in two phases: polish (Phase 5), then the redesigns (Phase 6) (user, 2026-10-13).
- Test campaigns run throttled on the workstation (`nice`, `ionice`, CPU cap, no stress); repetition goes to CI.
