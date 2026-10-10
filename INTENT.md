# grill-room

Collaborative grill sessions. Remote humans talk, a Claude agent grills them on a plan, and the room produces versioned documents.

Built for product people and PMs, not only engineers. Joining a room needs a browser and a link, nothing else.

## Sources merged

| Project | What we take |
|---|---|
| airc | rooms, `claude -p --resume` driver, per-agent queue, crash rollback, SQLite transcript, SSE with replay |
| grip-live-diff | GitHub-style MD render, live reload, word-level HTML diff, text-quote annotations with threads |
| [lavish-axi](https://github.com/kunchenguid/lavish-axi) | features ported into gld, not embedded: HTML in a sandboxed iframe, element annotations, Mermaid editing, image attachments, queued feedback |

## Architecture

```
[Browsers] ──WebRTC──▶ LiveKit (kube)
     │                     └─▶ STT CPU worker (if Chrome on-device STT fails) ─┐
     └──HTTPS──▶ Room server (kube) ◀──────────────────────────────────────────┘
                  transcripts · docs · annotations · one git repo per room
                       ▲ outbound WS (no inbound port on user machines)
[Host daemon, machine A] ── local claude -p ── clone of the room repo
[Host daemon, machine B] ── takes over A's session when A drops
```

- One audio track per participant, so transcripts are per speaker without diarization.
- The STT output goes into the room as plain messages with `sender=<human>`. The transcriber is just another client of the room API.
- The agent runs on a participant's machine through a local daemon. The room server never runs Claude itself.

## Decisions

| # | Decision |
|---|---|
| D1 | Several remote humans per room. Fully self-hosted. |
| D2 | Room server, LiveKit and STT run in kube, one STT instance per room. |
| D3 | STT on CPU, server side: faster-whisper `small` int8, utterances cut on VAD pauses, language detected per utterance. Pod sized on simultaneous talkers, starting at 4 P-class cores and 1.5 GiB, with a queue; resize on the real kube node CPU (S1-bis). Chrome on-device STT is out (S1: no transcript in Chromium 153 or Chrome 155). |
| D4 | The agent runs on a participant's machine via a local daemon driving their Claude Code. |
| D5 | The daemon uploads the Claude session after every turn, not at disconnect, since a disconnect is often a crash. |
| D6 | When the host drops, another connected participant's daemon resumes the session. |
| D7 | The first participant with a connected daemon hosts. Anyone can switch the host to another connected participant. |
| D8 | With no host connected, the room keeps recording transcripts. The UI shows the agent loop as off with a red LED. |
| D9 | Lavish features are ported into gld (Go). One annotation model on the server, with text anchors and element anchors. |
| D10 | SQLite (airc store) is the only store: docs, revisions, transcripts, annotations. Files are a view built for each agent turn: `GET /snapshot` before, `POST` changed files after. Git export later if needed. |
| D11 | The daemon runs Claude in a Docker sandbox that mounts only the room clone. Nothing from the host's `~/.claude*` enters it: the daemon generates a minimal `.claude.json` and a `.credentials.json` holding a dummy token. The host's `~/.claude.json` carries `mcpServers` and 460 project entries, so it is never copied. |
| D12 | The agent acts only when a participant presses a button. One press runs one turn that reads the new transcript, posts a synthesis, annotates and edits the documents. |
| D13 | The PoC is text only. The agent writes, it does not speak. |
| D14 | Guests join with an invite link carrying a token. |
| D15 | A room outlives its sessions. A session either resumes the previous Claude session or starts fresh with access to the full room history. |
| D16 | Speech in French and English, mixed freely. No language restriction. |
| D17 | Credentials: option B from day 1. The proxy reads the real `~/.claude/.credentials.json` read-only; the host's Claude Code owns the refresh. A proxy outside the container holds the host's subscription token; the container never sees it. Each host uses only their own token. |
| D18 | The host may press the agent button. The host can grant the right to others or hand over the host role temporarily. |
| D19 | A session produces any number of free-form documents (MD or HTML), as the session needs. No fixed template. |
| D20 | Transcripts are exposed to the agent as Markdown files in its workspace, so a fresh session reads the room history with no extra API. |
| D21 | The sandbox sits on an internal Docker network whose only route is the proxy. With egress, Claude Code calls `api.anthropic.com` directly with the dummy token, gets a 401, and wipes its own credentials file (S3). |
| D22 | Before each turn the daemon writes a fresh `oauthAccount` stub (`profileFetchedAt` = now) into the sandbox `.claude.json`, and sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `ENABLE_CLAUDEAI_MCP_SERVERS=false`. |
| D23 | Session upload (D5) is the `<id>.jsonl` bytes past the last uploaded offset. The file is append-only, and the jsonl alone resumes in a fresh home with any HOME or cwd (S2). |
| D24 | The daemon is one small portable static Go binary that participants run on their machine. It opens an outbound connection to the room, embeds the bearer-swap proxy (S3), and drives Claude Code in the Docker sandbox. |
| D25 | Daemon targets Linux, macOS and Windows; Windows runs it inside WSL (wslc). |
| D26 | Daemon reads Claude credentials like `claude-quota` does: macOS Keychain first (`security find-generic-password -s "Claude Code-credentials" -a $USER -w`, `.claudeAiOauth`, with a timeout so a locked Keychain never blocks), then `<config dir>/.credentials.json`. Honour `CLAUDE_CONFIG_DIR`. Reuse `claude-quota`'s Go code and self-update. |
| D27 | Pairing: the daemon serves a local admin UI on `127.0.0.1` and opens it in the default browser. The room URL comes from the install one-liner or the CLI argument, or the user pastes the room link into the local UI. The user approves in the browser where the room session already exists, and the room hands the daemon its token. The local UI also shows status, logs and a stop button. |
| D28 | Distribution: a download button in the room UI, a `curl \| sh` one-liner carrying the room URL, or the standalone binary. |
| D29 | Any participant can upload documents into the room. Uploads land in the store (D10) and in the agent's workspace on the next turn. |
| D30 | Uploads are read-only sources: annotatable, never revised by the agent, which writes its own documents from them. MD and HTML render in gld, PDF and images go to Claude as-is, DOCX/ODT/PPTX get a pandoc copy in MD for annotation next to the original. |
| D31 | Uploads are blobs inside SQLite, 20 MB max per file, 200 MB max per room (defaults, configurable). |
| D32 | Logo and favicon: `design/icons/round2/r2-03-b-three-quarter.svg` (people around a Weber, 3/4 view). One asset; modern browsers render favicons at 32px and up. |
| D33 | The agent runtime is `claude -p` for now. Custom tools come as MCP via `--mcp-config` if files fall short; an own Agent SDK binary only for a concrete need the CLI cannot meet. |
| D34 | LiveKit needs no exposed UDP: embedded TURN behind traefik TLS termination on 443 (`HostSNI`), plus ICE over TCP. Traefik HTTP/3 (QUIC) does not help WebRTC media (S4). |

## Spikes before any code

- **S1, Chrome on-device STT.** Test page with `SpeechRecognition.processLocally = true`, in French, in English, then switching between the two mid-sentence. Compare with faster-whisper on CPU. Chrome takes one `lang` per recognizer, so mixed speech may fail where Whisper auto-detects.
- **S1-bis, faster-whisper on CPU.** Baseline for S1 and pod sizing for D2: real-time factor per model and thread count, concurrent streams per core, mixed FR/EN detection.
- **S2, cross-machine `--resume`.** Sessions live in `~/.claude/projects/<slug of absolute cwd>/<id>.jsonl`, and the slug depends on each user's `$HOME`. Check whether a jsonl copied to another machine resumes, with a fixed room path inside the container.
- **S3, bearer swap.** Give the container a `.credentials.json` with a dummy access token and a far expiry, and set `ANTHROPIC_BASE_URL` to the proxy. Claude Code stays in OAuth mode and sends its own headers; the proxy, running outside Docker, reads the real `~/.claude/.credentials.json` and swaps the dummy bearer for the real one. Check that Claude Code honours the base URL in OAuth mode, and list every endpoint it calls. Find the minimal `.claude.json` keys Claude Code needs to start in OAuth mode. The host's Claude Code owns the refresh and the proxy only reads, re-reading the file when it changes or on a 401. Check what happens when the access token expires while the host's Claude Code is not running. Later, if that breaks rooms: the proxy reads a dedicated `claude setup-token` token (subscription OAuth, `sk-ant-oat01`, about one year per community reports, several 401 issues reported), with no refresh.

| Spike | Status | Report |
|---|---|---|
| S1 | done, no-go: API present, `available()` true, zero results in Chromium and Chrome | `spikes/S1-chrome-stt.md` |
| S1-bis | done: `small` int8 only, ~2 s latency on 4 P-cores; medium/turbo too slow | `spikes/S1bis-whisper-cpu.md` |
| S2 | done, pass | `spikes/S2-resume.md` |
| S3 | done, pass with conditions D21-D22 | `spikes/S3-proxy-swap.md` |
| S4 | done, pass: TURN/TLS 443 via traefik and ICE/TCP, 0 loss | `spikes/S4-livekit-sni.md` |

## Credentials

The container must run Claude without being able to read a usable secret, since any participant can ask the agent to print a file.

Anthropic restricts subscription OAuth tokens (Free, Pro, Max) to Claude Code and Claude.ai. Tools that turn a subscription into a generic API, like CLIProxyAPI, are what that rule targets. The Agent SDK part of that rule was walked back on Feb 20, 2026, and a June 15 plan to move Agent SDK and `claude -p` usage to a separate credit was paused the same day. Recheck before relying on either.

Chosen: B from day 1 (D17). Rationale: the client is still Claude Code, each host uses their own token, nobody shares it, and every turn starts from a human pressing the button (D12). The tool replaces copy-pasting verbatims by hand. Precedent: T3 Code drives the Agent SDK on a subscription through this kind of proxy; here the client is the Claude Code CLI itself. S3 checks that B works without forging any header.

| Option | Exfiltration | Terms | Cost |
|---|---|---|---|
| A. Copy the host's subscription credentials, or a `claude setup-token` token, into the container | possible | Claude Code with the user's own token, but one subscription serves a whole room | subscription |
| B. Header-swapping proxy outside the container holding the subscription token | blocked | grey zone: Claude Code runs in API mode, so the proxy must forge the OAuth headers | subscription |
| C. Header-swapping proxy outside the container holding a Console API key, with Claude Code in documented gateway mode (`ANTHROPIC_BASE_URL`) | blocked | allowed | per token, with metering and a kill switch per room |

## Risks

- **Remote code execution on the host's machine.** Other participants steer the host's Claude. D11 bounds it to the container.
- **Uploaded sessions leak what Claude read.** The sandbox bounds this to the room clone.
- **Non-technical users.** No `!commands`, no CLI for participants. Hosting needs the daemon, but joining does not.

## Open questions

None. All spikes done. Next: the spec.
