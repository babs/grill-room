# 029 — Files in the chat, kept as documents in a Chat folder

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-06

## Problem

A participant who wants to show a screenshot or share a file during a session has to leave the chat, upload it in the Documents pane and then say in the chat what they just added. Pasting a capture is impossible anywhere in the room. Chat messages carry text only (`POST /api/rooms/{room}/messages`); images only reach annotation threads (001-AC20). The agent learns about the file from the Documents list alone, with no link to what was said about it.

## Solution

The chat composer takes files: paste a capture, drop files on the chat, or pick them with an attach button. They show as chips under the composer until the message is sent. Sending stores the message and each file as a room document, in one go. The documents land in a **Chat** folder of the Documents list. The message shows them under its text: images as thumbnails, other files as a chip with name and size. A click opens the document in the viewer, as from the list. The agent finds them in `uploads/chat/`, and the transcript line of the message lists their paths.

## Scope

- Composer: paste (images from the clipboard), drop on the chat pane, an attach button with a file picker.
- One request sends the text and the files; the message and its documents are stored together or not at all.
- Every format the Documents pane accepts, under the same per-file and per-room limits.
- A Chat folder in the Documents list (room page and docs window).
- Thumbnails and chips under a chat message.
- Agent workspace: `uploads/chat/`, the transcript lines, the prompt's workspace contract.
- Export and import of the folder and the message–document links.
- A one-shot screen capture button in the composer (user, 2026-11-06).

## Out of scope

- Folders made, renamed or chosen by participants. The store gets a `folder` column, but only `chat` is set, and only by this flow.
- Moving a document in or out of the Chat folder.
- Files in agent messages or through the MCP server (012): `upload_document` keeps writing to the root.
- Attaching files when editing a message: an edit changes the text only.
- Changing the annotation thread attachments (001-AC20).
- Image editing or cropping before sending.
- Live screen sharing and the camera: a separate feature, later. No video recording.

## Acceptance criteria

### Sending
- [ ] **AC1** The composer has an attach button (paperclip icon from the icon set, accessible name "Attach files", 44 × 44 px target at 390 px). It opens a file picker that takes several files.
- [ ] **AC2** Pasting into the composer when the clipboard holds an image adds it as a pending file named `capture-YYYYMMDD-HHMMSS.png` (browser local time; the image's own extension when the clipboard holds another accepted type, such as `.jpg`); pasted text still goes into the text as today, and a clipboard that also holds text (cells copied from a spreadsheet) pastes the text only. Dropping files anywhere on the chat pane adds them as pending files; the chat pane shows a drop state while files hover, and the Documents pane's drop (`room.js` zone) keeps working unchanged.
- [ ] **AC3** Pending files show as chips under the composer: name (ellipsis, full name in tooltip), size, and a ✕ that removes it. An image chip shows a small preview. At most 4 files per message: a fifth is refused on the spot with "At most 4 files per message". A file over the upload limit or with a format the Documents pane refuses is refused on the spot with the same text the server would give.
- [ ] **AC4** Send posts the text and the files in one `multipart/form-data` request to `POST /api/rooms/{room}/messages` (field `body`, fields `file`). A JSON body still works as today. A message with files and no text is accepted; a message with neither is a 400 as today.
- [ ] **AC5** The server stores the message, each upload and its document (folder `chat`) in one transaction. If any file fails (format 415, size 413, room quota 413 naming the room limit, conversion 422), nothing is stored and the answer names the file. A test injects a failure on the second of two files and finds no message, no upload and no document afterwards.
- [ ] **AC6** While the request runs, the composer shows the progress and blocks a second send; on failure the text and the pending files stay in the composer with the error, so a retry needs no re-pick.

### Showing
- [ ] **AC7** A message's JSON (list, live event, post answer) carries `documents: [{id, name, format, size, deleted}]` in the order sent. Under the text, an image document shows as a thumbnail no larger than 240 × 180 px (aspect kept, served from the existing original route); any other shows as a chip with the document icon, its name and size. A click on either opens the document in the viewer on the room page, and in the docs window from `/r/<room>/docs`.
- [ ] **AC8** A document renamed later shows its new name in the message. A deleted document shows as a greyed chip "Deleted document", not clickable, and no thumbnail request is made.
- [ ] **AC9** The Documents list shows a **Chat** folder row, with the disclosure triangle of 008-AC5, listing the chat documents in the list's order, folded by default, its state kept in this browser like the drawers. With no chat document, no Chat row shows. Documents added from the Documents pane stay at the root.
- [ ] **AC10** Deleting a chat message keeps its documents (they are room documents; room admins delete them as any other, 005-AC26). Editing a message keeps its documents.

### Agent
- [ ] **AC11** In the agent workspace a chat document's original is at `uploads/chat/<id>-<name>`, read-only like any upload, with the Markdown copy beside it for office files. Annotations created by the agent on `uploads/chat/<id>-<name>` paths resolve to the document (`annotations-new.json`).
- [ ] **AC12** The message's transcript line is followed by one indented line per document, `  - file: uploads/chat/<id>-<name>` (the `.md` copy for office files), and `  - file: (deleted) <name>` for a deleted one. The prompt's workspace contract names `uploads/chat/` as files participants sent in the chat, listed under their message in the transcript.
- [ ] **AC13** A second turn with no room change transfers no upload bytes (002-AC37 holds with chat documents).

### Data
- [ ] **AC14** Room export carries each document's folder and each message's document ids; importing it gives the same Chat folder and the same thumbnails and chips. `docs/export-format.md` documents both. Deleting the room removes the links (001-AC4).

### Screen capture (user, 2026-11-06)
- [ ] **AC15** The composer has a "Capture screen" button (icon from the icon set, 44 × 44 px target at 390 px, as the paperclip), next to the paperclip. It calls `navigator.mediaDevices.getDisplayMedia({ video: true })` once (with a `CaptureController` asking the browser not to move focus to the shared tab, where supported), draws the first frame into a canvas, stops every track of the stream at once, and adds the PNG to the pending files as `capture-YYYYMMDD-HHMMSS.png` (browser local time, the naming of AC2). It follows the rules of the other pending files (AC3: the limit of 4, the size).
- [ ] **AC16** The button is absent when `getDisplayMedia` does not exist or the device has no fine pointer (touch-only: phones, tablets without mouse or pen, since Android browsers may define `getDisplayMedia` only to refuse it); it comes back when a fine pointer is attached. A refusal or a cancelled picker leaves nothing pending and shows no error. Any other failure shows its error in the composer.

## Phases

### Phase 1 — Store and API
- Work: migration; `AddUpload` takes a folder; a store call that writes a message with its uploads in one transaction; multipart branch of `postMessage` reusing `readUpload`'s rules, `convert.Detect`, pandoc and the upload slots; `documents` on message JSON; folder on document JSON.
- **Covers**: AC4, AC5, AC7 (JSON part), AC8 (JSON part), AC10
- **Data model impact**: `documents.folder TEXT NOT NULL DEFAULT '' CHECK (folder IN ('', 'chat'))`; new `message_documents (message_id → messages ON DELETE CASCADE, document_id → documents ON DELETE CASCADE, position)`, primary key `(message_id, document_id)`.
- **DoD**: `go test ./internal/store/... ./internal/web/...` green, with the AC5 rollback test and a test per refusal shape (415, 413 file, 413 room, 422, 5 files).

### Phase 2 — Agent workspace and export
- Work: `uploadPath` for the chat folder; transcript lines; prompt contract; `docs/agent-workspace.md`; export/import and `docs/export-format.md`.
- **Covers**: AC11, AC12, AC13, AC14
- **Data model impact**: none
- **DoD**: workspace tests show `uploads/chat/<id>-<name>` and the indented `file:` lines; the cache test of 002-AC37 passes with a chat document; an export/import round-trip test keeps folder and links.

### Phase 3 — Room UI
- Work: attach button, paste, drop on the chat pane, pending chips, multipart send with progress, thumbnails and chips under messages, Chat folder in the Documents list.
- **Covers**: AC1, AC2, AC3, AC6, AC7, AC8, AC9
- **Data model impact**: none
- **DoD**: e2e (Playwright) at 1440 and 390 px: paste a PNG through a synthetic `ClipboardEvent`, drop two files, send; another participant sees the thumbnail and the chip live, opens both; the Chat folder lists them; a fifth file and an oversize file are refused on the spot; a failed send keeps the pending files. The existing e2e suite stays green.

### Phase 4 — Screen capture
- Work: the "Capture screen" button, one-shot frame grab, tracks stopped, PNG added to the pending files; hidden without `getDisplayMedia` or a fine pointer, shown again when one is attached.
- **Covers**: AC15, AC16
- **Data model impact**: none
- **DoD**: e2e (Playwright) in Chrome with `--auto-select-desktop-capture-source` / `--use-fake-ui-for-media-stream`, or a stub of `getDisplayMedia` if headless cannot capture (the one used is recorded here: a stub, a canvas stream; headless Chrome answers `NotReadableError` even with both flags): a PNG is pending, every track reads `readyState === 'ended'`, the message sends. At 390 px with no `getDisplayMedia`, and on a touch phone that defines it, the button is absent. A rejected picker (`NotAllowedError`) leaves nothing pending and no error.

## Data model impact (summary)

- `documents.folder` (new column, default `''`).
- `message_documents` (new table).
- Migration number: next free at implementation time (positional, in-app migrations).

## Open questions

- (none)

## Decisions

- Fixed Chat folder rather than general folders: the request names one sub-folder; the `folder` column keeps general folders a later UI change, with no migration.
- Files are uploaded with the message, not on pick: no orphan documents when someone drops a file and never sends.
- 4 files per message, as for annotation messages (`maxAttachments`).
- Deleting a message keeps its documents: they may already be annotated or edited by the agent, and documents have their own delete rule.
- Pasted capture name in browser local time: it is what the person sees on their clock.
- No image in the agent prompt itself: the agent reads the file from its workspace, as for annotation attachments.
- A chat message takes one server upload slot per file, as one upload does, so the server's memory bound stays that of 4 files at once; a message that finds no free slot is refused whole (503).
- A pasted image keeps its own type and extension rather than being re-encoded to PNG; a type the room refuses is refused on the spot like any file (review of phase 3).
- Screen capture is one shot, never a continuous stream: personas see the screen through the attachment (`uploads/chat/`), never live.
