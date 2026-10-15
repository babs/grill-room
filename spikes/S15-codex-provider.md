# S15: Codex as a per-persona provider

Codex CLI 0.160.1, ChatGPT login (plan `team`), 2026-10-15. Scripts in `s15-codex/`, raw `--json` event logs in `s15-codex/runs/`.

## Verdict

**Go for spec 015**, with one design change. The sandbox should not run Codex in ChatGPT-login mode with a dummy `auth.json`. It should see a plain custom model provider whose `base_url` is the bearer-swap proxy and whose bearer comes from an env var. The proxy adds the real ChatGPT access token and `ChatGPT-Account-Id`. In that mode the sandbox has no `auth.json`, no refresh token and no refresh code path. A turn works through the proxy, in our sandbox image with `--network none --cap-drop ALL --read-only`, and it edits workspace files. A thread resumes in a fresh `CODEX_HOME` given only its rollout file.

Updated effort: **M, about 1000 to 1400 Go LOC** (was 800 to 1200). The extra goes to the Codex token source and refresher, the rollout sync, and getting the 290 MB binary into the image.

## Turns used (budget 8)

Model: `gpt-5.6-luna` at `low` effort (the account's cheapest listed model), one run on `gpt-6-luna` to test the override. A "turn" is one `codex exec` invocation. A request is one `POST /backend-api/codex/responses` that upstream answered 200.

| Run | Where | Mode | Result | Codex turns | 200 requests |
|---|---|---|---|---|---|
| t0 | host, dummy home | ChatGPT login, `chatgpt_base_url` on proxy | failed before the model: `selected workspace missing from routing discovery` | 0 | 0 |
| t0b | host, dummy home | same, proxy masks the account id | failed before the model: `workspace backend must use an HTTPS origin without credentials` | 0 | 0 |
| t1 | host, dummy home B | custom provider | `noted`, thread created | 1 | 1 |
| t2 | host, fresh home C holding only the rollout | `exec resume`, AGENTS.md added | `PAPRIKA-17 [agents-md-ok]` | 1 | 1 |
| t3 | sandbox container, home D holding only the rollout | `exec resume`, bypass flag | wrote `PAPRIKA-17` to `/workspace/answer.txt` | 1 | 3 |
| t401, t401b | sandbox container | proxy injects an invalid token | 401, `turn.failed`, no model compute | 0 | 0 |
| t4 | sandbox container | fresh `exec`, `-c model="gpt-6-luna"`, egress features off | appended to the workspace file | 1 | 2 |

Total: **4 Codex turns, 7 model requests.** One `codex app-server` call (`account/read`) against a dummy home, no model. The real `~/.codex` was never used by any run, and `auth.json` still has its 2026-10-07 mtime.

## 1. Bearer swap through a proxy

**ChatGPT-login mode (dummy `auth.json` + `chatgpt_base_url`) does not work through a plain HTTP proxy.**

- Before each turn, Codex 0.160.1 runs workspace routing discovery: `GET /backend-api/wham/accounts/check`, then it looks for the `auth.json` account id in the answer (`app-server/.../workspace_routing.rs`). With a placeholder account id, the turn fails with `selected workspace missing from routing discovery` (t0).
- With the proxy swapping the real account id for the placeholder in that answer, discovery passes. The workspace is `NO_CONSTRAINT`, so Codex then takes the origin of `chatgpt_base_url`, and `parse_backend_url` refuses anything that is not `https` (t0b).
- Making it work would need a TLS proxy with a CA trusted by Codex (`CODEX_CA_CERTIFICATE`). That would be more moving parts for a mode we do not need.

**Custom-provider mode works (t1 to t4).** Sandbox `config.toml`:

```toml
model_provider = "grillroom"

[model_providers.grillroom]
name = "grill-room proxy"
base_url = "http://127.0.0.1:8787/backend-api/codex"
env_key = "GRILL_ROOM_CODEX_BEARER"
wire_api = "responses"
```

- Host: only `https://chatgpt.com`, path `/backend-api/codex/responses`. The proxy only rewrites the scheme and host.
- Headers Codex sends: `authorization` (dummy), `originator: codex_exec`, `user-agent`, `session-id`, `thread-id`, `x-client-request-id`, `x-codex-beta-features`, `x-codex-turn-metadata`, `x-codex-window-id`, `x-openai-internal-codex-responses-lite`. It sends no account id in this mode.
- The proxy sets `Authorization: Bearer <real access_token>` and `ChatGPT-Account-Id: <tokens.account_id>`, both read from the host's `auth.json` on every request.
- Codex does not fetch `/models` in this mode. The context window still comes from its bundled catalog (`model_context_window: 258400` for both luna models).
- Transport: plain HTTPS streaming. Codex tried WebSockets only in ChatGPT mode (t0, t0b).

**Refresh from inside the sandbox.**

- Custom-provider mode: none possible. With an invalid token upstream answers 401 `Could not parse your authentication token`. Codex retries 5 times, then emits `turn.failed`. It never contacted `auth.openai.com` (t401). With `request_max_retries=0` and `stream_max_retries=0` it fails on the first 401 (t401b).
- ChatGPT mode, for the record: Codex refreshes proactively when the access token's JWT `exp` is within 5 minutes (or, for a non-JWT token, when `last_refresh` is over 8 days old). On a 401 it reloads `auth.json`, then POSTs to `https://auth.openai.com/oauth/token`, which `CODEX_REFRESH_TOKEN_URL_OVERRIDE` can redirect (`login/src/auth/manager.rs`). With that host blocked, the refresh fails and the dummy file is left untouched, as seen with `app-server` against a dummy home.

## 2. Who refreshes

Facts:

- The access token lives 10 days (`exp - iat` = 240 h). The host's file was last refreshed 8 days ago.
- The refresh token rotates. Codex has distinct errors for `refresh_token_reused`, `_expired` and `_invalidated`. A refresh made from a copy would leave the real login with a used refresh token, so copying the file is ruled out.
- `codex login status` only reads the file and never refreshes (`cli/src/login.rs`).
- `codex app-server` + `account/read {"refreshToken": true}` runs Codex's own refresh. It is **forced**: against a dummy home whose token was fresh, it still contacted `auth.openai.com` (refused by the spike proxy).
- Codex's refresh is guarded: it reloads `auth.json` first and skips the refresh if another process already changed it, and it saves through its own storage layer.

Recommendation, from least to most active:

1. **Default, passive.** The daemon reads `~/.codex/auth.json` read-only on each request (mtime cache, like `creds.Source`). It shows the JWT `exp` and warns under 48 h ("run codex on this machine"). The user's own Codex refreshes at `exp - 5 min` whenever they use it. This mirrors what we already do for Claude config-dir accounts.
2. **Opt-in, active.** When `exp` is under 24 h, the daemon runs `codex app-server` on the real `CODEX_HOME` and sends `account/read {"refreshToken": true}`, once per window, under a lock. Codex does the refresh and the write itself, and its guarded reload covers a TUI running at the same time. I did not run this against the real home, on purpose: it is a forced refresh. Spec 015 phase 1 should test it once, with the user present.
3. **Never.** No refresh POST from the daemon itself, and no copy of `auth.json` anywhere.

## 3. Session portability

- The thread is the rollout file `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl`. `state_5.sqlite` (table `threads`) is an index that Codex rebuilds from the rollouts: home C had no sqlite before t2 and had one, pointing at the copied rollout, after.
- The rollout is append-only: after the t2 resume, its first 68026 bytes were byte-identical to the original. Offset sync works the same way as for Claude sessions (`ReadSession` / `syncSession`).
- Resume across homes and cwds works. t1 ran in workspace 1 on home B, t2 in workspace 2 on a fresh home C (rollout only) and recalled the code word, and t3 resumed in the container (rollout only, cwd `/workspace`) and used it. Each resume records the new cwd in a fresh `turn_context`.
- An unknown thread id fails before any request: `thread/resume failed: no rollout found for thread id <id>`, exit 1. airc's lost-session regex matches it.
- Keep the rollout's relative path (date directories) when restoring it.

## 4. Codex in our sandbox container

The image was the locally built sandbox (`internal/sandbox/Dockerfile`, Codex absent). The `ghcr.io` tag was not pulled locally, so I did not use it. The host's static musl binary was mounted read-only. Flags: `--network none --cap-drop ALL --security-opt no-new-privileges --read-only --memory 2g --cpus 2 --pids-limit 512`, tmpfs on `/tmp`, `/home/agent` and `/workspace` (uid 1000). The proxy sat on a bind-mounted unix socket, exposed in-container as `127.0.0.1:8787` by socat (`s15-codex/sandbox.sh`).

- Codex's own sandbox bypass: `--dangerously-bypass-approvals-and-sandbox`, accepted by both `exec` and `exec resume`.
- t3 wrote `/workspace/answer.txt` (`file_change` event), t4 appended to it (`command_execution` event). Nothing failed on the read-only root.
- Stray egress seen in custom-provider mode with defaults: `chatgpt.com`, `github.com`, `api.github.com` (plugin sync) and `ab.chatgpt.com` (after the turn). With `--disable plugins --disable apps --disable remote_plugin --disable plugin_sharing -c analytics.enabled=false`, t401b and t4 made **no** request besides `/responses`.
- `github.com` is on our default egress allowlist, so those flags are required. The squid deny list should also get `.openai.com`, `.chatgpt.com` and `.oaiusercontent.com`, as we did for the Anthropic domains.

## 5. Flags and events

| Need | What works in 0.160.1 |
|---|---|
| First turn | `codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox -C /workspace -` (prompt on stdin) |
| Later turns | `codex exec resume <thread_id> --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox -` (no `-C`, no `-s`; cwd = process cwd) |
| Model / effort | `-c model="…"`, `-c model_reasoning_effort="low"`, on both (t2, t4). `-m` exists on both too. |
| Config check | `--strict-config` accepted every key above |
| Persona | `$CODEX_HOME/AGENTS.md`. Injected as a `# AGENTS.md instructions` user message at each process start. A file added before the resume took effect (t2, t3, t4), so persona edits apply to resumed threads. |
| Thread id | `{"type":"thread.started","thread_id":…}`, also on resume (same id) |
| Reply | last `item.completed` with `item.type == "agent_message"` |
| Usage | `turn.completed.usage`: `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens` |
| Errors | `error` events (retries), then `turn.failed.error.message` |

Gotchas:

- **`turn.completed.usage` is the thread's running total, not the turn's.** t2 reported 27809 input = 11461 (t1) + 16348 (t2's `last_token_usage` in the rollout). Per-turn usage = the difference, or `last_token_usage` from the rollout's last `token_count` event.
- **Neither the model id nor the context window appears in the `--json` events.** Both are in the rollout: `turn_context.model` / `.effort`, and `token_count.info.model_context_window` (258400 = 95% of 272000). The context the next turn starts from is `last_token_usage.input_tokens + output_tokens`.
- A fresh home installs `.system` skills into `$CODEX_HOME/skills`. A trivial first turn costs about 11.5k input tokens.

## Risks

| Risk | State | Evidence or mitigation |
|---|---|---|
| Bearer swap with a ChatGPT login | **Retired** | custom provider + proxy, t1 to t4 |
| ChatGPT-mode dummy `auth.json` through `chatgpt_base_url` | Dropped (not needed) | needs an HTTPS origin, t0b |
| Sandbox Codex refreshing or burning the user's refresh token | **Retired** | no refresh token in the sandbox, no `auth.openai.com` contact on 401 (t401) |
| Host-side refresh | **Mitigated** | passive by default; `app-server account/read refreshToken` as opt-in; forced refresh untested on the real home, to run once with the user |
| Session portability across daemons | **Retired** | rollout-only resume on a fresh home and in the container; append-only |
| Codex in the hardened container | **Retired** | t3, t4 |
| Stray egress (plugins, analytics) | **Mitigated** | `--disable` flags + `analytics.enabled=false`; add OpenAI domains to the squid deny list |
| Backend changes how it treats a non-`openai` provider on `/backend-api/codex` | **Open** | works on 0.160.1; pin the Codex version, smoke-test each bump |
| Model ids missing from the bundled catalog (unknown window) | **Mitigated** | set `model_context_window` per persona model, or pin |
| Image size: Codex binary is 289 MB (+74 MB `codex-code-mode-host`) | **Open** | spec decision: bake into the sandbox image, a separate image, or a cache volume; pin with a checksum |
| Usage double-counting | **Mitigated** | diff `turn.completed` or read the rollout |
| 401 retried 5 times with backoff | **Mitigated** | `request_max_retries=0`, `stream_max_retries=0` on the provider |

## Spec 015 deltas from the notes

- Auth: replace "dummy auth.json in the sandbox" with "custom provider + `GRILL_ROOM_CODEX_BEARER`". The Codex proxy injects `Authorization` and `ChatGPT-Account-Id` from the host's `auth.json`, read-only.
- Session: sync the rollout jsonl (append-only, offset) the way Claude sessions are synced. Ignore the sqlite.
- Usage: per-turn tokens and the context window come from the rollout, not from the events.
- Sandbox: Codex binary in the image; the `--disable` flags and the analytics override on every turn; OpenAI domains denied in squid.
