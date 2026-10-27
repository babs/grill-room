# 019: Anonymous room creation

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-24
**Context**: builds on 004 (room creation limits, import), 011 (profiles without email, rate limiter).

## Problem

A visitor without an invite cannot start a room: profiles are only born at `POST /api/join`, which needs an invite, and `POST /api/profile/rooms` needs a profile. An operator who wants an open instance must hand out invites by hand.

## Solution

An operator can turn on room creation without an invite. Then the home page, opened with no invite and no profile, offers "Create a room": a room name and a display name. The visitor gets a profile and lands in the new room as its room admin, like a profile creating a room today. Each IP may do this a set number of times per hour. While this is on, a profile without an email cannot import a room.

## Scope

- Env flag turning anonymous creation on, off by default.
- Env setting for the per-IP hourly limit.
- New route creating a profile and its first room in one go.
- Home page form.
- Import refused to profiles without email while the flag is on.
- README and spec index.

## Out of scope

- Captcha or proof of work: the per-IP limit and the storage quota are the guard; revisit on abuse.
- Starters on the anonymous form: blank room with the Default persona; the new profile uses `/profile` › New room for starters.
- Per-IP limit on `POST /api/profile/rooms` for existing profiles: `rooms_per_profile` already bounds it.
- Any change to import when the flag is off.

## Acceptance criteria

- [x] **AC1** `GRILL_ROOM_ANONYMOUS_ROOMS` (default `false`, parsed with `strconv.ParseBool`; any other value refuses the start) turns anonymous creation on. Off, `POST /api/rooms/anonymous` answers 404 and the home page shows today's "You need an invite link" line.
- [x] **AC2** On, `POST /api/rooms/anonymous` with `name` and `display_name` and no valid profile cookie creates, in one transaction, a profile, the room, the profile's seat as room admin, and the creator's shareable invite, as 004-AC7 does for a profile. It answers 201 with the room, sets the profile cookie and the room cookie, and returns `recovery_url` once, as `POST /api/join` does.
- [x] **AC3** A request carrying a valid profile cookie gets 409 "You already have a profile: create the room from your profile page" and creates nothing.
- [x] **AC4** Each client IP (its /64 for IPv6, as 011's limiter counts) creates at most `GRILL_ROOM_ANONYMOUS_ROOMS_PER_HOUR` (default `3`, 1 to 1000, else the server refuses to start) anonymous rooms per sliding hour, across pods. Past it: 429 with `Retry-After` and `{"reason":"rate_limited","retry_after_s":N}`, nothing created. A refused or failed request does not count.
- [x] **AC5** The instance guardrails hold: with `room_creation` set to `super-admin`, or the instance at its storage quota, the route answers 403 with the same reason as `POST /api/profile/rooms` and creates no profile.
- [x] **AC6** A bad `name` or `display_name` gets 400 with the same messages as `POST /api/profile/rooms`, and creates no profile.
- [x] **AC7** The creation is audited as `room.create` with actor `profile:<uuid>` and details `{"as": <display>, "anonymous": true}`, and logs `room_created` with `anonymous=true`.
- [x] **AC8** With the flag on, the home page opened with no invite and no profile shows "Create a room" (room name, display name, submit). Success opens `/r/<room>` and handles `recovery_url` as a join does. A 429 or 403 shows its reason under the form. With a profile, the page shows its "Your profile" link and no form.
- [x] **AC9** With the flag on, `POST /api/rooms/import` by a profile without an email answers 403 "Add an email to your profile to import a room", preview included, before reading the body. The same profile with an email imports as today. The admin is never refused by this rule.
- [x] **AC10** With the flag off, a profile without an email imports as today.
- [x] **AC11** README documents both variables and the route; `specs/README.md` lists 019.

## Phases

### Phase 1 — Server: route, limit, import rule
- Work: options and env parsing in `cmd/grill-room`; a store method creating profile + room in one transaction (shares `CreateProfileRoom`'s body); `createAnonymousRoom` handler with a new limiter scope `anon-room`; import check; audit and log fields.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC9, AC10
- **Data model impact**: `rate_events` scope check accepts `anon-room`; next PostgreSQL migration
- **DoD**: `go test ./internal/web/... ./internal/store/...` green on PostgreSQL, with tests that go red when: the flag is off and the route answers anything but 404; a profile cookie is ignored; the N+1th request from one IP succeeds or a refused one counts; a 403 or 400 path leaves a profile row behind; an emailless profile imports with the flag on, or is refused with it off.

### Phase 2 — Home page and docs
- Work: `join.html` / `join.js` form when no invite and no profile, shown when `GET /api/profile` reports the flag (`anonymous_rooms: true`); README, spec index.
- **Covers**: AC8, AC11
- **Data model impact**: none
- **DoD**: an e2e spec under `e2e/` green for: form visible with the flag on and hidden with it off; submit lands in the room as room admin; a 429 shows its reason. `make lint` and `go test -race ./internal/web/` green.

## Data model impact (summary)

`rate_events_scope_check` gains `anon-room`. One migration, phase 1.

## Open questions

- [ ] None.

## Decisions

- Route is `POST /api/rooms/anonymous`, not a mode of `POST /api/profile/rooms` — the latter keeps "needs a profile" as its contract.
- A browser with a profile is sent to `/profile` › New room (AC3) instead of being given a second profile.
- The anonymous form creates a blank room only — starters need the editor, which lives on `/profile`.
- The home page learns the flag from `GET /api/profile` (`anonymous_rooms`), which it already calls, instead of a new endpoint.
- The import rule keys on "no email" (011's anonymous marker) and only while the flag is on — per your answer; the profile lifts it by adding an email.
- The limiter gets its own `anon-room` scope (a migration) rather than sharing `login` — anonymous traffic filling a shared scope's cap would block admin logins.
- `room_creation: super-admin` beats the env flag: the admin's live setting wins over the deploy-time one.
