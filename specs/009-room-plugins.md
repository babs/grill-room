# 009 — Room plugins from git, persona management rework, note mentions and host transfer

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-14
**Context**: grill session Q1 to Q8 (2026-10-14), notes on annotations and host transfer (2026-10-14); builds on 002 (skills, host approvals), 003 (host hand-over), 004 (room skills, export), 005 (Personas sheet, mention chips), 006 (starters), 008 (`⋯` menus)

## Problem

A persona gets only the skills someone uploaded to grill-room. A team that already keeps Claude Code plugins in git (skills, agents, commands, hooks, MCP servers) has to copy each skill by hand, loses everything that is not a skill, and has no way to follow upstream versions.

The Personas sheet is already dense (005-AC40). Adding plugins to it as it stands would make it unreadable.

Notes (annotation threads) reach a persona only when someone presses it. An `@name` in a note does nothing, and no persona watches the notes as they come in.

Only the current AI host can hand the role to someone else. When the host is away, a room admin cannot move it.

## Solution

The Personas sheet becomes a compact list. Each persona opens a sheet with four tabs: Identity, Model, Triggers, Capabilities. Rare actions sit in a `⋯` menu.

A room admin adds a Claude Code plugin from a git repository and a ref (branch, tag or commit). The room pins the commit and keeps a snapshot of the plugin's files. The room shows "babs, main @ 53195d8". Nothing tracks the branch: "Update" re-resolves the ref and proposes a new version. The admin picks which personas use the plugin. The host sees what the plugin contains (components, hooks and MCP servers with their commands, the domains they reach, its token cost) and approves it under "approve everything". A turn loads each active plugin through one more `--plugin-dir`, next to today's `grill-room` plugin.

An `@name` in a note starts that persona, as in the chat. One persona per room may watch the notes: it answers threads that name nobody, 20 s after the last new message. A room admin or the super-admin can make any participant with a connected AI host app the AI host.

Private repos are fetched by the host's daemon with the host's own git access, after the host confirms on the daemon page. Plugin options and secrets stay on the host. Exports, imports and starters carry the snapshots, never the secrets.

## Scope

- Persona management rework (list, tabbed sheet, `⋯` menu).
- `@mentions` in notes; the "Watches the notes" persona setting.
- Host transfer by a room admin or the super-admin.
- Adding a plugin from a public https repo: a plugin repo, or a marketplace repo where the admin picks plugins.
- Pinning, snapshot, limits, update with a change summary.
- Host inspection and approval; per-persona activation; delivery to `claude -p`.
- Private repos through the host's daemon.
- Plugin options (`userConfig`) and secrets on the daemon page; allowlist assist for MCP domains.
- Export format 3, import, starters with plugins.

## Out of scope

- Marketplace entries whose `source` is not a relative path (`github`, `url`, `git-subdir`, `npm`, `archive`, `command`). The add form names the entry and says why.
- Live tracking of a branch, scheduled updates.
- Git credentials of any kind on the server, in a room or in an export.
- Plugin options shared across rooms.
- Persisting `${CLAUDE_PLUGIN_DATA}` between sandboxes: it lives in the sandbox home and goes with it.
- Monitors and themes: Claude Code runs monitors only in interactive sessions, and `claude -p` has no theme.
- Changing 006: starters gain plugins here; 006's ACs stay as shipped.
- Mentioning people (not personas) in notes: no push or highlight change.
- Several watchers per room, or a watcher per document.

## Acceptance criteria

### Persona management (Q5)

- [ ] **AC1** Room settings › Personas lists one row per persona: badge, name, model and effort in words ("sonnet · effort medium"), its trigger ("every 3 answers", "on mention only"), and its state (ready, working, queued, offline, waiting for approval). A row fits one line at 390 px and 1440 px, with ellipsis on the name.
- [ ] **AC2** A row opens the persona's sheet with four tabs. Identity holds name, badge, colour and text. Model holds model, effort, memory size (autocompact) and the context use. Triggers holds the trigger, "Can follow up on its own" and its chain behaviour. Capabilities holds the persona's skills, and from Phase 2 its plugins. Each tab has a deep link (`#personas/<id>/model`), and the tab bar is keyboard reachable and scrolls sideways under 40rem.
- [ ] **AC3** Start fresh, New session with handoff, and Remove move into the sheet's `⋯` menu (the 008 component and its keyboard rules). Remove keeps its confirm sheet (005-AC39). Save stays the only primary button and saves every tab at once.
- [ ] **AC4** Nothing is lost: every field and action of the 005 sheet is reachable in the new one. The right-click or long-press on a persona pill opens that persona's sheet (005-AC40).

- [ ] **AC35** When a Room settings or `/admin` module fails to load (a dropped request, `ERR_NETWORK_CHANGED`), the page shows "Couldn't load, reload the page" with a Reload button as soon as the load fails, above any open sheet, and keeps it until reload, never nothing. An e2e test blocks the module and expects the line.

### Mentions in notes and the notes watcher (notes, 2026-10-14)

- [ ] **AC29** An `@name` naming a persona in a note message, by a participant, queues that persona's turn, like a chat mention (005-AC4): the message shows "→ @Persona" with the turn's live state, kept after a reload. The cause is `@mention in note #<id> by <name>: "<first 50 characters>"`, and the prompt puts that thread first under "You were mentioned in note #<id>". A participant without "Can ask the AI" gets the same refusal as in the chat.
- [ ] **AC30** A thread whose messages name one or more personas belongs to them: another persona's prompt leaves it out of the threads it lists as new, and a reply or close it writes to that thread is refused and listed in the turn's report. A thread that names no persona goes to whichever persona runs next, as today. `annotations.md` still lists every thread.
- [ ] **AC31** The Triggers tab has "Watches the notes", off by default. At most one persona per room has it: turning it on for one turns it off for the previous one, and the room's chat says so. The persona shows a badge in the list and on its pill, and the Documents view says "<Persona> watches the notes".
- [ ] **AC32** The watcher runs when new note messages arrive in threads that name no persona, once 20 s pass with no new one (each new message restarts the wait). Its own messages and its own new notes never start it. Its cause is `new notes: #<id>, #<id>`. It counts as an automatic turn against the room's auto-turn limit, and waits while the limit is reached or suspended. A watcher already queued or running takes the new messages in its next prompt instead of queuing twice.

### Host transfer (notes, 2026-10-14)

- [ ] **AC33** A room admin, whether host or not, makes any participant whose AI host app is connected and can host the AI host, from People › `⋯` › "Make AI host". The super-admin does the same from `/admin`'s room view. A plain member who is not the host gets a 403; a participant without a connected app gets a 409 with the reason. The host's own hand-over (003) keeps working.
- [ ] **AC34** The room's chat gets "<who> made <name> the AI host" with its time, the instance's audit log records it (who, room, from, to), and the previous host sees a notice that their app stays connected as a standby. A turn already running finishes on the previous host's app; queued and new turns go to the new host.

### Adding and pinning a plugin (Q1, Q3a, Q3b, Q4, Q6)

- [ ] **AC5** A room admin adds a plugin with a repo URL and a ref. The ref is a branch, a tag or a full commit hash; empty means the default branch. The server resolves it to a commit and keeps it. The room shows the plugin as "<name>, <ref> @ <short commit>", with the commit linked. A plain member sees the list and gets a 403 on the add, update and remove routes.
- [ ] **AC6** A repo with `.claude-plugin/marketplace.json` lists its plugin entries, and the admin checks the ones to add, none preselected. Only relative-path entries resolve inside the repo; another source type is listed disabled with its reason. A repo without a marketplace is one plugin rooted at the repo root. A plugin with no `plugin.json` gets one built from its marketplace entry, as Claude Code would treat the entry as its manifest.
- [ ] **AC7** A URL with credentials (`https://user:token@…`, or any userinfo) is refused with "Never put credentials in a link. For a private repo, the AI host fetches it." The URL is never stored or logged.
- [ ] **AC8** The server fetches public repos over https only. It refuses, after DNS resolution and on every connection (redirects included), any address that is loopback, private, link-local, unique-local, CGNAT, multicast or unspecified, IPv4 and IPv6. It fetches only the pinned commit, shallow, with a 60 s timeout.
- [ ] **AC9** The snapshot holds the plugin's directory at the pinned commit, `.git` left out. Limits: 20 MB by default (a super-admin setting in `/admin` › Instance), enforced while fetching and again on the extracted files; at most 5,000 files. A symlink that resolves inside the plugin is stored as the file it points to. A symlink that leaves the plugin, an absolute or dangling one, or a non-regular file refuses the snapshot, naming the path. Paths follow the export rules: no `..`, no absolute path. The snapshot counts in the room's volume and quota.
- [ ] **AC10** The add form shows the checklist of the room's personas, none checked. A room admin turns a plugin on or off per persona later from the persona's Capabilities tab. A persona holds at most 5 plugins; a room at most 10. A plugin named `grill-room`, or two active plugins with the same name on one persona, are refused with the reason.
- [ ] **AC11** "Update" re-resolves the plugin's ref. The same commit says "Up to date". A new commit makes a new version, pending; the approved one keeps running until the new one is approved. A version records who added it and when; the room's chat gets a line for add, update, approval, refusal and removal.
- [ ] **AC12** Removing a plugin takes it off every persona from their next turn and purges its snapshots, unless a running turn uses it: then it goes when that turn ends.

### Inspection and approval (Q3b, Q6)

- [ ] **AC13** Each new version is inspected by the host's daemon in a throwaway container from the sandbox image: `claude plugin validate --json` and `claude --plugin-dir <snapshot> plugin details <name>`. The report is posted to the room and stored on the version. A version whose validation fails is "refused by Claude Code" with the errors and never loads. With no host connected, the version shows "waiting for the AI host to check it".
- [ ] **AC14** The approval sheet, host only, shows: repo, ref, commit link, snapshot size, components with counts and names (skills, agents, commands, hooks, MCP servers, LSP servers, `bin/`), each hook's event and command and each MCP server's command or URL highlighted, the domains MCP configs name, the validation warnings, and the token cost per turn from `plugin details`. With "approve everything" on, a version waits for this approval; off, it activates once its inspection passes.
- [ ] **AC15** For an update, the sheet adds the change against the approved version: components added and removed, files added, removed and changed (by hash), and a link to the repo's compare view on GitHub and GitLab, the two commit links elsewhere.
- [ ] **AC16** The persona's Capabilities tab lists its plugins with version, state and the token cost per turn, and their total.

### Delivery (Q3a)

- [ ] **AC17** A turn copies the approved versions of its persona's active plugins onto the turn when it starts, like skills. The job lists them; the daemon fetches each snapshot as a tar, keeps it in its cache by sha256, wipes `/home/agent/.grill-room/plugins/` in the sandbox, unpacks each under its own directory and passes one `--plugin-dir` per plugin after the `grill-room` one. A turn with no plugin passes no extra flag and leaves no plugin from an earlier turn. Plugins never go to `/workspace` or the prompt.
- [ ] **AC18** A pending, refused, failed or "needs host setup" (AC24) version never loads; the turn runs without it and the persona's Capabilities tab says why. A turn never fails because of a plugin's state.

### Private repos (Q2)

- [ ] **AC19** When the server cannot fetch a repo anonymously (auth required, not found, or an `ssh` URL such as `git@host:path`), the add form offers "Ask the AI host to fetch it". The host's daemon page then shows the request: room, who asked, URL, ref. Nothing is fetched until the host presses Fetch, whatever the approval mode, since it runs with the host's own git access.
- [ ] **AC20** The daemon fetches with the host's git (ssh key, `gh` auth, credential helper), outside every sandbox, with `GIT_TERMINAL_PROMPT=0`, protocols limited to https and ssh, no submodules, no LFS smudge, and the AC9 limits. It uploads the snapshot and the commit; the server checks the AC9 rules again. The plugin is marked private.
- [ ] **AC21** "Update" on a private plugin goes through the host the same way. On a host without access it fails with "This AI host cannot reach the repo"; the approved version keeps running.

### Options, secrets and network (Q7)

- [ ] **AC22** The daemon page's Rooms tab lists, per room, the plugins with `userConfig` and a form per plugin: each option with its title, description and type; sensitive ones masked and never shown again. Values are kept per room and plugin in the instance's state dir, file mode 0600, and never sent to the server, a room or an export.
- [ ] **AC23** At turn time the daemon gives the values to Claude Code in the sandbox, so `${user_config.KEY}` resolves in MCP and hook configs and `CLAUDE_PLUGIN_OPTION_<KEY>` reaches hooks. The form warns: "The AI can read these values during a turn. Anyone in the room can ask it to show them."
- [ ] **AC24** A plugin with a `required` option unset and no default is "needs host setup": it does not load, the turn runs, and the persona's Capabilities tab and the host's daemon page say so.
- [ ] **AC25** The approval sheet lists the domains of MCP server URLs and marks those off the host's allowlist. The daemon page shows the same list with one "Add to allowlist" button per domain, under the allowlist's existing rules (no IP, no wildcard, no Anthropic host). Nothing is added without that click.

### Export, import, starters (Q8)

- [ ] **AC26** An export carries each plugin with its source, ref, every version's commit, snapshot and inspection report, and the persona activations; never an option value. The export format moves to 3; formats 1 and 2 still import. The export dialog warns when a plugin comes from a private repo, naming it.
- [ ] **AC27** An import lands every plugin pinned at its exported versions and pending, until the new instance's host approves it. "Update" on an imported plugin works only when the repo is public or the new host can reach it (AC21).
- [ ] **AC28** A starter may embed plugin snapshots: "Save as starter" lists the room's plugins, and a room created from the starter gets them pending, by the 006-AC13 rule for starter skills. Built-in starters carry none. Snapshots count against the starter quota (006-AC4).

## Phases

### Phase 1 — Persona management, note mentions, notes watcher, host transfer
- Work: compact list, tabbed sheet with deep links, `⋯` menu, Capabilities with skills only; mentions in notes with their chip, thread ownership in the prompt and the result; the watcher setting, its debounce and badge; "Make AI host" for room admins and the super-admin.
- **Covers**: AC1, AC2, AC3, AC4, AC35, AC29, AC30, AC31, AC32, AC33, AC34
- **Data model impact**: `room_personas.watches_notes` (at most one per room, a partial unique index); note-message mention links (`annotation_message_turns`, like `message_turns`); a `notes_watch` turn source and cause; an audit action for host transfer
- **DoD**:
  - Playwright at 390 and 1440 px: one-line rows (bounding boxes), each tab's deep link, keyboard-only through tabs and `⋯`; a test listing every control id of the 005 sheet finds each one in the new sheet; the existing persona e2e specs green on the new selectors; before and after screenshots in the tracking file;
  - fake-claude e2e: a note naming `@Secretary` queues Secretary with the chip; the prompt of another persona leaves that thread out; a reply to it from another persona is refused;
  - watcher unit tests with an injected clock: two messages 10 s apart start one turn 20 s after the second; its own reply starts nothing; a thread naming a persona starts nothing; the auto-turn limit stops it; moving the role leaves one watcher;
  - host transfer: room admin (not host) moves the role (204, chat line, audit entry), member 403, no daemon 409, super-admin route; a turn running on the old host finishes there while the next queued turn goes to the new host (two fake daemons);
  - one live haiku turn on the lab started by the watcher.

### Phase 2 — Public plugins: add, pin, snapshot, approval, delivery
- Work: add form with marketplace picking, server-side fetch with SSRF guard and limits, snapshot storage, update with change summary, daemon inspection, approval sheet, per-persona activation, the extra `--plugin-dir`, `FAKE_PLUGINS` in fake claude (answers with each `--plugin-dir` and its file list).
- **Covers**: AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC17, AC18
- **Data model impact**: `room_plugins` (room, name, source URL, ref, marketplace entry, private flag); `plugin_versions` (commit, snapshot sha256, size, files, inspection report, state, approved by and at); `room_persona_plugins`; `turns.plugins` (the versions copied at start); snapshots stored like skill versions
- **DoD**:
  - fake-claude e2e: a persona with the fixture plugin on gets two `--plugin-dir` flags, `grill-room` then the plugin's, and the second holds `.claude-plugin/plugin.json` and `skills/spec-feature/SKILL.md` at the fixture commit's hashes; a persona without it gets one flag; a pending version gets one flag;
  - `claude plugin validate` run on the stored snapshot in the sandbox image exits 0 (warnings allowed, as the fixture ships a `CLAUDE.md`);
  - SSRF refusal tests, each red before the guard: `http://`, userinfo, `127.0.0.1`, `10.0.0.1`, `169.254.169.254`, `[::1]`, `[fd00::1]`, `100.64.0.1`, a name resolving to a private address, a redirect to one, and a name that resolves public then private (rebinding);
  - snapshot limit tests: a 21 MB plugin aborted during fetch, 5,001 files, a symlink to `../x`, an absolute symlink, a dangling one, a FIFO; `.git` absent from a stored snapshot;
  - one live haiku turn on the lab with the fixture plugin approved: the session shows a `Skill` call to `babs:spec-feature`, and the persona's tab shows the plugin's token cost.

### Phase 3 — Private repos, options and secrets, allowlist assist
- Work: daemon fetch on host confirmation, options form and store, injection into the sandbox, "needs host setup", MCP domain list and one-click allowlist add.
- **Covers**: AC19, AC20, AC21, AC22, AC23, AC24, AC25
- **Data model impact**: server: none (the private flag is Phase 2's); daemon: per-room plugin options file in the instance's state dir
- **DoD**: a daemon test fetching a local test repo over ssh with a test key, and one refusing `file://` and `ext::`; no request reaches git before the host's Fetch (test); a test fixture plugin with a required sensitive option: unset gives "needs host setup" and a turn without it, set makes a fake-claude hook print `CLAUDE_PLUGIN_OPTION_<KEY>`'s length; a grep of the server DB, the room's export and the server logs after the test finds no option value; e2e for "Add to allowlist" (one domain added, an IP refused).

### Phase 4 — Export, import, starters
- Work: export format 3, import with pending plugins, private-repo warning, plugins in "Save as starter" and room creation.
- **Covers**: AC26, AC27, AC28
- **Data model impact**: export format 3 (`plugins.jsonl`, `plugin_versions.jsonl`, `plugins/<plugin>/<sha256>.tar`); starter manifests and `starter_files` gain plugin snapshots
- **DoD**: export then import across the local stack and the lab: plugins pinned at the same commits and hashes, pending, no option value in the archive (grep); formats 1 and 2 still import (existing fixtures); the private warning shown in e2e; a room created from a starter with the fixture plugin gets it pending; `docs/export-format.md` updated.

## Data model impact (summary)

- New: `room_plugins`, `plugin_versions`, `room_persona_plugins`; `turns.plugins`.
- `room_personas.watches_notes`; `annotation_message_turns`; turn source `notes_watch`.
- Snapshots stored like skill versions, counted in the room's volume.
- Instance setting: plugin snapshot size cap (20 MB).
- Export format 3; starter manifests and files gain plugins.
- Daemon: per-room plugin options in its state dir. Nothing on the server.

## Open questions

- [ ] How do plugin option values reach Claude Code in the sandbox? `claude plugin configure --values-stdin` may not apply to `--plugin-dir` plugins, and on Linux sensitive values go to a credential store the daemon overwrites each turn (it writes a dummy `.credentials.json`). Phase 3 starts with a spike on Claude Code 2.1.296.

## Decisions

Folded from the notes after the draft (2026-10-14):

- Note mentions, thread ownership and the watcher are the user's (notes, 2026-10-14). The setting reads "Watches the notes": the room calls annotations notes (005-AC37), and the user left the wording open.
- Host transfer by a room admin or the super-admin, with chat line, audit entry and standby notice (user, notes, 2026-10-14). It joins Phase 1.
- `claude plugin details` has no `--json` in 2.1.296: the daemon parses its table, with a golden test on 2.1.296 output, and shows "cost unknown" when the table does not parse (coordinator, overturnable).
- The daemon redacts the known sensitive option values from what it posts back: synthesis, replies, new notes, documents and session bytes (coordinator, overturnable). How this keeps the session continuity check is settled in the Phase 3 spike.
- Skills show no token cost until a skill inspection exists (overturnable).
- 006 shipped: "Save as starter" and room creation from a starter are real code, so AC28 extends the starter manifest and `starter_files` (006-AC6) instead of a new store.
- A thread belongs to a persona when a participant's message in it names that persona; a persona's own `@name` in its reply does not claim the thread (coordinator, overturnable).
- Under 40rem a persona row reads "sonnet · medium · on mention only" and shows its state as the pill's state dot (shape per state, the word in its accessible name and tooltip), so the trigger always fits (009-AC1; routine call, overturnable).
- AC35, a failed module load says so, was added by the coordinator after the v0.0.15 lab run (routine call, overturnable).
- The watcher's turns use the room's queue and auto-turn limit, like "every N" turns; a press of the watcher by a human is a normal press (coordinator, overturnable).

From the draft:

- Plugins are Claude Code plugins loaded with one `--plugin-dir` each; the `grill-room` plugin of skills stays as is (user, notes).
- Commit pinned at add time; Update re-resolves; no live tracking (user, Q1).
- No credentials in URLs; public repos fetched by the server, private ones by the host's daemon with its own git access; no git secret on the server (user, Q2).
- Snapshot kept in the room, delivered by `--plugin-dir`, never in the workspace or the prompt (user, Q3a).
- Room admins add and update; the host approves under "approve everything" (user, Q3b).
- Per-persona activation, none preselected; marketplace plugins picked at add time (user, Q4).
- Persona management rework is Phase 1 (user, Q5).
- Approval sheet content and limits: 20 MB default, `.git` excluded, escaping symlinks refused (user, Q6).
- Options and secrets on the host only; "needs host setup" instead of a failed turn; allowlist assist without auto-open (user, Q7).
- Export carries snapshots without secrets, with a private-repo warning; import pending the new host; starters may embed plugins (user, Q8).
- Server fetch: https only, address checks after resolution on every connection, shallow fetch of the pinned commit, size cap and timeout during fetch (coordinator, overturnable).
- Only relative-path marketplace entries are supported: other sources would make the server fetch from npm, archives or arbitrary commands (overturnable).
- A private fetch always needs the host's click on the daemon page, whatever the approval mode: otherwise any room admin could make the host's daemon read the host's own private repos into the room (overturnable).
- Turning a plugin on or off for a persona is for room admins, like adding it (overturnable; skills stay any member's).
- Options are kept per room, not per host: a secret given to one room is not readable by another room's agent (overturnable).
- Imports wait for the new host's approval whatever the mode (Q8 wording); starter plugins follow 006-AC13 (overturnable).
- Limits: 5 plugins per persona, 10 per room, 5,000 files per snapshot, 60 s fetch timeout (overturnable).
- Token cost shown for plugins only; skills show none until a skill inspection exists (overturnable).
- Fixture: the public `github.com/babs/skills` marketplace `babs-skills`, plugin `babs` (`source: "./"`), pinned at commit `53195d86d96aa2917ab23a9729c51d1b0118dc82`. On 2.1.296 it validates with one warning (root `CLAUDE.md` not loaded) and lists 13 skills.
