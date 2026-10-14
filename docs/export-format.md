# Room export format, version 2

A room export is a gzip'd tar of regular files. The last entry is `manifest.json`. An import accepts formats 1 and 2, and refuses anything newer with the reason. Format 2 adds `mentions.jsonl` and the documents' `deleted_at`; a format 1 archive imports with no mention chips and every document live. Within format 2, `note_mentions.jsonl` and a persona's `watches_notes` came later and stay optional: an older format 2 archive imports without them, and an older instance ignores them.

Paths are plain and relative: `[A-Za-z0-9._-]` segments joined by `/`, no `.` or `..` segment, no leading `/`. Only regular files appear: no directory, link or device entry.

## manifest.json

```json
{
  "format": 2,
  "instance_version": "v0.0.4",
  "instance_id": "<random id of the exporting instance>",
  "created_at": "2026-10-12T09:00:00Z",
  "room": { "id": "<room id>", "name": "Pricing review" },
  "files": { "room.json": { "size": 512, "sha256": "<hex>" } }
}
```

`files` lists every other entry of the archive with its size and sha256. An import refuses an archive holding a file the manifest does not list, missing a file it lists, or holding one of another size or hash. `instance_id` tells an instance its own exports apart: only those link skills and templates back to its library.

## Refs

Every row carries `ref`, its id on the exporting instance. Other rows point to it by that ref (`participant`, `persona`, `message`, `document`, `upload`, `annotation`, `skill`). Refs mean nothing to the importing instance, which gives every row a new id. Rows come in ref order, skills and their versions in name order. Timestamps are RFC 3339 strings, kept as stored.

## Files

| File | Holds |
|---|---|
| `room.json` | `name`, `created_at`, `chain` (`budget`, `count`, `suspended`), `parallel_turns`, `stt_glossary`, `created_from` (the starter's `title` and `version`, absent for a room from no starter), and `personas` |
| `participants.jsonl` | `ref`, `name`, `joined_at`, `can_trigger`, `active` (false once their invite was revoked), `profile_uuid` (or the hint an earlier import kept), `admin` |
| `messages.jsonl` | `ref`, `ts`, `kind` (`voice`, `text`, `agent`, `system`), `participant`, `persona`, `sender`, `body`, `model`, `model_note`, `model_mismatch`, `effort`, `requested_model`, `requested_effort`, `edited_at`, `deleted_at`, `change_seq` |
| `mentions.jsonl` | how each line's @mention of a persona ended: `message`, `persona`, `status` (`done`, `failed`, `stopped`, `cancelled`, or `queued` and `running` at export, which import as `cancelled`: the turn history stays behind), `line` (the synthesis, or the line telling the failure or stop) |
| `note_mentions.jsonl` | the same for note messages: `message` is an annotation message's ref. Optional: an archive from before it has none, and imports with no chips on its notes |
| `message_versions.jsonl` | earlier bodies of edited lines: `message`, `seq`, `body`, `written_at` |
| `transcript.md` | the lines as Markdown, each followed by its earlier versions; deletions marked. For people: the import never reads it |
| `uploads.jsonl` and `uploads/<ref>` | `ref`, `participant`, `filename`, `content_type`, `size`, `created_at`, `file`; the file is the original |
| `documents.jsonl` | `ref`, `kind` (`upload`, `agent`), `name`, `format` (`md`, `html`, `pdf`, `image`), `upload`, `author`, `created_at`, and `deleted_at` for a document a room admin deleted, which imports deleted |
| `revisions.jsonl` and `documents/<document>/<n>.<format>` | `document`, `n`, `author`, `created_at`, `file`. An office upload's revision 1 is its Markdown copy |
| `annotations.jsonl` | `ref`, `document`, `revision`, `anchor` (null for a whole-document thread), `orphaned`, `status`, `version`, `created_at` |
| `annotation_messages.jsonl` | `ref`, `annotation`, `by` (`participant`, `agent`), `participant`, `persona`, `author`, `body`, `created_at` |
| `attachments.jsonl` and `attachments/<ref>` | `ref`, `message` (an annotation message), `content_type`, `size`, `created_at`, `file` |
| `skills.jsonl` | `ref`, `name`, `scope` (`room`, `shared`, `personal`), `pending`, `created_at` |
| `skill_versions.jsonl` and `skills/<skill>/<n>.tar` | `skill`, `n`, `author`, `description`, `files`, `size`, `sha256`, `created_at`, `file`; the file is the skill's normalised tar |
| `sessions.jsonl` and `sessions/<persona>/<i>.jsonl` | `persona`, `session_id`, `updated_at`, `context_tokens`, `context_window`, `context_updated_at`, `size`, `file`; the file is the Claude Code session jsonl |

A persona in `room.json` has `ref`, `name`, `badge`, `colour`, `text`, `model`, `effort`, `template` (its library template's ref) and `template_preset` (the key of a shipped template), `template_version`, `every_n`, `every_voice`, `every_minutes`, `self_call`, `watches_notes` (optional, true for the room's notes watcher, at most one), `autocompact`, `created_at`, `skills` (`skill` and `version` of each skill it has on), and `resume`. An archive from before explicit models may leave `model` or `effort` empty: the import reads them as `sonnet` and `medium`. `resume` is where its next turn picks up: the `session_id` of its last successful turn, the `message_cursor` and `annotation_cursor` refs that turn had read up to, `change_cursor`, `model`, `effort` and `ended_at`.

Skills: the room's own skills come with every version. A shared or personal skill a persona has on comes with the pinned versions only.

## What an export leaves out

- Emails, profile ids, tokens and token hashes, invites, and any profile uuid but the participants'.
- Daemons, the host, and the host's cap decisions on personas.
- Personas waiting for the host's approval, and held persona changes.
- The turn history, apart from each persona's resume point, and the turn queue.
- The room's own quota: the importing instance applies its default.

## Import checks

On top of the archive checks above, an import re-reads the content the room trusts:

- a line's HTML is rendered again from its body, before the import's transaction starts;
- names, bodies and authors pass the room API's text rules: UTF-8, no control character, no bidi control in a single-line field, and its length limit (100 for the room name, 40 for a participant, 4000 for a human line or thread message, 20 000 for an agent or system line, 200 for a document or file name);
- an upload's type comes from its content, and must match its document's format;
- an attachment must read as a PNG, JPEG, GIF or WebP image;
- an anchor must validate as one the room would take;
- a skill name must be one Claude Code takes, and a skill version a normalised skill tar whose files pass the upload rules and hash to its `sha256`;
- a persona must pass the room API's checks: at most 12 personas, each with at most 10 skills on; at most 20 room skills, 20 versions each;
- every ref must point to a row of the archive, and a session id must be a UUID;
- a JSON line is at most 1 MiB.

Relinking by uuid: the super-admin's import relinks every participant whose uuid matches a profile here. A profile's import relinks its own uuid, and the room's participants only when the profile administers the exported room on this instance. Any other uuid becomes a hint: an archive cannot seat someone in a room its importer controls.

Each limit of the importing instance applies: largest upload and image, room quota on uploads plus images, instance storage quota on the uncompressed size, rooms per profile. A refusal names the first check that failed, and creates nothing: the whole import is one transaction.
