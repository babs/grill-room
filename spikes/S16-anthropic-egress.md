# S16: Anthropic egress from the sandbox

**Verdict: no MitM needed.** WebSearch already works. WebFetch needs one setting, `skipWebFetchPreflight: true`, and Anthropic stays blocked. claude.ai connectors work with no MitM: the daemon lists them on the host and hands the sandbox an `--mcp-config` that points at the bearer-swap proxy, which forwards to `mcp-proxy.anthropic.com`. I ran this end to end. The spike also found a live hole. The proxy forwards **any** path to `api.anthropic.com` with the real token, and the agent can read the dummy bearer, so a prompt-injected agent can spend the real token on any API. Fix that first, whatever happens to S16.

Claude Code 2.1.296 (the pinned sandbox image), 2026-10-15. Harness: `s16-egress/main.go` (build tag `spike`). It runs the production `internal/sandbox` (internal network, socat relay, squid egress, dummy creds and stubs, the same env) behind the production `internal/proxy`, fed by this machine's login through `creds.Source`, which never writes. Captures per run: squid's access log (the daemon's denied list reads the same log), the proxy's JSON log, Claude Code's `--debug-file`, the session jsonl and the sandbox's `.credentials.json` afterwards. Every bearer is redacted on save.

Claude requests: 4 `claude -p` turns on haiku (3 answered, 1 refused with 401 by design). Plus, from the host and with no model call: 3 `GET /v1/mcp_servers` and MCP `initialize` + `tools/list` on each connector. No connector tool was called.

## Runs

| Run | Setup | squid | Proxy | Tool result |
|---|---|---|---|---|
| tools-prod | production args, WebSearch + WebFetch pypi.org + WebFetch example.com | `TCP_DENIED CONNECT api.anthropic.com` x2 | `POST /v1/messages` 200 x5 | WebSearch ok, 2 sources. Both WebFetch fail: "Unable to verify if domain pypi.org is safe to fetch. This may be due to network restrictions or enterprise security policies blocking claude.ai." |
| tools-skip-preflight | as above + `--settings '{"skipWebFetchPreflight":true}'` | `TCP_TUNNEL CONNECT pypi.org`, `TCP_DENIED CONNECT example.com` | `POST /v1/messages` 200 x6 | WebSearch ok. pypi.org ok (requests 2.34.2). example.com: "proxy refused the connection" |
| mcp | `ENABLE_CLAUDEAI_MCP_SERVERS=true`, `claude mcp list`, no model call | none, then `TCP_DENIED CONNECT api.anthropic.com` x3 | none | production stub scopes: `[claudeai-mcp] Missing user:mcp_servers scope`, no call. With `user:mcp_servers` added: `Fetching from https://api.anthropic.com/v1/mcp_servers...` then `Fetch failed (ERR_PROXY_TUNNEL) after 3 attempt(s)` |
| mcp (agent probe) | `curl` from the sandbox: dummy bearer read from `~/.claude/.credentials.json`, sent to `http://proxy:8787/v1/mcp_servers` | none | `GET /v1/mcp_servers` **200** | the agent used the real token outside inference |
| mcp-route | one connector (Calendar) in `--mcp-config` as `type: http` to `http://proxy:8787/v1/mcp/<id>`, dummy bearer header; the harness swaps it and forwards to `mcp-proxy.anthropic.com` | none | `POST /v1/mcp/<id>` 200 x2, `POST /v1/messages` 200 | connected in 401 ms, the model listed the read tools. The write tools were in `--disallowedTools` |
| refresh-401 | proxy fed a forged token, `say ok` | `TCP_DENIED CONNECT platform.claude.com` x2 | `POST /v1/messages` 401 x2 | "OAuth refresh failed (expected): proxy refused the connection", turn fails. Sandbox creds **unchanged** |

The harness on the host, with the real token: `GET /v1/mcp_servers` returned 10 connectors. Each entry has a `url` (the connector's own server) but Claude Code never dials it: it calls `mcp-proxy.anthropic.com/v1/mcp/{id}`. mcp-proxy answers 400 without `X-Mcp-Client-Session-Id`, 200 with it (5 connectors), and 401 `mcp_invalid_oauth_token` for the 5 whose claude.ai-side login has lapsed.

## What the binary does (static read of 2.1.296)

- `BASE_API_URL` (`https://api.anthropic.com`), `TOKEN_URL` (`https://platform.claude.com/v1/oauth/token`) and `MCP_PROXY_URL` (`https://mcp-proxy.anthropic.com`, path `/v1/mcp/{server_id}`) are constants. `ANTHROPIC_BASE_URL` moves only the Messages API client. `CLAUDE_CODE_CUSTOM_OAUTH_URL` moves them all, but the binary throws unless it is one of 3 hardcoded Anthropic endpoints, so it cannot point at our proxy.
- WebSearch is the server tool `web_search_20250305`, sent on `/v1/messages`, so it follows `ANTHROPIC_BASE_URL`.
- WebFetch preflight: `GET https://api.anthropic.com/api/web/domain_info?domain=<host>` with only a `User-Agent`, **no bearer**. A failed check refuses the fetch. The setting `skipWebFetchPreflight` turns it off. The fetch itself goes through `HTTPS_PROXY`, squid here, and the page summary goes back through `/v1/messages`.
- Connectors: Claude Code skips them on `ENABLE_CLAUDEAI_MCP_SERVERS=false` or the `disableClaudeAiConnectors` setting, on a third-party provider, or on a token without `user:mcp_servers`. There is no `-p` exception, so with a real login `-p` loads them. The list call carries the OAuth bearer and `anthropic-beta: mcp-servers-2025-12-04`. Calls to mcp-proxy carry the bearer and `X-Mcp-Client-Session-Id`.
- Claude Code's own side calls (`/v1/mcp_servers`, `domain_info`, refresh) honour `HTTPS_PROXY`, so squid sees and logs them.
- Side note: with a non-Anthropic `ANTHROPIC_BASE_URL`, Claude Code turns off ToolSearch unless `ENABLE_TOOL_SEARCH=true`. That matters once connectors bring 40+ tools.

## Host table

| Host, path | Used by | Bearer | Today in the sandbox | Fix |
|---|---|---|---|---|
| `api.anthropic.com /v1/messages` | inference, WebSearch, WebFetch summary | yes | ok via the proxy | none. Path-allowlist the proxy (Risks) |
| `api.anthropic.com HEAD /api/hello` | connectivity probe (S3) | no | via the proxy | allow in the proxy's path list |
| `api.anthropic.com /api/web/domain_info` | WebFetch preflight | no | squid 403, WebFetch fails | keep blocked. Set `skipWebFetchPreflight: true` |
| `api.anthropic.com /v1/mcp_servers` | connector list | yes + scope | off (env); if on, squid 403 | keep blocked. The daemon lists connectors on the host |
| `mcp-proxy.anthropic.com /v1/mcp/{id}` | connector calls | yes | off; never reached | route through the bearer-swap proxy by `--mcp-config`, ids allowlisted |
| `api.anthropic.com /api/oauth/profile`, bootstrap, fast-mode, event logging, MCP registry | profile, telemetry, flags (S3) | yes | none sent (stub + `DISABLE_NONESSENTIAL_TRAFFIC`) | keep blocked |
| `platform.claude.com /v1/oauth/token` | refresh after a 401 | refresh token | squid 403, creds kept | keep blocked |
| `claude.ai`, `downloads.claude.ai` | installer, updates, connectors page | no | none sent (`DISABLE_AUTOUPDATER=1`) | keep blocked |

## Is a MitM needed?

No. Every Anthropic call that needs a bearer either follows `ANTHROPIC_BASE_URL` already or can be replaced by daemon-side work plus config. That holds for the connector list, and the mcp-route run proved it for connector calls. The one unauthenticated call, the preflight, has an off switch. Since nothing calls for it, here is the cost of a MitM, kept short. squid would need the `squid-openssl` build, `ssl_bump peek` then `bump` on `mcp-proxy.anthropic.com` and `api.anthropic.com` only, `splice` for the rest, and a per-sandbox CA whose key stays out of the sandbox, with `NODE_EXTRA_CA_CERTS` and `SSL_CERT_FILE` set in the sandbox. squid cannot swap a bearer per request, so an ICAP service or a Go MitM would have to hold the real token. It would also need path filtering and certificates rotated about daily. Estimate: 500 lines or more plus image work. It adds a CA the agent's own tools also trust, and it breaks if Claude Code ever pins its certificates. The config route is about 200 lines.

Proposed work, smallest first:
1. Proxy path allowlist: `POST /v1/messages`, `POST /v1/messages/count_tokens`, `HEAD /api/hello`, 403 on anything else. About 30 lines and a test.
2. WebFetch: add `"skipWebFetchPreflight": true` to the `settings.json` that `writeStubs` writes. One line and a test. I verified the `--settings` flag form. The settings.json form is the same key in the user settings source, which I did not run.
3. Connectors, if the product wants them (a product decision): the daemon fetches `/v1/mcp_servers` with the real token, the host picks which connectors a room may use, and the sandbox gets `--mcp-config` entries `{type: http, url: http://proxy:8787/v1/mcp/<id>, headers: {Authorization: Bearer <dummy>, X-Mcp-Client-Session-Id: <uuid>}}`. The proxy gains a route to `mcp-proxy.anthropic.com`, limited to the approved ids. Keep `ENABLE_CLAUDEAI_MCP_SERVERS=false`. About 150 to 250 lines plus the UI.

## Risks to the user's credentials

- **Today, found here:** the proxy has no path filter, and the dummy bearer sits in a file the agent reads. Any code in the sandbox can call any `api.anthropic.com` endpoint with the real token. The probe got 200 on `/v1/mcp_servers`. The same goes for `/api/oauth/*`, `/api/oauth/claude_cli/create_api_key` (not tried) and the usage endpoints. Fix 1 closes it. Fix 3 needs the same discipline on its new route: approved ids only, no other mcp-proxy path.
- Connector actions use the user's third-party logins (mail, calendar, drive). An agent with them can act as the user there. Approve per connector and per room, and use `--disallowedTools` for write tools unless the host opts in.
- Fix 2 sends nothing to Anthropic. Leaving the preflight on, with the host allowed, would send every fetched hostname to Anthropic.

**Refresh-wipe check.** The wipe needs Claude Code to reach the token endpoint and get an HTTP answer (S3 run a: 400, creds emptied). In refresh-401 the refresh went to squid and got a refused CONNECT. Claude Code logged it as a network failure and kept the creds. The proposed fixes keep that true:
- Fix 1 adds no route.
- Fix 2 removes a call.
- Fix 3 sends connector traffic to the proxy, never to `platform.claude.com`. A connector 401 (`mcp_invalid_oauth_token`) reaches the generic `http` MCP transport. From the static read, the claude.ai token refresh hangs off the main client and the `claudeai-proxy` type, which stays disabled. I did not run a connector 401 in the sandbox. Either way, a refresh attempt would still die at squid as in refresh-401. On the proxy side, a 401 on the mcp route must not count as the account's 401: today `ModifyResponse` would call `Invalidate` and bump `Unauthorized`.
- Guard: `Isolated()` already asserts squid answers 403 to `api.anthropic.com`. Add `platform.claude.com` to the same assertion and to the proxy's path list test.

In all 6 runs the sandbox `.credentials.json` came out intact, and the captures hold 0 copies of the real token (checked by byte count).

## Reproduce

```
go build -tags spike -o .scratchpad/s16/s16 ./spikes/s16-egress
.scratchpad/s16/s16 -out .scratchpad/s16 tools-prod          # 1 haiku turn
.scratchpad/s16/s16 -out .scratchpad/s16 tools-skip-preflight # 1 haiku turn
.scratchpad/s16/s16 -out .scratchpad/s16 mcp                 # no model call
.scratchpad/s16/s16 -out .scratchpad/s16 refresh-401         # 401, no usage
.scratchpad/s16/s16 -out .scratchpad/s16 connectors          # host-side list, read only
S16_CONNECTOR='Google Calendar' .scratchpad/s16/s16 -out .scratchpad/s16 mcp-route  # 1 haiku turn
```

## Resources (all removed)

Each run creates one sandbox, `grill-room-s16-*` (container, relay, egress, two networks, two volumes), labelled `grill-room-spike=s16`. Each run removes it on exit, plus anything else carrying the label. Afterwards `docker ps -a`, `docker network ls` and `docker volume ls` filtered on that label all return nothing. The proxy runs inside the harness process and stops with it. The host's `~/.claude` was only read: the credentials file's mtime predates the runs.
