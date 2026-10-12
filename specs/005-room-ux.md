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
- Unread count in the tab title.
- Modal dismissal.

## Out of scope

- Renaming or removing another participant (moderation).
- WebSockets: SSE stays.
- Typing indicators.

## Acceptance criteria

### Identity
- [ ] **AC1** A participant can change their own display name from the room page, with the same validation as at join. A system line says "<old> is now <new>". Past messages show the new name with the old one on hover. An option also applies it to their profile. The presence preamble keeps the same short uuid across the rename.

### Mentions and turns
- [ ] **AC2** Typing `@` in the message box suggests the room's personas first, then participants, filtered as you type. It works with keyboard and touch.
- [ ] **AC3** A message mentioning a persona that cannot run gets a notice visible only to its author, with the reason: no right to trigger agents, no host online, chain paused, or unknown persona.
- [ ] **AC4** A message whose mention queued a turn shows "→ @Persona" with that turn's live state (queued, running, done, failed, stopped), linked to the synthesis.
- [ ] **AC5** Whoever started a turn, the host or a room admin can stop it while it runs. The daemon kills `claude` in the sandbox. The turn ends as "stopped" with nothing applied: no revision, no synthesis, no replies. The persona's next turn resumes from the server's session copy, and a stopped turn is not a chain link.

### Presence and audio
- [ ] **AC6** The server keeps each participant's presence from their open streams and a heartbeat, with a grace period of about 20 s, so reloads, background tabs and several tabs do not flap. It sends `presence` events (here, gone, audio on or off, mic on or off) and replays the full state when a stream opens. Tests cover a flapping connection, a backgrounded phone, and two tabs with one closed.
- [ ] **AC7** The participant list shows "In the room", then "Not here" collapsed, with "seen <time> ago". Each person shows their audio state: mic live, muted, listening only, not in audio. It also shows the speaking pulse, and a "transcribed" badge when the STT worker is subscribed to that person's track.
- [ ] **AC8** The room no longer shows "Transcribing" as if it applied to the viewer. It shows how many voices are transcribed ("2 voices transcribed"). A viewer whose voice is captured sees "your voice is being transcribed", and one who is not sees nothing implying capture.

### Models
- [ ] **AC9** Every persona has an explicit model (haiku, sonnet, opus, fable) and an explicit effort. "Default" is never offered or shown. Existing personas without a value migrate to sonnet / medium. Effort shows in full words ("effort medium").
- [ ] **AC10** The synthesis header names the model the turn ran on: the requested one, checked against `claude -p`'s `modelUsage`. Any other model in `modelUsage` shows as secondary with its cost share, for example on a resumed session started with another model.

### Documents and tabs
- [ ] **AC11** "Open in a new window" opens the whole document manager at `/r/<room>/docs`: list, upload, viewer, annotations, revisions and diff, all live. Deep links `/r/<room>/docs/<doc>` open one document. The room page and the docs window stay in sync.
- [ ] **AC12** When the room tab is not focused, its title reads "(N) <room name>", where N counts new text messages from others and persona syntheses. Voice lines are counted only if the user opts in. The count resets when the tab regains focus.
- [ ] **AC13** Every modal (persona panel and the others) closes on a click outside it and on Escape, and keeps focus correctly for keyboard users.

## Phases

### Phase 1 — Models and labels
- Work: explicit model and effort with migration, the correct model label from `modelUsage`, and the room transcription label.
- **Covers**: AC8, AC9, AC10
- **Data model impact**: `room_personas` and templates' model and effort become NOT NULL after migration
- **DoD**: unit tests with recorded `modelUsage` shapes, including the resumed-session case with two models; migration on a copy of the lab DB; daemon release `v0.0.4` and lab redeploy (DB backup first), so the host's worker picks up the fix.

### Phase 2 — Presence and audio indicators
- Work: server presence state and events, heartbeat, participant list sections, audio and capture states (from LiveKit and the STT worker's subscriptions).
- **Covers**: AC6, AC7
- **Data model impact**: `participants.last_seen_at` (if not present)
- **DoD**: presence tests (flap, background, multi-tab); e2e for the list at phone width.

### Phase 3 — Mentions, autocomplete, stop
- Work: `@` autocompletion, mention feedback, trigger state on messages, stopping a running turn end to end (server, protocol, daemon, sandbox).
- **Covers**: AC2, AC3, AC4, AC5
- **Data model impact**: `messages.turn_id` link (or a mentions table); turn status `stopped`
- **DoD**: e2e for each mention outcome; a stop test with the fake claude (nothing applied, next turn resumes); one live haiku turn stopped mid-run.

### Phase 4 — Self-rename, docs window, unread, modals
- Work: self-rename, the document manager in its own window, the unread title count, modal dismissal.
- **Covers**: AC1, AC11, AC12, AC13
- **Data model impact**: none beyond the name history needed for "old name on hover" (`participant_names` or message author snapshot)
- **DoD**: e2e for each, at phone and desktop width.

## Data model impact (summary)

- `room_personas` and template model and effort become explicit and NOT NULL.
- `participants.last_seen_at`.
- A mention to turn link.
- Turn status `stopped`.
- A name history for renames.

## Open questions

- (none)

## Decisions

- Moderation by others is out; only self-rename (user, 2026-10-12).
- SSE stays; presence uses SSE plus a heartbeat POST (user agreed after the SSE vs WebSocket comparison).
- Migrated personas get sonnet / medium.
- Test campaigns run throttled on the workstation (`nice`, `ionice`, CPU cap, no stress); repetition goes to CI.
