# 011 — Profile identity and recovery

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-14
**Context**: specs 002 (profiles, recovery link, magic link), 004 (`/admin`, audit log), 005 (user menu, recovery notice, profile page), 009 (People details card), grill notes (011), notes on the email flows, the landing page and the invite popup (2026-10-14)

## Problem

A profile lives in one browser's cookie. Lose the browser, or skip "I saved it" on the recovery link, and the profile is gone unless an email was bound, and nothing tells people to bind one. The email sign-in hides on `/profile`, a page a signed-out browser has no reason to open. The admin cannot help: the server keeps only the recovery link's hash, so it cannot show it again. Someone who joined from a phone and from a laptop ends up with two profiles, two room lists and split skills, with no way to join them. Profiles whose rooms are all gone pile up in `/admin`. Revoking an invite cuts off everyone who joined through it, and through the invites they made, and the button does not say who that is.

Binding an email is hard to follow: the profile page says "No email bound" or "Bound", nothing in between, and a link that fails only says it is invalid. The invite popup offers "Create an invite link" and a list of dates with Revoke, but no link to copy once the popup is reopened.

## Solution

A signed-out browser sees "Recover my profile by email" on the join page. A profile without an email carries a small "anonymous" marker that only its owner sees, and one gentle prompt suggests adding an email. In `/admin` › Profiles, the admin generates a one-time recovery link for a profile and hands it over, and merges two profiles of the same person into one. Profiles left with nothing in them disappear after 24 hours of inactivity, an `/admin` switch the admin can turn off. Before an invite is revoked, a confirmation names the people who lose access. The profile page's Email section shows where the binding stands, and every emailed link lands on a page that says what happened and what to do next. The invite popup opens on your shareable link with Copy; extra links and revoking move to Room settings › Invites.

## Scope

- Admin-generated recovery link: one time, 24 h, audited, from `/admin` › Profiles.
- Email recovery from the join page, shown only while the browser holds no profile and SMTP is set.
- Anonymous marker on your own avatar and name, its hover card, and one nudge to add an email.
- Admin profile merge: pick two, preview, merge, audited.
- Automatic purge of empty profiles after 24 h of inactivity, with a switch in `/admin` › Instance, audited.
- Invite revoke confirmation naming who loses access, wherever an invite is revoked.
- Email flows: one Email section on `/profile` with explicit states, a neutral signed-out request with resend, a clear outcome for every emailed link, one wording across mail, page and toasts.
- Landing page: "Your profile" with the display name when signed in, "Recover my profile by email" when not.
- Invite popup: your shareable link and Copy, made on demand; extra links and revoke in Room settings › Invites.

## Out of scope

- Device-to-device transfer by QR code (grill Q1b: no).
- Self-service merge by email confirmation (backlog, after 011).
- Rotating or showing again the permanent recovery link (005 Decisions F08 stands).
- Relinking a pre-merge export: an archive exported before a merge carries B's uuid, which matches no profile after it (see Decisions).
- Merging rooms, or deduplicating two seats of one person in a room.
- Notifying a merged or purged profile's owner by email.

## Acceptance criteria

### Recovery link from the admin
- [x] **AC1** In `/admin` › Profiles, "Generate a recovery link" on a profile shows a link once, with a copy button and its expiry. The link works once, for 24 h. Generating another voids the earlier unused one.
- [x] **AC2** Opening the link in a browser shows the same confirmation as the permanent recovery link (005), including the warning when the browser already holds a profile, and confirming signs that browser into the profile. A second use, a voided link or an expired one gets "this recovery link is invalid" and signs nobody in.
- [x] **AC3** The audit log records the generation (admin, profile uuid and name, expiry) and the use (profile uuid). It never holds the token. Deleting the profile deletes its links.

### Email recovery from the join page
- [x] **AC4** With SMTP set and no profile in the browser, the join page (`/` and `/join`, with or without an invite) shows "Recover my profile by email". It opens an email field. Sending it answers with the neutral confirmation of AC28 whether or not a profile holds the address, under the 002 rate limits. A browser holding a profile, or an instance without SMTP, never sees the link.
- [x] **AC5** The mailed link signs the browser in through the existing magic-link confirmation. A join page left open in another tab of that browser hides the recovery link and shows the profile's name once the tab is focused again, so the invite then joins as that profile.

### Anonymous marker and nudge
- [x] **AC6** With SMTP set, a profile without an email shows a small silhouette marker on its own avatar and name: in the room header's user menu, on its own lines and entries in People, and on `/profile`. Nobody else sees it, and no API returns another profile's email state.
- [x] **AC7** Hovering, focusing or tapping the marker shows "Profile without email: add one to find it on another device". Clicking it opens the email form. Where spec 009's People details card exists, the card for yourself carries the same text and link; other people's cards never do.
- [x] **AC8** After your first message in a room, a profile without an email gets one dismissible banner offering to add an email. "Later" or closing it snoozes the banner for 14 days on that profile, on every device. With SMTP set, this banner replaces the 005 recovery-link notice; without SMTP, the 005 notice stays and no marker or banner shows.
- [x] **AC9** Once an email is bound, the marker and the banner go, and `/profile` shows the address with "Change".

### Profile merge
- [x] **AC10** In `/admin` › Profiles, the admin picks a profile to keep (A) and one to merge into it (B), and sees a preview before anything changes: both names, emails, rooms (rooms where both have a seat marked), personal skills, persona templates, starters, rooms created and room-admin roles, the name collisions and how they resolve, the per-profile limits A would pass, and which email A ends up with. A swap button exchanges A and B. Merging a profile into itself is refused.
- [x] **AC11** Confirming moves everything of B to A in one transaction, then deletes B:
  - B's participants point to A. In a room where both have a seat, both seats stay, history stays intact (messages keep their author seat), and both seats belong to A.
  - B's personal skills, published shared skills, persona templates, starters and created rooms move to A. A personal skill whose name A already has gets the suffix `-2` (then `-3`, ...).
  - B's room-admin roles move to A; a room where both were admins keeps one row.
  - B's browser sessions move to A, so B's devices are signed into A (amended by 026-AC4: not when B's credentials end, that is when the result is bound to the provider and B is not the side holding the binding). B's pending magic and recovery links are deleted.
- [x] **AC12** A keeps its email. If A has none, it takes B's. B's email, if not taken, is deleted with B. A keeps its own name, limit override and email-nudge snooze. The skills disclaimer counts as accepted if either had accepted it.
- [x] **AC13** The merge writes one audit entry: admin, A and B uuids and names, and the counts moved per kind. B's earlier audit entries stay under B's uuid. A failure rolls everything back and leaves both profiles as they were.
- [x] **AC14** After a merge, A's room list shows the union of both lists with no duplicate room, and a merged profile may sit above a per-profile limit (skills, templates, starters, rooms created): it keeps everything and adds nothing more of that kind until under the limit.

### Purge of empty profiles
- [x] **AC15** A profile is empty when it has no email, no room it can still enter (no seat with a valid invite), no personal skill, published shared skill, persona template or starter, and no room-admin role. An empty profile whose last activity (creation, last join, last message, last sign-in) is older than 24 h is deleted as `DELETE /api/profiles/{id}` deletes one.
- [x] **AC16** The server checks at start and then every hour. A run that deletes profiles writes one audit entry (actor `system`) with the count and the uuids. A run that deletes nothing writes none.
- [x] **AC17** `/admin` › Instance has "Delete empty profiles after 24 h of inactivity", on by default. Changing it is audited. Off, no run deletes anything.
- [x] **AC18** A browser whose profile was purged is treated as signed out: the join page offers email recovery (AC4), and its next join creates a new profile.

### Invite revoke confirmation
- [x] **AC19** Revoking an invite opens a confirm sheet before anything is revoked: from Room settings › Invites, from `/admin` › room › Invites, and from a member's own invites. It names everyone who loses access, through that invite and the invites its joiners made down the chain, and marks room admins and "you". People who keep access through another valid seat are listed apart as "keeps access". An invite nobody joined says so. The sheet counts the chained invites it also revokes.
- [x] **AC20** The list in the sheet matches what the revoke then cuts: the same invites, and the same people get a 401 on their next request. An invite revoked meanwhile, or a new joiner between the sheet and the click, makes the revoke cover the current state, and the result toast names who lost access.

### Email flows (notes, 2026-10-14)
- [x] **AC21** `/profile` has one Email section with three states. No email: "Add an email" and an address field. Pending: the AC28 panel, "Link sent to <address>" with the lifetime left, "Resend" with a 60 s cooldown that counts down, and "Use another address". Bound: the address and "Change", which opens the field; the new address replaces the old one once its link is confirmed. The pending state survives a reload while its link is valid, and only the profile's owner sees the pending address.
- [x] **AC22** The signed-out request ("Recover my profile by email", AC4) is one field. Its answer is the neutral AC28 panel: "If a profile uses this address, a link is on its way", the link's lifetime, and "Resend" under the same cooldown.
- [x] **AC23** Opening an emailed link ends on one of these outcomes, each with its next step: email bound (the address shown); signed in (the profile's name and its rooms); expired ("Send a new link", which opens the email field); already used (sign in by email or open your profile); a bind link opened in another browser (a confirmation naming the profile the address goes to, then bound, this browser's own profile unchanged); the address already bound to another profile (explains it, and that the admin can merge the two profiles).
- [x] **AC24** The mail, the page and the toasts use the same words. The mail says what its link does (adds the address to your profile, or signs you in) and how long it works.

- [x] **AC28** Asking for an emailed link (sign-in from the join page or `/profile`, binding from `/profile`, the marker or the banner) gives feedback in place. The button shows it is busy and stays disabled until the answer. The form is then replaced by a confirmation panel: "Link sent to <address>" for a bind, the neutral wording for a sign-in, the link's lifetime, "Check your spam folder", "Resend" with a visible countdown and "Use another address". Errors show in the same place with the next step: mail not going out (try again later, or tell the admin), too many requests (when to retry), an invalid address (fix it). Never a toast alone.

### Landing page (notes, 2026-10-14)
- [x] **AC25** With a profile in the browser, the join page (`/` and `/join`) shows "Your profile" with the profile's display name, a link to `/profile`. Signed out, it shows "Recover my profile by email" instead (AC4).

### Invite popup (notes, 2026-10-14)
- [x] **AC26** "Invite people" opens on your shareable link for the room with a Copy button, first thing. The server creates it on first open when you have none, and creates the room creator's with the room. Each member has one per room. The popup has no "new link" button. The empty chat's invite card copies the same link. Every button that sends a request is disabled until the answer comes.
- [x] **AC27** Room settings › Invites shows to every member: a plain member sees their own invites, a room admin every invite. "New invite link" there makes an extra link, shown once, revocable on its own. Revoke opens the AC19 sheet. Once your shareable link is revoked, the popup makes a new one on its next open.

## Phases

### Phase 1 — Recovery and email flows
- Work: one-time recovery links (store, admin route, `/admin` button, `POST /api/profile/recover` also accepting them); the Email section states, pending address in `GET /api/profile`, link outcomes, mail wording; join page recovery link, profile link and focus re-check; anonymous marker, hover card, People card hook for 009, nudge banner with snooze; audit entries.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC21, AC22, AC23, AC24, AC25, AC28
- **Data model impact**: `recovery_links` (profile, token hash, created, expiry, used; cascades from `profiles`); `profiles.email_nudge_until`
- **DoD**:
  - store tests: a recovery link works once, fails after 24 h with an injected clock, and a newer link voids the older; cascade invariant extended to `recovery_links`;
  - e2e (Playwright) for the admin recovery link on a fresh context, its second use refused;
  - mailpit e2e: signed-out join page → "Recover my profile by email" → mailed link → signed in, and the open join tab then shows the profile's name; the link absent with a profile and without SMTP;
  - mailpit e2e per Email state and per link outcome of AC23; the AC28 states (busy, sent, resent, error) for sign-in and bind;
  - e2e: marker and hover card on your own avatar only (a second participant sees none), banner after the first message, "Later" hides it on a second context of the same profile, gone after binding an email;
  - `make test`, `make lint` and `tsc --checkJs` green.

### Phase 2 — Invites
- Work: the shareable link per member (stored so the popup can show it again), the popup redesign, Room settings › Invites for every member; revoke preview route and confirm sheet in the three places.
- **Covers**: AC19, AC20, AC26, AC27
- **Data model impact**: `invites.link`, the token of a member's shareable invite, NULL for every other invite
- **DoD**:
  - revoke preview test on a three-level invite chain with one person keeping access through another seat: the preview's people and invite ids equal what the revoke cuts;
  - store tests: one shareable link per member and room, a new one after a revoke, the creator's made with the room;
  - e2e: the popup shows and copies the same link on reopen, the empty-chat card copies it, buttons disabled in flight; the sheet at phone width in Room settings and `/admin`;
  - `make test`, `make lint` and `tsc --checkJs` green.

### Phase 3 — Merge and purge
- Work: merge preview and merge in one transaction, `/admin` › Profiles merge sheet; purge query, hourly loop on the server clock, Instance switch; audit entries.
- **Covers**: AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC17, AC18
- **Data model impact**: instance setting `purge_empty_profiles`; no new table or column for the merge
- **DoD**:
  - merge store tests: a room where both are seated keeps every message's author seat and both seats on A; skills, templates and starters moved, a same-named skill renamed `-2`; room-admin rows deduplicated; sessions moved and B's links gone; email precedence in all four cases (A only, B only, both, neither); the audit entry's uuids and counts; B deleted; a failure injected mid-merge leaves both profiles unchanged;
  - e2e: merge from `/admin` with preview and swap, then B's browser opens A's `/profile` with the union of rooms;
  - purge tests with an injected clock: an empty profile at 23 h stays, at 25 h goes; one with an email, a valid seat, a skill, a template, a starter or an admin role stays at any age; the switch off deletes nothing; one audit entry per non-empty run;
  - a purged profile's browser is signed out: e2e on the join page and the next join (the e2e stack has no SMTP), and a web test with the fake relay that it is offered email recovery;
  - `make test`, `make lint` and `tsc --checkJs` green.

## Data model impact (summary)

- New table `recovery_links`, cascading from `profiles`.
- `profiles.email_nudge_until`.
- `invites.link`: the token of each member's shareable invite.
- Instance setting `purge_empty_profiles`.
- The merge only rewrites existing `profile_id`, `publisher_id` and `creator_profile_id` references and deletes B.

## Personal data

- A one-time recovery link is a bearer secret for 24 h. Only its hash is stored. The admin sees it once and decides how to hand it over.
- The anonymous marker reveals whether a profile has an email. Only its owner sees it.
- A merge deletes B's email when A keeps its own. The audit entry holds both uuids and names, never an email.
- The purge deletes profiles that hold no email and no content. The audit entry lists their uuids for the retention period (1 year).
- The revoke sheet shows display names already visible to the room's members; no email.
- A shareable invite's token is stored as is, so the popup can show it again: the database now holds a working invite per member. Every other invite keeps only its hash.
- The pending address shows only to the profile that asked for it.

## Open questions

- (none)

## Decisions

From the grill (user, 2026-10-14):

- No device-to-device QR transfer. Recovery is email sign-in (join page link when signed out) plus the admin recovery link (Q1, Q1b).
- Marker on your own avatar and name only, hover card text, email form on click, one nudge after your first message, "Later" snoozes 14 days, bound email replaces the marker with the address and "Change" (Q2).
- Merge by the admin in `/admin` › Profiles: pick two, preview, merge, audited. Self-service merge by email later (Q3).
- Merge semantics as in AC11 and AC12 (Q3, coordinator call, overturnable).

Routine calls while writing, overturnable:

- The admin recovery link lasts 24 h: the admin hands it over by another channel, and 15 minutes is too short for that.
- It reuses `/profile#recover=` and `POST /api/profile/recover`: the endpoint tries the permanent hash, then the one-time links. One confirm page, one route.
- A new table rather than a `magic_links` purpose: `magic_links` requires an email and its purpose CHECK would need a table rebuild.
- Without SMTP, no marker and no banner: there is no email to add. The admin link still works.
- The nudge banner replaces the 005 recovery-link notice when SMTP is set, so the first message brings one notice, not two.
- A merge ignores per-profile limits rather than refusing: refusing would block the very cleanup it exists for.
- A colliding personal skill takes a `-2` suffix: skill names must stay valid skill names, and nothing is lost.
- B's sessions move to A: the person's other device keeps working without signing in again. Amended by 026: not when B's credentials end (the result is bound to the provider and B is not the side holding the binding).
- B's uuid is not kept as an alias. Exports made before the merge relink B's seats to nobody on import. Add an alias table if that hurts.
- Empty means "nothing a person would miss", as AC15 lists. A seat with a revoked invite does not count: its room is already closed to it.
- Last sign-in counts as activity, read from the newest `profile_sessions.created_at`; no new column.
- The purge runs hourly so 24 h is honoured within an hour; on by default because these profiles are unreachable anyway.
- The revoke sheet uses a preview route that runs the same recursive query as the revoke without writing, so the list and the cut cannot drift.

From the notes after the grill (user, 2026-10-14):

- The Email section's states, the signed-out request, the link outcomes and one wording across mail, page and toasts (AC21 to AC24).
- The landing page links to `/profile` when signed in, and offers email recovery when not (AC25).
- The invite popup opens on one shareable link with Copy; extra links and revoke live in Room settings › Invites with the revoke confirmation; buttons disabled while in flight (AC26, AC27).

Routine calls while folding the notes in, overturnable:

- "One per room" reads as one shareable link per member and room: an invite belongs to the member who made it, and revoke chains follow that. A single room link would put every joiner under its creator.
- The shareable link's token is kept in clear in `invites.link`, the one way to show it again; other invites stay hash-only.
- A bind link opened in another browser works after a confirmation naming the profile the address goes to. The 002 rule "only the browser that asked confirms" becomes "the person confirms, seeing which profile".
- "Expired" and "already used" come from the stored link while it is still there; once swept, a link reads as invalid.
- Resend waits 60 s in the page; the 002 rate limits still apply on the server.
- Phase 1 of the draft was split in two (recovery and email flows, then invites) for size.
- AC28 (user, 2026-10-14, after trying v0.0.17): a request for an emailed link confirms in place, with errors in place too. Added after Phases 1 and 2 shipped on the branch, built as a follow-up slice.
- A bind mail goes out before the answer, so a relay failure shows on the page; the address is the requester's own, nothing leaks. A sign-in request answers before any lookup, as 002 requires; it reports "mail not going out" only from the last bind mail having failed at the relay (connection, login, sender). Sign-in mails never touch that state, and a relay refusing one recipient does not set it, so the answer says nothing about any address.

Shipped (2026-10-15), loop notes:

- Live on the lab (v0.0.20), desktop and phone widths: the join page's email recovery and its sent panel, the room's marker and invite popup, Room settings › Invites, the `/profile` Email section with the pending panel after a reload, and the `/admin` › Profiles merge preview. The v0.0.19 check found the sent panel's buttons right-aligned; fixed, with an e2e check of the alignment.
- The first start with the purge on removed 7 empty profiles on the lab (AC16, on by default).
- Not checked live: a merge confirmed, and a link opened from a real inbox; the mailpit e2e and the merge e2e cover both.
- Owned: the shareable link is per seat, not per person (two seats of one profile in a room are rare); a merge pins the kept profile's display name; `address_rejected` tells a relay that verifies recipients (rate-limited; in-cluster relays usually accept and bounce); the marker is not repeated on chat lines (AC6 read as People rows, the header and `/profile`).
