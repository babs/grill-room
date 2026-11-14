# 037 — A room that fits tablets and phones

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-14
**Context**: UX audit part A, findings A4 and A5. Amends the room layout of `docs/design.md` §6 for 40–64rem with a document open, and the phone header and composer: 005-AC32 (header zones), 020 (chat filter), 030-AC14 (room name width at 390 px), 031 (raise hand), 034-AC1 (compact pills).

## Problem

- **Tablet with a document (A4).** At 820 px a document leaves the chat 188 px wide (composer 156 px): the placeholder is cut, Send wraps to its own row, lines hold 2 to 3 words. The side panel keeps 256 px. `docs/design.md` gives the chat a 22rem minimum and a collapsible sidebar on tablets, but nothing folds it.
- **Phone room (A5).** At 390 × 844, fixed chrome takes 338 px: header 96 (persona pills on a second row), chat filter 45 (shown even in a room without voice), composer 141 (a two-row box plus a tool row), tab bar 56. Messages get 506 px, 60% of the screen.

## Solution

- Between 40 and 64rem, opening a document folds the side panel to its existing 56 px rail; the chat keeps 22rem. Closing the document gives the panel back as it was.
- Under 40rem: the chat filter shows only when the room has voice; the composer rests on one row, with a "+" menu holding Attach, Capture and Raise hand; persona pills are always compact, on their own row.

## Acceptance criteria

- [x] **AC1** Between 40rem and 64rem, when a document opens in the room (not the docs window), the side panel folds to its 56 px rail (Documents and People both folded) if it was unfolded. When the last document closes, each section returns to the fold state it had before the document opened. A section the person unfolds while the document is open stays as they set it, and the chat then gives way as today.
- [x] **AC2** At 820 × 1180 and 768 × 1024 with a document open, the chat column is at least 22rem wide, the composer's Send sits on the textarea's row, and the page has no horizontal scroll. Under 40rem and from 64rem nothing changes.
- [x] **AC3** Under 40rem, the chat filter (All, Written, Voice) is hidden until the room has voice: a voice line has arrived, or someone is in the room's audio. It then shows, with the filter the person last chose (020), and stays for the page's life; until then the chat shows every line and the stored choice is kept.
- [x] **AC4** Under 40rem, the composer at rest is one row: a 44 px "+" button, the textarea one line high, Send. The textarea grows with its content up to 5 lines, then scrolls; it shrinks back after sending. The placeholder fits on one line at 390 px.
- [x] **AC5** Under 40rem, "+" opens a menu (kit popover, keyboard reachable, Escape closes, focus returns to "+") with Attach files, Capture screen (only where it is offered today) and Raise my hand / Lower my hand, each keeping its current behaviour and disabled state. While the person's hand is raised, the hand button also shows in the composer row so it can be lowered in one tap. From 40rem the composer is unchanged.
- [x] **AC6** Under 40rem, persona pills are always in compact mode (034-AC1) and keep their own row under the room name, as today; the header stays two rows. The row keeps a 44 px slot after the pills' scroll area for spec 035's "Add a persona" button (035-AC14), whether or not that button exists yet. 034's edge "+N" buttons and the header warning keep working there, and the room name keeps at least 6rem (030-AC14).
- [x] **AC7** At 390 × 844 in a room with three personas, no voice and no open audio, the message list is at least 75% of the viewport height (633 px). With the audio joined it is at least 65%.
- [x] **AC8** Every control touched here keeps a 44 px target under 40rem, a visible focus ring, and the existing `layout-guards`/`ui-guards` overlap checks stay empty.

## Phases

### Phase 1 — Tablet: fold the side panel for a document
- Work: `room.js` (remember fold state on document open, restore on close, 40–64rem only), `app.css` (chat 22rem minimum in that band), `docs/design.md` §6 tablet row.
- **Covers**: AC1, AC2
- **Data model impact**: none
- **DoD**: `e2e/layout-guards.spec.js` extended at 820 × 1180 and 768 × 1024: a document opened folds both sections, chat ≥ 352 px, Send on the textarea row, no horizontal scroll; closing restores the previous folds; a section unfolded while open stays unfolded; 390 and 1440 unchanged. `docs-window`, `documents`, `mobile` green.

### Phase 2 — Phone: one-row composer, compact pills on their own row
- Work: `room.html` ("+" button and menu), `room.js` (filter visibility from voice state, composer autosize, menu wiring, hand mirror, forced compact pills under 40rem), `app.css` (phone pills row, composer row), `docs/design.md` header and composer rows.
- **Covers**: AC3, AC4, AC5, AC6, AC7, AC8
- **Data model impact**: none
- **DoD**: `e2e/mobile.spec.js` extended at 390 × 844: filter hidden without voice, shown once a voice line arrives or someone joins the audio, and stays once they leave (AC3); composer height ≤ 60 px at rest, grows to 5 lines then scrolls, shrinks after send (AC4); "+" menu by keyboard and touch, each item works, raised hand mirrored in the row (AC5); compact pills on their own row, the 44 px slot, "+N" present with 9, room name ≥ 96 px (AC6); message list ≥ 633 px, ≥ 549 px in the audio (AC7); 44 px targets and empty overlaps (AC8). `design.spec.js`, `ui-guards`, `layout-guards`, `chat-filter`, `hands`, `chat-files`, `screen-capture` green.

## Out of scope

- The docs window's own layout, the annotations panel, the Video stage.
- Hiding the persona bar on the Docs and People tabs.
- A tablet layout without a document open.

## Decisions

- The rail and its fold classes already exist (`app.css` `fold-docs`, `fold-people`): the tablet fix reuses them instead of a new collapsed sidebar.
- AC6 amended during phase 2: pills in the header's first row do not fit at 390 px. With voice the audio bar and the 6rem name leave them 0 px; without voice the bar is 138 px and every 108 px compact pill reaches under an edge "+N", so none can be tapped (034-AC4). The pills keep their own row; AC7 still holds through the one-row composer and the hidden filter.
- "Has voice" for the filter = a voice line has arrived in the room, or someone is in its audio (presence). `data.voice` only says the server offers audio, true for every room on a deployment with LiveKit, so it is no signal. Once shown, the filter stays for the page's life.

## Open questions

- None.
