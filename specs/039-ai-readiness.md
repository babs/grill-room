# 039 — AI readiness: what the AI in this room needs, and from whom

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-14
**Context**: UX audit part B, direction S2 (journey 9, settings far from their effect). Builds on 002 (cap, approvals), 003 (AI host, pairing, readiness), 004 (room skills), 009 (plugins, host setup), 014 (websites, connectors), 038-AC3 (the "Ask someone to run it" message, which ships first).

## Problem

Whether a persona will answer depends on up to six conditions:
- someone runs the AI host app;
- that app can host (Docker, a Claude account, the sandbox image);
- I may press ▶;
- changes wait for the host's approval;
- a plugin waits for host setup;
- a website waits for the host.

The room shows each condition in a different place: a pill tip, an approval strip, a persona sheet line, a chat line, a banner. Several are visible only to the host. A PM with a silent persona cannot tell what is missing or who can fix it.

The host's decisions are set in the host app on their computer and never shown in the room: model and effort cap, holding changes for approval, connectors. They surface only as "requested → ran" or a blocked line.

## Solution

An "AI in this room" panel lists each condition as a row. A row shows its state in words, names the person who acts on it (you, the AI host by name, a room admin, anyone), and offers the one action that fits: a button for the person who can act, a copyable message for anyone else. Under the rows, a read-only "The AI host's choices" section mirrors what the host decided. The panel opens from the room menu, from any "cannot answer" tip, and inline in an empty room.

## Scope

- The panel, its rows and actions, built from the agent state the room already receives (`agent` event) and the room's plugin views.
- Entry points: room menu, offline or locked pill and its ▶ tip, the approval strip, an empty room.
- A read-only mirror of the host's cap, approval mode and connectors.
- A host's daemon readiness reason shown in plain words with who acts.

## Out of scope

- Changing any host decision from the room: the host app stays the only place.
- A new notification channel to the host: actions copy a message, the user sends it.
- Splitting Room settings (040), the voice sheet (042).
- Telling which seats share a profile (003's privacy rule stands: `you_host` names no seat).

## Acceptance criteria

### The panel

- [ ] **AC1** The room menu has "AI in this room", with the deep link `#ai`. It opens a sheet, full screen on a phone, titled "AI in this room". It lists the rows of AC2 to AC6 in that order, each as a kit row: an icon for its state (ready, waiting, blocked), one line of state, one line "Who: <you | Alice (AI host) | a room admin | anyone in the room>", and at most one action per person (AC2's no-host and removed states offer two).
- [ ] **AC2** **Who runs the AI.**

  | State | Line | Who | Action |
  |---|---|---|---|
  | Ready | "Alice runs the AI on their computer." ("You run…" for the host) | none | none |
  | No host | "No one runs the AI for this room." | anyone in the room | "Run the AI on my computer" (computer); "Ask someone to run it" (038-AC3) |
  | Host's app not connected | "Alice's AI host app is not connected." | Alice | for Alice: "Open my AI host app"; for others: "Copy a message to Alice" |
  | Room removed from the host's app | "Alice removed this room from their AI host app." | Alice, or anyone to take over | as above, plus "Run the AI on my computer" |
- [ ] **AC3** **The host's computer can host.** Shown only while a host's app is connected and cannot host. The daemon's reason reads in plain words: Docker not running → "Docker is not running on Alice's computer."; no usable Claude account → "Alice's AI host app has no Claude account it can use."; sandbox image failed or not ready → "The sandbox is still getting ready on Alice's computer." / "…could not be prepared on Alice's computer.". An unknown reason shows as the host app sent it. Who: Alice. Action for the host: "Open my AI host app".
- [ ] **AC4** **You can ask.** "You can ask the personas." or "Alice decides who can ask the personas. You can still mention them in a message." Who: Alice. Action for others: "Copy a message to Alice" ("Can you tick "Can ask the AI" next to my name in <room>?").
- [ ] **AC5** **Waiting for the AI host.** Counts by kind, each a line when non-zero: persona changes and new personas, room skills, plugin versions, personas asking above the cap, websites a turn was refused. Who: Alice. For the host, the action opens the approval strip; for others, "Copy a message to Alice" listing the counts. With nothing waiting, the row reads "Nothing waits for the AI host."
- [ ] **AC6** **Plugins that do not load.** One line per room plugin with a `why` (009-AC18), naming it and its reason. Who: Alice when the reason is hers to act on: host setup (009-AC24), a version waiting for her approval or being checked by her app; a room admin otherwise, with "Open Plugins" (`#manage/plugins`) for room admins. Hidden when every plugin loads or the room has none.

### The AI host's choices

- [ ] **AC7** Under the rows, "The AI host's choices" lists, read-only, while a host has a paired app: "Models up to <model>, effort up to <effort>" (or "No cap on models"); "New personas and changes: wait for Alice's approval" or "apply at once" (`approve_all`, same words as 038-AC1); "Connectors: <names>" or "No connectors"; and "Websites: see Room settings › Websites and connectors" linking to `#manage/network`. A line: "Only Alice changes these, in their AI host app."
- [ ] **AC8** With no paired host app, the section reads "No AI host yet: these choices appear once someone runs the AI."

### Entry points

- [ ] **AC9** Every tip or line that says a persona cannot answer (offline pill and ▶ tip, the "Only Alice can let you ask" tip, a plugin's "does not load" line, the blocked-website banner) ends with a "What's missing?" link that opens `#ai`.
- [ ] **AC10** A room with personas and no message yet shows, in the chat's empty state, the panel's rows that are not ready (AC2 to AC4) with their actions, under "Before the personas can answer". It goes once every row is ready or the first message arrives.
- [ ] **AC11** The rows follow the `agent` event live: a host connecting, an approval, a plugin set up changes the row without a reload.

## Phases

### Phase 1 — Panel and host's choices
- Work: `#ai` sheet, rows AC2 to AC6 from the agent state and plugin views, reason mapping, copy-message actions, read-only choices.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC11
- **Data model impact**: none. Server side, the daemon's readiness reason gains a stable code next to its text (docker, account, image_pending, image_failed) so the room maps it without parsing English; the agent state carries it.
- **DoD**: `go test -race ./internal/web/... ./internal/daemon/...` green with a test per reason code; e2e (`--workers=1`) with the fake AI host: no host, host offline, host can't host (account), changes waiting, plugin needing setup; captures at 390 and 1440 px for host and plain member.

### Phase 2 — Entry points
- Work: "What's missing?" links, empty-room checklist.
- **Covers**: AC9, AC10
- **Data model impact**: none
- **DoD**: e2e: an offline pill's tip opens `#ai`; a new room with a persona shows the checklist, which goes when the host connects; `make lint` green.

## Data model impact (summary)

None. One new field in the daemon's readiness report and the agent state: a reason code.

## Open questions

- [ ] None blocking.

## Decisions

- Actions copy a message instead of notifying the host. Because the room has no channel to the host outside it, and the audit's arbitration put the hand-off message first.
- The panel reads the host's choices but never changes them. Because the host app owns them (002, 014) and two places to set one cap would disagree.
- A reason code joins the readiness text. Because the room must not match English strings that the host app may reword.
- The empty-room checklist shows only rows that are not ready. Because a ready room must not open on a checklist.
- The reason code travels in the existing `daemon_status.reason` column, joined to the words by a separator, rather than in a new column. Because the spec keeps the data model unchanged and adds no migration; the status table is unlogged and rewritten by every heartbeat, so a column can come with its next migration.
- A room the host paused or cut off from Claude carries no code and shows the host app's words. Because it is the host's choice, not a missing Docker, account or image. A proxy that cannot start for lack of Docker maps to `docker`.
- The server adds the code `removed` when the host has no paired app left for the room. Because only the server knows the pairing is gone; the page derives "no host" and "app not connected" from the state it already has.
- With no one running the AI, the ask and waiting rows name "anyone in the room" and offer no copy action. Because there is no host to send a message to.
- While the host's app is offline its connectors read "shown once Alice's AI host app connects", and while it is connected but cannot host, "shown once Alice's AI host app can host". Because the room forgets them on disconnect and the app sends none without a working account proxy, so "No connectors" would be false.
- "Open my AI host app" shows only where the room menu shows it (not on a phone or tablet). Because the link opens the app on this device, which a phone never runs.
