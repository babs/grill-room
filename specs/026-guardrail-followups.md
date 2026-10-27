# 026: Guardrail follow-ups for anonymous rooms and the restricted instance

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-28
**Context**: open decisions left by 019 (anonymous rooms) and 021 (restricted instance).

## Problem

- Anonymous room creation can be turned on with no storage quota: the per-IP limit alone does not bound storage.
- At the storage quota, every refused creation scans every table under the write lock and gives its limiter slot back, so a client can slow every pod down for free.
- On a restricted instance, a guest who signs in with the organisation account gets a new profile; their guest rooms stay on the old one until the admin merges them.

## Solution

Anonymous creation answers 403 until the admin sets a storage quota. A pod that sees the instance at its quota refuses creations for 30 seconds without scanning again. After a sign-in, a browser that held a guest profile without an email is asked whether to attach it to the organisation account.

## Scope

- Quota required for anonymous creation.
- Short "at quota" memory per pod for every room creation path.
- Attach-your-guest-profile prompt after the OIDC callback.
- A merge (attach or admin) that leaves the kept profile bound to the provider ends the other profile's credentials: browser sessions, seat tokens, paired daemons, approved pairings, audio and open streams.
- Binding an unbound profile by verified email at provider sign-in signs its other devices out, as a bound merge does.
- README.

## Out of scope

- A running byte total kept by the writes (the `usedBytes` upgrade path).
- Attaching a guest profile that holds an email: email binding (021-AC4) covers it.
- Any change to 021-AC7 (invites of an expired internal lapse, room admin or not): kept as shipped.

## Acceptance criteria

- [x] **AC1** With `GRILL_ROOM_ANONYMOUS_ROOMS` on and no storage quota set, `POST /api/rooms/anonymous` answers 403 "Room creation without an invite needs a storage quota: set one in /admin › Server" before the limiter, and creates nothing. With a quota set, 019 behaves as shipped. The join page shows the reason under the form.
- [x] **AC2** When a room creation (anonymous, profile, starter or import) is refused because the instance is at its storage quota, that pod refuses every creation with the same reason for the next 30 seconds without scanning the tables; the anonymous route does so before the limiter, spending no slot. A change of the instance settings on that pod clears it at once; other pods follow within 30 seconds.
- [x] **AC3** On a restricted instance, when the browser held at `/auth/login` a valid session of a profile that is unbound, has no email and is not the profile signed in, the callback does not end that session but shows a page: "Attach your guest profile <name> (<n> rooms) to <provider name>, signed in as <account>?" (the provider name defaults to "your organisation account"; <name> is "Guest" when it has none; <account> is the signed-in profile's name, else its email), listing what would move, with Attach and Not now. A guest with nothing to move gets no offer. The guest is sealed into the flow state at login; the offer the page posts back is sealed on its own, bound to both profiles and the guest's session; it expires with the flow (10 minutes), and that session with it.
- [x] **AC4** Attach merges the guest profile into the signed-in one as the admin's merge does (011), audited `profile.merge` with the signed-in profile as actor and `"via": "oidc_attach"`, then ends the guest's sessions, seat tokens, paired daemons and approved pairings on every device, as any merge into a bound profile does, and lands on the flow's `next`. Not now ends the guest session, as 021 does today, and lands on `next`.
- [x] **AC5** Attach is refused (the page says "This offer expired. Your guest profile stays as it is; the admin can merge it into yours.") when the offer is expired, forged, replayed, sent cross-site, or when either profile changed since (the guest got an email or a binding, or was deleted); nothing is merged.
- [x] **AC6** README documents AC1 to AC5.

## Phases

### Phase 1: quota guardrails
- Work: AC1 check in the anonymous pre-limiter settings read and in the creation transaction; per-pod "at quota" memory checked by every creation path, cleared on settings change.
- **Covers**: AC1, AC2, AC6
- **Data model impact**: none
- **DoD**: `go test -race ./internal/web/ ./internal/store/` green with tests red when the quota check is dropped, when a refusal inside the 30 s still scans (count `usedBytes` calls), when the memory survives a settings change, or when the anonymous route spends a slot on it; anon e2e green.

### Phase 2: attach the guest profile
- Work: offer sealed into the flow at `/auth/login`; callback page; `POST /auth/attach` (same-origin, sealed token); merge reuse; e2e on the oidc stack.
- **Covers**: AC3, AC4, AC5, AC6
- **Data model impact**: none
- **DoD**: Go tests for each AC5 refusal shape red when its check is dropped; e2e: a guest joins by invite, signs in, attaches, and sees their rooms on the bound profile; Not now keeps them apart.

## Data model impact (summary)

None.

## Open questions

- [ ] None.

## Decisions

- Quota required rather than a start-up refusal: the quota lives in the database and is set after start.
- "At quota" memory per pod, 30 s, rather than counting refusals against the per-IP limit: it bounds the scans for the whole instance, not per address, and keeps 019-AC4.
- An import into an instance already at its quota answers the shared 403 at-quota refusal (was 422), so it feeds the memory; an archive that would only pass the quota keeps its 422 and is not remembered.
- A merge whose result is bound to the provider signs the other profile's devices out, amending 011-AC11: a bound profile is reached through the provider only, so no guest credential may carry over into it (also when the bound side is the merged one).
- Binding by verified email ends the profile's other credentials too (amending 021-AC4): the same rule as a bound merge, since the profile becomes one reached through the provider only.
- Attach is offered, never automatic: on a shared browser the next person signing in must not take a guest profile silently.
