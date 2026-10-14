# Agent workspace and prompt contract

TL;DR: each turn runs one persona of the room. The room server builds a workspace tar, a prompt, and hands over the persona's text, model, effort and skills. The host daemon unpacks the tar into the sandbox's `/workspace`, runs `claude -p` there, and posts back the files that changed, the final answer and the new session bytes. The server applies all of it in one transaction or none of it. Files are the only channel (D20, D33): no MCP, no custom tool.

The Go side of this contract is `internal/turn`. Change both together.

## Layout

| Path | Mode | Content |
|---|---|---|
| `docs/<name>.md`, `docs/<name>.html` | writable | the agent's documents, latest revision |
| `uploads/<id>-<name>` | read-only | a participant's upload, byte for byte (PDF and images go to Claude as they are) |
| `uploads/<id>-<name>.md` | read-only | pandoc's Markdown copy of a DOCX, ODT or PPTX upload |
| `transcript/<YYYY-MM-DD>/<NNN>.md` | read-only | every room message up to the turn's start: a folder per UTC day, chunks of 200 messages from `001.md` |
| `annotations.md` | read-only | every annotation thread, with its id, document path, anchor and status |
| `attachments/<id>.<ext>` | read-only | images attached to annotation messages, referenced from `annotations.md` |
| `replies.json` | absent | the agent creates it to answer threads |
| `annotations-new.json` | absent | the agent creates it to open annotations of its own |

The id prefix on uploads keeps two uploads with the same name apart.

A transcript chunk holds 200 messages, so about 200 lines. Chunks are counted in messages, not lines: an edit that makes a message longer must not push later messages into another chunk. Messages only append, so a full chunk keeps its bytes from turn to turn and stays in the daemon's cache. It changes only when one of its messages is edited or deleted, which the prompt flags anyway (item 5 below). New messages land in the day's last chunk. The chunk header names the day and the part, not the room, so a room rename rewrites nothing. Before spec 003 the layout was one `transcript/<YYYY-MM-DD>.md` per day; a resumed session that remembers those paths reads the prompt's layout line, which names the new ones. The daemon wipes `/workspace` before unpacking, so nothing carries over from a previous turn, even when most files came from its cache (wire protocol, step 3). The read-only modes only stop honest edits; the server is what enforces D30.

## Personas

A room holds several personas, Grill (from the Default template) and Secretary for example. Each one is a separate agent: its own Claude session, its own cursors, its own text, model and effort. "Your last turn" below means the last successful turn of that persona, never another one's.

The persona's text is its role. It does not go in the prompt: the daemon writes it to `/home/agent/.grill-room/persona.md` in the sandbox home, out of `/workspace`, and runs `claude -p --append-system-prompt-file` on it with `--system-prompt-snapshot off`. With the snapshot on, a resumed session would keep the text of its first turn. The text, the model and the effort are copied onto the turn when it starts, so an edit made during a turn applies to the next one.

## Prompt

The server writes the prompt when the turn starts and stores it on the turn. The daemon passes it to `claude -p` on stdin. It holds:

1. The presence preamble, below.
2. The persona's name and the room's, and that other agents' lines are tagged `(agent)`.
3. The layout above, in plain words, the rule that uploads are read-only, the naming rule for new documents, and both annotation outputs.
4. "This is a new session" when the turn does not resume: the persona's first turn, a fresh session (D15), or a lost session copy. The agent is told to read `transcript/`, oldest first. The first turn of a session started by a handoff is told to read `docs/_handoff-<persona>.md` first (see Handoff).
5. "Changed since your last turn": lines the persona's last successful turn saw that their author edited or deleted since, as `edited: <the text it saw> → <the text now>` or `deleted: <the text it saw>`. A line edited back to the text the agent saw is left out.
6. The messages since the persona's last successful turn, with day and time, sender, `(voice)` for speech and `(agent)` for another persona's synthesis. The persona's own syntheses are left out, since its session already holds them. Only the latest 100 KB are quoted; the prompt says how many earlier lines are in `transcript/`.
7. The annotation threads with a message by anyone but this persona since its last successful turn, in full, with those messages flagged `(new)` and image paths listed. A thread whose participants named other personas, and not this one, is theirs and left out (009-AC30); `annotations.md` still has it. A thread whose `@name` queued this turn comes first, under "You were mentioned in note #<id>" (009-AC29).
8. On a handoff's last turn, a "Handoff" section asking for `docs/_handoff-<persona>.md`.
9. "Your final answer is posted in the room as your synthesis."

### Skills

A persona's active skills reach `claude -p` as one plugin directory, passed with `--plugin-dir`, never through the workspace or the prompt. Claude Code lists them as `grill-room:<name>`; the agent invokes one like any skill and is told its folder.

- The room pins each skill at a version when a member turns it on. The versions pinned when the turn starts are copied onto the turn, so a change during the turn applies to the next one, like the persona's text. A newer upload of the skill is offered in the room, never applied by itself.
- A personal skill rides only while its owner is a member of the room. Skills never come from the host's disk: the server sends them.
- The daemon fetches the plugin as a tar (wire protocol, step 3), deletes `/home/agent/.grill-room/plugin` in the sandbox home, unpacks the tar there and passes that path. A turn without skills deletes the directory and passes no flag, so no skill carries over from an earlier turn.
- The tar holds `.claude-plugin/plugin.json` (`"name": "grill-room"`) and `skills/<name>/` for each skill: its `SKILL.md` and its other files, modes `0644` or `0755`. Claude Code 2.1.296 loads this layout: `claude plugin validate` passes on it and `claude --plugin-dir <dir> plugin details grill-room` lists each skill.
- Scripts in a skill run in the sandbox as the agent user, with the workspace as their only room data. Anyone in the room can ask the agent to print a skill's files.
- A script has Python with the Office libraries, uv, pip, node, pandoc, jq, yq, rg, git and curl, and reaches only the domains on the host's egress allowlist, through the `HTTPS_PROXY` the sandbox sets. The README's host daemon section lists both.

### Plugins

A room admin adds Claude Code plugins from git (spec 009). Each one reaches `claude -p` through its own `--plugin-dir`, after the `grill-room` one, never through the workspace or the prompt.

- The server pins the commit the ref resolves to and stores the plugin's files as a normalised tar, a version. "Update" makes a new version when the ref moved.
- A new version is `inspecting` until the host's daemon checks it, then `approved`, or `pending` while the host approves everything, or `invalid` when `claude plugin validate` fails. The host approves or refuses a pending one (`refused`). A plugin's running version is its latest approved one.
- A turn copies, as it starts, the running version of each plugin its persona has on (`turns.plugins`). A plugin with no approved version is left out, and the turn runs without it.
- Once a newer version is approved, an older one keeps its record but drops its files as soon as no running turn loads it, and the host can no longer approve it. Its `size` stays the original size; the room's volume and quota count only the files kept.
- A removed plugin leaves every persona at once. Its versions stay while a running turn loads one of them, and go when that turn ends.
- The daemon keeps each snapshot in `plugin-cache/<sha256>.tar` in the instance's state dir. It checks a tar against `plugin.ValidateTar` and its sha256 after each download and on each read from the cache; a corrupt copy is fetched again. A copy no turn loaded for 30 days goes at the next download.
- Before each turn the daemon deletes `/home/agent/.grill-room/plugins` in the sandbox home, unpacks each plugin into `/home/agent/.grill-room/plugins/<name>/` and passes those folders in the job's order, after `/home/agent/.grill-room/plugin`. A turn without plugins passes none, and no plugin from an earlier turn stays. A name `plugin.CheckName` refuses fails the turn before `claude` runs.
- A plugin's options (`userConfig`) get their values on the host's daemon page, per room, kept in the instance's state dir (`plugin-options.json`, owner-only) and never sent to the server. They are keyed by the room's plugin id, which the job carries as `id`: a plugin removed and added again under its name starts with none, and the daemon forgets the values of a plugin the room no longer lists. Claude Code 2.1.296 calls a `--plugin-dir` plugin `<name>@inline` and reads its values from two files of the sandbox home, which the daemon rewrites whole before every turn (spikes/S12-plugin-options.md): the sensitive ones in `.claude/.credentials.json` as `pluginSecrets`, beside the dummy login, the others in `.claude/settings.json` as `pluginConfigs.<id>.options`. An option the host left unset gets its default. Only the plugins the turn loads get values, so no value stays from an earlier turn. `${user_config.KEY}` then resolves in MCP and hook configs, and hooks get `CLAUDE_PLUGIN_OPTION_<KEY>`.
- Claude Code does not refuse a plugin whose `required` option has no value. The daemon does: a plugin with a required option unset and no default is left out of the turn, which runs without it, and the result lists it in `skipped_plugins` with the reason `needs host setup`. The server marks the plugin (`needs_setup`) until a turn that pinned it loads it, and the persona's Capabilities tab shows "needs host setup on the AI host's computer".
- Before posting a result, the daemon replaces every sensitive value the host set for the room with `[redacted]`: the string values of 4 characters or more of the options a plugin declared sensitive, when the host saved them or in this job, whichever plugin they belong to; never a number or a boolean. It looks for each value as written, escaped in a JSON string with and without HTML escaping, and escaped twice; overlapping matches become one `[redacted]`. The synthesis, the documents and the turn's error are redacted as text. The session, `replies.json` and `annotations-new.json` are redacted string by string, each JSON string decoded, redacted and encoded again, so every line still parses. It rewrites the session's local copy the same way, so its size and hash match the room's and the next turn resumes without fetching it.
- A repo the server cannot read, a private one or an ssh link, is fetched by the host's daemon on request (below).

### Presence preamble

The prompt opens with who is in the room, from one template, `internal/web/presence.tmpl`, embedded in the server. Edit that file to change what agents are told. It holds:

- the room name;
- when the room has an agent document named `agenda.md`, a line naming it the room's agenda, which every persona aligns on (006-AC14); a room without one gets no such line;
- the persona itself: its name, the model and effort it runs at on the host's daemon after the cap, and its triggers, ending in `watches the notes` for the room's notes watcher;
- why it runs, as `Why you are running: <cause>` (003-AC22). The cause is set when the turn is pressed or queued and stored on the turn (`turns.cause`):

  | Source | Cause |
  |---|---|
  | a persona's button | `pressed by <name>` |
  | a human's message naming `@persona` | `@mention by <name>: "<first 50 characters of the message>"`, with `…` when cut |
  | a human's note message naming `@persona` | `@mention in note #<id> by <name>: "<the start of the message>"`, cut with `…` so the line fits |
  | another persona's synthesis naming it | `called by @<persona>` |
  | its "every N rounds" trigger | `every <N> rounds` |
  | its "every N voice lines" trigger, N voice lines after its latest turn started | `every <N> voice lines` |
  | its "every T minutes" trigger, T minutes after its latest turn started, with a voice or text line since and someone here | `every <T> minutes` |
  | "Watches the notes": note messages by others since its latest turn started, in threads naming no persona, the last one 20 s old (`GRILL_ROOM_NOTES_WATCH_DELAY`) | `new notes: #<id>, #<id>`, ending `, …` past what fits the line |
  | a re-run after a conflict | `re-run after a conflict on <doc>` |
  | the host's "New session with handoff", last turn | `the host <name> asked for a handoff note before a new session` |
  | the new session after it | `handoff from your previous session` |

- each other persona: model and effort after the cap, state (`idle`, `running`, `queued`, or waiting for the host's approval), completed turns, triggers, and the first line of its latest synthesis on a line of its own;
- the humans present (those with the room page open, or who had it open in the last 20 seconds), the host, and who speaks now (the author of a voice line from the last 10 seconds). Each name carries a short id, `Alice ·a1b2`, so two people with the same name stay apart (003-AC32):
  - with a profile: the first 4 hex digits of the profile's uuid (`profiles.uuid`, random v4). When two profiles among the room's participants share those 4 digits, every profile id in that preamble shows 6, or 8 if 6 still clash;
  - without a profile: `#` and the participant's id in the server, `Bob ·#12`. A profile id never starts with `#`;
  - one person who joined twice under the same profile shows the same id twice.
- the chain budget left, or "unlimited" while the limit is suspended;
- how to call another persona with `@name`, and whether the persona's own `@name` gives it another turn.

The server cuts each rendered line at 100 characters. The preamble is fixed when the turn starts, like the rest of the prompt.

A failed turn does not move the cursors: the next turn of that persona quotes its messages and changes again. A persona added to a room has no turn yet, so its first prompt quotes the room from the start.

Lines show their current text. An edited line is tagged `(edited)`, and a deleted one `(deleted)` with the text it had: a delete hides a line from participants, not from the agent. That holds in the prompt and in `transcript/`.

## Context use and autocompact

Each persona has an `autocompact` setting (003-AC24), set on the persona sheet's Model tab next to its model: `auto` (the default, stored as 0) or a token count from 100,000 to 1,000,000, the range `claude --autocompact` accepts in Claude Code 2.1.296. The job carries it as `autocompact`, and the daemon passes `--autocompact <tokens>`, or no flag for auto. A value out of that range never reaches `claude`.

After each turn the daemon measures the session's context use from `claude -p`'s JSON result:

- tokens: `usage.input_tokens + usage.cache_read_input_tokens + usage.cache_creation_input_tokens`. In Claude Code 2.1.296, `usage` is the turn's last request, the context the next turn starts from; `modelUsage` sums every request of the turn, so it would overcount;
- window: the `contextWindow` that `modelUsage` reports for the model that wrote most of the answer. Claude Code knows each model's window, so there is no table to keep up to date. A result without it leaves the window unknown, and the room shows no percentage.

The daemon posts both with the result (`context_tokens`, `context_window`). The server keeps them on the session (`agent_sessions.context_tokens`, `context_window`, `context_updated_at`), ignoring a window smaller than the tokens. The persona sheet's Model tab shows the use of the session the persona resumes next as a percentage, and the daemon reports both to its supervisor in the turn's end event for monitoring.

## Handoff

"New session with handoff" (003-AC25) is the host's, per persona, in the persona sheet's `⋯` menu as "Start fresh, keep a summary". Above 80% context use the room also offers it in the "waiting for the host" banner. It needs a session to hand over: a persona with no successful turn answers 409.

1. It runs a last turn on the current session, cause `the host <name> asked for a handoff note before a new session`, queued like a press. The prompt asks the agent to write `docs/_handoff-<persona>.md`: decisions made, the current state, open questions, what to do next.
2. When that turn is done and created or revised the note, the server queues a fresh turn of the persona in place of its queued one, cause `handoff from your previous session`. Its prompt says it is a new session and to read the note first, then `transcript/`.
3. If the last turn fails, or ends without writing the note, the persona stays on its session, and the room says so: `No handoff: <persona> stays on its current session.` A last turn rejected for a conflict re-runs as a handoff turn.

The note is an ordinary agent document: the room sees it, and any persona may read or revise it.

## What the agent produces

| Output | How | Becomes |
|---|---|---|
| synthesis | the final answer of `claude -p` | a room message of kind `agent`, signed with the persona's name and shown with its badge |
| document edits | writing `docs/<name>.md` or `.html` | a new revision by the persona; annotations re-anchor on it |
| new documents | a new file in `docs/` | a new agent document by the persona |
| thread replies | `replies.json`: `[{"annotation": <id>, "body": "...", "status": "done"}]`, `status` optional | an annotation message by the persona; `"done"` also closes the thread. Only on a thread that names no persona, or names this one |
| new annotations | `annotations-new.json`: `[{"doc": "<path>", "exact": "...", "prefix": "...", "suffix": "...", "body": "..."}]` | an annotation by the persona on that quote, with `body` as its first message |
| calls | `@name` in the synthesis, outside code | a queued turn of that persona, one chain link each; its own name only with self-call on |
| model and effort | the model and effort the daemon passed, checked against `modelUsage` in `claude -p`'s JSON | shown on the synthesis, stored on the turn |

Documents are the room's, not a persona's: any persona may revise any agent document.

An agent document never takes an upload's name, or the room would list two documents as one. The prompt asks the agent to name its version of `plan.md` something like `plan-revised.md`. A new document named like an upload anyway is stored as `plan-agent.md` (with `-agent` again until no upload has the name), and a system message says so. Later writes to `docs/plan.md` revise that document. An agent document that had its name before a participant uploaded a file of the same name keeps it, so the list shows two documents with that name. Their tooltips tell them apart.

`doc` in `annotations-new.json` is a workspace path: `docs/<name>`, `uploads/<id>-<name>`, or an office upload's `.md` copy. Annotating an upload is not editing it, so D30 holds. The quote is a text anchor placed on the document's latest revision, after this turn's document writes, so it may quote what the turn just wrote. Whitespace is folded and the quote must appear word for word in the rendered text; prefix and suffix only pick among several hits. A turn opens at most 50.

A participant reply on a thread the agent closed reopens it, as for any thread. A close is not applied when a participant wrote in the thread after the turn read it: the agent's reply lands, the thread stays open, and the report says why. The next prompt quotes that thread, with the reply flagged `(new)`. A thread holding only the agent's own messages is not quoted back to it.

A persona asks for a model (`haiku`, `sonnet`, `opus`, `fable`) and an effort (`low`, `medium`, `high`, `xhigh`, `max`), always both. The daemon turns the request into what runs in one place: a value outside those lists is dropped, since it becomes a `claude` argument, then the host cap clamps model and effort apart, each to the higher of the cap and what the host approved for this persona on this daemon. Only a room server from before explicit models sends neither; Claude Code then picks. The README's "Host cap and approvals" has the ranking rules. The server shows an effort only when it is one `--effort` accepts.

The daemon reports the model it asked for, checked against `modelUsage`, where `claude -p` lists every model it called. A session resumed on another model lists its earlier model too, and Claude Code may call a small one on the side, so the model that wrote most is not the answer. Ids map to their family (`claude-sonnet-5-5` is `sonnet`). Every other model goes in `model_note` with its share of the turn's cost, `+haiku 3%` (by tokens when claude reports no cost). When `modelUsage` lists models but not the one asked for, `model_mismatch` is set, `model_note` lists what claude did report, and both the daemon and the server log `turn_model_mismatch`. With no `modelUsage` there is nothing to check against.

Refused or ignored, and listed in a system message after the synthesis:

- any created, changed or deleted path under `uploads/` (AC16);
- a deleted document, a file in a subdirectory of `docs/`, a name that is not `.md` or `.html`, a document over 2 MB or not UTF-8;
- a `replies.json` that does not parse, a reply to an unknown annotation, a reply without an id or a body, a `status` other than `"done"`, a close on a thread a participant wrote in during the turn, a reply or close on a thread that belongs to other personas (`it belongs to @<persona>`);
- an `annotations-new.json` that does not parse, an entry without a doc, a quote or a body, an unknown document, a document with no text (PDF, image), a quote not found, entries past the 50th.

Anything else the agent writes, in `transcript/` or at the root, is scratch and is dropped silently.

## Wire protocol

1. The server sends `event: turn` with `{"id": N}` on the daemon's stream, once per running turn of that daemon: in parallel mode a room runs one turn per persona at once.
2. `GET .../daemon/turns/N` returns the job: `prompt`, `persona` (its name), `system` (its text), `model`, `effort`, `approved` (the `model` and `effort` the host approved above its cap for this persona, on this daemon; empty otherwise), `skills` (the persona's active skills at their pinned versions; absent for none), `plugins` (below), `autocompact` (absent for auto), `resume` (the persona's session id or empty), `session_size`, `session_sha256`, `timeout_seconds`.

   Before going on, a daemon under a supervisor asks it for a slot under the host's global cap. While it waits, it posts `POST .../daemon/turns/N/state` with `{"host_queued": true}`, and `false` once the slot is granted: the room shows the turn "queued (host busy)", and the turn timeout starts when it runs. A server without that route answers 404, which the daemon ignores.
3. The workspace. The first fetch of a turn's manifest or tar pins the revision of every agent document in it (`turns.base_revisions`). Later fetches of the same turn serve those revisions, and leave out agent documents written after the pin. A daemon with a cache asks for what changed:
   - `GET .../daemon/turns/N/manifest` returns `{"dirs": [...], "files": [{"path", "sha256", "size", "mode"}]}`, every workspace file sorted by path.
   - The daemon keeps one cache per room under its state dir (`workspace-cache/`), out of every sandbox, with files named by their sha256. It hashes each cached file the manifest names, then `POST .../daemon/turns/N/workspace` with `{"paths": [...]}` for the missing ones only, and gets a tar of those. A turn whose files are all cached sends no POST, and an unchanged upload is not sent again while it stays cached.
   - It builds the tar it unpacks in `/workspace` from the cache. The cache has a size cap, `GRILL_ROOM_DAEMON_CACHE_MB` (1024 by default, 0 turns it off), and drops the least recently used files first, all rooms together. The daemon deletes a room's cache when the room revokes it or is deleted.
   - It falls back to `GET .../daemon/turns/N/workspace?fallback=<reason>`, the full tar, which refills the cache: `missing` with no cache for the room, `corrupt` when a cached file fails its hash (the file is deleted), `partial` when the POST fails or its tar holds anything but the requested files with the manifest's hashes, `manifest` when the manifest is unavailable (an older room server) or invalid. The daemon logs `workspace_fallback`. The server logs it too and counts it in `grill_room_workspace_fallbacks_total{reason}`.
   - The server answers a POST only for paths of this turn's workspace, or 400. Before writing anything, the daemon refuses a manifest path or a tar entry that is absolute or has a `.` or `..` element, a manifest path listed twice, and a tar entry that is not a regular file or a directory.
   - A daemon without a cache, as before 002 or with the cap at 0, gets the full tar with a plain `GET .../daemon/turns/N/workspace`, which the server keeps for daemons not yet updated.

   When the job lists `skills` (`[{"name", "version"}]`), `GET .../daemon/turns/N/skills` returns the plugin directory as a tar.

   When the job lists `plugins` (`[{"id", "name", "sha256", "version", "options"}]`, absent for none; `id` is the room's plugin id; `options` lists each option's `key`, `sensitive`, `required` and `default`, never a value), `GET .../daemon/turns/N/plugins/<sha256>` returns each one's snapshot, a tar whose sha256 is that value. The server answers 404 for a sha256 the turn does not list. The daemon unpacks each under its own directory named after the plugin and passes one `--plugin-dir` per plugin after the `grill-room` one.
4. If `resume` is set and the daemon's local `<id>.jsonl` does not match the size and hash, it fetches `GET .../daemon/sessions/<id>` and writes it into the sandbox home before running `claude -p --resume <id>` (D6). The cwd is always `/workspace`, so the session's project slug never changes (S2). Every persona's session lives in the same sandbox home under its own id; the room stores each under its persona, so a takeover resumes each persona's own session.
5. The daemon hashes `/workspace` before and after the run and sends each changed path. It sends contents only for `docs/`, `replies.json` and `annotations-new.json`, so an edited 20 MB upload costs one path.
6. `POST .../daemon/turns/N` with `{"synthesis", "model", "model_note", "model_mismatch", "effort", "capped", "changes", "session_id", "session_offset", "session", "context_tokens", "context_window", "skipped_plugins"}`, or `{"error"}`, which may carry `skipped_plugins` too. `skipped_plugins` lists the job's plugins the daemon left out, `[{"name", "reason"}]`. The context fields are absent when unknown. `capped` is true when the host cap lowered what the persona asked for. `session` holds the jsonl bytes past `session_offset`: the server's size when resuming, 0 for a new session (D23). The server refuses a gap or an overlap. A document edit whose document is past its pinned revision, or was created after the pin, is a conflict: the server answers 409 `conflict on <name>` and keeps nothing of the result.

7. When a turn handed to the daemon ends on the server without its result, stopped from the room or failed there, the server sends `event: stop` with `{"id": N}`. The daemon kills `claude` in the sandbox, or ends the turn while it waits for a slot, and posts no result. A daemon from before ignores the event: its result then gets a 409. The supervisor's monitor records the turn as `stopped`.

### Plugin inspection

The host's daemon checks each new plugin version before it can load (009-AC13). Only the room's host daemon gets these events and may use these routes; another daemon gets a 403.

1. The server sends `event: inspect` with `{"id": V}` for each version waiting for inspection, once per stream: when the stream opens, when a version is added, and when the daemon becomes the host.
2. `GET .../daemon/plugins/V/snapshot` returns the version's tar.
   The daemon waits until its sandbox image is ready, then runs a container from it named `grill-room-inspect-<room>-<V>`, with the sandboxes' labels, `--network none`, a read-only root, no capability, 1 CPU, 1 GiB, a 400 MiB tmpfs `/tmp` as `HOME`, and `sleep 60` as its process, so it ends by itself within 60 s. It unpacks the tar into `/tmp/p` and runs `claude plugin validate --json /tmp/p`, then, when that passes, `claude --plugin-dir /tmp/p plugin details <name>` with the name from `plugin.json`, and removes the container. Neither command runs the plugin's hooks, MCP servers or scripts: a Docker test checks that a SessionStart hook leaves nothing behind. The daemon inspects one version at a time and skips a version it reported already.
   Only two outcomes refuse a version at once: a tar `plugin.ValidateTar` refuses, and a failed validation. A failure of the plugin itself, `claude plugin validate` printing something other than JSON or not finishing within 60 s, is tried once more, then reported with `ok` false and the error. Any other failure (the image, Docker, the room) is the host's or the room's: the daemon posts nothing and tries again, waiting twice as long each time up to 5 minutes, for as long as its stream lives; a new stream announces the version again.
3. `POST .../daemon/plugins/V/report` with `{"ok", "validate", "details", "error", "allowlist_missing"}`. `allowlist_missing` lists the domains the version's MCP servers name that the host's egress allowlist leaves out, at the time of the inspection; the approval sheet marks them. `validate` is the output of `claude plugin validate --json`, compacted, left out past 64 KiB. `details` is what `claude plugin details` showed: `components`, `always_on_tokens`, `cost_known`. `error` is the first validation error, or why the inspection could not run, when `ok` is false. The server answers `{"state"}`: `invalid` when `ok` is false, `pending` while the host approves everything, `approved` otherwise. A version not waiting for inspection gets a 409.

While no host daemon is connected, a version stays `inspecting` and the room shows "waiting for the AI host to check it".

### Fetch through the AI host

A room admin asks the host's daemon to fetch a repo the server cannot read anonymously (009-AC19 to 009-AC21). Only the room's host daemon gets these events and may use these routes.

1. The server sends `event: plugins` with `{}` when the stream opens and whenever the room's plugins or its requests to the AI host change. The daemon reads `GET .../daemon/plugins`: each plugin's `name`, `version`, `options` and `domains`, for its page, the `fetches` waiting (`id`, `url`, `ref`, `entries`, `plugin_name` for an update, `requested_by`, `created_at`) and `max_bytes`, the snapshot cap.
2. The daemon page lists each request with Fetch and Decline. Nothing runs git before the host presses Fetch.
3. Fetch runs the host's `git` in a folder of the instance's state dir, outside every sandbox, with the host's ssh keys and credential helpers: `git init --bare`, then `git fetch --depth 1 --no-tags -- <url> <ref or HEAD>`, with `protocol.allow=never` but for https and ssh, `GIT_ALLOW_PROTOCOL=https:ssh`, no hooks (`core.hooksPath` is the null device), no submodules, `GIT_TERMINAL_PROMPT=0`, an empty `GIT_ASKPASS`, `SSH_ASKPASS` and `core.askPass`, `credential.interactive=never`, `GCM_INTERACTIVE=never`, `GIT_LFS_SKIP_SMUDGE=1`, and on Unix a session of its own, so ssh has no terminal to prompt on. At the deadline the daemon kills git's whole process group, ssh and helpers included; on Windows it kills git only and stops waiting on its pipes 5 s later. The worker deletes `plugin-fetch/` as it starts. The link must be https without userinfo, `ssh://` with no password, or `user@host:path`; neither it nor the ref may start with a dash. The fetch stops past 60 s, or once its folder grows past four times the snapshot cap. The daemon then reads the commit's tree with `git ls-tree` and streams its files from `git cat-file --batch`, at most four times the snapshot cap, with no checkout, so no filter or attribute runs, and builds the snapshots under the rules of a server fetch: the marketplace's entries the request names, or the repo as one plugin when it has no marketplace.
4. `POST .../daemon/plugin-fetches/F` with `multipart/form-data`: `commit`, `ref_name`, and one `snapshot` part per plugin whose file name is its marketplace entry, `.` for the repo. The server checks each tar with `plugin.ValidateTar` against its cap, requires the plugins the request named and no other, marks them private and adds them, or for an update the new version, as `inspecting`; it answers 201, or 422 and fails the request.
5. When the fetch fails, the daemon posts `{"error"}` instead: "This AI host cannot reach the repo" for any failure of git; the reason itself when the repo or the request caused it (too big, not a valid plugin, a marketplace without names, a bad ref); for any other failure on the host's machine, a generic sentence, the details staying in its log. Decline posts `{"declined": true}`. The request is then `failed` with its reason and stays in the room until a room admin dismisses it; an update's running version keeps running.

## Failure rules

| Event | Outcome |
|---|---|
| press during a turn, or while others wait | 202, queued: one queue per room, first in first out, one entry per persona; starts by itself when the turn ends and the host's daemon is there; only who queued it can cancel it. In parallel mode a press waits only while its persona runs or waits, and the queue starts every entry whose persona does not run |
| a result edits a document revised since the turn's workspace was pinned | 409; the turn fails with "conflict on <name>" and runs again ahead of the queue, as the persona's queued turn if it had one, with `rerun_of` and the cause "re-run after a conflict on <name>"; not a chain link. The re-run's own conflict fails it for good. Replies, new annotations, syntheses and new documents never conflict |
| press of a persona already queued | 409 |
| persona removed during its turn | refused, 409 |
| host daemon drops mid-turn | turn failed, nothing kept; a late result gets a 409 |
| stopped from the room by who started it, the host or a room admin | turn `stopped` at once, the line "<persona> turn stopped by <name>." in the room, nothing kept; the daemon gets `event: stop`, and a late result gets a 409. The persona's next turn resumes its last done turn's session; a stopped turn triggers no chain link and does not count for "every N rounds" |
| daemon reports an error (credentials expired, Claude failed) | turn failed, the error in the room |
| result over 32 MB or malformed | 413 or 400, turn failed |
| session bytes do not continue the stored copy | 409, turn failed |
| no answer within `GRILL_ROOM_TURN_TIMEOUT` plus one minute, counted from when the turn left the host's queue | turn failed |
| room server restarts | every running turn failed at start |
