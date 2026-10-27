# 020: Written chat and voice transcription apart

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-26
**Context**: builds on 018 (voice paragraphs), 005 (room UX, unread count AC12), 010 (device STT).

## Problem

Typed messages, persona replies and voice transcription share one list. In a talking room the voice paragraphs bury the written exchange, and reading the transcription alone means scrolling past everything else. Typing while someone speaks also cuts their voice paragraph (018-AC4), so the transcription gets choppy exactly when the chat is busy.

## Solution

On a wide screen, the chat shows the written exchange: typed messages, persona replies and room events. Voice transcription runs live in its own pane, to the right of the chat, or above the chat while a document is open. The composer always stays at the bottom. The pane can be folded. On a narrow screen, or with the pane folded, the chat gets a filter, All · Written · Voice, set to Written by default. Message text on the room page gets slightly smaller above 40rem (tablet and desktop) to make room. A typed message no longer cuts a voice paragraph.

## Scope

- Voice paragraphs ignore typed messages (server).
- Chat filter All · Written · Voice, Written by default, kept per browser.
- Transcription pane: right of the chat, above it while a document is open, foldable.
- Message text on the room page, wider than 40rem: 1rem → 0.9375rem.

## Out of scope

- Resizing the transcription pane with a handle: fixed size until someone needs it.
- Changing what agents read: their transcript stays interleaved (003-AC23).
- A smaller type scale on other pages, or any size under 0.9375rem.
- Phones: font size unchanged (inputs stay at 16 px), no pane, only the filter.
- Re-flowing past paragraphs that a typed message cut before this ships.

## Acceptance criteria

### Voice paragraphs

- [ ] **AC1** A voice line joins its speaker's open paragraph (018-AC4) when that paragraph is the room's latest message other than a typed (`text`) one. All other 018-AC4 conditions are unchanged. A persona reply, a room event or another speaker's voice line still ends it.
- [ ] **AC2** A typed message no longer closes a paragraph for "every N voice lines" (018-AC7): the paragraph closes when a non-typed message follows it or when the close delay passes.
- [ ] **AC17** A room's and a profile's last activity date a paragraph by its last append even when it grew past typed messages (018-AC5); a typed message later than that append wins.
- [ ] **AC3** A typed message that starts a turn which reads the paragraph still ends it (018-AC4, "no agent turn has read it yet"): the next line opens a new paragraph.

### Filter

- [ ] **AC4** When the transcription pane is not shown (narrow screen or pane folded), a segmented control above the chat's messages shows: All, Written, Voice. Written shows `text`, `agent` and `system` messages; Voice shows `voice` messages; All shows everything, as today.
- [ ] **AC5** The filter defaults to Written and the choice is kept in the browser across reloads and rooms.
- [ ] **AC6** While the filter hides a kind, a new message of that kind adds to a count on its segment ("Voice · 3"). The count clears when that segment, or All, is shown.
- [ ] **AC7** A deep link to a message (`#m<id>`) that the current filter hides switches the filter to All before scrolling to it.

### Transcription pane

- [ ] **AC8** At a viewport of 64rem or wider with no document open, while the chat keeps 22rem beside it, the room shows a "Transcription" pane, 20rem wide, to the right of the chat. It lists the room's voice messages; the chat shows Written messages and no filter.
- [ ] **AC9** With a document open at that width, the pane sits above the chat's messages, in the chat column, at most 40% of its height. The composer stays at the bottom of the column.
- [ ] **AC10** The pane is live: a new voice line and a paragraph append (018-AC5) show at once. It sticks to the bottom as the chat does: pinned while the reader is within 40 px of the end, left alone once they scroll up.
- [ ] **AC11** A voice message in the pane keeps what it has in the chat today: speaker, time, source (device/server), the mention state of a voice trigger (006-AC18), and the edit/delete menu for its author.
- [ ] **AC12** A fold button in the pane header folds it to a strip with an unfold button and a count of the voice lines the chat has not shown since it was folded; folding brings the filter back (AC4). The folded state is kept in the browser.
- [ ] **AC13** With no voice message yet, the pane says "Nobody has been transcribed yet." While the STT worker is down (001-AC10), the pane header shows "Voice-to-text is off", the words of the audio menu.
- [ ] **AC14** The pane is an `aside` labelled "Transcription", its list `aria-live="polite"`; the fold button carries `aria-expanded`. Keyboard and screen reader reach it in order after the chat's messages.

### Type size

- [ ] **AC15** On the room page, wider than 40rem, message text (system lines excepted), the composer and the pane use 0.9375rem; `--fs-xs` and `--fs-sm` are unchanged. At 40rem and under nothing changes. `docs/design.md` records the room-page exception.

### Unchanged

- [ ] **AC16** The unread title count (005-AC12), push notifications and the agent's transcript chunks behave as today, except that a paragraph grown past a typed message sits before it, at its first line's place (Decisions).

## Phases

### Phase 1 — Paragraphs ignore typed messages
- Work: `appendVoiceLine` reads the latest non-`text` message; the close check of `DueVoicePersonas` stops counting `text` as a closing message; room and profile last activity take the later of the latest message and the latest non-text message's last append.
- **Covers**: AC1, AC2, AC3, AC17
- **Data model impact**: two partial indexes, `messages(room_id, id)` and `messages(participant_id, id)` `WHERE kind <> 'text'`, in a migration
- **DoD**: `make test` green with new cases in `internal/web/voiceparagraph_test.go`: voice, text, voice from the same speaker → one paragraph; voice, agent, voice → two; a text @mention that runs a turn → two; "every 2 voice lines" not fired by a typed message.

### Phase 2 — Filter and type size
- Work: segmented control above the chat's messages, per-kind counts, deep-link switch, room-page font size above 40rem, `docs/design.md` note.
- **Covers**: AC4, AC5, AC6, AC7, AC15, AC16
- **Data model impact**: none
- **DoD**: `make e2e` green with `e2e/chat-filter.spec.js` cases: default Written hides a voice line; Voice and All; count on a hidden kind; reload keeps the choice; `#m<id>` of a voice line switches to All; computed font size 15 px at 1280 px wide and 16 px at 375 px.

### Phase 3 — Transcription pane
- Work: pane markup and CSS (right of the chat, above it with a document open), fold strip and count, live updates, empty and unavailable states, filter hidden while the pane shows.
- **Covers**: AC8, AC9, AC10, AC11, AC12, AC13, AC14
- **Data model impact**: none
- **DoD**: `make e2e` green with cases: at 1280 px a voice line lands in the pane, not the chat, and a paragraph append updates it in place; with a document open the pane is above the messages and the composer is the column's last element; fold shows the filter and the count, reload keeps it folded; at 900 px no pane and the filter shows; the author's menu edits a line in the pane; the AC14 test checks the pane's name, `aria-live`, `aria-expanded`, DOM order, focus ring and target size.

## Data model impact (summary)

Two partial indexes on `messages` (Phase 1), `WHERE kind <> 'text'`, by room and by participant. No table or column change.

## Open questions

- [ ] None.

## Decisions

- Wide screen means 64rem and up: `#side` (16rem) + chat + pane (20rem) leave the chat about 28rem at 64rem. Overturnable.
- Pane width fixed at 20rem, no resize handle (Out of scope).
- Filter and fold state are per browser (`localStorage`), not per room or per profile, like the voice unread opt-in (005-AC12).
- "Written" includes persona replies and room events: they answer the written exchange as much as the spoken one.
- A typed message stays a paragraph boundary only through the existing "a turn has read it" rule (AC3).
- A paragraph keeps its first line's id and place, so words said after a typed message read before it, in the chat's All view and in the agent's transcript. Accepted: the reorder is bounded by one paragraph (the 2.5 s gap, 1200 chars, the 20 s close), and splitting on typing is what made the transcription choppy.
- The non-text lookups (paragraph append, paragraph close, last activity) get partial indexes: without them a room with no voice line walks its whole history on every profile read (measured 424 ms at 1M typed messages, 0.3 ms with the index).
- Your own lines never add to a segment count (AC6), and sending a typed message while Voice is shown switches the view to All for that visit: you see what you sent next to what is being said. When the pane opens or closes, a line holding focus (its edit box or its menu button) stays in view, and shows All for the visit if the filter would hide it.
- A jump that switches the filter (deep link, push click, mention chip, AC7) does not change the stored choice: the next visit opens on it again.
- The pane shows only while the chat keeps its 22rem minimum beside it (docs/design.md), not on the viewport width alone: a wide side panel at 64rem would otherwise squeeze the chat to 14rem.
- No axe dependency for AC14: the e2e asserts the attributes and order it needs.
