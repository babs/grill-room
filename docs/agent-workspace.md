# Agent workspace and prompt contract

TL;DR: each turn runs one persona of the room. The room server builds a workspace tar, a prompt, and hands over the persona's text, model and effort. The host daemon unpacks the tar into the sandbox's `/workspace`, runs `claude -p` there, and posts back the files that changed, the final answer and the new session bytes. The server applies all of it in one transaction or none of it. Files are the only channel (D20, D33): no MCP, no custom tool.

The Go side of this contract is `internal/turn`. Change both together.

## Layout

| Path | Mode | Content |
|---|---|---|
| `docs/<name>.md`, `docs/<name>.html` | writable | the agent's documents, latest revision |
| `uploads/<id>-<name>` | read-only | a participant's upload, byte for byte (PDF and images go to Claude as they are) |
| `uploads/<id>-<name>.md` | read-only | pandoc's Markdown copy of a DOCX, ODT or PPTX upload |
| `transcript/<YYYY-MM-DD>.md` | read-only | every room message up to the turn's start, one file per UTC day |
| `annotations.md` | read-only | every annotation thread, with its id, document path, anchor and status |
| `attachments/<id>.<ext>` | read-only | images attached to annotation messages, referenced from `annotations.md` |
| `replies.json` | absent | the agent creates it to answer threads |
| `annotations-new.json` | absent | the agent creates it to open annotations of its own |

The id prefix on uploads keeps two uploads with the same name apart. The daemon wipes `/workspace` before unpacking, so nothing carries over from a previous turn. The read-only modes only stop honest edits; the server is what enforces D30.

## Personas

A room holds several personas, Grill (from the Default template) and Secretary for example. Each one is a separate agent: its own Claude session, its own cursors, its own text, model and effort. "Your last turn" below means the last successful turn of that persona, never another one's.

The persona's text is its role. It does not go in the prompt: the daemon writes it to `/home/agent/.grill-room/persona.md` in the sandbox home, out of `/workspace`, and runs `claude -p --append-system-prompt-file` on it with `--system-prompt-snapshot off`. With the snapshot on, a resumed session would keep the text of its first turn. The text, the model and the effort are copied onto the turn when it starts, so an edit made during a turn applies to the next one.

## Prompt

The server writes the prompt when the turn starts and stores it on the turn. The daemon passes it to `claude -p` on stdin. It holds:

1. A preamble, empty until the presence preamble of spec 002 phase 5 fills it.
2. The persona's name and the room's, and that other agents' lines are tagged `(agent)`.
3. The layout above, in plain words, the rule that uploads are read-only, the naming rule for new documents, and both annotation outputs.
4. "This is a new session" when the turn does not resume: the persona's first turn, a fresh session (D15), or a lost session copy. The agent is told to read `transcript/`.
5. "Changed since your last turn": lines the persona's last successful turn saw that their author edited or deleted since, as `edited: <the text it saw> → <the text now>` or `deleted: <the text it saw>`. A line edited back to the text the agent saw is left out.
6. The messages since the persona's last successful turn, with day and time, sender, `(voice)` for speech and `(agent)` for another persona's synthesis. The persona's own syntheses are left out, since its session already holds them. Only the latest 100 KB are quoted; the prompt says how many earlier lines are in `transcript/`.
7. The annotation threads with a message by anyone but this persona since its last successful turn, in full, with those messages flagged `(new)` and image paths listed.
8. "Your final answer is posted in the room as your synthesis."

A failed turn does not move the cursors: the next turn of that persona quotes its messages and changes again. A persona added to a room has no turn yet, so its first prompt quotes the room from the start.

Lines show their current text. An edited line is tagged `(edited)`, and a deleted one `(deleted)` with the text it had: a delete hides a line from participants, not from the agent. That holds in the prompt and in `transcript/`.

## What the agent produces

| Output | How | Becomes |
|---|---|---|
| synthesis | the final answer of `claude -p` | a room message of kind `agent`, signed with the persona's name and shown with its badge |
| document edits | writing `docs/<name>.md` or `.html` | a new revision by the persona; annotations re-anchor on it |
| new documents | a new file in `docs/` | a new agent document by the persona |
| thread replies | `replies.json`: `[{"annotation": <id>, "body": "...", "status": "done"}]`, `status` optional | an annotation message by the persona; `"done"` also closes the thread |
| new annotations | `annotations-new.json`: `[{"doc": "<path>", "exact": "...", "prefix": "...", "suffix": "...", "body": "..."}]` | an annotation by the persona on that quote, with `body` as its first message |
| model and effort | `modelUsage` in `claude -p`'s JSON; the effort the daemon passed | shown on the synthesis, stored on the turn |

Documents are the room's, not a persona's: any persona may revise any agent document.

An agent document never takes an upload's name, or the room would list two documents as one. The prompt asks the agent to name its version of `plan.md` something like `plan-revised.md`. A new document named like an upload anyway is stored as `plan-agent.md` (with `-agent` again until no upload has the name), and a system message says so. Later writes to `docs/plan.md` revise that document. An agent document that had its name before a participant uploaded a file of the same name keeps it, so the list shows two documents with that name. Their tooltips tell them apart.

`doc` in `annotations-new.json` is a workspace path: `docs/<name>`, `uploads/<id>-<name>`, or an office upload's `.md` copy. Annotating an upload is not editing it, so D30 holds. The quote is a text anchor placed on the document's latest revision, after this turn's document writes, so it may quote what the turn just wrote. Whitespace is folded and the quote must appear word for word in the rendered text; prefix and suffix only pick among several hits. A turn opens at most 50.

A participant reply on a thread the agent closed reopens it, as for any thread. A close is not applied when a participant wrote in the thread after the turn read it: the agent's reply lands, the thread stays open, and the report says why. The next prompt quotes that thread, with the reply flagged `(new)`. A thread holding only the agent's own messages is not quoted back to it.

The daemon reports the model that wrote most of the answer: `claude -p` lists every model it called in `modelUsage`, a small one on the side included. With no `modelUsage`, it reports the model it asked for. A persona asks for a model (`haiku`, `sonnet`, `opus`, `fable`) and an effort (`low`, `medium`, `high`, `xhigh`, `max`), or leaves either empty. The daemon turns the request into what runs in one place: an empty model becomes its `GRILL_ROOM_AGENT_MODEL`, an empty effort is left to Claude Code, and a value outside those lists is treated as empty, since it becomes a `claude` argument. The host cap of spec 002 phase 4 clamps there. The server shows an effort only when it is one `--effort` accepts.

Refused or ignored, and listed in a system message after the synthesis:

- any created, changed or deleted path under `uploads/` (AC16);
- a deleted document, a file in a subdirectory of `docs/`, a name that is not `.md` or `.html`, a document over 2 MB or not UTF-8;
- a `replies.json` that does not parse, a reply to an unknown annotation, a reply without an id or a body, a `status` other than `"done"`, a close on a thread a participant wrote in during the turn;
- an `annotations-new.json` that does not parse, an entry without a doc, a quote or a body, an unknown document, a document with no text (PDF, image), a quote not found, entries past the 50th.

Anything else the agent writes, in `transcript/` or at the root, is scratch and is dropped silently.

## Wire protocol

1. The server sends `event: turn` with `{"id": N}` on the daemon's stream.
2. `GET .../daemon/turns/N` returns the job: `prompt`, `persona` (its name), `system` (its text), `model`, `effort`, `resume` (the persona's session id or empty), `session_size`, `session_sha256`, `timeout_seconds`.
3. `GET .../daemon/turns/N/workspace` returns the tar.
4. If `resume` is set and the daemon's local `<id>.jsonl` does not match the size and hash, it fetches `GET .../daemon/sessions/<id>` and writes it into the sandbox home before running `claude -p --resume <id>` (D6). The cwd is always `/workspace`, so the session's project slug never changes (S2). Every persona's session lives in the same sandbox home under its own id; the room stores each under its persona, so a takeover resumes each persona's own session.
5. The daemon hashes `/workspace` before and after the run and sends each changed path. It sends contents only for `docs/`, `replies.json` and `annotations-new.json`, so an edited 20 MB upload costs one path.
6. `POST .../daemon/turns/N` with `{"synthesis", "model", "effort", "changes", "session_id", "session_offset", "session"}`, or `{"error"}`. `session` holds the jsonl bytes past `session_offset`: the server's size when resuming, 0 for a new session (D23). The server refuses a gap or an overlap.

## Failure rules

| Event | Outcome |
|---|---|
| press during a turn, or while others wait | 202, queued: one queue per room, first in first out, one entry per persona; starts by itself when the turn ends and the host's daemon is there; only who queued it can cancel it |
| press of a persona already queued | 409 |
| persona removed during its turn | refused, 409 |
| host daemon drops mid-turn | turn failed, nothing kept; a late result gets a 409 |
| daemon reports an error (credentials expired, Claude failed) | turn failed, the error in the room |
| result over 32 MB or malformed | 413 or 400, turn failed |
| session bytes do not continue the stored copy | 409, turn failed |
| no answer within `GRILL_ROOM_TURN_TIMEOUT` plus one minute | turn failed |
| room server restarts | every running turn failed at start |
