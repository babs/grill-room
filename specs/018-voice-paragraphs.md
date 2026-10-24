# 018: Voice paragraphs

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-23
**Context**: live session review, 2026-10-23: transcripts are accurate but "super choppy". Builds on 007 (STT worker, VAD), 010 (device STT, dedupe 010-AC16), 006 (voice triggers 006-AC18), 016 (message edits).

## Problem

Every VAD cut becomes one chat message. The VAD cuts on 800 ms of silence, so one speaker's monologue lands as a string of 2-4 s fragments: one session logged 13 lines from one speaker in 55 s. The 15 s forced cut lands mid-word and leaves 1-2 character fragments behind it. Readers get a stutter instead of paragraphs, and "every N voice lines" personas count fragments, so they fire mid-thought.

## Solution

A speaker's consecutive utterances grow one voice message, live, until a real pause or someone else speaks. The VAD keeps cutting short utterances for the engine; only the chat changes. The forced cut at the speech cap lands on the quietest moment before it instead of mid-word. Both the paragraph pause and the cut search window are settings.

## Scope

- Worker and device segmenters report, with each utterance, the pause that preceded it.
- Forced cut at the speech cap moves to the quietest window of the last seconds before it.
- The room server appends a continuing utterance to the speaker's open voice message and broadcasts the change.
- "Every N voice lines" counts closed paragraphs.
- Settings, metrics, docs.

## Out of scope

- Punctuation or language-model continuity checks: the pause carries the signal; revisit if paragraphs still break wrong.
- Per-room paragraph settings: one instance-wide value until someone needs two.
- Re-flowing past messages: only lines stored after deploy merge.
- Changing `min_silence_ms`: utterance size for the engine stays as tuned in S5.

## Acceptance criteria

- [ ] **AC1** The worker posts each line with `gap_s`: seconds from the end of the previous utterance's speech on this track to the start of this one's. `gap_s` is 0 when the previous utterance was a forced cut and speech goes on within the VAD's silence (`min_silence_ms`); a longer pause after a forced cut is reported as is. It is absent for a track's first utterance, the first after a resume, and the first after the speaker mutes.
- [ ] **AC2** The device segmenter (`internal/web/ui/stt/segmenter.js`) sends `gap_s` on `voice/device-lines` with the same meaning.
- [ ] **AC3** At the speech cap, the cut lands on the window with the lowest speech probability in the last `vad_cut_search_s` seconds (worker `APP_VAD_CUT_SEARCH_S`, default 3). The audio after that window opens the next utterance, with `gap_s` as in AC1. `vad_cut_search_s` 0 keeps today's hard cut. The device segmenter does the same with the default value.
- [ ] **AC4** A line is appended to its speaker's latest voice message when all hold: `gap_s` is present and below the paragraph gap (`GRILL_ROOM_VOICE_PARAGRAPH_GAP`, default 2.5 s); that message is the room's latest message of any kind; it has the same source; it is neither deleted nor edited by its author; the joined body stays within `GRILL_ROOM_VOICE_PARAGRAPH_MAX_CHARS` (default 1200); no agent turn has read it yet. Otherwise the line is a new message, as today.
- [ ] **AC5** An append joins the bodies with one space, keeps the message's id and timestamp, sets no `edited_at` and writes no version, bumps `change_seq`, and reaches every browser through the existing `message_edited` stream on every pod within a second.
- [ ] **AC6** A server line and a device line of the same utterance still store once (010-AC16): the repeat check compares the new line with word-aligned stretches of the other source's voice messages, not only their whole body, so a line already inside a paragraph is caught.
- [ ] **AC7** A paragraph is closed once another message follows it or `GRILL_ROOM_VOICE_PARAGRAPH_CLOSE` (default 20 s) has passed since its last append. "Every N voice lines" counts closed paragraphs only, and is checked on each voice line and on the existing trigger tick while someone is in the room and paragraphs are on.
- [ ] **AC8** A line posted without `gap_s` (older worker or app) is stored as today.
- [ ] **AC9** `grill_room_voice_lines_appended_total{source}` counts appends; the worker's `line_done` log carries `gap_s` and whether the cut was forced.
- [ ] **AC10** In a lab session, a one-minute monologue with breathing pauses shows as at most 3 messages, and no line of 2 characters or fewer follows a forced cut.

## Phases

### Phase 1 — Segmenters: gap and quiet cut
- Work: `Segmenter` (Python) keeps per-window probabilities, cuts at the quietest window at the cap, returns the gap before each utterance; worker sends `gap_s`. Same in `segmenter.js` and the device pipeline. Server accepts and ignores `gap_s`.
- **Covers**: AC1, AC2, AC3, AC8
- **Data model impact**: none
- **DoD**: `make stt-test` green with tests for: gap after a pause, gap 0 after a forced cut, quiet cut position, `vad_cut_search_s` 0 = hard cut; `e2e/device-stt.spec.js` extended with the gap and quiet-cut cases and green; `go test ./internal/web/...` green.

### Phase 2 — Server: append to the open paragraph
- Work: `PostVoiceLine` decides append vs insert inside its write transaction; env settings; `appended_at` set on append; repeat check over word-aligned stretches (AC6); metric.
- **Covers**: AC4, AC5, AC6, AC9
- **Data model impact**: `messages.appended_at` (nullable timestamp), PostgreSQL migration 51; dbcopy carries it as a column added since the SQLite source.
- **DoD**: table-driven store tests for each AC4 condition, red when any one fails; web test: two lines with `gap_s` 1 give one message and one `message_edited` event; dedupe test with a two-line paragraph; touched packages green on PostgreSQL and in the no-PG run.

### Phase 3 — Triggers count closed paragraphs
- Work: `DueVoicePersonas` excludes open paragraphs; tick re-checks.
- **Covers**: AC7
- **Data model impact**: none
- **DoD**: test: an "every 2" persona does not fire on an open paragraph, fires once the gap passes with no new line, and fires when another speaker's line closes it.

### Phase 4 — Docs and lab check
- Work: settings in README / deploy docs; lab session check.
- **Covers**: AC10
- **Data model impact**: none
- **DoD**: lab session (image built from the branch): message count and `grill_room_voice_lines_appended_total` quoted in the PR.

## Data model impact (summary)

- `messages.appended_at`: nullable timestamp, last append to a voice paragraph. PostgreSQL migration 51, no backfill.

## Open questions

None blocking.

## Decisions

- Server-side append rather than worker buffering — keeps lines live (user, 2026-10-23, option A).
- The worker sends the raw pause (`gap_s`), the server decides — one threshold, one place, both sources.
- No append once an agent turn read the message — `MessageChanges` reports changes through versions; an append without a version would hide the tail from that persona. A new message starts instead.
- Device STT included — its segmenter is a port of the worker's and has the same choppiness.
- Paragraph gap 2.5 s, max 1200 chars, cut search 3 s — starting values, all settings.
- Appends write no version and no "(edited)" — the speaker did not edit anything.
- `gap_s` above 3600 s is clamped, not refused — a line is never lost to a long silence (phase 1 review).
- On a mic mute the worker forgets the previous pause, so the next line starts a new paragraph. It neither flushes nor sets a timer: without frames during a mute the pause would otherwise read as ~1 s, and three timer-based rounds kept losing the speech tail or measuring the wrong pause (phase 1 review).
- A paragraph closes by time only after `GRILL_ROOM_VOICE_PARAGRAPH_CLOSE` (20 s), not after the paragraph gap: the next line of a monologue lands up to one utterance (15 s cap) plus the VAD silence and STT latency after the previous one, so closing after 2.5 s would fire personas mid-monologue and split the paragraph (phase 3 review).
- The trigger tick checks voice personas only while paragraphs are on (gap above 0); with paragraphs off, a re-armed persona waits for the next voice line, as before 018.
- The trigger tick checks only rooms where someone is present, every tick: a time window on recent rooms kept losing fires after absences, leader changes or slow ticks (phase 3 review).
