# 036 — What screen readers hear in a room

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-14
**Context**: UX audit part A, findings A1 and A2. Reworks how the chat (005, 020-AC14, 031 chat window) and the Transcription pane (020, 018-AC5 growing paragraphs) reach screen readers. Uses the Notifications sheet's "This tab" section (005-AC12) and `#toast-live` (kit `grillAnnounce`).

## Problem

`#messages` and `#voice-lines` are `aria-live=polite`. Three things follow:
- The stream replays the room's history on every load into `#messages`, so the whole backlog is queued for speech. The stream sends no end-of-replay signal, and `aria-busy` only defers what was queued.
- A voice paragraph grows by replacing its whole line, so each growth is read again from its first word, over call audio the person already hears. No setting turns it off.
- An edit, a delete, a proposal card or a blocked-website card is re-rendered by swapping its line, so it is read again in full.

## Solution

The visible lists stop being live regions. One hidden log, `#chat-log`, says what is new: each line written by someone else after the page loaded, once, as "Sender: text". Changes to lines already there are silent. The transcription is silent by default; "Read the transcription aloud" in Notifications, this tab, turns on an announcement of only the words a paragraph gained.

## Acceptance criteria

- [x] **AC1** `#messages` and `#voice-lines` carry no `aria-live` and no `aria-busy`, in the room, the chat window and the docs window. Each is a list named by its section heading: "Chat" and "Transcription".
- [x] **AC2** The page has one visually hidden `role=log` element, `#chat-log`, `aria-live=polite`, present from first paint. Lines replayed by the stream on load or on reconnect that were written before the page loaded never reach it.
- [x] **AC3** A new line from someone else (a written line, a persona answer, a file line, a system line, a proposal card) adds one entry to `#chat-log`: the sender's display name, ": ", then the line's plain text (Markdown rendered to text, no markup, files by name); a system line is said as its text alone, every one being a sentence that names its actor. Text over 280 characters is cut at a word boundary and ends with "…, more in the chat". The viewer's own lines add nothing.
- [x] **AC4** When more than 3 entries would be added within 2 seconds (a burst, a reconnect after a gap), the first 3 are added and the rest become one entry "N more new messages", N their count ("1 more new message" for one).
- [x] **AC5** An edit, a delete, a voice paragraph growing, a proposal card created or dismissed, a blocked-website card's buttons changing: nothing is added to `#chat-log`, and the line updates in the list without being announced. Except: a blocked-website line whose outcome changes (allowed, or not allowed with a reason) is said once, for everyone, through `#toast-live`.
- [x] **AC6** Voice lines add nothing to `#chat-log` by default, wherever they show (pane or chat filter).
- [x] **AC7** Notifications, "This tab" has a checkbox "Read the transcription aloud", off by default, kept in this browser like "Count voice lines in the tab title". When on, a new voice paragraph from someone else adds "Speaker: words", and each later growth of that paragraph adds only the words it gained, without the speaker's name. When the opening was not said (read aloud was off, or the opening folded), the first growth said names the speaker; so does the first growth said after any other entry was added to the log. A paragraph begun before the page loaded is read from its next growth, never re-reading the words already there. The viewer's own voice lines add nothing. AC4 applies.
- [x] **AC8** `#chat-log` keeps at most 20 entries; older ones are removed, and removal is not announced (`aria-relevant="additions"`).
- [x] **AC9** Mention suggestion lists inside a line's edit box no longer need their own `aria-live=off` workaround; screen readers do not read suggestions on each keystroke (no live ancestor).

## Phases

### Phase 1 — Quiet lists, one log
- Work: `room.html` (lists, `#chat-log`, checkbox), `room.js` (`addMessage` path to the log, plain-text extraction, burst folding, growth diff for voice), removal of `aria-busy` toggling on the lists.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9
- **Data model impact**: none
- **DoD**: `e2e/announcements.spec.js` (new): load a room with 30 lines of history and assert `#chat-log` is empty after the stream settles (AC2); a line from another person adds exactly one entry with name and text, a long one is cut (AC3); 6 lines in one burst give 3 entries plus "3 more new messages" (AC4); an edit, a delete and a growing voice paragraph add nothing (AC5, AC6); with the checkbox on, a paragraph grown twice gives "Speaker: first words", then only the added words (AC7); 25 lines leave 20 entries (AC8); attribute checks on both lists and the edit box list (AC1, AC9). `ui-guards`, `chat`, `chat-filter`, `chat-window`, `transcription-pane`, `mentions` stay green.

## Out of scope

- Roving focus inside the message list (A3 second half).
- Announcing documents, presence or persona state changes.
- Storing the transcription choice on the profile.

## Decisions

- A hidden log fed by the app, not live lists with `aria-busy`: WAI-ARIA lets assistive technology announce what accumulated once busy clears, and the stream has no end-of-replay event.
- The docs window adds nothing to `#chat-log`: it shows no chat, and the room's own tab says the line, as the unread count skips that window.
- This amends 020-AC14's "its list `aria-live="polite"`": the pane's list is named "Transcription" and is not live.
- Feedback on the viewer's own action (mention notices, "Allowing <site>…" from the chat card or the approvals banner, "Dismissed <site>" from the banner, "Proposal dismissed") goes through `#toast-live`, not `#chat-log`: the line it lands in is the viewer's own or already there.
- A blocked website's outcome (the line's new text, or "Not allowed: <reason>") is a room event, said once to every viewer when a redraw changes it, whoever asked: tracking each viewer's own request raced with coalesced redraws.
- "Before the page loaded" uses the existing `isLive` test (005-AC12, 020-AC6), so the unread count and the log agree on what is new.

## Open questions

- None. A screen-reader pass (NVDA, VoiceOver) after shipping confirms the result; the audit's P1 severity was provisional on it.
