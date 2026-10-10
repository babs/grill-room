# S3: bearer-swap proxy

**Verdict: works, with one hard condition.** Claude Code 2.1.296 in OAuth mode sends `/v1/messages` to `ANTHROPIC_BASE_URL` with its own OAuth headers. The proxy only swaps `Authorization`. The condition is that the sandbox has no route to `api.anthropic.com` or `platform.claude.com`. With egress, side calls go out directly with the dummy token, get a 401, and trigger a refresh that fails. Claude Code then empties its credentials file and the home is dead.

Code: `s3/proxy/` (Go, `httputil.ReverseProxy`, 7 table tests + lint clean), `s3/Dockerfile`, `s3/entrypoint.sh`, `s3/run-probe.sh` (container plus a tcpdump sidecar in its netns, `--debug-file`).
Proxy: binds one bridge IP (refuses `:port`/`0.0.0.0`), rejects any bearer except the expected dummy (403), re-reads the file on mtime change or after an upstream 401. It logs method, path, status, header names, and the values of `anthropic-beta`, `user-agent` and `x-app`.

## Runs (haiku, `say ok`)

| Run | Network | Config | Result | Direct calls (bypassing base URL) |
|---|---|---|---|---|
| a | egress | `.claude.json`=`{}` | FAIL "OAuth session expired and could not be refreshed" | profile, mcp_servers, bootstrap, fast-mode: 401. Refresh to platform.claude.com: 400. Container creds wiped to `""` |
| b | internal | `{}` | ok | all EAI_AGAIN: `api/oauth/profile`, `v1/mcp_servers`, bootstrap, fast-mode, MCP registry, `api/event_logging/v2/batch` |
| c | internal | b + `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` | ok | `api/oauth/profile`, `v1/mcp_servers` only |
| d | internal | c + `ENABLE_CLAUDEAI_MCP_SERVERS=false` + `oauthAccount` stub | ok | none, 0 DNS queries |
| e | egress | same as d | ok | none, 0 outbound SYNs |
| f | internal | no `.claude.json` at all | ok | n/a |
| g | internal | proxy fed an expired fake token | FAIL 401 "OAuth access token is invalid", 2 s | refresh to platform.claude.com x2, EAI_AGAIN. Creds unchanged (sha256 = seed) |

Proxy log for run b (excerpt):
```
POST /v1/messages 200  user-agent=claude-cli/2.1.296 (external, sdk-cli)  x-app=cli
anthropic-beta=oauth-2025-04-20,interleaved-thinking-2025-05-14,claude-code-20250219,context-management-2025-06-27,...
header_names=[accept,...,anthropic-beta,anthropic-dangerous-direct-browser-access,anthropic-version,authorization,x-claude-code-session-id,x-stainless-*]
HEAD /api/hello -> 403 (connectivity probe, no Authorization; harmless)
```
Debug log for run a: `Failed to fetch oauth profile ... 401` → `OAuth refresh failed (expected): 400` → `OAuth refresh token is no longer valid`. The binary hardcodes `BASE_API_URL:"https://api.anthropic.com"` and `TOKEN_URL:"https://platform.claude.com/v1/oauth/token"`. Neither follows `ANTHROPIC_BASE_URL`.

## Answers

1. Yes. The base URL is honoured for `/v1/messages`, and Claude Code adds `anthropic-beta: oauth-2025-04-20` and its other headers on its own. The proxy forges nothing.
2. Through the proxy: `POST /v1/messages`, `HEAD /api/hello`. Direct: the hosts in the table, all on `api.anthropic.com`, plus `platform.claude.com` for refresh after a 401. Nothing went to claude.ai, statsig or growthbook. `DISABLE_NONESSENTIAL_TRAFFIC` removes telemetry, bootstrap, fast-mode and the MCP registry, but not the profile fetch or `v1/mcp_servers`. Run d removes those too.
3. None. `claude -p` starts with no `.claude.json` (run f). To get zero direct calls, the daemon writes `{"oauthAccount":{accountUuid, emailAddress, organizationUuid, billingType, accountCreatedAt, subscriptionCreatedAt, ccOnboardingFlags:{}, profileFetchedAt:<now ms>}}` with fake values. The binary skips the profile fetch only when these are set and `profileFetchedAt` is under 24 h old, so the daemon rewrites it every turn.
4. The access token lives 8 h. The real file's mtime is 05:12:53Z and `expiresAt` is 13:12:53Z. `refreshTokenExpiresAt` is 2026-11-04. Once the token expires, the proxy forwards it anyway (logging `token_expired`), upstream returns a 401, and the turn fails in about 2 s. The proxy drops its cache on the 401 and picks up the new token on the next mtime change, and a reused home works again (run g, then retry: ok). The container does try to refresh its dummy token after any 401. With no egress that attempt dies at DNS. With egress it reaches platform.claude.com, gets a 400, and wipes the container's creds (run a; the retry stays broken). The host's Claude Code refreshes only while it runs, within 5 min of expiry (`n+300000>=e` in the binary).
5. Yes: run d gives `"result":"ok"` with zero direct calls.

## Daemon requirements found

- Internal Docker network only. The proxy listens on that bridge's gateway IP via `--add-host=host.docker.internal:<gw>`.
- Env: `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `ENABLE_CLAUDEAI_MCP_SERVERS=false`. Regenerate `.credentials.json` and the stub before each turn.
- Side effect: ToolSearch is off for non-first-party base URLs (`ENABLE_TOOL_SEARCH=true` re-enables it).

## Unknown

- Live expiry while the host's Claude Code is idle. It was simulated with a fake token, not observed.
- A cheap host command that triggers a refresh without an inference call. Not tested, since it would write the real file.
- Whether Anthropic flags the fake `organizationUuid` (it is sent as a `credential_org` attachment). Accepted in all 8 stub sessions.
- Proxy skips the Prometheus/metrics server from golang.md (spike scope).

## Resources (all removed)

Images `grs-claude:spike`, `grs-tcpdump:spike`. Networks `grs-noegress` (internal), `grs-egress`. Volumes `grs-home-s3{a..g}`. Containers `grs-s3*` (removed by `run-probe.sh`). Proxy processes on the bridge gateway, ports 8787/8788, killed. Everything carries the label `grill-room-spike=1`:
```
docker ps -aq --filter label=grill-room-spike=1 | xargs -r docker rm -f
docker volume ls -q --filter label=grill-room-spike=1 | xargs -r docker volume rm
docker network rm grs-noegress grs-egress; docker rmi grs-claude:spike grs-tcpdump:spike
pkill -f '/.scratchpad/bin/grs-proxy$'
```
Kept: `.scratchpad/{bin,logs,seed*,fakehost}`, the pulled base images `debian:trixie-slim` and `alpine:3.22`, and the Go module cache (go1.27.2 toolchain, golangci-lint v2).
Usage: 12 inference requests through the proxy across S2 and S3.
