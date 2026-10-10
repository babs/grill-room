# Dark mode for the room UI

## Goal

Participants who work at night want a dark room. The room page, the participant list and the
transcript switch to a dark palette.

## Approach

- The theme follows the system setting through `prefers-color-scheme`.
- A toggle in the header lets each participant force light or dark.
- Dark colours live in one extra style sheet loaded after the main one.

## Out of scope

- The daemon's local admin page.
- Printing.

## Rollout

Ship it to every room at once in the next release.
