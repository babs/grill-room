# 042 — Call and voice-to-text overview, and help

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-15
**Context**: UX audit part B, directions S6 (voice sheet) and S7 (help); journeys 1, 9, 10. Builds on 005-AC23 (words to recognise, any member), 010 (voice-to-text on this device), 013 (call voices, persona voices, pronunciation), 038 (vocabulary, "Not available on this server"), 039 (`#ai` readiness, host and ask derivation), 040 (AI in this room tabs). Council review (Codex, Fable) on 2026-11-15.

## Problem

What the call does with a PM's voice is set in six places with three owners: the audio bar, the Audio options menu, AI in this room › Words to recognise and › Call voices, a persona's sheet (voice, rate) and `/admin` (pronunciation). Some vanish or read "Not available" without saying who could change that.

The product has no help. Its core words are explained only where they first appear, if at all, and "what can I do here" depends on roles the page never names.

## Solution

- **Overview:** "Call and voice-to-text" lists, read-only, what applies to this room: the viewer's state in the call, the room's settings, the personas that speak, the server's engines. Each row names who changes it and links to where, or says why it is absent. It holds no control of its own.
- **Help:** "Help" in the avatar menu: the viewer's roles in this room in words, and short definitions of the product's words.

## Scope

- Two read-only sheets, `#call-overview` and `#help`, and their menu entries.
- The help text below, in one HTML file the vocabulary test reads.

## Out of scope

- Moving or duplicating any control: joining, muting, transcribing and the device settings stay in the audio bar and its menu.
- Devices (mic, camera, speaker), a person's Volume, the chat filter (020), Transcription text size (028): not listed in the overview.
- Searchable docs, tours, translations.

## Acceptance criteria

### Call and voice-to-text

- [x] **AC1** "Call and voice-to-text…" sits in the Audio options menu (after its toggles) and in the room menu (after "Personas", before the window and AI host entries; 041's "Switch room…" stays last). Both open `#call-overview`, a dialog with that id, titled "Call and voice-to-text", full screen on a phone. It opens when the server has no audio too, with every row reading "Not available on this server". In an ended room, docs and chat windows it opens nothing and the hash goes, as `#ai` does.
- [x] **AC2** Section "You", state lines only, read from the same effective state the audio bar uses (it tells "on" from "writing now"):
  - not in the call: one line, "Not in the call. Join it from the audio bar.", and nothing else in the section;
  - in the call: "In the call · mic on / muted · hearing the call / stopped hearing the call";
  - "Your voice is written down now" only while transcription is on, the mic on and voice-to-text running; "Transcription is on, waiting: <your mic is muted | voice-to-text is off | voice-to-text is not in the call | voice-to-text is in the call but not writing down your voice | the model is downloading | this device is testing the model>" while it is on but not writing, "Loading…" before the first voice-to-text state; "Your voice is not written down" while it is off;
  - "Voice-to-text runs on this device" while the device's pipeline holds the voice, else "on the server" (a fall back, 010-AC17), only where 010 offers the choice.
  Each line follows the audio bar live.
- [x] **AC3** Section "This room": "Words to recognise: N words" (from the room's glossary) linking to AI in this room › Words to recognise, "Who: any member"; "Call voices: pause <quiet> s, resume <resume> s, queue <queue> s, up to <words> words", "Who: room admins", linking to › Call voices for room admins only (plain members have no such tab, 040-AC3); then one row per persona: "<badge> <name>: speaks in the call with <voice>, rate <rate>" or "stays silent in the call", linking to its sheet's Identity tab, "Who: whoever may edit the persona" (013-AC26). Without a worker that reads the glossary, Words to recognise gives its tab's reason, and shows its count until the voice-to-text state is known; without persona voices, Call voices and each persona row say so (AC5).
- [x] **AC4** Section "This server": "Voice-to-text": "Available", "Loading…", "Voice-to-text is off" or "Not available on this server" (038-AC12's texts); "Persona voices": "Available" when the server has call voices, else "Not available on this server"; "Pronunciation: set by the server admin, on the server's admin page." No link to `/admin`.
- [x] **AC5** A row whose setting is absent stays and says why: "Not available on this server: the server admin turns it on." A row the viewer cannot change names who can ("Who: room admins").
- [x] **AC6** Rows follow the room, agent, voice-to-text and call events live: a join, a mute, a glossary edit, a persona's voice changed in another session update the row without polling.
- [x] **AC7** One return rule for every link out of the overview and Help (AC9): the link opens its target as a new history entry and closes the origin sheet; closing the target (Escape, ✕, outside, or Back) returns to the origin sheet's hash with the focus on the row that linked. Forward after that reopens the target. A target loaded directly has no origin and closes as it does today. A row gone meanwhile (a persona removed) hands the focus to the origin sheet's title. 040-AC8's return to `#ai/personas` applies when the persona sheet was opened from there. Closing the overview or Help returns the focus to its opener, or to "Join the call" when the opener is gone (leaving the call hides the Audio options button).

### Help

- [x] **AC8** "Help" sits in the avatar menu, before "My rooms and profile", and opens `#help`, a dialog titled "Help" with two sections, "What you can do here" and "Words". The avatar menu exists in every mode (`showMe`), so Help opens in docs and chat windows too. In an ended room `#help` opens nothing and the hash goes, as `#ai` does.
- [x] **AC9** "What you can do here" shows the lines that apply, in this order, and follows host transfers, ask permission and room admin changes live. The ask line is 039's own "ask" row text (`room.js`, readiness rows), not a copy:

  | When | Line | Link |
  |---|---|---|
  | always | "You are in this room: you read, write, add documents and notes, and talk to the personas by @name." | none |
  | always | 039's ask row: "You can ask the personas.", the `you_host` seat's line, "Whoever runs the AI decides…" or "Alice decides who can ask the personas. You can still mention them in a message." | none |
  | no host | "No one runs the AI for this room yet." | "What's missing?" (`#ai`) |
  | room admin | "You are a room admin: you rename the room, manage members and invites, and delete the room." | Room settings |
  | AI host, or `you_host` | "You run the AI for this room: you approve its personas, skills and websites." | AI in this room |
  | the profile has no email (`noEmail()`) | "Your profile has no email: add one to keep your rooms across devices." | `/profile#email` |
  | last room admin | "You are the last room admin: make someone else room admin before you leave." | Room settings › Members |

- [x] **AC10** "Words" lists these definitions, one line each, in this order:
  - **Room**: a place where people and personas talk, with its chat, documents and call.
  - **Persona**: an AI with a name, a prompt and a model, who answers in the room when asked or on its own.
  - **AI host**: the person whose computer runs the personas. **AI host app**: the app the AI host runs on their computer for the personas.
  - **Call**: the room's audio, and its cameras and screens.
  - **Voice-to-text**: what writes the call down as lines in the chat.
  - **Note**: a comment pinned to a part of a document.
  - **Starter**: a saved room setup (agenda, personas, skills) to start new rooms the same way.
  - **Skill**: a set of instructions and files a persona can use.
  - **Plugin**: code from a git repository that gives a persona tools, run by the AI host.
  - **Linked room**: another room whose chat and documents this room's personas can read.
  - **Room admin**: a member who manages the room, its members and invites.
  - **Server admin**: the person who runs this grill-room server.
- [x] **AC11** The help text is a `<dl>` inside the Help dialog in `room.html`, not a fetched file, with each term in `<dt>` and capitalised. 038's vocabulary test reads it; the DoD shows it red on a replaced word written there as a lone lowercase term.

## Phases

### Phase 1 — Call and voice-to-text
- **Covers**: AC1 to AC7
- **Data model impact**: none
- **DoD**: e2e (`--workers=1`): server with and without audio, voice-to-text and persona voices; not joined, joined, muted, stopped hearing, transcription on while muted and while voice-to-text is off; a plain member and a room admin (Call voices link absent for the member); a glossary edited in another session; AC7's returns, a persona removed while its sheet is open, Back and Forward; captures at 390 and 1440 px, XL text.

### Phase 2 — Help
- **Covers**: AC8 to AC11
- **Data model impact**: none
- **DoD**: e2e for each AC9 row: plain member who can ask, plain member who cannot (host present), no host, room admin, AI host, a `you_host` seat that cannot ask, a profile without an email, last room admin; a host transfer while Help is open; AC7's returns from Help to AI in this room and to Room settings › Members, and Back and Forward; two seats of one profile seen from a third session reveal nothing (003); the vocabulary test red case of AC11; `make lint` green.

## Data model impact (summary)

None.

## Open questions

- [ ] None blocking.

## Decisions

- The overview is read-only. Because duplicating the audio bar's controls would need their sync, mic error and device download states in a modal (both reviewers).
- No `/admin` link. Because the room page cannot tell the server admin: the admin cookie is scoped to `/api/admin` (Fable).
- The hash is `#call-overview`, not `#voice`. Because `#voice` is the audio bar's id (Fable).
- Words to recognise: any member. Because 005-AC23 and 040-AC3 say so (both reviewers).
- Help does not repeat 039's readiness; it links to it, and reuses 039's ask line. Because the "Who runs the AI" row already holds the hand-off and 039 already words every ask case (Fable).
- The profile line is about a missing email, not a missing profile. Because a seat without a profile lands on `/profile`'s sign-in card, and the page already nudges a profile without an email (Fable, second pass).
- The transcription line says what is happening, not what is ticked. Because "written down" while muted would be false (Codex, second pass).
- Rows show the setting on one line and its value on the next ("Words to recognise", "3 words"); the colon in the AC wording is that layout. Because the absent text carries its own colon (implementation).
- Persona rows stay without persona voices, each saying why, with no link. Because AC3 and AC5 keep every row (Codex, code review).
- With transcription on, the mic on and no worker on the voice, the reason names the worker state the audio bar has: "voice-to-text is off" only while "This server" reads it off, "voice-to-text is not in the call" while no worker is in this room's audio, else "voice-to-text is in the call but not writing down your voice". Because the two lines must not contradict each other, and a word like "starting" promises an end the state does not know (Fable, code review).
- Before the first voice-to-text state, Words to recognise shows its count, where its tab reads "Loading…". Because the count does not depend on a worker, and the state comes with the stream's opening (Fable, code review).
- While the device engine self-tests after its download, the reason reads "this device is testing the model". Because the download is over (Fable, code review).
- "Open the approvals" and Room settings' "Open the Personas sheet" hand over to another surface: they drop the return, and closing that surface does not reopen the overview or Help. Back still reaches the origin's own entry. Because the hand-over is the action asked for (Fable, code review).
- The line on where voice-to-text runs reads "on the server" whenever the device does not hold the voice, the server's voice-to-text off included; the line above then says it waits. Because it names where the voice goes, and "This server" says the rest (code review).
- The device holds the voice only while its pipeline runs in device mode: a ready engine whose pipeline fell back leaves it to the server, which may have no worker. Because a loaded engine is not a transcribing one (Codex, code review).
- Pronunciation reads "Not available on this server" without persona voices. Because it only shapes them (implementation).
- Help's email link reads "Add an email", as the page's nudge, and stays in the tab; Back reopens Help on that row. Because it is a step of the same visit (implementation).
- The last-admin link reads "Room settings › Members and admins", the tab's name until 040 renames it "Members". Because a link names the place as the page shows it (implementation).
- An ended room hides Help's entry. Because `#help` opens nothing there (implementation).
- A sheet reopened by a reload or a Back from another page waits for the room, agent and profile reads before it settles the focus on the row. Because the row is drawn from them (implementation).
- Links read "Open Words to recognise", "Open Call voices", "Open <persona>", and go to `#manage/glossary` and `#manage/voices` until 040 lands. Because the labels then survive 040's renames (implementation).
- AC7's return rides on `history.state`: the link stamps the entry it opens, the target's tabs carry the stamp, and closing goes back as many entries. Because Forward must reopen the target, which a `replaceState` close cannot give (implementation).
