# 012 — Agent-configurable: personal API tokens and a built-in MCP server

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-14, reconciled 2026-10-16 with what shipped since (v0.0.28)
**Context**: notes Q1 to Q3 (2026-10-14); builds on 002 (profiles), 004 and 004b (`/admin`, its lists, audit log, room admins), 006 (starters), 009 (plugins, persona sheet, host approvals), 011 (profile merge, purge, the shareable invite link) and 011b (the `/profile` tabs)

## Problem

Everything a profile can do in grill-room goes through a browser. The REST API exists, but its credentials are a room cookie, a profile cookie, the instance's admin token and the daemon's token. None of them fits a script or an AI agent: the cookies live in one browser, the admin token opens the whole instance, and the daemon token belongs to the AI host app.

Someone who wants Claude Code to set up a room for them (create it from a starter, add personas, upload the documents) has to click through the room settings by hand.

## Solution

`/profile` gets an "API tokens" tab. There you create a personal token with a name, an expiry and a scope: read only, or full (whatever your profile can do). The token shows once. `curl` and agents then call the existing API with `Authorization: Bearer grt_…` and get your rights in each of your rooms, room admin rights included where your profile has them. A token never opens `/admin`, never makes another lasting credential, and you revoke it from its row's menu in one click.

The room server also speaks MCP at `/mcp` with the same token. Its tools read your rooms and configure them: create a room from a starter, add and tune personas, add skills and plugins, upload documents, get your invite link. Deleting something needs a `confirm` argument equal to the target's name. The new-token sheet shows a `claude mcp add` command ready to paste.

## Scope

- Personal API tokens: store, the `/profile` › API tokens tab, read and full scopes, expiry, revoke, last use, audit, a cap of 100 per profile, purge of expired tokens, revocation on profile merge.
- Token auth on the existing REST API with the profile's rights, never on admin routes.
- Two read routes the MCP tools need and the UI does not: messages as a page, a revision's Markdown text.
- An MCP server at `/mcp` (Streamable HTTP, JSON responses, stateless) with the tools of Q2.
- The ready-made `claude mcp add` command in the new-token sheet.

## Out of scope

- OAuth 2.1 and the claude.ai connector (Q3, backlog).
- A persona inside a room changing the room's configuration (notes: "not B").
- Tools that speak or run the AI: no `post_message`, no `run_persona`, no turn start or stop (Q2).
- Approving anything through a token: host approvals of persona changes, room skills, plugin versions and the host cap stay in the room UI.
- Tokens limited to one room, or per-tool scopes beyond read and full.
- MCP resources, prompts, sampling, server notifications and SSE responses.
- An `/admin` list of profiles' tokens. Deleting a profile deletes them.
- A rate limit of its own. Tokens carry 130 random bits; the existing limits apply.

## Acceptance criteria

### Tokens (Q1)

- [ ] **AC1** `/profile` has an "API tokens" tab (`#tokens`) after Skills, with a count badge. "New token" opens a sheet asking for a name (1 to 60 characters, one line), an expiry (30 days, 90 days by default, 1 year, never) and a scope ("Read only" or "Full: what your profile can do"). The server answers once with the token, `grt_` followed by 26 base32 characters. The sheet then shows it as a kit `.value` with its Copy button and "You won't see this token again". The database keeps its SHA-256 hash only: after a create, the plaintext appears in no table.
- [ ] **AC2** The tab lists the tokens as kit list rows (`.cols` with a head row on a wide screen, cards on a phone): name, scope, created date, expiry ("expires in 12 days", "never expires", "expired 3 days ago") and last use ("never used", "used 3 months ago", with the client IP in its tip). Only the owner's browsers see it. No route returns a token or its hash after the create.
- [ ] **AC3** `Authorization: Bearer grt_…` on a room route acts as the profile's participant in that room, the newest one, exactly as the profile cookie does, with that participant's rights: member, room admin, AI host. On `/api/profile/*` routes it acts as the profile. A room where the profile has no participant gets 403 "Your profile is not in this room". A bearer token turns cookies off for the request, as today.
- [ ] **AC4** A read-only token gets 403 "This token is read only" on any method other than GET and HEAD, before the handler runs, on every route.
- [ ] **AC5** A token never reaches an admin route. Every `/api/admin/*` route and every route behind the admin check (`GET` and `POST /api/rooms`, `/api/profiles*`, `/api/skills*`, `/api/templates*`) answers 403 "API tokens never reach admin routes", whatever the scope. Routes open to the admin or a room admin treat the token as the profile's participant. The server refuses to start when `GRILL_ROOM_ADMIN_TOKEN` starts with `grt_`.
- [ ] **AC6** A token never makes another lasting credential. It gets 403 "API tokens cannot do this; use the browser" on the token routes, `POST /api/join`, the email bind, sign-in and magic-link routes, profile recovery, the daemon pairing routes and Web Push subscriptions. These stay cookie only. Invites are the one exception (Q2): an invite made with a token, the shareable link (011-AC26) included, outlives the token, and the new-token sheet says so.
- [ ] **AC7** An expired token gets 401 "This token has expired". The list keeps it, marked expired, for 30 days after its expiry; the daily purge deletes it then.
- [ ] **AC8** "Revoke" in a row's ⋯ menu revokes the token at once, with no confirm sheet (Q1, one click), and a "Revoked" toast. The next request with it gets 401: no cache sits between the token and its row.
- [ ] **AC9** Each use records the time and the client IP (the `GRILL_ROOM_TRUSTED_PROXIES` rule), written at most once a minute per token. The IP is the owner's to see. The purge clears it after `GRILL_ROOM_CLIENT_RETENTION` and keeps the time. README's Personal data table gains the row.
- [ ] **AC10** Creating and revoking a token write audit entries in the same transaction (004-AC6): actor `profile:<uuid>`, action `api_token.create` or `api_token.revoke`, target the profile, details with the token's id, name, scope and expiry. The `/admin` audit view names both actions. An action that is audited today and done with a token carries `api_token: <id>` in its details. No audit entry, log line or error body holds a token or its hash; logs name the token id.
- [ ] **AC11** A profile holds at most 100 unexpired tokens. The 101st gets 409 "You have 100 API tokens. Revoke one first." Expired tokens do not count.
- [ ] **AC12** Deleting a profile deletes its tokens. A merge (011-AC11) deletes B's tokens: the merge preview counts them as `api_tokens`, the `/admin` merge sheet shows the count, and the merge's `moved` counts and audit entry carry `api_tokens_revoked`. An unexpired token makes a profile non-empty for the purge (011-AC15); a purged profile's expired tokens go with it.

### MCP server (Q2, Q3)

- [ ] **AC13** `POST /mcp` serves MCP over Streamable HTTP with JSON responses only, statelessly. It speaks the 2026-07-28 revision (`server/discover`, per-request `_meta`) and the 2025-11-25 handshake (`initialize`, `notifications/initialized`, `ping`) that clients still fall back to. GET and DELETE get 405. A request with an `Origin` that is not the server's own gets 403.
- [ ] **AC14** `/mcp` accepts only `Authorization: Bearer grt_…`. No token, an unknown, expired or revoked one, the admin token or a cookie alone get HTTP 401 with no OAuth metadata, so Claude Code reports a failed connection instead of starting an OAuth flow.
- [ ] **AC15** `tools/list` returns the catalogue below, sorted by name as the SDK lists tools. A read-only token lists the read tools only. Each `inputSchema` is JSON Schema 2020-12, `type: object`, `additionalProperties: false`, with its required fields, and each tool sets `readOnlyHint`, `destructiveHint` and `idempotentHint`.
- [ ] **AC16** A tool runs as the REST route it maps to, with the caller's token, so authorization is the route's own. A 2xx answer becomes `structuredContent` (the route's JSON) plus the same JSON as a text block. A 4xx becomes `isError: true` with the route's message ("only a room admin can do this"). A 5xx becomes `isError: true` with "Server error, try again", logged with the tool name. An unknown tool is JSON-RPC error -32602. Arguments that fail the schema give `isError: true` naming the field. A route's list answer comes under `items`, as `structuredContent` is an object.
- [ ] **AC17** The read tools (`list_rooms`, `get_room`, `list_documents`, `read_document`, `get_transcript`, `list_notes`, `list_invites`, `list_starters`, `get_starter`, `list_templates`, `list_skills`, `list_plugins`) work with a read-only token and change nothing.
- [ ] **AC18** The configure tools need a full token: `create_room` (blank, or from a starter with its fields, within the room creation limits), `add_persona` (from a template), `edit_persona` (name, badge, colour, text, model, effort, the three automatic triggers `every_n`, `every_voice` and `every_minutes`, "Can follow up on its own" `self_call`, "Watches the notes" `watches_notes`), `set_persona_skill`, `set_persona_plugin`, `add_room_skill`, `check_plugin`, `add_plugin`, `save_room_as_starter`, `upload_document` (text, or base64 bytes, under the upload and room limits) and `get_invite_link`. `get_invite_link` returns the caller's shareable invite link for the room (011-AC26) and makes it only on the first call, so repeated calls add no invite.
- [ ] **AC19** Host approval works as in the UI. Under "approve everything", a persona added or changed by someone who is not the AI host comes back `held` with the text "Waiting for the AI host's approval", a room skill comes back pending, and a plugin version waits for inspection and approval (009-AC13, 009-AC14). The AI host's own calls apply at once, as their clicks do. Plugin tools need a room admin, as in the room sheet (009-AC5, 009-AC10). No tool approves or refuses anything.
- [ ] **AC20** The destructive tools (`delete_room`, `delete_document`, `remove_persona`, `revoke_invite`) need `confirm` equal to the target's current name, byte for byte: the room's name, the document's name, the persona's name, and for an invite its id as `list_invites` gives it. A missing or different `confirm` gives `isError: true` with "confirm must be the exact name of the <kind>; read it with <tool>", without echoing the name, and changes nothing. A call that succeeds leaves exactly one audit entry with the token id.
- [ ] **AC21** No tool posts a message, starts, stops or queues a turn, or decides an approval: the tools' sub-requests reach only the routes listed in the catalogue.
- [ ] **AC22** The new-token sheet (AC1) also shows `claude mcp add --transport http grill-room <public URL>/mcp --header "Authorization: Bearer <token>"`, built from `GRILL_ROOM_URL`, as a second `.value` with its own Copy button. Pasted as is, it adds a server that `claude mcp list` reports as connected.

## MCP tool catalogue

Room ids are strings; other ids are integers, as the REST API gives them. Every schema below also carries `"$schema": "https://json-schema.org/draft/2020-12/schema"` and `"additionalProperties": false`, left out here to keep each line short.

| Tool | Scope | Hints (ro, destr, idem) | REST route |
|---|---|---|---|
| `list_rooms` | read | yes, no, yes | `GET /api/profile` (its `rooms`) |
| `get_room` | read | yes, no, yes | `GET /api/rooms/{room}` and `GET /api/rooms/{room}/agent` |
| `list_documents` | read | yes, no, yes | `GET /api/rooms/{room}/documents` |
| `read_document` | read | yes, no, yes | new `GET /api/rooms/{room}/documents/{id}/text?rev=` |
| `get_transcript` | read | yes, no, yes | new `GET /api/rooms/{room}/messages?since=&limit=` |
| `list_notes` | read | yes, no, yes | `GET /api/rooms/{room}/documents/{id}/annotations` |
| `list_invites` | read | yes, no, yes | `GET /api/rooms/{room}/invites` |
| `list_starters` | read | yes, no, yes | `GET /api/profile/starters` |
| `get_starter` | read | yes, no, yes | `GET /api/profile/starters/{starter}?v=` |
| `list_templates` | read | yes, no, yes | `GET /api/rooms/{room}/templates` |
| `list_skills` | read | yes, no, yes | `GET /api/rooms/{room}/skills`, or `GET /api/profile/skills` without a room |
| `list_plugins` | read | yes, no, yes | `GET /api/rooms/{room}/plugins` |
| `create_room` | full | no, no, no | `POST /api/profile/rooms` |
| `add_persona` | full | no, no, no | `POST /api/rooms/{room}/personas` |
| `edit_persona` | full | no, no, yes | `PATCH /api/rooms/{room}/personas/{id}` |
| `set_persona_skill` | full | no, no, yes | `PUT` or `DELETE /api/rooms/{room}/personas/{id}/skills/{skill}` |
| `set_persona_plugin` | full | no, no, yes | `PUT` or `DELETE /api/rooms/{room}/personas/{id}/plugins/{plugin}` |
| `add_room_skill` | full | no, no, no | `POST /api/rooms/{room}/skills` |
| `check_plugin` | full | yes, no, yes | `POST /api/rooms/{room}/plugins/check` (full: it makes the server fetch) |
| `add_plugin` | full | no, no, no | `POST /api/rooms/{room}/plugins` |
| `save_room_as_starter` | full | no, no, no | `GET /api/rooms/{room}/starter-draft` then `POST /api/rooms/{room}/starters` |
| `upload_document` | full | no, no, no | `POST /api/rooms/{room}/documents` (multipart built by the server) |
| `get_invite_link` | full | no, no, yes | `POST /api/rooms/{room}/invites/share` |
| `delete_room` | full | no, yes, no | `GET /api/rooms/{room}` then `DELETE /api/rooms/{room}` |
| `delete_document` | full | no, yes, no | `GET /api/rooms/{room}/documents` then `DELETE /api/rooms/{room}/documents/{id}` (soft delete, restorable) |
| `remove_persona` | full | no, yes, no | `GET /api/rooms/{room}/agent` then `DELETE /api/rooms/{room}/personas/{id}` |
| `revoke_invite` | full | no, yes, no | `DELETE /api/rooms/{room}/invites/{id}` |

```json
{"list_rooms":        {"type":"object"},
 "get_room":          {"type":"object","required":["room"],"properties":{"room":{"type":"string"}}},
 "list_documents":    {"type":"object","required":["room"],"properties":{"room":{"type":"string"}}},
 "read_document":     {"type":"object","required":["room","document"],"properties":{"room":{"type":"string"},"document":{"type":"integer"},"revision":{"type":"integer","minimum":1}}},
 "get_transcript":    {"type":"object","required":["room"],"properties":{"room":{"type":"string"},"since":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":500,"default":200}}},
 "list_notes":        {"type":"object","required":["room","document"],"properties":{"room":{"type":"string"},"document":{"type":"integer"}}},
 "list_invites":      {"type":"object","required":["room"],"properties":{"room":{"type":"string"}}},
 "list_starters":     {"type":"object"},
 "get_starter":       {"type":"object","required":["starter"],"properties":{"starter":{"type":"integer","minimum":0},"version":{"type":"integer","minimum":1}}},
 "list_templates":    {"type":"object","required":["room"],"properties":{"room":{"type":"string"}}},
 "list_skills":       {"type":"object","properties":{"room":{"type":"string"}}},
 "list_plugins":      {"type":"object","required":["room"],"properties":{"room":{"type":"string"}}},
 "create_room":       {"type":"object","required":["display_name"],"properties":{"name":{"type":"string"},"display_name":{"type":"string","minLength":1},"starter":{"type":"integer","minimum":1},"starter_version":{"type":"integer","minimum":1},"fields":{"type":"object","additionalProperties":{"type":"string"}}}},
 "add_persona":       {"type":"object","required":["room","template"],"properties":{"room":{"type":"string"},"template":{"type":"integer"}}},
 "edit_persona":      {"type":"object","required":["room","persona"],"properties":{"room":{"type":"string"},"persona":{"type":"integer"},"name":{"type":"string"},"badge":{"type":"string"},"colour":{"type":"string","pattern":"^#[0-9a-fA-F]{6}$"},"text":{"type":"string"},"model":{"enum":["haiku","sonnet","opus","fable"]},"effort":{"enum":["low","medium","high","xhigh","max"]},"every_n":{"type":"integer","minimum":0},"every_voice":{"type":"integer","minimum":0},"every_minutes":{"type":"integer","minimum":0},"self_call":{"type":"boolean"},"watches_notes":{"type":"boolean"}}},
 "set_persona_skill": {"type":"object","required":["room","persona","skill","on"],"properties":{"room":{"type":"string"},"persona":{"type":"integer"},"skill":{"type":"integer"},"on":{"type":"boolean"},"accept_disclaimer":{"type":"boolean"}}},
 "set_persona_plugin":{"type":"object","required":["room","persona","plugin","on"],"properties":{"room":{"type":"string"},"persona":{"type":"integer"},"plugin":{"type":"integer"},"on":{"type":"boolean"}}},
 "add_room_skill":    {"type":"object","required":["room","skill"],"properties":{"room":{"type":"string"},"skill":{"type":"integer"}}},
 "check_plugin":      {"type":"object","required":["room","url"],"properties":{"room":{"type":"string"},"url":{"type":"string","pattern":"^https://"},"ref":{"type":"string"}}},
 "add_plugin":        {"type":"object","required":["room","url"],"properties":{"room":{"type":"string"},"url":{"type":"string","pattern":"^https://"},"ref":{"type":"string"},"entries":{"type":"array","items":{"type":"string"}},"personas":{"type":"array","items":{"type":"integer"}}}},
 "save_room_as_starter":{"type":"object","required":["room","title"],"properties":{"room":{"type":"string"},"title":{"type":"string","minLength":1},"description":{"type":"string"},"documents":{"type":"array","items":{"type":"string"}}}},
 "upload_document":   {"type":"object","required":["room","name"],"properties":{"room":{"type":"string"},"name":{"type":"string","minLength":1},"text":{"type":"string"},"content_base64":{"type":"string","contentEncoding":"base64"}}},
 "get_invite_link":   {"type":"object","required":["room"],"properties":{"room":{"type":"string"}}},
 "delete_room":       {"type":"object","required":["room","confirm"],"properties":{"room":{"type":"string"},"confirm":{"type":"string"}}},
 "delete_document":   {"type":"object","required":["room","document","confirm"],"properties":{"room":{"type":"string"},"document":{"type":"integer"},"confirm":{"type":"string"}}},
 "remove_persona":    {"type":"object","required":["room","persona","confirm"],"properties":{"room":{"type":"string"},"persona":{"type":"integer"},"confirm":{"type":"string"}}},
 "revoke_invite":     {"type":"object","required":["room","invite","confirm"],"properties":{"room":{"type":"string"},"invite":{"type":"integer"},"confirm":{"type":"string"}}}}
```

Bounds the schemas leave out (name lengths and patterns, trigger ranges, one automatic trigger at most, the models a host allows, exactly one of `text` and `content_base64`) stay the routes' or the tool's own checks and come back as `isError` (AC16).

## Phases

### Phase 1 — Personal API tokens
- Work: `api_tokens` store; `GET`, `POST /api/profile/tokens` and `DELETE /api/profile/tokens/{id}` (cookie only); `grt_` bearer lookup once per request ahead of the routes, used by the participant, member-or-admin and profile auth paths; read-only method gate; admin refusal and the cookie-only list; throttled last-use write; purge in the existing daily job; audit actions and their `/admin` labels; merge revocation and preview count; `/profile` › API tokens tab and the shown-once sheet (with a `curl` example); README (routes, auth paragraph, Personal data row) and `docs/architecture.md` (Interfaces table).
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC12
- **Data model impact**: new `api_tokens` (`id`, `profile_id` FK to `profiles` with `ON DELETE CASCADE`, `name`, `token_hash` UNIQUE, `scope` CHECK in `read`, `full`, `created_at`, `expires_at_ms` NULL for never, `last_used_at_ms`, `last_used_ip`), indexes on `profile_id` and `expires_at_ms`; audit actions `api_token.create`, `api_token.revoke`
- **DoD**:
  - store tests: after a create, a `LIKE '%grt_%'` scan of every text column of every table finds nothing, and `token_hash` equals `HashToken(token)`; the list JSON has no `token` or `token_hash` key;
  - table-driven auth tests with each scope on three route shapes (participant, member-or-admin, profile): read-only GET 200 and POST 403; full token room-admin route 200 for a room admin's profile and 403 for a plain member's; a room the profile is not in 403;
  - a test that walks the server's registered patterns and calls every `/api/admin/*` and admin-only route with both scopes: all 403, so a route added later is covered without editing the test; a test per cookie-only route of AC6: 403; start refused with `GRILL_ROOM_ADMIN_TOKEN=grt_x`;
  - with an injected clock: expired gives 401; revoked gives 401 on the next request; two uses 10 s apart write `last_used_at` once, two uses 61 s apart twice; the IP comes from `X-Forwarded-For` only behind a trusted proxy; the purge keeps a token 29 days past expiry and deletes it at 31; the IP clears after `GRILL_ROOM_CLIENT_RETENTION`;
  - 100 tokens accepted, the 101st 409, accepted again once one expires;
  - audit entries for create and revoke with the listed details, and a room rename done with a token carries `api_token`; a zap observer over the auth tests sees no `grt_` in any field;
  - merge: B's tokens gone, counted in the preview, `moved` and the audit entry; purge skips a profile holding an unexpired token;
  - Playwright on `/profile` at 390 and 1440 px: create, the shown-once sheet, Copy, "never used" then "used just now" after one request with the token, Revoke then the request 401;
  - `make lint test` green, the changed e2e specs green.

### Phase 2 — MCP server
- Work: `/mcp` on the official Go SDK (Decisions) with `Stateless` and `JSONResponse`, wrapped in `http.NewCrossOriginProtection` and the token check; the catalogue as typed tools, each a sub-request through the server's own handler with the caller's `Authorization` header; the two new read routes; `confirm` checks; audit entries for destructive calls; one counter `grill_room_mcp_tool_calls_total{tool,result}`; the `claude mcp add` line in the new-token sheet; README "Agent access" section, `docs/architecture.md` row, third-party notices for the new modules; `make e2e-mcp-live`.
- **Covers**: AC13, AC14, AC15, AC16, AC17, AC18, AC19, AC20, AC21, AC22
- **Data model impact**: none (audit action `persona.remove` for the destructive call that the UI path does not audit today; `invite.revoke` and `room.delete` exist)
- **DoD**:
  - conformance tests with `httptest` and raw JSON-RPC: `initialize` with 2025-11-25 returns that version, `capabilities.tools` and `serverInfo`; `notifications/initialized` gets 202; `tools/list` holds 27 tools for a full token and 12 for a read one, each schema has `additionalProperties: false`; `tools/call list_rooms` returns `structuredContent`; a body that is not JSON and an unknown method get the SDK's HTTP 400, an unknown tool -32602; a wrong-type argument and a route's 403 each give `isError: true` with the message; GET and DELETE 405; a foreign `Origin` 403; no token, a cookie alone and the admin token each 401 with no `WWW-Authenticate` `resource_metadata`; a request with a public `Host` on a server bound to 127.0.0.1 passes;
  - the SDK's own client (`StreamableClientTransport`) lists the tools and calls `get_room` against the test server;
  - authorization parity: tools run as a plain member, a room admin and a read-only token, and their error or success matches the REST route's status for the same caller;
  - a recording handler asserts each tool reaches only its catalogue routes; none reaches `POST .../messages`, `.../turns`, `.../decision`, `.../changes/` or `.../cap`;
  - confirm tests, per destructive tool: `confirm` missing, in another case, with a trailing space, or set to the id where a name is due each give `isError` and leave the target and the audit log unchanged; the exact name deletes and writes one audit entry with the token id;
  - host approval: under "approve everything" `add_persona` by a member returns `held` and the persona is pending; the same call by the AI host applies;
  - `get_invite_link` called twice returns the same link and the room holds one more invite, not two;
  - `make e2e-mcp-live` on a server of its own on loopback, run locally with this machine's Claude Code login on a throwaway config dir: Playwright creates a profile and a full token and reads the command from the sheet; the test runs it with `-s project` in a temporary directory, and `claude mcp list` there reports `grill-room` connected (no model request); then one `claude -p --model haiku --strict-mcp-config --mcp-config .mcp.json --allowedTools "mcp__grill-room__*"` run asks for a room from the built-in Retro starter with one more persona from the shipped Default template (persona Grill). The REST API then shows the room "created from Retro", its personas Facilitator, Secretary and Grill, and the audit log the room's creation. Budget: that one run, at most 6 model requests;
  - Playwright: the sheet shows the command with the public URL and the token, and Copy puts it on the clipboard;
  - `make lint test` green, the changed e2e specs green.

## Data model impact (summary)

- New table `api_tokens` (Phase 1).
- Audit actions `api_token.create`, `api_token.revoke` (Phase 1), `persona.remove` (Phase 2, MCP calls only).
- No export change: tokens belong to profiles, which exports never carry.
- New Go modules (Phase 2): the MCP Go SDK and its direct dependencies.

## Decisions

From the notes:

- Tokens per profile, read or full, full meaning the profile's rights with room admin rights following the profile (Q1).
- Hash only, shown once, `grt_` prefix, last use time and IP for the owner, one-click revoke, audit on create and revoke, never `/api/admin`, 100 per profile, expired tokens deleted after 30 days (Q1).
- MCP tool groups (read, configure, destructive with `confirm`), host approval rules unchanged, no `post_message` or `run_persona` (Q2).
- MCP auth is the same token as a bearer; OAuth 2.1 later (Q3).
- The live e2e may spend up to 6 haiku requests: one `claude -p` makes one model request per tool round trip (user, 2026-10-16).

Reconciled with what shipped after the draft (2026-10-16):

- `/profile` has tabs since 011b: the tokens get their own "API tokens" tab (`#tokens`), not a section, placed after Skills and before Account, with a count badge like the other tabs.
- Rows follow the kit (docs/design.md, List row): `.list-rows` in `.cols` with a head row on desktop, cards on a phone, as the 004b `/admin` lists. Revoke sits in the row's ⋯ menu, one click, no confirm sheet: the kit puts a row's actions in ⋯, and Q1 asks for one click.
- The plaintext token and the `claude mcp add` line are kit `.value` blocks with their own Copy button, as docs/design.md asks for a key or a link.
- 011 shipped: AC12 is no longer conditional. A merge deletes B's tokens instead of moving them to A: a token was granted by B's browsers, and moving it would hand A's rights to whoever holds it. The count shows in the preview, the `/admin` merge sheet, `moved` and the audit entry. The purge keeps a profile with an unexpired token; a purged profile's expired tokens cascade.
- `/admin` gets no token list (out of scope as drafted); its audit view gains the labels of the new actions, and its merge sheet the token count.
- 011 replaced per-click invites with one shareable link per member and room: `get_invite_link` maps to `POST .../invites/share`, idempotent by construction, instead of the draft's "GET then POST". AC6's invite exception is that link.
- 009's persona model has no single `trigger` field: `edit_persona` takes the REST route's `every_n`, `every_voice` and `every_minutes` (0 is off, one at most), and the name pattern stays the route's check. Plugin tools need a room admin, as the plugin routes do.
- The built-in persona template that yields the persona Grill is titled "Default"; the live e2e adds it under that title.
- The destructive tools read the target's current name through a read route first (`GET` room, documents, agent): the `confirm` check is the tool's, not the route's.

Made while drafting (overturnable):

- MCP library: the official Go SDK, `github.com/modelcontextprotocol/go-sdk`, pinned at v1.8.0 (latest on 2026-10-16, checked with `go list -m -versions`). Its `go.mod` asks for Go 1.25, under ours. Licence: MIT, moving to Apache-2.0 for new contributions (its LICENSE file). It supports the 2026-07-28 revision as well as 2025-11-25 and older. A stdlib version would take about 250 to 400 lines, plus hand-written argument checks, to serve both the 2026-07-28 stateless revision and the 2025-11-25 handshake that Claude Code still falls back to. The spec changed twice in a year, and the SDK tracks it. The tools are thin sub-requests, so swapping the library later touches one package.
- Tools call the REST routes through the server's own handler with the caller's `Authorization` header, instead of calling the store. Authorization then cannot drift from the UI's, and AC16's parity test checks it.
- `/mcp` takes JSON responses only, holds no session and returns 405 on GET: no tool streams, and stateless serving is what 2026-07-28 asks for.
- Origin check through `http.NewCrossOriginProtection` (stdlib). The SDK's localhost Host-header protection is off: the server listens on 127.0.0.1 behind its reverse proxy by default, and that check would refuse every proxied request. A DNS-rebinding page cannot send the bearer token, so the token and the Origin check cover it.
- A bare 401 with no OAuth metadata: Claude Code starts no OAuth flow when an `Authorization` header is configured, and advertising metadata would make it try.
- A read-only token lists only the read tools, so an agent does not plan calls it cannot make.
- `confirm` errors do not echo the target's name, so the agent has to read it on purpose. It is about intent, not secrecy.
- "add starter" in Q2 reads as `save_room_as_starter` (a room admin saves the room to their personal library).
- An invite has no name, so `revoke_invite` takes its id as `confirm`.
- A token cannot make another lasting credential (tokens, email binding, recovery, pairing, push subscriptions). Otherwise a leaked token could outlive its revocation. The shareable invite stays allowed because Q2 asks for it, and the sheet warns that it outlives the token.
- Expiry choices 30, 90 (default), 365 days or never; name 1 to 60 characters, duplicates allowed.
- A room the token's profile is not in gets 403, not the cookie path's 401: a valid token that reads as "invalid or revoked" would mislead an agent.
- Last use written at most once a minute, so a busy agent does not write to SQLite on every request.
- Two new read routes (`.../messages` paged, `.../documents/{id}/text`): the UI reads messages through SSE and documents as rendered HTML, neither of which fits a tool.
- `upload_document` accepts text or base64; the MCP request body cap is set from `GRILL_ROOM_MAX_UPLOAD_MB` (×4/3 for base64, plus 64 KiB), not the SDK's 4 MiB default.
- The live e2e runs Claude Code locally, like `e2e-agent-live`, not in the sandbox image, whose egress allowlist would have to admit the room server.
- The `claude mcp add` line carries no `-s`: Claude Code's default scope applies, and you pick another one when you paste it.

Made while building (2026-10-16):

- `tools/list` comes sorted by name: the SDK sorts its tools, and the order carries no meaning for a client. A body that is not JSON and an unknown method get the SDK's HTTP 400 rather than a JSON-RPC error: the SDK checks both before dispatch, and a client reads either as a failed call.
- A route answering a list comes back under `items`: `structuredContent` is an object for clients that predate array content.
- `/mcp` is exempt from the read-only method gate (every MCP call is a POST); a read-only token still lists only the read tools, and every sub-request goes through the gate again with the token.
- The tools pass the `/mcp` request to their sub-requests through the SDK's token info (`auth.RequireBearerToken`, with no metadata URL so no `WWW-Authenticate`); the token gate has already checked the token.
- An action a route audits for room admins is also audited when done with an API token by a plain member (`roomAuditing`), and `remove_persona` writes `persona.remove` for token calls only: AC20's one audit entry per destructive call.
- The live e2e runs Claude Code on a throwaway `CLAUDE_CONFIG_DIR`, with the login's access token in its environment only, so `~/.claude` and `~/.claude.json` stay untouched; it runs against a server of its own on loopback rather than the compose stack, which it does not need. The prompt goes on stdin: `--allowedTools` takes every argument after it.
- The new-token sheet keeps a press outside from closing it once the token shows: the token would be lost.
