# S8: setup token through the bearer-swap proxy

**Verdict: go, pending the day-long recheck.** A `claude setup-token` token works as the proxy's token source with no proxy change. Claude Code 2.1.296 in the sandbox sends its own OAuth headers, the proxy swaps the bearer, and the only proxied call is `POST /v1/messages` (200). The token carries only the `user:inference` scope and no expiry, so AC12's status and expiry must come from elsewhere (see 3).

Started 2026-10-11T21:33:32Z. Durability check due after 2026-10-12T21:33Z: `spikes/s8-setup-token/recheck.sh` (one haiku turn, appends a line to `.scratchpad/s8/recheck.log`, exit 0 if the turn answered).

Scripts: `s8-setup-token/direct.sh` (test 1), `s8-setup-token/recheck.sh` (test 2: the S3 proxy built from `s3/proxy`, internal network, socat relay aliased `proxy`, the daemon's dummy credentials and `oauthAccount` stub, read-only container; no squid, which only carries tool traffic). The token reaches containers and the proxy through 0600 files deleted on exit, never argv. The token file is kept for tests.

## Runs (haiku, `say ok`)

| Run | Setup | Result | Calls seen |
|---|---|---|---|
| 1 | egress, `CLAUDE_CODE_OAUTH_TOKEN`, no config | ok | bootstrap 403 ("no profile scope"), policy limits, telemetry 200. Claude Code skips claude.ai MCP and Chrome itself ("setup-token sessions default to user:inference only") |
| 2 | proxy, dummy creds expire in 10 y | ok | proxy: `POST /v1/messages` 200 only. No refresh, no 401 |
| 3 | as 2, dummy creds expired 1 h ago | ok | 2 refresh attempts to `platform.claude.com`, both `EAI_AGAIN`, then the turn goes out with the dummy: 200 |

Proxied request headers match S3: `authorization`, `anthropic-beta: oauth-2025-04-20,...`, `anthropic-version`, `x-app`, `x-claude-code-session-id`. The proxy forges nothing.

## Answers

1. Direct use works (run 1).
2. Through the proxy: yes, Claude Code adds the OAuth headers itself. Endpoints: `/v1/messages` only. No 401. With no refresh token on the host side, nothing changes for the proxy: it never refreshes. In the container, a dummy expiry in the past triggers a refresh that dies at DNS and the turn still succeeds (run 3). The daemon's 10-year dummy expiry avoids even that.
3. Expiry is not learnable. The token is opaque (no JWT dots) and no response header carries a date. The docs give a one-year lifetime, so the daemon can only estimate expiry as "added + 365 d". Non-inference probes with the token:

| Probe | Status | Use |
|---|---|---|
| `GET /api/oauth/profile` | 403 `oauth_scope_insufficient` (needs `user:profile`) | none: no email or plan for this account kind |
| `GET /api/oauth/usage` | 429 `retry-after: 3600` | inconclusive |
| `GET /v1/models?limit=1` | 200 | validity check |
| `POST /v1/messages/count_tokens` | 200; a bogus token gets 401 "OAuth access token is invalid." | validity check, free |

   For AC12, `count_tokens` (with `anthropic-beta: oauth-2025-04-20`) gives the status without spending inference. "Last use" comes from the proxy's own log.
4. Durability: see the recheck above. Not yet observed.
5. Community 401 reports: anthropics/claude-code#65320 was a token truncated by terminal wrapping (79 of 108 chars, dup of #54738). Ours is 108 chars and works, so that one does not apply. #58926 was a Windows 2.1.141 regression where every command failed, closed as stale. Not this version or OS. One blog post reports intermittent 401s on long-running headless setups with a static token. That is the case the recheck covers.

## Daemon requirements found

- A second token source for setup-token accounts: a raw token from the keyring or a 0600 file, `ExpiresAt` zero (`creds.Token.Expired` already treats zero as never expired). Keep the 401 counter as the "token refused" signal.
- Keep the `oauthAccount` stub and the env from S3. The container's dummy credentials claim `user:profile`, so without the stub it would try the profile fetch.
- Status per account: `count_tokens` probe plus "added + 365 d" as estimated expiry, shown as an estimate.

## Usage and resources

4 inference requests (runs 1-3, then run 2 again at 21:36Z after review fixes: ok), 5 non-inference probes. Containers `grs-s8*` and network `grs-s8` are removed by the scripts. Kept: `.scratchpad/s8/` (logs, redacted debug logs, proxy binary).
