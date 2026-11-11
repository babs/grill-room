# 033 — Timekeeper in the starters

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-10
**Context**: builds on 006 (room starters), 032 (time-keeper option, Timekeeper preset).

## Problem

The Timekeeper (032) exists only in the persona library. A room started from a shipped starter has none, and adding one by hand is a step most rooms skip. A starter cannot carry it anyway: a starter persona has no time-keeper role and no speak-over wait, so saving a room with a Timekeeper as a starter loses both (032 Out of scope).

## Solution

A starter persona can be the room's time keeper, with its speak-over wait. Saving a room as a starter keeps both, and a room started from it gets them. The shipped Retro and Incident starters gain the Timekeeper: every 5 minutes, speaking in the call, speaking over after 20 s.

## Acceptance criteria

- [x] **AC1** A starter persona has `time_keeper` and `cut_in_s` (0, 10, 20, 30 or 60), both optional, read as false and 0 when absent. Publishing a starter with two time keepers, or with another `cut_in_s`, answers 400 with the reason, as two notes watchers do (009-AC31).
- [x] **AC2** A room started from a starter gives each persona its `time_keeper` and `cut_in_s`. Whoever starts the room sets them, as they set every other persona setting the starter carries (032's room-admin rule governs later edits in the room).
- [x] **AC3** Saving a room as a starter keeps its time keeper's role and `cut_in_s`.
- [x] **AC4** The shipped Retro and Incident starters have a Timekeeper persona: the preset's text, badge, colour, model and effort, `every_minutes` 5, speaking in the call, `time_keeper` on, `cut_in_s` 20. Spec grill and Doc review are unchanged. Rooms made from earlier versions keep their copy (006).

## Phases

### Phase 1 — Starters carry the time keeper
- Work: the two fields on `StarterPersona`; validation beside the notes-watcher check; the room insert; the save-as-starter draft; the Timekeeper in Retro and Incident, built from the preset's fields so the two cannot drift apart.
- **Covers**: AC1, AC2, AC3, AC4
- **Data model impact**: none (starter manifests are JSON; `room_personas` has the columns since 032).
- **DoD**: `go test -p 1 ./internal/store/ ./internal/web/...` green, with tests for the 400s of AC1, a round trip room → starter → room keeping the role and the wait, and the Retro and Incident manifests carrying the Timekeeper with AC4's settings. `e2e/starters.spec.js` extended: start a Retro, the room lists the Timekeeper with time keeper on and "Speak over after" 20 s. `e2e/design.spec.js` green.

## Out of scope

- The MCP `edit_persona` tool (unchanged from 032).
- A Timekeeper in Spec grill or Doc review.

## Open questions

- (none)

## Decisions

- Retro and Incident get the Timekeeper: long meetings where people take turns. Spec grill already has three personas, and a doc review is mostly reading. Overturnable (2026-11-10).
- Starting a room sets the role whoever starts it: the starter is the room's initial setup, chosen by its creator, not a member's edit inside a running room.
- The built-in Timekeeper reuses the preset's fields, as Spec grill reuses `rodin`, so a preset text change reaches the starters too.
