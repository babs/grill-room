# S2: cross-machine `--resume`

**Verdict: works.** Copying the single `<id>.jsonl` into an empty home is enough. Resume keeps the session id and only appends to the file. Neither a different `$HOME` nor a different cwd needed any rewrite in 2.1.296.

Setup: the S3 image and proxy, internal network, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `ENABLE_CLAUDEAI_MCP_SERVERS=false`, generated creds and `oauthAccount` stub. Each container gets its own new named volume as home. Runner: `s3/run-probe.sh`.

## Evidence

```
A  (HOME=/home/agent, cwd=/workspace, fresh vol)
   claude -p --model haiku --output-format json "The code word is PAPRIKA-42. Reply only: noted."
   -> {"result":"noted.","session_id":"60dc4912-36ec-4225-a44a-6a6c1ed85b1b"}
   file: /home/agent/.claude/projects/-workspace/60dc4912-....jsonl  (128,345 B, 21 lines)
B  (fresh vol holding only that jsonl, same path)
   claude -p --resume 60dc4912-... --model haiku "What was the code word? Reply with it only."
   -> {"result":"PAPRIKA-42","session_id":"60dc4912-36ec-4225-a44a-6a6c1ed85b1b"}   (file now 29 lines)
C  HOME=/home/other, cwd=/workspace, jsonl at /home/other/.claude/projects/-workspace/   -> "PAPRIKA-42"
D  HOME=/home/agent, cwd=/room, jsonl left under -workspace (not moved)                -> "PAPRIKA-42"
   appended lines carry "cwd":"/room" (4) next to the old "cwd":"/workspace" (13), same file
E  resume on B's volume with Write + Bash tool use (3 turns)                         -> "done."
   cmp: the original 128,345 B are a byte-identical prefix of the 141,306 B file
```

## Answers

1. Session id `60dc4912-36ec-4225-a44a-6a6c1ed85b1b`, taken from `--output-format json`.
2. The slug for `/workspace` is `-workspace`, the cwd with `/` replaced by `-`. `$HOME` plays no part in it.
3. Yes, B remembers. The session id stays the same and the turn is appended to the same file.
4. A different HOME works with no rewrite (C), because the slug comes only from cwd. A different cwd also works with no rewrite or move (D). 2.1.296 finds `<id>.jsonl` under any project dir and appends there. Old lines keep `cwd:/workspace`. Still, keep cwd fixed at `/workspace` so the upload path never changes.
5. Resume needs nothing besides the jsonl. B and C had only the jsonl, plus the generated creds and `.claude.json`. After a tool turn, the other paths Claude Code creates are all empty at exit: `session-env/<id>/`, `shell-snapshots/`, `sessions/`, `projects/-workspace/memory/`. No `todos/` or `file-history/` appears in `-p` mode. `.claude.json` and `backups/` hold local state the daemon regenerates anyway.
   The minimal upload per turn (D5) is `projects/-workspace/<id>.jsonl`. The file is append-only, so the daemon can send the bytes past the last uploaded offset. Upload `projects/-workspace/<id>/` too when it exists (see below).

## Notes for the spec

- The first turn alone writes 128 KB. Most of it is session overhead: a 92 KB `prompt_snapshot` attachment and a 13 KB `skill_listing`. Later turns add about 3 to 10 KB.
- The jsonl holds no secret (exact-match scan for the real tokens: 0). It does hold `HOME` paths inside the prompt snapshot (`/home/agent/.claude/projects/-workspace/memory/`) and the stub's fake `organizationUuid` (`credential_org` attachment). Fake stub values keep the host's identity out of uploaded sessions.
- `--allowedTools` is variadic. Put the prompt before it, or it swallows the prompt ("No deferred tool marker found").

## Unknown

- `<id>/subagents/*.jsonl` and `<id>/tool-results/` (large tool outputs saved beside the transcript) never appeared in these short turns. Resume behaviour when they are missing is untested.
- Cross-version resume (session from version X resumed by version Y) is untested. Both sides ran 2.1.296.
- Two daemons resuming the same id at once would both append to diverging copies. The room server has to serialise hosts (D6/D7).
- Whether the cross-slug lookup in run D is stable behaviour or an implementation detail.

## Resources (all removed)

Volumes `grs-home-s2{a..d}` (run E reused `s2b`). Containers `grs-s2{a..e}` and `-dump` sidecars, removed by `run-probe.sh`. Prep containers ran with `--rm`. Image, networks and proxy are shared with S3 (see `S3-proxy-swap.md`).
```
docker volume ls -q --filter label=grill-room-spike=1 | xargs -r docker volume rm
```
Kept: `.scratchpad/s2/` (session jsonl from A, plus `after-b.jsonl`).
