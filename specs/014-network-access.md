# 014 — Agents' network access

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-16, open questions settled the same day (notes of 2026-10-16)
**Context**: notes of 2026-10-15; spike S16 (Anthropic egress, branch `spike/anthropic-egress`); builds on 002 (sandbox, egress allowlist, 002-AC34 to AC36), 003b (the allowlist table with hits, 003b-AC16), 004 (room admins, audit log, 004-AC6), 009 (AI host role, MCP domains and the allowlist assist, 009-AC25, 009-AC33) and the proxy path allowlist (PR #17)

## Problem

An agent reaches the web only through the per-server allowlist on the host's daemon page. When it hits a domain off the list, the turn fails quietly. The refusal shows up only in the daemon page's "Blocked websites" table, which the people in the room never see. The host has to notice it there, guess which room wanted it, and add the domain for every room on that server.

WebFetch fails on every domain, allowed ones included: Claude Code checks each domain with Anthropic first, and the sandbox blocks that check (S16). Hosts who trust their agents have no way to open the web wholesale. The claude.ai connectors of the host's account (calendar, mail, drive) cannot run in the sandbox at all.

## Solution

When an agent is refused a website, the room's chat says so: "Grill tried to reach example.com (blocked)". The AI host answers on that line with "Allow for this room" or "Allow everywhere". The host can let the room's admins allow websites for that one room. Each approval leaves a chat line and an audit entry, and the daemon page's allowlist table shows where every entry applies and removes it in one click.

A host who wants it can turn on "Allow every website" for a server, behind a red warning. A second switch under it also opens the local network and raw IP addresses, behind a stronger warning. Anthropic's hosts and the cloud metadata range stay blocked and every request is still counted. WebFetch works on allowed domains. The host can give a room some of the account's claude.ai connectors. They run through the same bearer-swap proxy as inference, never by intercepting TLS.

## Scope

- "Allow every website" per server: off by default, red warning, Anthropic hosts still blocked, every request logged and counted.
- "Also allow the local network and IP addresses", nested under it: off by default, a stronger red warning naming the LAN risk, the link-local range (cloud metadata) still blocked.
- Blocked requests in the room chat, approval by the AI host per room or per server, delegation of per-room approval to the room's admins, audit, revocation from the daemon page.
- The allowlist becomes per server plus per room.
- WebFetch: `skipWebFetchPreflight` in the sandbox settings.
- Anthropic documentation hosts (content only, no auth) on the default allowlist of new servers, and a one-click add for servers with a saved list.
- A check of Claude Code's remaining calls to Anthropic with the current settings (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` and similar).
- claude.ai connectors per room through the proxy, ids allowlisted, no TLS interception.

## Out of scope

- TLS interception (MitM) of any kind (S16: not needed, and it would add a CA the agent's tools trust).
- Plain HTTP or ports other than 443, even with "Allow every website".
- Raw IP addresses as allowlist entries. As destinations they are reachable only under AC26.
- Approving a website from `/admin`, from an API token or from the MCP server (012-AC21 holds).
- A room-side list of the room's websites. The daemon page table is where entries are listed and removed.
- Blocking or approving per persona: an entry applies to every persona of the room.
- Connector logins: a connector whose claude.ai login has lapsed is fixed on claude.ai, not here.
- Writes through a connector by default (see AC23).
- Exporting per-room entries or connector choices: they belong to the host, not the room.

## Acceptance criteria

### Allow every website

- [x] **AC1** "Per server" on the daemon page has an "Allow every website" switch, off by default, under the allowlist table. Turning it on asks for confirmation in a sheet with a red warning: "Agents in every room of this server can reach any website over HTTPS. Anything they read can steer them, and anything they hold can leave." Turning it off needs no confirmation. Either way the running sandboxes take it at once and the setting survives a restart.
- [x] **AC2** With the switch on, the egress proxy lets through a CONNECT to port 443 of any domain name. It still refuses, in this order: Anthropic's hosts, anything but CONNECT to 443, an IP literal, and a name that resolves to a loopback, private, link-local or unique-local address. A domain on the server's or the room's list keeps working as it does with the switch off, whatever it resolves to. With the switch off, the generated configuration is byte-identical to today's for the same list, apart from AC6's Anthropic lines.
- [x] **AC26** Under "Allow every website", a second switch, "Also allow the local network and IP addresses", is off by default and disabled while the first is off. Turning it on asks for confirmation in a sheet with a stronger red warning: "Agents can also reach your own network: the router, a NAS, admin pages, other machines and this computer, by name or by IP address. A page they read can send them there." With it on, the egress proxy also lets through a CONNECT to port 443 of an IP literal and of a name resolving to a loopback, private or unique-local address. It still refuses Anthropic's hosts, anything but CONNECT to 443, and the link-local ranges `169.254.0.0/16` (cloud metadata) and `fe80::/10`, held in one constant. Turning the first switch off turns this one off too. It survives a restart.
- [x] **AC3** Every request the egress proxy sees during a turn, let through or refused, is written once to the daemon's log as an `egress` line with server, room, persona, host and verdict. Hits are counted per host and per room from each running sandbox's recent proxy log, as 003b's are: they reset when the sandbox goes. With the switch on, the allowlist table adds the hosts reached that no entry covers, marked "reached via Allow every website", with their hits.

### WebFetch and Anthropic hosts

- [x] **AC4** Every turn's `settings.json` carries `"skipWebFetchPreflight": true` next to the plugins' `pluginConfigs` (009-AC23).
- [x] **AC5** In the sandbox, a real WebFetch of a page on an allowed domain returns its content. A WebFetch of a domain off the list fails with the proxy's refusal. Neither produces a request to Anthropic's API host in the egress log.
- [x] **AC6** The Anthropic documentation hosts that serve content with no bearer and no account data are `code.claude.com` (Claude Code docs) and `support.claude.com` (help center); Phase 1 found them in the pinned binary and fetched each without credentials. `docs.claude.com` and `docs.anthropic.com` only redirect to `platform.claude.com`, the OAuth token host, and stay refused. The Anthropic deny gains `.claude.com`, which the binary uses for the token host, the docs and the `*.mcp.claude.com` connectors. The two docs hosts go, by exact name with no leading dot, on the default allowlist of a server with no saved list (AC27) and on an exception list that `NormalizeAllowlist` and the egress configuration accept ahead of the Anthropic deny. Every other Anthropic host stays refused by both: the API host, the OAuth token host, the console and the connector relay. `Isolated()` asserts a 403 for the API host and the token host.
- [x] **AC27** Docs hosts reach servers with a saved list only by the host's click: when the server's list lacks one of AC6's docs hosts, the allowlist table shows "Add Anthropic docs sites", which adds the missing ones in one step. A server with no saved list gets them in its default. Superseded (user, 2026-10-16): the button is gone; the docs hosts are in the default list of a new server only, and the host adds them by hand elsewhere.
- [x] **AC7** With the sandbox's current environment (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `ENABLE_CLAUDEAI_MCP_SERVERS=false`, the auto-updater off) plus AC4, one turn that runs WebSearch and WebFetch leaves no CONNECT to an Anthropic host in the egress log. Any call that remains is either turned off by a setting or recorded in Decisions with why it stays.

### Blocked requests in the chat

- [x] **AC8** When a turn ends, each domain its sandbox was refused gives the room one chat line with its time: "<Persona> tried to reach example.com (blocked)". Only CONNECT to 443 of a domain name counts. Anthropic hosts, IP literals and other methods stay on the daemon page only. One line per domain per turn. While a line for the same domain in the same room is still undecided, a new refusal adds to its count ("3 times") instead of posting again.
- [x] **AC9** The AI host sees "Allow for this room" and "Allow everywhere" on an undecided line. A room admin sees "Allow for this room" only when delegation is on (AC12). Everyone else sees the line without buttons. With the host's app offline, or older than this feature, the buttons are disabled with "AI host app offline" or "Update the AI host app".
- [x] **AC28** (user, 2026-10-16) A website still waiting for the AI host also sits in the banner under the room's header, one entry per domain with its count, for the AI host ("Allow for this room", "Allow everywhere", "Dismiss") and for a room admin the host lets ("Allow for this room", "Dismiss"). It stays until it is allowed or dismissed, across a reload and a host change; a dismissed one comes back at its next refusal. The chat line stays as the record, and anyone who cannot allow it reads "Pending: only the AI host, <name>, can allow it."
- [x] **AC29** (user, 2026-10-16) A CONNECT to a public name off the list, during a turn, waits while the room decides. The egress proxy asks the daemon through a socket only that sandbox's egress proxy mounts (squid `external_acl_type`, concurrent), after every rule that never asks: Anthropic hosts, anything but CONNECT to 443, link-local, IP literals, private destinations. The website gets its chat line and banner at once. Allowed in time, the same CONNECT goes on in the same turn. Dismissed, unanswered for the hold time (45 s by default, 30 to 60 on the daemon page), or with neither the AI host nor a room admin the host lets in the room, which is refused at once, it is refused as before and the banner stays. One question per website per room at a time, which later requests join; other traffic keeps flowing. Requests outside a turn are refused at once. Only on Linux with Docker Engine: Docker Desktop's VM cannot mount the host socket, and those hosts keep the refusal at the turn's end.
- [x] **AC10** "Allow for this room" adds the domain to the room's own list. The room's sandboxes reach it from their next request; other rooms on the same server still get refused. The line becomes "<who> allowed example.com for this room".
- [x] **AC11** "Allow everywhere" adds the domain to the server's list (the existing table), for every room of that server. The line becomes "<who> allowed example.com for every room on this AI host".
- [x] **AC12** Room settings has a switch, shown to the AI host only: "Room admins may allow websites for this room", off by default. Changing it posts a chat line and writes an audit entry. It never lets a room admin use "Allow everywhere". It turns off when the AI host role moves to someone else (009-AC33).
- [x] **AC13** The server checks every approval: a plain member gets 403; a room admin gets 403 without delegation, 403 on "Allow everywhere", and 403 in a room they do not administer. An approval of an Anthropic host, an IP or an invalid name gets 400 with the allowlist's message.
- [x] **AC14** Each approval writes an audit entry (004-AC6) in the same transaction as its chat line: action `egress.allow`, actor, room, details with the domain, the scope (`room` or `server`) and the persona that was refused.
- [x] **AC15** The daemon page's allowlist table (003b-AC16) gains a scope column: "Every room" or the room's name. An entry added from the chat shows who added it and when in its tooltip. The remove button revokes the entry at once on the running sandboxes. When the room server is connected, the revocation posts "<host> removed example.com from this room's websites" (or "from every room") and writes `egress.revoke`.
- [x] **AC16** A room's list follows the server list's rules: no wildcard, no IP, no Anthropic host, at most 100 entries. It survives a daemon restart. Removing the room from the daemon page (003b-AC6) deletes it.
- [x] **AC17** The plugin allowlist assist (009-AC25) treats a domain as reachable when the server's list, the room's list or "Allow every website" covers it.

### claude.ai connectors

- [x] **AC18** The daemon page shows, per room, the connectors of the room's AI account as the host's daemon lists them with the account's token: name and state ("ready", or "needs login on claude.ai"). No connector is on by default. The host turns each one on or off per room. The list call never runs inside a sandbox.
- [x] **AC19** Room members see which connectors are on for their room, by name, in the room's settings. Turning one on or off posts a chat line and writes `connector.enable` or `connector.disable` to the audit log.
- [x] **AC20** A turn in a room with connectors on gets one `--mcp-config` entry of type `http` per connector. Each entry points at the room's account proxy, with a bearer scoped to that room and different from the inference bearer. `ENABLE_CLAUDEAI_MCP_SERVERS` stays `false`. `ENABLE_TOOL_SEARCH=true` is set once a connector is on. A turn in a room with none gets no such flag and no such entry.
- [x] **AC21** The proxy forwards `POST /v1/mcp/<id>` to the connector relay, with the real token swapped in and the session id header kept, only when `<id>` is on for the room the bearer belongs to. Any other id, method or path under that route gets 403 and a `proxy_path_denied` line, with the same exact escaped-path match as PR #17. A connector on for room A gets 403 from room B's sandbox.
- [x] **AC22** A 401 on the connector route does not invalidate the account's token and does not count in `Unauthorized`. The agent sees the connector fail, and the daemon page marks it "needs login on claude.ai".
- [x] **AC23** By default a connector's tools without `readOnlyHint: true` go into `--disallowedTools`. The host can tick "Allow actions" per connector and per room; that tick posts its own chat line and audit entry.
- [x] **AC24** Turning a connector off takes effect on the room's next request to it, even mid-turn: 403.
- [x] **AC25** After a turn that used a connector, `Scan` finds no copy of the real token in the sandbox, its volumes or its inspect output, and the sandbox's `.credentials.json` is unchanged.

## Phases

### Phase 1 — Egress policy, WebFetch, Anthropic hosts
- Work: "Allow every website" and its local-network extension (settings, page switches with their warning sheets, squid configuration with the private-range, IP-literal and link-local denies); per-host and per-room counters and the `egress` log line; `skipWebFetchPreflight` in `writeStubs`; the docs hosts' exception list, the `.claude.com` deny and the one-click add; check the remaining Anthropic calls; the OAuth token host added to `Isolated()`.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC26, AC27
- **Data model impact**: the daemon's per-server allowlist file becomes an object (`all_websites`, `local_network`, `server` entries with `domain`, `by`, `at`, and `rooms`, filled in Phase 2). The legacy array still loads, as the server list.
- **DoD**:
  - Go table tests on the squid configuration generation: switch off gives today's text byte for byte, AC6's lines aside; switch on denies an Anthropic host, a plain-HTTP request, port 22, an IP literal and a name resolving to each private range, and allows a public name; the local-network level allows an IP literal and a private name and still denies the link-local constant's ranges and Anthropic; a docs host from AC6 is allowed while the API host, the token host and the connector relay stay denied; `squid -k parse` accepts each generated file in the egress image;
  - `NormalizeAllowlist` tests: the docs hosts accepted, every other Anthropic host refused, as today;
  - legacy array file loads unchanged; the object form round-trips;
  - docker test in the sandbox image with a real login, 1 haiku turn: WebFetch of a page on the default list returns its text, WebFetch of `example.com` fails with the proxy refusal, and the egress log holds no CONNECT to an Anthropic host (AC5, AC7);
  - `Isolated()` green with the token host added;
  - `make lint test` green.

### Phase 2 — Blocked requests in the chat, per-room allow, delegation
- Work: per-turn read of the sandbox's egress log since the turn's start; the blocked line (new message kind with its state and count); the approve routes and their checks; delegation switch in room settings; per-room lists on the daemon, merged with the server list into each room's sandboxes; supervisor-worker protocol version 6 and the room stream messages (additive); scope column, tooltip and revocation in the daemon table; the assist change; fake claude `FAKE_FETCH=<domain>`, which makes a CONNECT through the egress proxy.
- **Covers**: AC8, AC9, AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC17
- **Data model impact**: room server: `rooms.egress_admins_allow` (0 or 1, default 0); a blocked line is a `system` message with a `blocked_requests` row holding persona, domain, count, decision and the approval waiting for the host's app; audit actions `egress.allow`, `egress.revoke`, `egress.delegate`. Daemon: the `rooms` part of the allowlist file.
- **DoD**:
  - Go tests: one room's merged list reaches its sandboxes and no other room's; "everywhere" reaches every room of the server; removal applies at once; 101st room entry refused; a removed room's entries gone; restart keeps them;
  - Go table test of the approve route over actor × scope × delegation × room: host room 200, host everywhere 200, delegated admin room 200, delegated admin everywhere 403, undelegated admin 403, admin of another room 403, member 403; delegation off after a host transfer; Anthropic host, IP and `*.x` give 400;
  - Go test of the blocked-line rules: one line per domain per turn, count bumped while undecided, Anthropic host and IP never posted;
  - audit entries present with the listed details, in the same transaction as the chat line;
  - Playwright e2e with fake claude: `FAKE_FETCH=example.com` posts the blocked line; the host clicks "Allow for this room", the line updates and the next turn's fetch goes through; a second domain is blocked, the host turns delegation on, a room admin's browser shows only "Allow for this room" and approves it; a plain member's browser shows no button; the daemon page lists both entries with their scope and removes one;
  - `make lint test` green, the changed e2e specs green.

### Phase 3 — claude.ai connectors through the proxy
- Work: host-side connector listing per AI account; per-room choice and "Allow actions" on the daemon page; room settings display; per-room connector bearers on the proxy and the `/v1/mcp/<id>` route; 401 handling on that route; `--mcp-config`, `--disallowedTools` and `ENABLE_TOOL_SEARCH` in the turn.
- **Covers**: AC18, AC19, AC20, AC21, AC22, AC23, AC24, AC25
- **Data model impact**: daemon: the server's connectors by room (connector id, name, actions allowed), in its `instance.json`. Room server: audit actions `connector.enable`, `connector.disable`, `connector.actions`; the room's connector names come with the host's status, not stored.
- **DoD**:
  - proxy tests against a fake relay: enabled id forwarded with the real token and the session header; unknown id, other room's id, `GET`, a sub-path, a dot segment and an encoded slash all 403 and counted in `Denied`; a relay 401 leaves `Unauthorized` at 0 and calls no `Invalidate`; disabling an id refuses the next request;
  - turn tests: the `--mcp-config` JSON for two connectors, write tools in `--disallowedTools`, none of it for a room with no connector;
  - live check on the lab with a real login and one connector on, at most 4 haiku turns in total: the model lists the connector's read tools; `Scan` finds 0 copies of the real token and `.credentials.json` is unchanged; the egress log has no Anthropic CONNECT;
  - `make lint test` green.

## Data model impact (summary)

- Daemon, per server: the allowlist file becomes an object with the two switches, server entries and room entries (Phases 1 and 2); the connectors by room in its `instance.json` (Phase 3). The legacy array still loads.
- Room server: `rooms.egress_admins_allow`; blocked lines, `system` messages with a `blocked_requests` row; audit actions `egress.allow`, `egress.revoke`, `egress.delegate`, `connector.enable`, `connector.disable`, `connector.actions`.
- Supervisor-worker protocol version 6 and new room stream messages, additive.
- No export change: per-room entries and connectors stay on the host; `egress_admins_allow` is not exported.

## Measured for AC29 (2026-10-16, Claude Code 2.1.296)

| Tool | Under a held CONNECT |
|---|---|
| WebFetch | a 45 s hold, then OK: the page fetched, the turn took 50 s in all (1 haiku turn). Its domain preflight has a 10 s timeout, but `skipWebFetchPreflight` turns it off |
| curl | waits: a 60 s hold, then 200 (no default connect timeout) |
| pip | a 15 s read timeout with retries: a 60 s hold passed after 121 s |

60 s with WebFetch was not measured, within the haiku budget: the default stays at the measured 45 s.

## Open questions

None open. The draft's three were answered on 2026-10-16 (see Decisions).

## Decisions

From the notes:

- "Allow every website" per server, off by default, red warning; Anthropic hosts stay blocked; every request logged with counters (user).
- Blocked requests in the chat; the host approves for the room or everywhere; the host may delegate approval to the room's admins, for that room only; each approval is a chat line plus an audit entry, revocable from the table (user).
- No MitM. WebFetch through `skipWebFetchPreflight`; connectors through `--mcp-config` and the proxy, approved ids only (S16).
- Connectors: none by default. The host turns them on per room on the daemon page, and room members see which ones are on (user, 2026-10-16).
- Connector write tools are off by default, with an "Allow actions" tick per connector and per room (user, 2026-10-16).
- Anthropic docs hosts go on the default list of new servers only. A server with a saved list gets them by the host's one click (user, 2026-10-16).
- Each room gets its own connector bearer, so the proxy can tell rooms of one server apart; the inference bearer stays per worker (user, 2026-10-16).
- A second option under "Allow every website" opens private and local addresses and raw IPs: off by default, with a stronger red warning that names the LAN risk (user, 2026-10-16).
- `169.254.0.0/16` (cloud metadata) stays blocked even with the local-network option on (user, 2026-10-16). It lives in one constant with `fe80::/10`, its IPv6 twin, and a table test holds it.

Made while drafting (overturnable):

- Blocked requests are read when the turn ends, not live: an agent does not wait for an approval mid-turn, and the next turn retries.
- "Allow every website" keeps HTTPS on 443 only and adds denies for IP literals and private, loopback and link-local destinations. Otherwise it would open the host's own network to the agent. Entries on the lists come before those denies, so a host who listed an internal name keeps reaching it.
- The Anthropic deny covers `.claude.com` too: the token host, the platform docs and the hosted connectors live there. The binary already names hosts under it that today's list missed.
- "Allow everywhere" from the chat stays within the server whose room it came from. A room server can only widen its own rooms' access.
- Delegation lives on the room server, which knows the room's admins, and resets on a host transfer: the new host never inherits a choice they did not make.
- A blocked line is a `system` message with a `blocked_requests` row, not a new message kind: the kinds are a CHECK on `messages`, which SQLite changes only by rebuilding the table.
- An approval goes to the host's app first, which applies it and confirms; only then does the line say who allowed it, with its audit entry. A full room list or a lost connection leaves the line undecided, with why, rather than claiming an approval that did not apply.
- A host change also drops the approvals the previous host's app had not applied: they would land on the new host's computer. The line says why and takes a new approval.
- Removing from the server list on the daemon page tells the rooms only of entries added from a room's chat: the host's own edits of the default list are not room events.
- A revocation from the daemon page applies at once. Its chat line and audit entry are written only if the room server is connected then; the daemon log always has it.
- Hits come from each sandbox's recent proxy log, as 003b's do, and reset with the sandbox; the daemon log line is the durable record.
- A connector whose tools could not be read is left out of a turn unless its actions are allowed: its write tools could not be disallowed.
- The supervisor keeps each room's connectors in the server's `instance.json`: it holds the accounts and the proxies, and the worker never sees a token.
- Connector tools without `readOnlyHint: true` are disallowed by default (S16 risk: an agent with a connector acts as the user there).
- `ENABLE_TOOL_SEARCH=true` once a connector is on: with a non-Anthropic `ANTHROPIC_BASE_URL`, Claude Code turns ToolSearch off, and connectors bring dozens of tools (S16).
