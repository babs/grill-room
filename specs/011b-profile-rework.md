# 011b — Profile page rework

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-15
**Context**: addendum to spec 011. A review of `/profile` on v0.0.22 at 1440×900 and 390×844, light and dark, found 18 defects.

## Problem

`/profile` is one scroll of 1,300 to 2,100 px. The Email row, the one spec 011 wants people to act on, sits below the fold on every viewport. Starter sub-headings are larger than their section heading, there is no visible title, and three filled buttons compete. "Use on another device" says "Add an email above" even when an address is bound. The admin badge covers the end of a long room name.

## Solution

Two steps on one branch. First, fixes that keep the layout: the wording, the headings, one primary button, empty states, a styled file picker and the signed-out card. Then the `/admin` layout: a short identity strip that always shows the email state, then tabs (Rooms, Starters, Skills, Account), content at 72rem, left-aligned.

## Out of scope

- The signed-in landing page listing your rooms (defect D18): its own ticket.
- The API tokens tab: spec 012 adds it to these tabs.

## Acceptance criteria

### Fixes, no layout change

- [x] **AC1** "Other devices" follows the email state. No email: "Add an email, then sign in with it on the other device." Pending: "Confirm the link we sent, then sign in on the other device." Bound: "Sign in with <address> on the other device."
- [x] **AC2** A room card's admin badge sits in the meta line, never over the name. A 60-character room name and a long display name do not overlap it at 1440 and 390 px.
- [x] **AC3** Starter sub-headings render at `--fs-md`, weight 600: smaller than the section heading, larger than the hints.
- [x] **AC4** A visible "Your profile" title at `--fs-xl`. An empty status line takes no space; the title starts at most 24 px under the header.
- [x] **AC5** One filled button per view: "Send me a link" and "Add skill" are secondary while "+ New room" is on screen.
- [x] **AC6** The skills empty state is the `.empty` component with one sentence and one action. The shared starters empty state says who fills it.
- [x] **AC7** The skill file picker is a button at `--control-h` (44 px on a phone) that shows the chosen file's name. No native file input shows.
- [x] **AC8** The wording table below is applied. The old strings are gone from the UI, the e2e specs and the docs.
- [x] **AC9** Hints are capped at 70ch.
- [x] **AC10** A browser with no profile gets the 28rem join card with a heading.

### Layout

- [x] **AC11** An identity strip under the header: the name, then the email state (the bound address with a check, "No email yet" with "Add an email", or "Link sent to <address>, N min left"). It shows on every tab.
- [x] **AC12** Tabs Rooms, Starters, Skills and Account, with the kit's tab component: sticky, scrolling sideways on a phone, counts in the labels, the tab in the hash (`#rooms`, `#starters`, `#skills`, `#account`). `/profile#email` (011-AC7) opens Account with the field focused. A tab can be added for spec 012's API tokens. Superseded (user, 2026-10-17): on a phone a tab bar that overflows folds into one menu button listing its tabs (docs/design.md §5). Superseded (user, 2026-10-17): that button is a ☰ at the left of the app header (docs/design.md §5).
- [x] **AC13** Content `max-width: 72rem`, left-aligned with the tabs, as `/admin`. docs/design.md §6 says so for the profile.
- [x] **AC14** The Starters tab lists Yours, then Shared with everyone, then Built-in.
- [x] **AC15** Skills: "Add a skill" opens a sheet with the file and "Who can use it". The list shows the name, version, scope and a `⋯` with Remove.
- [x] **AC16** Room cards are clickable in full (at least 44 px), show the role in the meta line, and say "as <name>" only when that name differs from the profile's.
- [x] **AC17** The header matches the room: the logo links to `/`, the avatar shows initials and the name, and its menu holds the profile's links.
- [x] **AC18** e2e at 1440 and 390 px, light and dark: no horizontal overflow, every target at least 44 px on a phone, the email state above the fold in all three states, tab deep links, one filled button per tab.

## Wording

| was | now |
|---|---|
| (no visible title) | Your profile |
| This profile | Account |
| Your rooms | Rooms |
| … as Pat Review, on each card | shown only when the room's name for you differs from the profile's |
| Shared (starters) | Shared with everyone |
| A starter sets a new room up: its agenda, personas, documents and skills. Rooms keep their own copy. | Set up a new room in one go: agenda, AI personas, documents and skills. Changing a starter later leaves existing rooms alone. |
| None yet. (shared starters) | Nobody has shared a starter yet. Anyone can share one of theirs from its More actions menu. |
| Skills (for advanced users) | Skills |
| A skill is a Claude Code skill folder… | Skills give your AI personas extra abilities. Add one as a .zip. Adding it again makes a new version; rooms keep theirs until they update. The format sits under "How do I make one?" |
| None yet. (skills) | No skills yet. Add a .zip to give your AI personas a new ability. [Add a skill] |
| Upload a skill folder as a zip | Skill file (.zip) |
| Offer it | Who can use it |
| Only in my rooms (personal) / In every room (shared library) | Only my rooms / Everyone on this server |
| Upload / Uploaded meeting-notes v1. | Add skill / Added meeting-notes v1. |
| Profile without email: add one to find it on another device (on `/profile`) | No email yet. The room's marker keeps its tip. |
| No email on this profile yet. | No email yet. Add one to sign in on other devices. |
| Use on another device (on `/profile`) | Other devices |
| Not set: it comes from the name you join rooms with. | Set when you join a room. |
| This browser holds no profile. Join a room from an invite link, or open your recovery link here. | You're not signed in on this browser. Open an invite link to join a room, or sign in below. |
| Or sign in with the email bound to your profile | Sign in with your email |

## Phases

### Phase 1 — Fixes

- **Covers**: AC1 to AC10
- **DoD**: `e2e/profile-rework.spec.js` green; the profile, mail and recovery specs and `ui-guards` stay green.

### Phase 2 — Tabs

- **Covers**: AC11 to AC18
- **DoD**: the layout checks at 390 and 1440 px green; `/profile` checked on the lab in every email state, desktop and phone.

## Decisions

- User approved the proposal, both parts, 2026-10-15.
- One branch, one PR: the fixes, then the tabs, as two commit series.
- Without SMTP the identity strip shows the name only: there is no email state to show.
- No "Sign out of this browser" exists, so the header menu holds "Your rooms" and "Account and email" (AC17).
- The Starters tab has no primary button: a starter comes from duplicating one or from a room's settings, not from a blank form.
- A bind link lands on `#account`, where the address shows; a sign-in or recovery link lands on the rooms.
- The kit gains `.page` (72rem, left-aligned) and `.tabs.sticky`; `/admin`, the daemon page and the persona sheet still carry their own sticky rules.
