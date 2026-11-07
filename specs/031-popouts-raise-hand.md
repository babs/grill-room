# 031 — Raise hand, a chat window, a pop-out video

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-07
**Context**: builds on 005 (presence, People list), 008 (docs window), 029 (files in the chat), 030 (Video stage). 032 (Timekeeper) reads this spec's hand queue.

## Problem

In a call of six or more, people talk over each other to get a word in, and the quiet ones never do: there is no way to ask for the floor without speaking. Someone who keeps another tool or document full screen loses the room: the chat lives only in the room page, and the Video stage disappears behind the other window.

## Solution

Anyone in the room can **raise their hand**, in the audio or not. Everyone sees the queue: a hand icon with its rank next to each name in People, and a count. You lower your own hand; a room admin can lower anyone's. Hands never drop on their own while you stay in the room.

The chat can open **in a window of its own**, as the documents already can: the whole chat, writing, files and the hand button included, with no audio. The call stays in the room page.

The Video stage can **pop out** into a small always-on-top window, with the mic, camera and hand controls, so the call stays in view over another app. It is the same call in the same page: nobody joins twice.

## Scope

- A hand per participant per room: raise, lower your own, a room admin lowers anyone's. One shared queue, ordered by raise time, across pods.
- A hand icon and rank in People, a count on the People pane head and the People tab, a raise/lower button by the chat composer.
- `/r/<room>/chat`: the room page in a chat mode, on the docs window's pattern (008). An "Open chat in a new window" entry in the room menu from 40rem up.
- A "Pop out" button on the Video stage: Document Picture-in-Picture (Chrome, Edge) with the stage and the call controls; the native video Picture-in-Picture of the large video where Document PiP is missing (Safari).

## Out of scope

- Lowering a hand automatically on speaking, posting, or joining or leaving the audio (decided with the user). Only leaving the room lowers it (AC6).
- Notifying anyone of a raised hand: no push, no sound, no system message.
- Hands in the transcript, the export, or a persona's prompt or workspace. 032's time-keeper capability is the one exception, specified there.
- Audio or video in the chat window. A second window that joins the call kicks the first: LiveKit drops a duplicate identity, and the identity is the participant id.
- A pop-out in Firefox: it has no Picture-in-Picture API, only its own built-in toggle on videos.
- Layouts, speaker view, push-to-talk, keyboard shortcuts (not requested).

## Acceptance criteria

### Raise hand
- [ ] **AC1** `PUT /api/rooms/{room}/hand` raises the caller's hand and `DELETE /api/rooms/{room}/hand` lowers it; both are idempotent, and raising a raised hand keeps its first raise time. `DELETE /api/rooms/{room}/hands/{participant}` lowers someone else's hand: a room admin gets 204, anyone else 403. Each route answers a non-participant as the room's other participant routes do. A removed participant (016) loses their hand.
- [ ] **AC2** The queue is ordered by raise time, earliest first, and is the same on every pod: a hand raised through pod A shows in a page served by pod B within 2 s (017).
- [ ] **AC3** By the chat composer, a button "Raise my hand" / "Lower my hand" (`aria-pressed`) works in or out of the audio, 44 × 44 px at 390 px.
- [ ] **AC4** In People, each raised hand shows a hand icon and its rank (`2`, `aria-label` "Hand raised, 2nd"), and the People pane head and the phone's People tab show the count of raised hands while it is above zero. The list keeps its order.
- [ ] **AC5** A room admin has "Lower hand" on each raised hand's person row; the participant has it on their own row. Nobody else has it.
- [ ] **AC6** A hand stays raised through speaking, posting, joining and leaving the audio, and a reload within the presence grace. When the participant has had no live tab for longer than the grace (20 s, 005), their hand is lowered.
- [ ] **AC7** Raising or lowering a hand writes no message, no audit entry and nothing to the export. No persona prompt, preamble or workspace file mentions hands (032 adds the exception).

### Chat window
- [ ] **AC8** `GET /r/{room}/chat` serves the room page in chat mode, behind the same access as `/r/{room}`. From 40rem up, the room menu has "Open chat in a new window"; a second click focuses the window already open for that room.
- [ ] **AC9** The chat window shows the chat only: messages, the Written/Voice filter (020), the composer with mentions, attach and paste (029), and the hand button (AC3). It has no audio bar, no Video stage or button, no documents pane, no People pane, no persona strip. A document link in a message opens it in the docs window (008).
- [ ] **AC10** The chat window never asks for a voice token (`GET /api/rooms/{room}/voice`): with the room page in the audio, opening and using the chat window leaves the call connected and the LiveKit room's participant count unchanged.
- [ ] **AC11** The chat window counts as one of the participant's tabs for presence (005), and its title reads `Chat · <room> · grill-room` with the unread count before it, as the room page's does.

### Pop-out video
- [ ] **AC12** Where `window.documentPictureInPicture` exists, the open Video stage has a "Pop out" button. It opens an always-on-top window showing the stage with 030's layout rules (screen share large and camera row, or camera grid, names, speaking ring, mirrored self-view), and the controls: mute, camera, screen share where 030 shows it, raise hand, leave the audio. The window follows the room's light or dark theme.
- [ ] **AC13** While the pop-out is open, the room page's stage says "Video is in the pop-out window" with a "Bring it back" button, videos stay subscribed whatever the page's stage does (030-AC13), and the Video header button and the phone tab keep their count. The page holds one LiveKit connection: the room's participant count is unchanged by the pop-out.
- [ ] **AC14** Closing the pop-out, "Bring it back", or leaving the audio closes the window and puts the stage back in the page as it was, open or closed. Leaving the audio from the pop-out's own button ends the call as the page's button does (030-AC5).
- [ ] **AC15** Without Document PiP but with `document.pictureInPictureEnabled`, "Pop out" puts the large video in the browser's own Picture-in-Picture: the large screen share, or with cameras only, the first camera tile. With neither API the button is absent.

## Phases

### Phase 1 — Raise hand
- Work: the hand table and its store calls; the three routes; the queue in the "presence" event; lowering on removal and on leaving past the grace; the button, the People badge and rank, the counts, "Lower hand".
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7
- **Data model impact**: a new table `hands` (room, participant, raised at), one row per raised hand, by a positional in-app migration.
- **DoD**: `go test -p 1 ./internal/store/ ./internal/web/... ./internal/uikit/` green, with route tests for each status of AC1, a two-pod test for AC2, a lapse test for AC6 and a preamble/export test for AC7. A new `e2e/hands.spec.js`: A and B raise in turn, both see ranks 1 and 2; B lowers; the admin lowers A's; A reloads and keeps the hand; at 390 px the button is 44 px and the People tab shows the count. `e2e/twopod` gains a hand raised on one pod and seen on the other. The main specs touching the room UI stay green: `e2e/design.spec.js`, `e2e/presence.spec.js`, `e2e/person-card.spec.js`, `e2e/mobile.spec.js`, `e2e/layout-guards.spec.js`, `e2e/ui-guards.spec.js`.

### Phase 2 — Chat window
- Work: the route; the chat mode in `room.js` and `app.css`; the menu entry; document links to the docs window; the title.
- **Covers**: AC8, AC9, AC10, AC11
- **Data model impact**: none
- **DoD**: `go test -p 1 ./internal/web/... ./internal/uikit/` green with a route test (access as `/r/{room}`). A new `e2e/chat-window.spec.js`: open from the menu, the second click focuses the same page; write, mention, attach and paste a file, filter, raise a hand there and see it in the room page; no audio bar, Video or People in the window; a document link opens the docs window; the title. `e2e/voice-error.spec.js` (fake LiveKit) gains AC10: the room page in the audio, the chat window open, no second voice token asked and the call still up. `e2e/docs-window.spec.js`, `e2e/chat.spec.js`, `e2e/chat-files.spec.js`, `e2e/chat-filter.spec.js`, `e2e/design.spec.js` stay green.

### Phase 3 — Pop-out video
- Work: the button; the Document PiP window with the stage moved into it and the room's stylesheets and theme; the controls; "Bring it back"; subscription while popped out; the native PiP fallback.
- **Covers**: AC12, AC13, AC14, AC15
- **Data model impact**: none
- **DoD**: `e2e/video/video.spec.js` extended (Chrome has Document PiP; the test opens it from a user gesture): A pops out and the window shows B's camera and screen share large; mute, camera and hand from the window act on the call; the page shows "Bring it back"; the LiveKit room still counts two participants; closing the window brings the stage back; leaving the audio from the window ends every local track (`readyState === 'ended'`); with `documentPictureInPicture` deleted by an init script, "Pop out" calls `requestPictureInPicture` on the large video; with both deleted, no button. `go test ./internal/uikit/` and `e2e/design.spec.js` green. Then by hand on the lab: Chrome and Safari each pop out over another app for 2 minutes. The report goes in the tracking file.

## Data model impact (summary)

One new table `hands`: room, participant, raised at (ms), primary key (room, participant). No backfill. Rows go when the hand is lowered, the participant lapses past the grace or is removed, or the room is deleted.

## Open questions

- (none)

## Decisions

From the user:
- Anyone in the room raises a hand, in the audio or not. Everyone sees the ordered queue. You lower your own, a room admin lowers anyone's. Lowering is manual only: never on speaking or posting.
- Leaving the room lowers the hand (AC6), so the queue holds no hand of someone gone (2026-11-07).
- Hands stay out of the transcript, the export and the personas, except a persona with 032's time-keeper capability.
- The chat window carries the whole chat with no audio; the call stays in the room page.

Made while drafting (overturnable):
- The hand button sits by the chat composer, which every participant has on every width, rather than in the header: at 390 px the header has no room left (030-AC14).
- Hands ride the existing "presence" event, polled from the shared table like presence (017), rather than a new event or a NOTIFY channel: same path, same 1 s delay.
- The People list keeps its order and shows the rank on the badge, so people do not jump around the list as hands go up and down.
- The hand is a kit SVG icon, not the ✋ emoji: docs/design.md bans emoji as UI icons. Found in phase 1.
- The pop-out uses Document PiP in the same page, so the LiveKit connection and the participant identity stay single. A second page would kick the first (see Out of scope).
- The chat window opens document links in the docs window: it has no viewer of its own, and the docs window is already one per room.
