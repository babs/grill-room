# 027: Restricted instance e2e against a real Keycloak

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-28
**Context**: 021 (restricted instance) and 026 (attach, bound merge) are tested against the in-repo fake provider `tools/e2eoidc` only.

## Problem

The fake provider issues the claims the tests ask for. It cannot show that a real identity provider's login page, SSO session, `prompt=none` answers, group mapper, `email_verified`, key rotation and account lifecycle work with the server as shipped.

## Solution

A second e2e stack runs the restricted-instance specs against a Keycloak container with a versioned realm, plus the scenarios only a real provider can play: a disabled account, a group removed, rotated keys, an expired SSO session. It runs in its own CI job; the fast fake stack stays in `make e2e`.

## Scope

- Keycloak fixture: image pinned by digest, realm imported at start (confidential client, PKCE required, groups `grill-members` and `grill-admins`, a groups mapper into the ID token, users with and without verified email).
- `e2e/keycloak/` stack sharing the `e2e/oidc/` scenarios through a provider adapter (sign in at the provider's page, provider-side admin actions).
- Real-provider scenarios.
- `make e2e-keycloak`, a CI job, README.

## Out of scope

- Testing against an operator's own identity provider.
- Other providers (Entra, Google): one real implementation proves the protocol path.
- Replacing the fake stack.

## Acceptance criteria

- [x] **AC1** `make e2e-keycloak` starts Keycloak (image pinned by digest) with the versioned realm, waits for its discovery document, starts a restricted-mode server against it, runs `e2e/keycloak/` with one worker, and stops both. It needs Docker and fails with a clear message without it.
- [x] **AC2** The `e2e/oidc/` scenarios that do not depend on the fake's control switches pass against Keycloak through a shared adapter: sign in from an invite lands in the room as internal with the invite kept; the member-group refusal; `/admin` through the admin group and Unbind; attach a guest profile (026) and Not now; a second account does not loop.
- [x] **AC3** Revalidation with a real SSO session: past `GRILL_ROOM_OIDC_REVALIDATE` (aged in the test database, as the oidc stack does), a page goes through `prompt=none` and comes back without a login form while the Keycloak session lives; after the Keycloak session is ended (admin API logout), the same page lands on the Keycloak login form, and signing in brings it back to the page it was on.
- [x] **AC4** Account disabled in Keycloak: once past revalidation, the member's page cannot sign in again (Keycloak refuses), their API token and seat answer 401 `reauth`, and an invite they made stops admitting.
- [x] **AC5** Group removed in Keycloak: after the member's next sign-in, `/admin` no longer opens for them and their provider admin session has ended; removing them from `grill-members` refuses the next sign-in with the member-group text.
- [x] **AC6** Signing keys rotated in Keycloak (new active key, old one disabled): a new sign-in succeeds with the server running.
- [x] **AC7** A CI job runs `make e2e-keycloak` on pull requests that touch the OIDC code, the reauth page script, the e2e oidc/keycloak stacks or the realm, on pushes to develop, and on demand; it uploads the test results on failure.
- [x] **AC8** README documents the target, its needs and what it covers.

## Phases

### Phase 1: fixture and shared scenarios
- Work: realm export, Keycloak start/stop wrapper, `e2e/keycloak/playwright.config.js`, provider adapter, shared scenarios, `make e2e-keycloak`.
- **Covers**: AC1, AC2, AC8
- **Data model impact**: none
- **DoD**: `make e2e-keycloak` green in one local run, its container removed on every exit path; the fake stack still green. CI runs it from phase 2.

### Phase 2: real-provider scenarios and CI job
- Work: AC3 to AC6 specs using the Keycloak admin API; the CI job.
- **Covers**: AC3, AC4, AC5, AC6, AC7
- **Data model impact**: none
- **DoD**: the CI job green on the PR; each new scenario shown red by breaking its server-side guard once.

## Data model impact (summary)

None.

## Open questions

- [ ] None.

## Decisions

- Keycloak: open source, runs as one container, admin API for lifecycle actions, the usual OIDC test reference.
- A dedicated CI job, not `make e2e`: the fixture weighs about 1 GB and 30 s of start-up; the fake stack keeps every PR fast.
- Revalidation is aged in the database rather than waiting: the minimum window is one hour.
