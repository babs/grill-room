# 021: Restricted instance with OIDC sign-in

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-27
**Context**: builds on 004 (admin, room creation limits), 011 (profiles, email sign-in, recovery links, merge), 012 (personal API tokens), 016 (invites apart from seats). Excludes 019 (anonymous rooms).

## Problem

Anyone holding an invite link gets a profile, may create rooms and may invite others, who may invite others in turn. An organisation cannot tie the instance to its own accounts, nor cut off someone who leaves it.

## Solution

An operator points the instance at one OIDC provider. The instance becomes restricted: people with an account at the provider (optionally only one group) sign in and are internal; everyone else is an external, who enters only through an invite. Externals cannot create or import rooms, and cannot invite unless they are a room admin. An internal must sign in again with the provider every N days; one whose account is disabled loses access, API tokens included, within N days. A provider group can grant `/admin`; the admin token stays as a break-glass.

## Scope

- OIDC settings by env; restricted mode is on when the issuer is set.
- Sign-in (authorization code + PKCE), callback, sign-out; profile bound to `iss` + `sub`.
- Linking an existing profile by verified email at first sign-in.
- Internal / external split for room creation, import and invites.
- Revalidation every N days for profile, room seats and API tokens of internals.
- Email and recovery-link sign-in refused for internal profiles.
- Admin through a provider group.
- Join page, profile page and `/admin` showing the mode; README and spec index.

## Out of scope

- Several providers: one issuer until someone needs two.
- SAML, LDAP, SCIM provisioning.
- Storing refresh tokens: revalidation is a fresh sign-in, silent when the provider allows it.
- Back-channel or front-channel logout from the provider.
- Mapping provider groups to rooms or room admin rights.
- Changing anything when no issuer is set.

## Acceptance criteria

- [ ] **AC1** `GRILL_ROOM_OIDC_ISSUER`, `GRILL_ROOM_OIDC_CLIENT_ID` and `GRILL_ROOM_OIDC_CLIENT_SECRET` turn restricted mode on, all three or none. The server fetches the issuer's discovery document at start and refuses to start if it fails, if `GRILL_ROOM_URL` is not https (localhost excepted), or if `GRILL_ROOM_ANONYMOUS_ROOMS` (019) is true. Unset, the instance behaves as today in every AC below.
- [ ] **AC2** `GET /auth/login` starts an authorization code flow with PKCE (S256), `state` and `nonce`, scopes `openid email profile` plus `GRILL_ROOM_OIDC_SCOPES` (space-separated, optional). `GET /auth/callback` verifies `state`, exchanges the code, verifies the ID token (signature, `iss`, `aud`, `exp`, `nonce`) and refuses any failure with a page that says what happened, logs `oidc_callback_failed` with the reason and creates nothing. `state`, `nonce` and the verifier live in a short-lived HttpOnly cookie, one per sign-in in flight; the verifier never leaves it, and `state` and `nonce` go only to the provider.
- [ ] **AC3** With `GRILL_ROOM_OIDC_MEMBER_GROUP` set, a sign-in whose `groups` claim (name set by `GRILL_ROOM_OIDC_GROUPS_CLAIM`, default `groups`) lacks it is refused: "Your account is not allowed on this instance. Ask for an invite to join a room as a guest." No profile is created or bound.
- [ ] **AC4** A successful sign-in finds the profile bound to its `iss` + `sub` and signs this browser into it. With none: if `email_verified` is true and an unbound profile holds that email, that profile is bound; otherwise a new profile is created and bound, with the claim's email if verified and unused, and `name` from the claim. A profile this browser already holds is never bound for being there, with or without an email: the browser moves to the matched or new profile, the replaced session ends, and the admin can merge them (011). Each case is audited (`profile.oidc_bind` with `how`: `existing`, `email`, `new`).
- [ ] **AC5** A profile bound to the configured provider is internal; every other profile, and a participant without a profile, is external. In restricted mode, `GET /api/profile` reports `internal: true|false`, `restricted: true` and the sign-in URL; without it, its JSON is unchanged.
- [ ] **AC6** In restricted mode, `POST /api/profile/rooms` and `POST /api/rooms/import` refuse an external with 403 "Sign in with your organisation account to create rooms". The admin is not affected.
- [ ] **AC7** In restricted mode, an external who is not a room admin of that room gets 403 "Only members of the organisation and room admins can invite" on `POST /api/rooms/{room}/invites` and `.../invites/share`. A join through an invite whose creator is now an external who is not a room admin of the room is refused with 401 "This invite no longer works", the same as a revoked one; invites created by the admin, an internal or a room admin keep working.
- [ ] **AC8** An internal profile's sign-in is valid for `GRILL_ROOM_OIDC_REVALIDATE` (default `168h`, `1h` to `720h`) from its last successful sign-in. Past it, the profile cookie, the profile's room seats and its API tokens get 401 `{"reason":"reauth"}` on every route and the open page streams close with it; the profile's paired daemons get 403 `{"reason":"reauth"}` and their stream closes without `revoked`; nothing is deleted. Pages answer by sending the browser to `/auth/login` with `prompt=none` first, then a normal sign-in if the provider answers `login_required`; the browser comes back to the page it was on. A new sign-in restores the cookie, the seats and the tokens.
- [ ] **AC9** An internal profile refuses email magic links and recovery links, the admin's one-time ones included: the link page says "This profile signs in with your organisation account" and links to `/auth/login`. Externals keep both, as today. The admin can still merge, delete or unbind (AC10) an internal profile.
- [ ] **AC10** `/admin` › Profiles shows whether each profile is internal, its last sign-in, and an "Unbind" action that makes it external again (audited `profile.oidc_unbind`); a profile with an expired sign-in shows "sign-in expired".
- [ ] **AC11** With `GRILL_ROOM_OIDC_ADMIN_GROUP` set, a signed-in internal whose groups claim holds it gets an admin session (as an admin-token login does, audited `admin.login` with `via: "oidc"` and the profile's uuid) when opening `/admin`; one without it sees the token form. That admin session ends at the earlier of its usual expiry and the profile's revalidation time. The admin token keeps working in every case.
- [ ] **AC12** The join page in restricted mode shows "Sign in with <provider name>" (`GRILL_ROOM_OIDC_NAME`, default "your organisation account") above the name form; signing in from there comes back to the invite and joins as internal. A visitor without an invite sees only the sign-in button.
- [ ] **AC13** Sign-in, callback and refusals log `oidc_signed_in` / `oidc_callback_failed` with `iss`, `how` and the reason, never a token or a code; `grill_room_oidc_logins_total{result}` counts them.
- [ ] **AC14** README documents every variable, the routes and the internal / external rules; `specs/README.md` lists 021.

## Phases

### Phase 1 — OIDC sign-in and binding
- Work: env and discovery at start; `/auth/login`, `/auth/callback`, `/auth/logout`; binding rules; `GET /api/profile` fields; audit, log, metric. `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`, both already in `go.sum`, become direct dependencies.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC13
- **Data model impact**: `profiles.oidc_issuer`, `profiles.oidc_subject` (unique pair, nullable), `profiles.oidc_signed_in_at`; next PostgreSQL migration; dbcopy carries them.
- **DoD**: `go test ./internal/web/... ./internal/store/...` green on PostgreSQL against an in-process test provider (httptest issuer signing its own keys), with tests red when: a wrong `state`, `nonce`, `aud`, `iss` or an expired token is accepted; a missing member group is let through; an unverified email binds an existing profile; a second `sub` binds an already bound profile.

### Phase 2 — Internal / external rules
- Work: checks on room creation, import, invite creation, shareable link, join through an external's invite.
- **Covers**: AC6, AC7
- **Data model impact**: none
- **DoD**: table-driven web tests over {admin, internal, external room admin, external member} × {create, import, invite, share, join through their invite}, each cell red if the rule flips; same table green with restricted mode off and every cell allowed as today.

### Phase 3 — Revalidation and other sign-ins
- Work: expiry check in the profile, seat and API token paths; streams closed; `prompt=none` round trip; magic and recovery links refused for internals.
- **Covers**: AC8, AC9
- **Data model impact**: none
- **DoD**: web tests with an injected clock: one second past the window, the profile cookie, a seat cookie and an API token each get 401 `reauth` and an open stream closes; a new sign-in restores all three; a magic link and an admin recovery link for an internal are refused; an external's still work.

### Phase 4 — Admin and pages
- Work: admin through the group; `/admin` Profiles column, last sign-in, Unbind; join page and profile page; README, spec index.
- **Covers**: AC10, AC11, AC12, AC14
- **Data model impact**: none
- **DoD**: web tests: group member gets an admin session capped at revalidation, non-member does not, the token still works; an e2e spec under `e2e/` with the test provider: sign in from an invite lands in the room as internal, an expired sign-in comes back through the provider to the same page; `pre-commit run --all-files` green.

## Data model impact (summary)

`profiles`: `oidc_issuer TEXT`, `oidc_subject TEXT`, `oidc_signed_in_at TEXT`, unique on (`oidc_issuer`, `oidc_subject`). One migration, phase 1.

## Open questions

- [ ] None.

## Decisions

- Phase 3's `login_required` fallback starts the new flow from the callback itself, not by redirecting to `/auth/login`: that route refuses cross-site starts, and the redirect chain from the provider is cross-site.
- Pages decide whether the browser is signed in from an API call, never from the cookie on the page request: the profile cookie is SameSite=Strict, so the first page after the provider's redirect arrives without it.
- The browser's current profile session is read at `/auth/login` (same-origin) and sealed into the flow, as the callback, reached cross-site, never sees the Strict profile cookie.
- Only a binding to the configured issuer makes a profile internal: after a change of provider, profiles bound to the former one are external.
- A profile bound to any issuer is taken for email linking: after a change of provider the new sign-in opens a new profile, which the admin can merge.
- Paired daemons get 403, not 401, on reauth: released daemons wipe their pairing on a 401 or a `revoked` event, and AC8 deletes nothing.
- One issuer only — several providers multiply the binding rules for no current need.
- Profile bound to `iss` + `sub`, not email — email changes, `sub` does not.
- Existing profiles link on verified email only — an unverified claim would let anyone take over a profile.
- Revalidation is a new sign-in (silent with `prompt=none`), no refresh token stored — no provider secret at rest.
- An external room admin may invite — a room run with guests stays usable.
- The rule on invites is checked at join time, not only at creation — invites made before the mode was on, or by a room admin who stepped down, stop working the same way.
- `GRILL_ROOM_OIDC_MEMBER_GROUP` stays optional (user's choice); unset, the server warns at start that every account of the issuer is internal.
- The issuer must be https like the public URL (loopback excepted): it serves the signing keys and receives the client secret.
- Discovery at start retries for about a minute before refusing to start, so one transient provider error does not stop a pod.
- The admin token stays — a provider outage must not lock the admin out.
- 019 and 021 cannot both be on — an open instance and a restricted one contradict each other.
