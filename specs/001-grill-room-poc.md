# 001 — grill-room PoC

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-10
**Context**: `INTENT.md` (decisions D1-D33), spike reports in `spikes/`

## Problem

Product people, PMs and engineers run "grill sessions" to stress-test a plan. Today the feedback ends up in scattered notes, or someone copies verbatims by hand into a doc and asks Claude to consolidate it. Remote participants have no shared place where what they say, the documents and the agent's synthesis live together. And the person driving Claude is the only one who can feed it.

## Solution

A self-hosted web room. People join with an invite link and a browser and talk to each other. What each person says shows up as text, labelled with their name, a few seconds later. Anyone can upload documents and annotate them. When the host presses a button, a Claude agent reads everything said and annotated since its last turn. It posts a synthesis, annotates the documents and writes new revisions. The agent runs on a participant's own machine through a small binary, with that participant's Claude subscription, in a sandbox. If that participant leaves, another one can take over the same agent session.

## Scope

- Room server: a single static Go binary with SQLite storage (airc-derived). It handles rooms, invite links with tokens, participants, a live event stream and the web UI.
- Voice: LiveKit (self-hosted) for audio between participants. A CPU STT worker per room (faster-whisper `small` int8, VAD-cut utterances, language detected per utterance) posts transcript lines. No audio is stored.
- Documents: MD rendered GitHub-style and HTML in a sandboxed iframe (gld-derived), with revisions and word-level diffs between revisions.
- Uploads by any participant: MD, HTML, PDF and images as-is. DOCX/ODT/PPTX are kept as originals plus a pandoc MD copy for annotation. Uploads are read-only sources.
- Annotations on text ranges and HTML elements, with threads, status and image attachments (gld anchoring plus the lavish-style element anchors and attachments).
- Agent turn triggered by a button, with the right to press it held by the host and delegable. Output is text only.
- Daemon: a portable static Go binary for Linux, macOS and Windows via WSL. It has a local admin UI, browser-based pairing, an embedded bearer-swap proxy (Keychain or file credentials), a Docker sandbox on an internal network, and it drives `claude -p`. It uploads the session after each turn, can take over a session, and self-updates.
- Host election: the first participant with a connected daemon hosts, a switch is possible, and a red LED shows when no host is connected.
- Session continuity: resume the previous Claude session, or start fresh with transcripts provided as MD files.
- Distribution: a download button in the room UI, a `curl | sh` one-liner carrying the room URL, and the standalone binary.
- Dev and test environment: a local traefik serving a valid wildcard certificate for a dev domain that resolves to loopback (`*.<dev-domain>`), so it works on the dev machine only. LiveKit runs in the project's docker compose. Phases 1 to 7 are verified there, with no kube build loop.
- Deployment: a lab k8s cluster via ArgoCD, with TLS ingress (traefik and cert-manager) and LiveKit reachable from the Internet (Phase 8).

## Out of scope

- Mermaid/Excalidraw editing and render-time layout checks (lavish advanced features). A later spec.
- Agent voice (TTS). The PoC agent writes, it does not speak (D13).
- An own agent built on the Agent SDK (D33).
- Git export of room history (D10).
- OIDC or accounts. Invite links with tokens only (D14).
- Chrome on-device STT (S1 no-go).
- Storing audio.
- Multi-room scheduling, billing, or org management.

## Acceptance criteria

### Room, access, personal data
- [ ] **AC1** Given a room, when its creator generates an invite link and a guest opens it in a browser, then the guest picks a display name and joins with no account and no install.
- [ ] **AC2** A request without a valid room token gets a 401 on every room API route and on the event stream.
- [ ] **AC3** A revoked invite token gets a 401 on its next request, and its open event stream is closed.
- [ ] **AC4** Deleting a room removes its transcripts, documents, revisions, annotations, uploads and session data from the store. A query for that room id afterwards returns nothing.
- [ ] **AC5** `GET /healthz` returns 200 when SQLite is writable and 503 otherwise. `/metrics` is served on a separate port.

### Voice and transcripts
- [ ] **AC6** Given two participants in a room, when one speaks, then the other hears them.
- [ ] **AC7** Given a participant speaking French, when they pause, then a transcript line with their display name appears for everyone within 10 s of the end of the utterance.
- [ ] **AC8** Given utterances alternating between French and English, each line is transcribed in the language spoken, not translated.
- [ ] **AC9** No audio file or audio blob exists on the server or in the store after a session. Verified by inspecting the STT pod filesystem and the SQLite tables.
- [ ] **AC10** When the STT worker is down, the room still works for audio and text. The UI shows "transcription unavailable", and lines resume when the worker returns.

### Documents and uploads
- [ ] **AC11** An uploaded MD renders like GitHub (headings, tables, code, mermaid as a diagram). An uploaded HTML renders in a sandboxed iframe with no access to the room's cookies or API.
- [ ] **AC12** An uploaded PDF or image is viewable in the room and appears in the agent workspace as the original file.
- [ ] **AC13** An uploaded DOCX, ODT or PPTX is kept as the original, and an MD copy produced by pandoc appears next to it and is annotatable.
- [ ] **AC14** An upload over 20 MB gets a 413, and an upload that would take the room over 200 MB gets a 413 with a message naming the room limit.
- [ ] **AC15** An upload with an unsupported type gets a 415 naming the accepted types.
- [ ] **AC16** An uploaded document cannot be modified by the agent. An agent change to an upload is rejected and reported in the turn result.
- [ ] **AC17** A document produced by the agent shows its revision list, and any two revisions can be compared with a word-level diff.

### Annotations
- [ ] **AC18** A participant can annotate a selected text range in an MD document, and other participants see the mark and the thread live.
- [ ] **AC19** A participant can annotate an element in an HTML document, and the mark follows the element after a re-render.
- [ ] **AC20** A participant can attach an image to an annotation message, and the agent receives the image with the thread.
- [ ] **AC21** When the annotated text is reworded in a new revision, the annotation re-anchors on its context. When the text is gone, it stays reachable and is flagged as orphaned.

### Daemon, pairing, sandbox, credentials
- [ ] **AC22** Running the daemon with a room URL opens a local admin UI on `127.0.0.1` in the default browser. Approving in the room browser session pairs it, with no token shown or typed.
- [ ] **AC23** The daemon runs on Linux, macOS and Windows/WSL. On macOS it reads credentials from the Keychain; elsewhere from `<config dir>/.credentials.json`, honouring `CLAUDE_CONFIG_DIR`.
- [ ] **AC24** A locked macOS Keychain makes the credentials read fail within 5 s with a clear message in the local UI, not a hang.
- [ ] **AC25** The sandbox container holds no real credential. A grep for the real access token over the container filesystem and environment finds nothing.
- [ ] **AC26** From inside the sandbox, any host other than the proxy is unreachable (DNS and TCP to `api.anthropic.com` fail).
- [ ] **AC27** The daemon never writes to the host's `~/.claude/`, `~/.claude.json` or Keychain entry. Their mtime is unchanged after a session.
- [ ] **AC28** When Docker is missing or not running, the local UI says so and the daemon does not offer to host.
- [ ] **AC29** When the host's access token is expired, the turn fails within 10 s with "Claude credentials expired on <host>" in the room, and the next turn works once the host's Claude Code has refreshed.

### Host, turns, continuity
- [ ] **AC30** The first participant whose daemon connects becomes host. With no daemon connected, the agent LED is red and the agent button is disabled, while transcripts keep recording.
- [ ] **AC31** The host can hand the host role to another participant with a connected daemon, and can grant the agent button to other participants.
- [ ] **AC32** Pressing the agent button runs one turn. The agent receives the transcript and annotations since its last turn and posts a synthesis message. Its document changes become new revisions attributed to the agent. A second press during a turn is refused with "turn in progress".
- [ ] **AC33** After each turn, the server holds the session jsonl up to the turn's end.
- [ ] **AC34** When the host's daemon disconnects mid-turn, the turn is marked failed and no partial revision is kept. When another daemon takes over, the next turn resumes the same Claude session (`--resume`) and the agent recalls a fact from before the switch.
- [ ] **AC35** Starting a fresh session in an existing room gives the agent the full room history as MD transcript files in its workspace.

### Distribution
- [ ] **AC36** The room UI offers the daemon download for the visitor's OS and a `curl | sh` one-liner that installs the binary and starts it paired to that room.
- [ ] **AC37** The daemon checks for a newer release at start, updates itself on approval in the local UI, and restarts.

### Deployment
- [ ] **AC38** The room server, LiveKit and the STT worker deploy on the lab cluster from ArgoCD. The room is reachable over HTTPS with a valid certificate, and AC6 passes between a participant on the LAN and one outside it.

## Phases

### Phase 1 — Room server skeleton
- Work: create the repo layout and the local dev environment (`compose.yaml` for the room server and LiveKit, plus `deploy/local/traefik-grill-room.yaml`, a file-provider rule for `grill-room.<dev-domain>` and `livekit.<dev-domain>` pointing to `host.docker.internal`). Port airc's store, bus and SSE into a room model (rooms, participants, invite tokens, messages). Build the web UI shell with the logo (D32), `/healthz` and `/metrics`, and the Dockerfile.
- **Covers**: AC1, AC2, AC3, AC4, AC5
- **Data model impact**: new SQLite schema with `rooms`, `participants`, `invites`, `messages`
- **DoD**: `https://grill-room.<dev-domain>/healthz` returns 200 with a valid certificate; `go test ./...` green; `golangci-lint` clean; an e2e test creates a room, joins by invite, posts a message, revokes the token (401), and deletes the room (empty query).

### Phase 2 — Documents, revisions, uploads
- Work: port gld's render and htmldiff. Add the documents and revisions tables, uploads with type and size checks, the pandoc conversion, and the HTML sandboxed iframe.
- **Covers**: AC11, AC12, AC13, AC14, AC15, AC17
- **Data model impact**: `documents`, `revisions`, `uploads` (blobs)
- **DoD**: table-driven tests for limits (413) and types (415); golden tests for render and diff; a pandoc fixture for each of DOCX, ODT and PPTX produces MD.

### Phase 3 — Annotations
- Work: one annotation model with text-quote and element anchors, threads, status and image attachments. Port `annotate.js` to the room UI with live updates over the event stream.
- **Covers**: AC18, AC19, AC20, AC21
- **Data model impact**: `annotations`, `annotation_messages`, `attachments`
- **DoD**: re-anchoring tests (reworded, removed, element re-render); a two-browser e2e (Playwright) shows a mark created in one appearing in the other.

### Phase 4 — Voice and STT
- Work: LiveKit in the lab via ArgoCD, room token minting by the room server, and browser audio join. The STT worker subscribes to each track, cuts on VAD pauses, transcribes, and posts lines through the room API. The UI shows STT health.
- **Covers**: AC6, AC7, AC8, AC9, AC10
- **Data model impact**: `messages.kind` (`voice` | `text` | `agent` | `system`)
- **DoD**: two browser profiles on `grill-room.<dev-domain>` exchange audio; a recorded FR+EN fixture played into a test participant yields lines in the right languages within 10 s; the STT pod has no audio files (`find / -name '*.wav' -o -name '*.opus'` empty); killing the STT pod shows the banner and recovers.

### Phase 5 — Daemon: pairing, proxy, sandbox
- Work: the daemon binary with the local admin UI, pairing flow, embedded proxy (credentials from the Keychain with a timeout or the file, `CLAUDE_CONFIG_DIR`), Docker sandbox on an internal network with generated stubs (D22), and the Docker checks. Reuse `claude-quota`'s credentials code.
- **Covers**: AC22, AC23, AC24, AC25, AC26, AC27, AC28, AC29
- **Data model impact**: `daemons` (paired daemon per participant, token hash)
- **DoD**: `claude -p --model haiku "say ok"` succeeds through the sandbox on Linux, macOS and WSL; a grep for the real token in `docker export` output and `docker inspect` env is empty; `curl api.anthropic.com` from the sandbox fails; host `~/.claude*` mtimes unchanged; locked Keychain test returns in under 5 s.

### Phase 6 — Agent turn, host election, continuity
- Work: host election and handover, LED, button rights and delegation. The turn runs as snapshot, then `claude -p`, then a POST of changes, then revisions, synthesis and annotations, and is refused when one is in progress. Read-only enforcement on uploads. Jsonl upload by offset. Takeover with `--resume`. A fresh session gets the transcripts as MD.
- **Covers**: AC16, AC30, AC31, AC32, AC33, AC34, AC35
- **Data model impact**: `turns`, `agent_sessions` (session id, jsonl blob, uploaded offset), `rooms.host_participant_id`, `participants.can_trigger`
- **DoD**: an e2e with two daemons: turn on A, kill A mid-turn (turn failed, no partial revision), take over on B, the agent recalls the code word; the fresh-session test finds the transcripts in the workspace; the agent's edit to an upload is rejected.

### Phase 7 — Distribution and self-update
- Work: release builds for linux/darwin amd64 and arm64, a download page per OS, `install.sh` carrying the room URL, and self-update ported from `claude-quota`.
- **Covers**: AC36, AC37
- **Data model impact**: none
- **DoD**: on a clean VM per OS, the one-liner installs, starts and pairs; bumping the release triggers the update prompt and restart.

### Phase 8 — Lab deployment end to end
- Work: ArgoCD apps (room server, LiveKit, STT), traefik ingress with cert-manager, LiveKit public reachability, and default-deny NetworkPolicies.
- **Covers**: AC38
- **Data model impact**: none
- **DoD**: ArgoCD apps Healthy and Synced; HTTPS certificate valid; a full grill session (two humans, one inside and one outside the LAN, upload, annotate, agent turn) passes.

## Data model impact (summary)

New SQLite database:
- `rooms`, `participants`, `invites`, `messages` (`kind`)
- `documents`, `revisions`, `uploads` (blobs)
- `annotations`, `annotation_messages`, `attachments`
- `daemons`, `turns`, `agent_sessions`

No existing data to migrate.

## Personal data

- The room stores display names and transcripts of what people said, which is personal data.
- Access relies on invite tokens only (D14). Anyone holding a valid link can read the room.
- No audio is stored (AC9). Deleting a room removes everything (AC4).
- No retention timer in the PoC: rooms live until deleted.
- These are the defaults chosen for the PoC; confirm them at approval (open question 2).

## Open questions

- [ ] 1. Blocks Phase 8 only. LiveKit reachability from outside the LAN: which public path does the lab offer for UDP (router port-forward to a LoadBalancer IP), or do we use TURN/TLS on 443 only?
- [ ] 2. Personal data defaults above (invite-link access, no retention timer): approve or change.

## Decisions

- Phases follow dependency order: the agent turn needs documents and the daemon, and voice is independent of both. Phase 4 can run in parallel with 2 and 3.
- The room server and the daemon live in one Go module, as two binaries (`cmd/grill-room`, `cmd/grill-room-daemon`), so they share types.
- The STT worker is a separate Python image (faster-whisper), talking to the room through its HTTP API like any client.
- Frontend is vanilla JS with no build step, as in airc and gld.
- AC7 allows 10 s: S1-bis measured about 2 s on 4 P-cores, and this is a PoC.
- The traefik rule ships in the repo (`deploy/local/`). Enabling it means copying it into the local traefik's rules directory, and removing the file reverts it. The copy is proposed for your approval at Phase 1, never done silently.
- Code copied from airc and gld keeps a header comment naming the upstream repo and commit.
