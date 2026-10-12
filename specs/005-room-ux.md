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

## Out of scope

- Removing (kicking) a single participant.
- WebSockets: SSE stays.
- Typing indicators.

## Acceptance criteria

### Identity
- [x] **AC1** A participant can change their own display name from the room page, with the same validation as at join. A system line says "<old> is now <new>". Past messages show the new name with the old one on hover. An option also applies it to their profile. The presence preamble keeps the same short uuid across the rename.

### Mentions and turns
- [x] **AC2** Typing `@` in the message box suggests the room's personas first, then participants, filtered as you type. It works with keyboard and touch.
- [ ] **AC3** A message mentioning a persona that cannot run gets a notice visible only to its author, with the reason: no right to trigger agents, no host online, or unknown persona. A human mention resets the chain (002-AC26), so it never meets a paused chain.
- [ ] **AC4** A message whose mention queued a turn shows "→ @Persona" with that turn's live state (queued, running, done, failed, stopped), linked to the synthesis. The link between the message and its turn is stored, so every message keeps its chip after a reload, however old, and an export carries it (the export format moves to 2; format 1 still imports).
- [ ] **AC27** In the chat, `@Persona` shows as a chip with the persona's badge and colour, and `@Person` as a chip with that person's name. Only a persona or member of the room makes a chip; any other `@name` stays plain text. The match is case-insensitive and is the server's (`mentionRe`): the server names the mentions of each line, and the page only draws them. A line mentioning the viewer gets a highlighted background and an accent bar, and counts as a mention for the unread title (AC12) and for push (AC24). Chips are reachable by keyboard and read by screen readers, and the text still copies as `@Name`. Chips are DOM nodes; the line's text stays escaped.
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
- [x] **AC23** Each room has an STT glossary, edited by any member in Manage, with a line in the journal. Every change applies from the next utterance. The worker passes it to faster-whisper as a natural sentence in `initial_prompt`, never as `hotwords` (spike S10). The prompt stays within Whisper's ~224-token limit. When the terms do not all fit, they are taken in this order: the edited glossary, then participant and persona names, then document titles. Manage shows "N / ~70 terms used". A Piper scenario with technical terms checks term accuracy against `small` without a glossary.
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
- [ ] **AC25** Any member can rename a document from the document manager. The document keeps its id, so its revisions, annotations and deep links survive. Its workspace path changes; the daemon's cache is keyed by sha256, so nothing is transferred again. The new name passes the same validation as an upload, keeps the extension, and no other document of the room may hold it, so paths stay unique. A timestamped chat line says "X renamed `a.md` → `b.md`". The personas' next turn tells them the old and new paths, so a resumed session does not keep the old one. An agent document is not renamed while a turn runs, as its result would recreate the old name.
- [ ] **AC26** A room admin can delete a document, softly. It leaves the list, the workspace and the agents' view, and a chat line records it. Its data (revisions, annotations, original) stays and counts in the room's volume. A room admin restores it from Manage, unless another document took its name meanwhile. Only deleting the room purges it. An export carries deleted documents, flagged. As for a rename, an agent document is not deleted while a turn runs.
- [x] **AC13** Every modal (persona panel, the "Manage" room-admin panel, and the others) closes on a click outside it and on Escape, and keeps focus correctly for keyboard users.

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

## Data model impact (summary)

- `room_personas` and template model and effort become explicit and NOT NULL.
- `participants.last_seen_at`.
- A mention to turn link, stored (`message_turns`), with the final state of an imported mention; export format 2.
- Turn status `stopped`.
- A name history for renames: the author snapshot each line keeps; `profiles.name`.
- `rooms.stt_glossary`.
- `push_subscriptions` and the VAPID keys.
- `documents.deleted_at` and a change counter; `document_renames`.

## Open questions

- (none)

## Decisions

- Moderation: self-rename plus admin rename; no kick (user, 2026-10-12).
- SSE stays; presence uses SSE plus a heartbeat POST (user agreed after the SSE vs WebSocket comparison).
- Migrated personas get sonnet / medium.
- STT stays on faster-whisper `small`; the glossary is passed as `initial_prompt` only (spike S10, user, 2026-10-12).
- Web Push with VAPID for background notifications (user, 2026-10-12); a notification opens the chat at the message that triggered it (user, 2026-10-13).
- Manage opens for every member, who sees only the Glossary tab; the admin tabs stay admin-only (user, 2026-10-13).
- AC3 drops "chain paused": a human mention resets the chain, so it never meets a paused one (user, 2026-10-13).
- The mention to turn link is stored, not kept as the latest 200 in memory, and travels in exports (user, 2026-10-13).
- Mentions show as chips, resolved by the server's rule only, and a line mentioning the viewer is highlighted (user, 2026-10-13).
- Documents: any member renames, room admins soft-delete and restore; only the room's deletion purges (user, 2026-10-13).
- Test campaigns run throttled on the workstation (`nice`, `ionice`, CPU cap, no stress); repetition goes to CI.
