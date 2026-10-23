# 008 — Docs window polish and push opt-in explainer

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-13
**Context**: follow-up to 005: the docs window (005-AC11, 005-AC37), document rename and delete (005-AC25, 005-AC26), push (005-AC24)

## Problem

The document manager works, but reading in it is awkward:
- In the docs window with no document open, the list stretches across the whole page. Once a document opens, the list and the Notes pane have fixed widths that nobody can adjust.
- The list cannot be put away, so a long document never gets the full width.
- The docs window offers no way back to the room.
- Markdown is capped at 896 px, even on a wide screen with everything else folded away.
- The viewer toolbar holds six controls. On a phone, the title wraps over three lines ("plan." / "md" / "added by Alice").
- "Turn on notifications" fires the browser's permission prompt with no word of what the notifications are for. A refused prompt is hard to undo, as the browser remembers the refusal.

## Solution

The docs view works like a master/detail app. The documents list has a steady width that you can drag. You can fold it into a rail and bring it back. On the room page, People and Documents fold one by one, and Notes keeps its own toggle. The Notes pane resizes like the list. Your widths and what you folded stay in this browser. In the docs window, the room's name leads back to the room. A width button switches Markdown between narrow, large and full, as in gld, and remembers your choice. Download, rename and delete move into a `⋯` menu on the document's toolbar. On a phone, the docs view is one pane at a time: list, then document, then notes, and Back steps through them in reverse.

Before the browser asks for permission, the room explains what notifications are for and asks "Turn on notifications" or "Not now". "Not now" keeps the room quiet about it for 14 days.

## Scope

- The documents list and the Notes pane in the docs window (`/r/<room>/docs`) and in the room page (the side panel `#side` and `#annotations`).
- Resizing, folding and storing them (desktop), and single-pane navigation (phone).
- A link back to the room from the docs window's header.
- A width toggle for Markdown documents.
- A `⋯` menu in the viewer toolbar.
- The push explainer, which applies wherever push is turned on, and a single offer of it after the participant's first message.

## Out of scope

- Resizing the chat, or the tablet layout (40–64rem) beyond folding the list.
- A width toggle for HTML documents, PDFs or images: their own layout rules them.
- Storing pane widths on the server or in the profile: they stay per browser.
- Translating the UI: it stays English (`lang="en"`). The French wording in the notes guided the English text.
- Changing which events send a push (005-AC24 stays as is).

## Acceptance criteria

Widths are measured with Playwright bounding boxes. "Desktop" is a 1440 × 900 viewport, "phone" 390 × 844. Stored state lives in `localStorage`, per browser, and is shared by every room, the room page and the docs window.

### Panes (desktop)
- [x] **AC1** With no document open in the docs window, the documents list is 256 px wide (16rem, ±1). The rest of the page shows an empty state, "Pick a document to read it here.", not a stretched list. Opening and closing a document leaves the list's width unchanged.
- [x] **AC2** A drag handle sits on the list's right edge, in the docs window and on the room page (where the list is the side panel). Dragging it resizes the list between 192 px (12rem) and 480 px (30rem). A drag to 100 px stops at 192, and a drag to 900 px stops at 480. The handle is a focusable `role="separator"` with `aria-valuenow`, `aria-valuemin` and `aria-valuemax`, and ← / → move it by 16 px. Its hit area is at least 12 px wide and runs the pane's full height.
- [x] **AC3** The Notes pane has the same kind of handle on its left edge, with a range of 256 px (16rem) to 640 px (40rem) and 320 px (20rem) by default. In the docs window, and on the room page with the chat folded, the document always keeps at least 384 px (24rem). When the window narrows, Notes gives way first, then the list, each down to its minimum. The stored widths stay unchanged.
- [x] **AC4** After a reload, the list and Notes come back at the widths last dragged (±1 px), in the docs window, on the room page and in another room. A resize under 64rem never overwrites a stored width.
- [x] **AC5** From 40rem up, each drawer of the side panel has a disclosure triangle at the start of its header: ▾ while open, ▸ (the same glyph turned in CSS) once folded, its state in `aria-expanded` and its name the drawer's (Documents, and on the room page People). A click anywhere on the header, its own actions aside, does the same. Drawers fold independently; a folded one keeps only its header, and the other takes the height. Each header always shows its section's icon before the title (👥 People, 📄 Documents, 💬 Notes), folded or not. Once every drawer shown is folded, the side panel narrows to a 56 px rail of triangles and icons: in the docs window that is Documents alone. In the docs window, folding the list grows the document's bounding box by the list's width minus 56 (±1); on the room page the viewer and the chat share that width. Unfolding brings the panel back at its stored width. Notes folds with its toolbar button, which already remembers its state (005-AC37). Each fold state survives a reload. (user, 2026-10-13: triangle and icons replace the `«` button and the icon swap.)

### Header, viewer
- [x] **AC6** In the docs window, the room's name in the header is a link to `/r/<room>`, titled "Back to the room", at least 44 px tall at 390 px. Following it opens the room in the same window. On the room page, the name stays plain text.
- [x] **AC7** With a Markdown document open, the toolbar shows a width button that cycles Narrow → Large → Full. At a 1920 × 1080 viewport, with the list folded and Notes closed, the document's `.container` measures at most 896 px on Narrow, more than 896 and at most 1400 px on Large, and the full width of the frame less its padding on Full. The choice survives a reload and applies to every Markdown document, including Word or slides shown as their Markdown copy. Changing it does not reload the frame: a marker set in the frame survives, and so does its scroll position. For an HTML document, a PDF or an image, the button is hidden.
- [x] **AC8** The viewer toolbar shows, in order, ✕, the title and its meta, then Notes, the width button (AC7), fold chat (room page, 005-AC37) and one `⋯` button. Download, Open in a new window, Rename and Delete move into the `⋯` menu, which keeps their rules: Download for every document (an upload its original file, any other the revision on screen as its text; user, 2026-10-23), Delete for room admins only (005-AC26), Open in a new window absent from the docs window. With the keyboard alone, Tab reaches `⋯`, Enter opens the menu and focuses its first item, Tab or ↓ moves through the items, Enter runs one, and Escape closes the menu and returns focus to `⋯`. At 390 px, the title fits on one line, with an ellipsis and the full name in a tooltip, and the toolbar is a single row.

### Phone
- [x] **AC9** Under 40rem, in the docs window and in the room's Docs tab, one pane shows at a time: list, document or Notes, each at full width. Opening a document replaces the list, and Notes replaces the document. Notes has a "← Back to the document" button. The browser's Back (Android's included) goes Notes → document → list, and then leaves the docs view as it does today. No resize handle and no fold button are visible (their bounding box is null), and every control of the three panes is at least 44 × 44 px.

### Push explainer
- [x] **AC10** `Notification.requestPermission` is called only from the explainer's "Turn on notifications" button, never anywhere else. The explainer is a sheet:
  - title "Turn on notifications?";
  - body "Get notified when someone mentions you directly, even when this tab is closed.";
  - hint "Your browser will ask for permission next.";
  - buttons "Not now" (secondary) and "Turn on notifications" (primary).

  A spy installed with `addInitScript` records every call. It shows 0 calls before the click on the primary button and exactly 1 after it. "Not now", Escape, a backdrop tap and ✕ each leave 0 calls. When the permission is already `granted` (from another room), turning on notifications subscribes with no explainer and no call. When it is `denied`, nothing is called, and the Notifications sheet keeps its text on how to allow notifications in the site settings.
- [x] **AC11** The explainer is also offered once, unprompted, right after the participant's first message in a page load. It shows only if every condition holds: push is supported, the permission is `default`, push is off for this room, the page is not the docs window, the browser is not an iPhone or iPad outside the home screen, and no snooze is running. "Not now", Escape, a backdrop tap or ✕ start a 14-day snooze, which covers every room in this browser. With Playwright's clock: no unprompted explainer after a reload and a message 13 days 23 h later, and one at 14 days 1 min. Through the snooze, the Notifications sheet's button still opens the explainer. Turning notifications on clears the snooze.

### People panel (user, 2026-10-13)
- [x] **AC13** Each person in People is one row: the status dot, the name (one line, ellipsis), a group of badges that never wraps, then `⋯`. Every badge is a 20 × 20 px square with its icon centred, a tooltip and an accessible name: roles (● you, ⚙ AI host, ★ admin), ✦ can ask the AI, ✍ transcribed, and the audio state. Badges sit in fixed columns: a column shows on every row of the list once any row has its badge, so the same badge has the same x and size on every row. No setting sits in a row: `⋯` opens the person's menu, reachable and usable with the keyboard alone (Enter opens it on its first item, ↑ ↓ move, Escape returns to `⋯`). It holds, as they apply to the viewer: Rename and Make or Remove room admin (room admins), Can ask the AI as a toggle and Make AI host (the AI host), and the volume (in the audio). A person the viewer can do nothing about has no active `⋯`. Checked at 390 and 1440 px: one row height, one line, the badge columns aligned.
- [x] **AC14** A drawer header's controls (the triangle, Open documents in a new window and +) share one style: 24 px squares to the eye, no border, the primary text colour, each with a 44 px target kept by an `::after` around it. "Open in a new window" is the external-link icon (a square with an arrow out of its top-right corner, an inline SVG drawn in `currentColor`) wherever it appears; ⧉ is gone. A document's name in the list is one line with an ellipsis, the full name in its tooltip.
- [x] **AC18** (user, 2026-10-16; replaces the you, admin and can-ask badges of AC13, 005-AC14's role badges and 005-AC7's "seen" in the row) A People row shows only the audio dot, a one-glyph sigil column (`@` for a room admin, empty otherwise), the name, the AI host's badge, the transcribed and audio badges, and `⋯`. You are marked by your name in bold and the accent text colour. The name always shows: it truncates with an ellipsis before the badges give way and never drops to 0 width. "Seen …" and "Can ask the AI" move to the details card, which says "Seen 5 days ago" in words and keeps the joined date, so two people of one name can be told apart. Checked with a 260 px side panel and at 390 and 1440 px: every name visible, the names' left edges equal (±1 px) with and without `@`, no "seen" in a row, no you, admin or can-ask badge.

### Hosting a room from a running AI host app (user, 2026-10-13)
- [x] **AC15** Any link inside a room names that room: `/r/<room>`, `/r/<room>/`, its docs window `/r/<room>/docs[/<doc>]`, any trailing path, query or fragment. One parser (`daemon.ParseRoomURL`) reads it for the command line, a second launch's handoff, the app page's "Add a room" and the worker; `install.sh` keeps the same rule. A link with no `/r/<room>` (`/admin`, any other path) is refused with a message that says it is not a room link. The AI host app's page, Rooms tab, always offers "Add a room": the room goes to the supervisor exactly as `grill-room-daemon <room-url>` does, also while another room is connected and before any room, and the page then shows that room's pairing state, its code and the "approve in the room" link.
- [x] **AC16** The room page's `⋯` › "Run the AI on my computer" is always there (outside the docs window) and explains the two ways: copy this room's link and paste it in the AI host app's page (Rooms, Add a room), or install the app (the download and the one-liner, when the server hands out a release).

- [x] **AC17** After an approval in the room, the "Connect the AI host app" notice stays until the approved app is connected to the room (its daemon listed), then goes. Its ✕ dismisses it at any time.

### Design
- [x] **AC12** Everything new follows `docs/design.md`: tokens only (`TestPageCSSHasNoColourLiterals` and `TestTokenContrast` green), `--control-h` buttons, the `.menu` and `.sheet` classes, the focus ring on the handles, sentence case and the wording table. Handles and rails show no motion under `prefers-reduced-motion`. The 005-AC30 bounding-box spec covers the new controls: 44 × 44 at 390 px, one height at 1440 px.

## Phases

### Phase 1 — Docs panes, header, viewer
- Work:
  - Pane widths as CSS custom properties set from `localStorage`.
  - A separator handle shared by the list (`#side`) and Notes (`#annotations`), and a fold button per drawer of `#side`, on the model of `#chat-fold` / `#chat-unfold`.
  - The viewer's empty state in the docs window.
  - The `#room-name` link in docs mode.
  - The width button: the first paint from a `width` query on `/view`, read by `templates/doc.html`, then later changes as a message to the frame. This mirrors gld's `data-width` on `:root`.
  - The `⋯` popover menu holding `#doc-original`, `#doc-window`, `#doc-rename` and `#doc-delete`.
  - The phone's single-pane flow, with history entries so Back steps through it.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC12
- **Data model impact**: none
- **DoD**:
  - The e2e spec for the panes is green: bounding boxes at 1440 px for the default, minimum and maximum widths, the clamps and the fold; a reload for persistence; 1920 px for the three Markdown widths and the button hidden for HTML; keyboard-only for `⋯`; 390 px for list → document → Notes and `goBack()`.
  - `design.spec.js`'s AC30 checks are extended to the new controls.
  - The two CSS tests are green.
  - Before and after screenshots of the docs window (desktop, phone) are in the tracking file.

### Phase 2 — Push explainer
- Work: the explainer sheet in `room.html`; `#push-toggle` opens it instead of calling `Notification.requestPermission`; the unprompted offer after the first message, and the snooze key.
- **Covers**: AC10, AC11, AC12
- **Data model impact**: none
- **DoD**:
  - The e2e spec for the explainer is green: the `requestPermission` spy for Turn on, Not now, Escape, the backdrop and ✕; the `granted` and `denied` paths; the snooze checked with `page.clock` at 13 days 23 h and 14 days 1 min.
  - The existing push e2e (local test push endpoint) passes with the explainer in the flow.
  - The room server is redeployed on the lab, the database backed up first.

### Phase 3 — People panel and hosting from the app's page
- Work: one-row People entries and one icon-button style in the drawer headers; "Add a room" on the AI host app's page; the two ways in the room page's hosting sheet.
- **Covers**: AC5, AC13, AC14, AC15, AC16, AC17, AC12
- **Data model impact**: none
- **DoD**:
  - Playwright: one row height per person at 390 and 1440 px, no wrap; one style for the header actions.
  - Table-driven tests of the room link parser (Go) and of `install.sh` with the same links; a Go test of "Add a room" before any room and while another is connected; an e2e with the fake daemon: start it on a docs-window link of room A, approve, the notice goes once connected; add room B from the page with a document link, approve, both rooms listed.
  - The room server redeployed on the lab, the database backed up first.

## Data model impact (summary)

None. All new state is client-side `localStorage`: the list width, the Notes width, the folded drawers, the Markdown width and the push snooze.

## Open questions

- None blocking. The decisions below are routine calls the user can overturn.

## Decisions

- The snooze lasts 14 days per browser, not per room: the browser grants the permission per site, so asking room by room would nag more. The Notifications sheet is the way in during the snooze.
- The unprompted offer comes after the first message in a page load, a moment the participant has shown they take part in the room. This mirrors the recovery notice (005-AC32).
- The primary button reads "Turn on notifications", not "Enable". `docs/design.md` §8 says an action keeps its name all through a flow, and the Notifications sheet's button already uses that name.
- Pane widths are shared by every room and by both surfaces, not stored per room. The same reader wants the same layout everywhere.
- Folding (grill, user): in the docs window the documents list folds to a rail. On the room page each drawer folds on its own (Documents, People and Notes), and the side panel becomes the rail only once both of its drawers are folded. Notes already folds with its toolbar button and stores that, so it gets no second control. Fold states are per browser. The phone's tabs are unaffected.
- Push offer (grill, user, option B): the explainer comes once, unprompted, after the participant's first message; "Not now" snoozes it for 14 days in this browser; the Notifications sheet reaches it at any time.
- With the chat shown, the room page keeps the chat's share of the width rather than the document's 384 px floor: at 1024 px with Notes open, the floor would leave the chat about 127 px. Folding the chat restores the floor.
- Resizing starts at 64rem (desktop) and folding at 40rem, matching the breakpoints in `docs/design.md` §6. The phone gets neither.
- The Markdown widths follow gld: 896 px (the stylesheet's current cap), 1400 px, then no cap. They are labelled Narrow, Large and Full, as in the notes.
- "Open in a new window" also moves into `⋯`, which frees space on a phone. The notes named only download, rename and delete.
- The room's name opens the room in the same window rather than focusing the opener: a plain link works when the opener is closed too.
- AC16 keeps the label "Run the AI on my computer" from `docs/design.md` §8, which replaced "Host the agent"; the request named the entry "Host this room's agents".
