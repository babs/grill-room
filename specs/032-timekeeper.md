# 032 — Timekeeper persona

**Status**: approved
**Requested by**: babs
**Date**: 2026-11-07
**Context**: builds on 002 (presets), 006 (timer trigger, agenda), 013 (persona voices, turn-taking), 031 (hand queue).

## Problem

A call drifts: one person holds the floor for ten minutes, raised hands wait, the agenda slips. Nobody wants to be the one who cuts a colleague off, and today no persona can help: none sees the hands or how long someone has been speaking, and 013 forbids a persona to ever speak over a human.

## Solution

The persona library ships a **Timekeeper**. Added to a room, it works at once: every 5 minutes it looks at the room (the agenda, the hand queue, who speaks and for how long) and, only when the room is off track, says so in the call: "Alice, Bob has had his hand up for four minutes." When the room is on track, it says nothing and posts nothing.

When it has something to say and someone is speaking, it does what a person would: a few short interjections first ("hmm", "pardon…"), and if the speaker goes on, it speaks over them. It never mutes anyone. How long it waits before speaking over is the one setting it adds, on the persona: off, 10, 20 (default), 30 or 60 seconds.

Between turns, its pill shows a red ⓘ while it rates the drift high, with its reason on hover. Any persona whose spoken line is stuck waiting for a gap shows a comic speech bubble on its pill.

What it sees comes from a **time-keeper** capability, a persona option like "Watches the notes", so any persona can be made a timekeeper.

## Scope

- A "Time keeper" option on room personas, one per room, and a "Speak over after" setting (off, 10, 20, 30, 60 s) shown with it.
- A time-keeper persona's preamble: the hand queue (rank, name, since when) and who speaks now and for how long.
- A time-keeper turn may end silent: no chat message, no spoken line.
- The shipped "Timekeeper" preset, which brings its room settings with it: every 5 minutes, speaks in the call, time keeper on, speak over after 20 s.
- Cutting in, in the `tts` pod, in real time: interjections, then speaking over, for a time-keeper line from its timer.
- A varied set of interjections per language, synthesised in the persona's own voice.
- On the persona pill: a comic speech bubble while a persona's spoken line waits to get in (any persona), and a red ⓘ while the time keeper rates the drift high.

## Out of scope

- Muting a microphone, lowering a hand, or acting on anything but the persona's own voice.
- Any other trigger than the timer for cutting in: a time-keeper persona called by `@name` or its button speaks by 013's rules.
- Recorded interjection clips; a per-room interjection list.
- Speaking over a human for any persona without the time-keeper option.
- Talk-time statistics, a speaking-time chart, a meeting timer in the UI.

## Acceptance criteria

### The capability
- [ ] **AC1** A room persona has "Time keeper" (`time_keeper`) beside "Watches the notes", edited by room admins: at most one per room, and turning it on for one turns it off for the one that had it, with a system line as for the notes watcher (009-AC31). With it on and "Speaks in the call" on, a "Speak over after" select offers Off, 10 s, 20 s, 30 s, 60 s (`cut_in_s`: 0, 10, 20, 30, 60; any other value is a 400).
- [ ] **AC2** A time-keeper persona's preamble adds, after the humans present: `Hands raised:` then one line per hand in queue order, `1. Alice ·a1b2, for 3 min`, at most 5 then `and N more`, or `Hands raised: none`; and the speaker line becomes `Alice ·a1b2 speaks now, for 4 min 10 s`, counted from the first voice line of their current run (their own voice lines with nobody else's in between and no gap over 30 s). Every other persona's preamble is unchanged and mentions no hands (031-AC7).
- [ ] **AC3** A time-keeper turn whose final answer is empty and that writes no `spoken.md` posts no message and says nothing; the persona's state shows "nothing to say" until its next turn, its timer restarts, and it counts as a completed turn for the chain. Any other persona's empty turn keeps today's behaviour.
- [ ] **AC4** The export carries `time_keeper` and `cut_in_s` on the persona, optional, and an import without them reads false and 0 (`docs/export-format.md`).

### The preset
- [ ] **AC5** The library has a shipped "Timekeeper" template (⏱️). Its text tells it to watch the agenda, the hand queue and talk time, to speak only when the room is off track, in one or two short sentences naming the people, and to stay silent otherwise (AC3). It asks for a drift rating in `drift.json` on every turn (AC13).
- [ ] **AC6** Adding the Timekeeper template to a room gives the copy `every_minutes` 5, "Speaks in the call" on, time keeper on (taken from another persona as in AC1), and `cut_in_s` 20. Adding any other template sets none of them. A room admin can change each afterwards. A new version of the preset's text is offered to rooms as for any preset (002-AC14) and never resets these settings.

### Cutting in
- [ ] **AC7** A line from a time-keeper persona's timer turn reaches the pod with its `cut_in_s`. Any other line, that persona's included, carries 0 and follows 013 unchanged.
- [ ] **AC8** With `cut_in_s` T > 0 and a human speaking when the line is ready, the pod says three interjections, at 0, T/3 and 2T/3 seconds, each under a second, then at T says the line over the speaker. While it speaks over, a human speaking does not stop it (this amends 013-AC21, 013-AC22 and 013-AC23 for these lines only). If the room goes quiet before T, it waits the room's quiet time and says the line normally, with no more interjections.
- [ ] **AC9** A cut-in line keeps one-persona-at-a-time and queue order (013-AC24); its queue maximum is T plus the room's queue maximum. A per-listener persona mute and the room mute (005-AC22) silence it as any persona line.
- [ ] **AC10** Interjections come from a set of at least 12 per language (French and English), picked at random with none repeated within one line, synthesised in the persona's voice and rate when its bot joins the call and kept in memory, so an interjection starts within 300 ms of its due time. The language is the line's first sentence's (013's text layer).
- [ ] **AC11** The pod's metrics count lines cut in and interjections said (`docs/architecture.md`), and its log has `cut_in_started`, `cut_in_spoke_over` and `cut_in_yielded` (room quiet before T), with the room and persona.

### Pill indicators
- [ ] **AC12** A persona whose spoken line is waiting in the pod (013's "waiting to speak": queued behind another line or held by a human speaking) shows a comic-style speech bubble icon on its pill, an SVG from `kit/ui.js`, static, with the word "waiting to speak" on a desktop and the meaning in `aria-label`. It replaces 013-AC29's outlined waiting icon, for every persona. The bubble goes when the line starts or is dropped.
- [ ] **AC13** A time-keeper turn may write `drift.json`, `{"level": "low" | "medium" | "high", "reason": "<one line, at most 200 characters>"}`. The persona keeps the latest level and reason. A time-keeper turn that writes none keeps the previous ones; a malformed file is listed in the turn's system message and keeps them too.
- [ ] **AC14** While the time keeper's latest level is `high`, its pill shows a red ⓘ (an SVG from `kit/ui.js`, the danger colour token), with the reason as its tooltip and in `aria-label` ("Off track: <reason>"). `low` and `medium` show nothing. Turning the option off clears the level and the icon. The level and the reason stay out of the transcript, the export and every other persona's prompt.

## Phases

### Phase 1 — The capability, the silent turn, the preset
- Work: the two persona columns; the option and select in the persona sheet; the preamble lines; the silent turn; the preset with its room defaults applied at copy; export and import.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6
- **Data model impact**: `room_personas` gains `time_keeper` (bool, default false) and `cut_in_s` (int, default 0), by a positional in-app migration.
- **DoD**: `go test -p 1 ./internal/store/ ./internal/web/... ./internal/uikit/` green, with tests for the one-per-room rule, the 400 on a bad `cut_in_s`, the preamble with and without the option (golden output, hands from 031's table, a speaker run broken by another speaker and by a 31 s gap), the silent turn (no message, state, timer restarted), the preset copy and the version offer leaving the settings, and an export round trip. `e2e/persona-sheet.spec.js` and `e2e/personas.spec.js` extended: add the Timekeeper, its settings show as AC6; turn the option on another persona and the first loses it. `e2e/triggers.spec.js`, `e2e/design.spec.js` stay green. Until phase 2 a cut-in line is spoken by 013's rules.

### Phase 2 — Cutting in, in the pod
- Work: `cut_in_s` on the line; the escalation in `tts/grill_tts/room.py`; the interjection sets and their cache per bot; metrics and logs.
- **Covers**: AC7, AC8, AC9, AC10, AC11
- **Data model impact**: none
- **DoD**: `uv run pytest` in `tts/` green, with fake-clock tests in `tests/test_room.py`: three interjections at 0, T/3, 2T/3 then the line spoken through a human speaking; quiet before T yields with no further interjection; T = 0 and a non-timer line behave as 013; queue order and the extended queue maximum; no repeated interjection within a line. A Go test for `cut_in_s` on the queued line by cause. `e2e/persona-voice/turn-taking.spec.js` gains, in CI only (real engine): a synthetic human speaking without pause, a time-keeper line with T = 10, the persona's track carries audio at about 0, 3.3 and 6.7 s and from 10 s on, while the human still speaks.

### Phase 3 — Pill indicators
- Work: the speech bubble icon for the waiting state; `drift.json` read at the end of a time-keeper turn, stored on the persona, sent with the persona to the page; the red ⓘ.
- **Covers**: AC12, AC13, AC14
- **Data model impact**: `room_personas` gains `drift_level` (text, default empty) and `drift_reason` (text, default empty), by a positional in-app migration.
- **DoD**: `go test -p 1 ./internal/turn/ ./internal/web/... ./internal/uikit/` green, with tests for a valid, a missing and a malformed `drift.json`, the clear on option off, and no drift in the export or another persona's preamble. `e2e/persona-voice/call-ui.spec.js` checks the bubble and its `aria-label` on a waiting line. A new case in `e2e/personas.spec.js` with the fake daemon: a time-keeper turn writing `high` shows the red ⓘ with its reason, a later `medium` hides it. `e2e/design.spec.js` stays green.

## Data model impact (summary)

`room_personas`: `time_keeper` (bool, default false), `cut_in_s` (int, default 0), `drift_level` and `drift_reason` (text, default empty, not exported). The hand queue is 031's `hands`. Preset room defaults live in code with the preset, not in the template tables. Interjection audio is never stored.

## Open questions

- (none)

## Decisions

From the user:
- A factory persona "Timekeeper / refocus", shipped as a preset that brings its settings (2026-11-07).
- A time-keeper capability gives its turns the hand queue and who speaks and for how long; hands stay hidden from every other persona.
- Trigger: every X minutes, default 5. It may say nothing when the room is on track.
- Cutting in is voice only, never a mic mute. First a few short interjections, then speaking over. The threshold is the only setting: off, 10, 20, 30, 60 s, default 20 (2026-11-07).
- Interjections are synthesised, in the persona's voice, with plenty of variety (2026-11-07).
- A persona that cannot get its spoken line in shows a comic speech bubble on its pill, any persona, not only the time keeper. Persona signals stay on the pill, apart from the humans' hand queue (2026-11-07).
- The time keeper's red ⓘ shows only at a `high` drift level, with no warning step below it (2026-11-07).

Made while drafting (overturnable):
- Only timer turns cut in (AC7): a persona answering an `@mention` should not talk over the room, and the turn needs no new output to say "cut in".
- Three interjections spread over the threshold: fewer reads as a glitch, more as nagging. Fixed, since the user asked for one setting.
- One time keeper per room, like the notes watcher: two would cut in on each other.
- The escalation runs in the pod against its live speaker state, not in the turn: a turn takes seconds, the pod reacts in 300 ms (013-AC22).
- The silent turn (AC3) is for the time keeper only, so no other persona's empty answer starts vanishing.
- The speaker's run ends at another speaker's line or a 30 s gap, longer than a voice paragraph's 20 s close (018), so a pause for breath does not reset the count.
- The preset runs `sonnet` at effort `low`: one turn every 5 minutes reads a short preamble and needs judgment, not depth.
- Preset room defaults live in code next to the preset and apply only at copy, so a new preset version never overrides a room admin's choice.
