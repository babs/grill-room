# 028 — Text size per area, in the profile

**Status**: shipped
**Requested by**: product owner
**Date**: 2026-10-28

## Problem

Text sizes in a room are fixed: chat 15 px, Transcription pane 14 px, both 16 px on phones (020-AC15, 020-AC18). Some people read better larger, some want more lines on screen. Nobody can change it, and a choice made on a laptop would not follow them to their phone.

## Solution

"Text size" in the room's own menu (your avatar) opens a small dialog with one row per area: Chat, Transcription. Each row offers S, M, L, XL; M is today's size. A pick applies at once, is saved in the person's profile, and follows them to every device and room. Other tabs of the same browser change with it; tabs on other devices change when they come back to the front.

## Scope

- Two areas: Chat (messages and composer) and Transcription pane.
- Four steps as a multiplier on each area's base size: S 0.875, M 1, L 1.125, XL 1.25.
- On phones the composer never goes under 16 px, whatever the step: iOS zooms on a smaller input.
- Stored on the profile; a seat without a profile keeps its choice in the browser.

## Out of scope

- Documents' Markdown text. Proposed as the next step: a third key `doc` in the same stored map and a third row in the dialog; the frame gets it by `postMessage` as it gets the text width (008-AC7). No migration needed then.
- The /profile page, admin pages, the daemon UI: their text does not change.
- Live push to tabs on other devices while they stay in the front: no per-profile channel exists.

## Acceptance criteria

- [x] **AC1** Given a person with a profile, when they pick L for Chat, then message bodies and the composer read at 1.125 times their base size, and the Transcription pane does not change.
- [x] **AC2** Given a person with a profile, when they pick XL for Transcription, then the pane's lines read at 1.25 times their base, and the chat does not change.
- [x] **AC3** A pick is stored on the profile: `GET /api/profile` returns it, and a fresh browser signed in to the same profile opens the room at that size.
- [x] **AC4** `PUT /api/profile/text-size` refuses a cross-site request (403), a browser without a profile (401), an unknown area or step (400).
- [x] **AC5** On a phone-width screen with Chat at S, the composer stays at 16 px.
- [x] **AC6** A second tab of the same browser applies a pick without reload; a tab of the same profile on another browser applies it when it comes back to the front.
- [x] **AC7** A seat without a profile can still pick; the choice lives in the browser and survives a reload.
- [x] **AC8** A profile with no pick yet reads at M everywhere: today's sizes.

## Phases

### Phase 1 — Store and API
- Work: migration 56, `text_sizes` on the profile, `PUT /api/profile/text-size`.
- **Covers**: AC3, AC4, AC8
- **Data model impact**: `profiles.text_sizes TEXT` (JSON map area → step), NULL for none.
- **DoD**: `go test -p 1 ./internal/store ./internal/web` green, PG schema count updated.

### Phase 2 — Room UI
- Work: menu item and dialog, CSS multipliers, localStorage cache and fallback, `storage` event between tabs, reload on visibility (already there).
- **Covers**: AC1, AC2, AC5, AC6, AC7
- **Data model impact**: none
- **DoD**: e2e `text-size.spec.js` green against the dev server.

## Data model impact (summary)

Migration 56: `ALTER TABLE profiles ADD COLUMN IF NOT EXISTS text_sizes TEXT`. A JSON object, keys `chat`, `transcript`; values `s`, `m`, `l`, `xl`.

## Open questions

- [x] None.

## Decisions

- Steps S/M/L/XL as a multiplier, not pixel values: one setting reads right on both the 15 px desktop base and the 16 px phone base.
- The control sits in the room's own menu, next to "Change my name": that is where the effect shows. The /profile page gets nothing.
- No profile: localStorage. The same key also caches the profile's value, so the page paints at the right size before `/api/profile` answers.
- One JSON column, not one column per area: adding the Markdown area needs no migration.
- Other devices update on focus, through the existing `/api/profile` reload on `visibilitychange` (011-AC9); a profile push channel is not worth it for this.
