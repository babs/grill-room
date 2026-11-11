# 034 — A persona bar that fits

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-11
**Context**: reworks the header's persona zone: 005-AC32 (header zones), 013-AC39 (pill and ▶), 032-AC12 and AC14 (speech bubble, red ⓘ), 030-AC14 (room name width at 390 px).

## Problem

A persona pill is about 200 px wide. With five personas the bar already overflows at 1280 px, and it scrolls sideways with nothing to show that more personas sit past the edge: at 1024 px the fifth is cut mid-pill, at 390 px two and a half show. A persona that answers, waits to speak or flags a drift off screen goes unseen. The room's name shrinks to "D…" at 768 px.

## Solution

When the full pills do not all fit, every pill turns compact: the colour stripe, the state dot, the badge, its signals (notes, speech bubble, red ⓘ, speaking), ▶ and the mute button. The name and the state word move to the tooltip and the card. A compact pill takes about half the width. If the compact pills still do not fit, the bar keeps scrolling, with a fade at each edge that hides pills and a round "+N" button there. That button counts the pills hidden on its side, carries the strongest signal among them, and scrolls toward them.

## Acceptance criteria

- [x] **AC1** While every full pill fits the persona bar, the bar is unchanged (013-AC39). When any would not fit, every pill turns compact together, never a mix: the colour stripe, the state dot, the badge, the notes, speech-bubble, drift and speaking icons when they apply, ▶ and, when the persona speaks in the call, the mute button. The name and the state word are hidden. The pill's tooltip and `aria-label` keep the name and the state (005-AC35), and a click still opens the card.
- [x] **AC2** The mode follows the room: a window resized, a persona added, removed or renamed, the agent status shown or hidden. Resizing across the threshold switches the mode once per crossing, with no back-and-forth between frames.
- [x] **AC3** A compact pill with no signal is at most 112 px wide, 156 px with the mute button. ▶ and mute keep their `--control-h` target (013-AC39), each "+N" is a 44 px target, and the focus ring stays inside the bar.
- [x] **AC4** When the compact pills overflow the bar, each edge that hides at least one pill, fully or partly, shows a fade and a round 44 px button "+N", N the count of pills hidden on that side, `aria-label` "N more personas, scroll right" (or "left"). A press scrolls the bar toward that side by its visible width less the two 44 px edge bands. An edge with nothing hidden has neither.
- [x] **AC5** A "+N" button carries the strongest signal among the pills it hides, as a dot with an icon on its corner: the red ⓘ when one rates the drift high (032-AC14), else the speech bubble when one waits to speak (032-AC12), else an activity dot when one answers or speaks. Its `aria-label` adds the signal and the persona's name ("… Timekeeper is off track"). The order of the pills never changes.
- [x] **AC6** The room's name keeps at least 6rem of width, or its whole text when shorter, at every window width, before the persona bar takes the rest (030-AC14 extended past 40rem).
- [x] **AC7** The header warning, the agent status and the auto-turn count keep their place beside the bar, compact or not, scrolled or not (the "header warning stays in sight past many personas" case).

## Phases

### Phase 1 — Compact pills and edge buttons
- Work: the fit measure and the compact class on the bar in `room.js`; the compact rules in `app.css`; the two edge buttons, their counts, signal and scroll; the room name's minimum width.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7
- **Data model impact**: none
- **DoD**: `go test ./internal/uikit/` green. `e2e/design.spec.js` extended, with 9 personas, at 390, 768, 1024, 1280 and 1600 px: full or compact as AC1 says, never a mix; compact widths and 44 px targets (AC3); the edge buttons' presence, counts and `aria-label` at the start, the middle and the end of the scroll, a press scrolling toward its side (AC4); a drift and a bubble set on a hidden pill showing on the right "+N" with the red ⓘ winning (AC5); the room name at least 6rem wide (AC6); `overlaps` empty and the existing warning case green (AC7); a resize back and forth across the threshold switching once each way (AC2). `e2e/ui-guards.spec.js`, `e2e/layout-guards.spec.js`, `e2e/mobile.spec.js`, `e2e/approvals.spec.js` stay green.

## Out of scope

- A menu listing the hidden personas (the user chose the scrolling bar).
- Moving active personas to the front: the order stays put.
- The persona card, the Personas sheet and the triggers' display.

## Open questions

- (none)

## Decisions

From the user (2026-11-11):
- Compact pills and a scrolling bar, no overflow menu.
- A hidden persona's signal shows on the edge's "+N", the pills never reorder.
- ▶ stays on the compact pill: running a persona is the bar's main action, one click.

Made while drafting (overturnable):
- All pills turn compact together: a mix of widths reads as two kinds of persona.
- The edge signal ranks drift, then speech bubble, then activity: the most urgent first, one dot only.
- A press on "+N" scrolls by a page, not to the signalling pill: one rule, predictable.
