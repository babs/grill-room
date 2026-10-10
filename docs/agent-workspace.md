# Agent workspace and prompt contract

TL;DR: each turn, the room server builds a workspace tar and a prompt. The host daemon unpacks the tar into the sandbox's `/workspace`, runs `claude -p` there, and posts back the files that changed, the final answer and the new session bytes. The server applies all of it in one transaction or none of it. Files are the only channel (D20, D33): no MCP, no custom tool.

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

The id prefix on uploads keeps two uploads with the same name apart. The daemon wipes `/workspace` before unpacking, so nothing carries over from a previous turn. The read-only modes only stop honest edits; the server is what enforces D30.

## Prompt

The server writes the prompt when the button is pressed and stores it on the turn. The daemon passes it to `claude -p` on stdin. It holds:

1. The layout above, in plain words, and the rule that uploads are read-only.
2. "This is a new session" when the turn does not resume: the first turn, a fresh session (D15), or a lost session copy. The agent is told to read `transcript/`.
3. The messages since the last successful turn, with day and time, sender, and `(voice)` for speech. The agent's own syntheses are left out, since its session already holds them. Only the latest 100 KB are quoted; the prompt says how many earlier lines are in `transcript/`.
4. The annotation threads with a participant message since the last successful turn, in full, with new messages flagged `(new)` and image paths listed.
5. "Your final answer is posted in the room as your synthesis."

A failed turn does not move the cursors: the next turn quotes its messages again.

## What the agent produces

| Output | How | Becomes |
|---|---|---|
| synthesis | the final answer of `claude -p` | a room message of kind `agent` |
| document edits | writing `docs/<name>.md` or `.html` | a new revision by `Agent`; annotations re-anchor on it |
| new documents | a new file in `docs/` | a new agent document |
| thread replies | `replies.json`: `[{"annotation": <id>, "body": "..."}]` | an annotation message by `Agent` |

Refused or ignored, and listed in a system message after the synthesis:

- any created, changed or deleted path under `uploads/` (AC16);
- a deleted document, a file in a subdirectory of `docs/`, a name that is not `.md` or `.html`, a document over 2 MB or not UTF-8;
- a `replies.json` that does not parse, a reply to an unknown annotation, a reply without an id or a body.

Anything else the agent writes, in `transcript/` or at the root, is scratch and is dropped silently.

## Wire protocol

1. The server sends `event: turn` with `{"id": N}` on the daemon's stream.
2. `GET .../daemon/turns/N` returns the job: `prompt`, `resume` (session id or empty), `session_size`, `session_sha256`, `timeout_seconds`.
3. `GET .../daemon/turns/N/workspace` returns the tar.
4. If `resume` is set and the daemon's local `<id>.jsonl` does not match the size and hash, it fetches `GET .../daemon/sessions/<id>` and writes it into the sandbox home before running `claude -p --resume <id>` (D6). The cwd is always `/workspace`, so the session's project slug never changes (S2).
5. The daemon hashes `/workspace` before and after the run and sends each changed path. It sends contents only for `docs/` and `replies.json`, so an edited 20 MB upload costs one path.
6. `POST .../daemon/turns/N` with `{"synthesis", "changes", "session_id", "session_offset", "session"}`, or `{"error"}`. `session` holds the jsonl bytes past `session_offset`: the server's size when resuming, 0 for a new session (D23). The server refuses a gap or an overlap.

## Failure rules

| Event | Outcome |
|---|---|
| second press during a turn | 409 `turn in progress` |
| host daemon drops mid-turn | turn failed, nothing kept; a late result gets a 409 |
| daemon reports an error (credentials expired, Claude failed) | turn failed, the error in the room |
| result over 32 MB or malformed | 413 or 400, turn failed |
| session bytes do not continue the stored copy | 409, turn failed |
| no answer within `GRILL_ROOM_TURN_TIMEOUT` plus one minute | turn failed |
| room server restarts | every running turn failed at start |
