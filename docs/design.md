# Design rules

These rules apply to the room page, the docs window, the profile, the join page, `/admin` and the daemon's local page. They share one tokens file and one set of component classes. Anything not covered here follows the closest existing component.

## 1. Principles

- **Plain words for a PM.** Name things by what people do, not by how the system works (§8).
- **One control scale.** Every button, input, select and tab uses the same height. Hierarchy comes from fill, not size.
- **Brand from the logo, kept quiet.** Ink chrome, a teal primary and an amber focus or highlight. The flame orange marks the brand and nothing else; it never carries a state, so it can't be confused with danger.
- **State is never colour alone.** A dot comes with a shape, a label or a tooltip that also works on touch.

## 2. Colour

The palette is the one the room page and `/admin` already use, harmonized. The logo (teal `#159F8C`, ink `#1E2238`, amber `#FFC23D`, flame `#F4512C`) confirms the accent. Ratios are WCAG 2.x contrast, computed.

### Existing value → token

| existing value(s) | where now | token | change |
|---|---|---|---|
| `#f7f6f2` (`--paper`), `#fbf8f3` (daemon) | page backgrounds | `--bg` `#f7f6f2` | merge |
| `#fff` | cards, inputs, pills | `--surface` `#ffffff` | none |
| `#1e2238` (`--ink`), `#222` (daemon) | text, room header | `--text` `#1e2238` and `--chrome` `#1e2238` | merge |
| `#6b6f80` (`--muted`), `#666`, `#444` (daemon) | hints, meta | `--text-muted` `#6b6f80` | merge |
| `#c9cbd6`, `#cfd2dc` | muted text on the header | `--chrome-muted` `#c9cbd6` | merge |
| `#dedbd2` (`--line`), `#d8d2c8` (daemon) | dividers | `--border` `#dedbd2` | merge, decorative only |
| none | input and button outlines | `--border-strong` `#8e8b80` | **new**: `#dedbd2` is 1.28:1 and fails the 3:1 for non-text |
| `#159f8c` (`--teal`) | buttons, active tab, links | `--brand` `#159f8c` | kept for borders, the active tab bar, the selected ring and large type |
| none (derived from `#159f8c`) | primary buttons and teal text | `--primary` `#0f7d6e` | **AA fix**: white on `#159f8c` is 3.30:1 and fails; on `#0f7d6e` it is 5.03:1 |
| `#e3f4f1` | notice, mention chip | `--primary-bg` `#eaf6f4` | **AA fix**: `--primary-text` on `#e3f4f1` is 4.42:1, on `#eaf6f4` 4.54:1 |
| `#ffc23d` (`--amber`), `#f4b942` (daemon) | focus ring, banners, host badge | `--accent` `#ffc23d` | merge |
| `#fff4d6`, `#fff4e5` | warn and approval backgrounds | `--warning-bg` `#fff4d6` | merge |
| `#b06000`, `#8a5a00` | capped and notice text | `--warning` `#8a5a00` | **AA fix**: `#b06000` on bg is 4.30:1 |
| `#b3261e` (`--bad`) | danger buttons, errors | `--danger` `#b3261e` | none |
| `#fbe4e2` | voice error background | `--danger-bg` `#fbe4e2` | none |
| `#d93025` | offline LED | `--danger` | merge (3.28:1 on ink was too weak) |
| `#1e7b34` (daemon `--good`) | ok text | `--success` `#1e7b34` | none |
| `#34c759` | ready LED | `--success-dot` `#34c759` | dots only, always with a 1px `--chrome` ring on white |
| `#e8f0fe`, `#8ab4f8` | queue strip, queued LED | `--info-bg` `#e8f0fe` and `--info-dot` `#8ab4f8` | kept |
| `#e8743b` | "reopened" badge | `--flame-soft` `#e8743b` with **ink** text | **AA fix**: white is 3.0:1, ink is 5.21:1 |
| `#c4c7d0` | pending LED | `--neutral-dot` `#c4c7d0` | none |
| `#efefef` | native admin buttons | none | replaced by `.btn-secondary` |
| `#F4512C` (logo) | logo, default persona colour | `--flame` `#f4512c` | brand only, never for UI state |

### Light and dark tokens

| token | light | dark | key pair, ratio (light / dark) |
|---|---|---|---|
| `--bg` | `#f7f6f2` | `#161a2c` | text 14.48 / 13.98 |
| `--surface` | `#ffffff` | `#1f2440` | text 15.66 / 12.30 |
| `--chrome` | `#1e2238` | `#1f2440` | `--chrome-text` `#fff` 15.66 / 12.30 |
| `--text` | `#1e2238` | `#e6e7ee` | see above |
| `--text-muted` | `#6b6f80` | `#9a9fb3` | on bg 4.61 / 6.55; on surface 4.99 / 5.77 |
| `--border` | `#dedbd2` | `#3a4062` | decorative |
| `--border-strong` | `#8e8b80` | `#7a809c` | on bg 3.16 / 4.43; on surface 3.41 / 3.90 (≥ 3:1 non-text) |
| `--brand` | `#159f8c` | `#3fc2ad` | on chrome 4.75 / 6.88 |
| `--primary` | `#0f7d6e` | `#3fc2ad` | button text: white 5.03 / `#161a2c` 7.82 |
| `--primary-text` | `#0f7d6e` | `#3fc2ad` | on bg 4.65 / 7.82 |
| `--secondary` | `--surface` + `--border-strong` outline | same | text 15.66 / 12.30 |
| `--accent` | `#ffc23d` | `#ffc23d` | ink on accent 9.72; accent on dark bg 10.70 |
| `--success` | `#1e7b34` | `#4fd18a` | on surface 5.33 / 7.81 |
| `--warning` | `#8a5a00` | `#ffd27a` | on bg 5.48, on `--warning-bg` 5.41 / 10.65 |
| `--danger` | `#b3261e` | `#ff8a80` | white on danger 6.54; on dark surface 6.64; `#161a2c` on dark danger 7.55 |
| `--warning-bg`, `--danger-bg`, `--primary-bg`, `--info-bg` | `#fff4d6`, `#fbe4e2`, `#eaf6f4`, `#e8f0fe` | 15 % tints of the dark token on `--surface` | ink on each ≥ 12.9 |
| `--on-accent`, `--on-flame-soft` | `#1e2238` | `#1e2238` | ink on amber 9.72, on `--flame-soft` 5.21 |
| `--dot-ring` | `#1e2238` | `#e6e7ee` | the ring of status dots and focus, ≥ 3:1 on bg and surface |
| `--highlight` | `#fff8e1` | amber at 12 % | the background of a line that mentions you |

- **Persona colours** are user data. Use them only as the pill's left stripe and avatar ring, never behind text.
- **Dark mode** follows `prefers-color-scheme`, and so does the document frame: a document is never darker or lighter than the UI around it.

## 3. Typography

- **Font stack:** `system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif`. Mono: `ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace`. No web fonts.
- **Scale:** 1.125 ratio, rem-based.

| token | size | use |
|---|---|---|
| `--fs-xs` | 0.75rem | badges, timestamps |
| `--fs-sm` | 0.875rem | hints, meta, table cells, controls on desktop |
| `--fs-md` | 1rem | body, messages, controls on a phone (prevents iOS zoom) |
| `--fs-lg` | 1.125rem | section titles, room name |
| `--fs-xl` | 1.375rem | page titles (profile, admin) |

- **Weights:** 400 and 600 only.
- **Line height:** 1.45 for body text, 1.2 for headings.
- **Case:** sentence case everywhere. No all-caps labels: the sidebar's uppercase "PARTICIPANTS" and "DOCUMENTS" become sentence case at `--fs-sm` 600.
- **Mono:** only for things people copy, such as links and code. Never for ids people don't need.

## 4. Spacing, size, radius, shadow, layers

| group | tokens |
|---|---|
| spacing (4 px base) | `--sp-1` 4 · `--sp-2` 8 · `--sp-3` 12 · `--sp-4` 16 · `--sp-5` 24 · `--sp-6` 32 · `--sp-7` 48 |
| control height | `--control-h` 36px desktop, 44px under 40rem · `--control-h-sm` 28px, desktop only and never the only target for an action |
| icon button | square, `--control-h` |
| radius | `--r-sm` 4 (badges) · `--r-md` 8 (controls, cards) · `--r-lg` 12 (sheets, dialogs) · `--r-pill` 999 (chips) |
| shadow | `--sh-1` `0 1px 2px rgb(30 34 56 / .08)` (cards) · `--sh-2` `0 4px 16px rgb(30 34 56 / .16)` (menus, sheets) |
| z-index | `--z-sticky` 10 · `--z-dropdown` 20 · `--z-sheet` 30 · `--z-dialog` 40 · `--z-toast` 50 |

- **Gaps:** `--sp-2` inside a group, `--sp-5` between groups.
- **Vertical rhythm** in a form: `--sp-3`.

## 5. Components

| component | rules |
|---|---|
| **Button** | One height (`--control-h`), padding `0 var(--sp-4)`, `--fs-sm` (desktop) or `--fs-md` (phone), weight 600, `--r-md`, with an optional 16 px icon. **Primary:** `--primary` fill, white text, one per view. **Secondary:** `--surface` with a `--border-strong` outline and `--text`. **Ghost:** transparent with `--primary-text`, for toolbars. **Danger:** `--danger` fill. A danger action that is not yet armed uses the danger outline, never a pale primary. **Disabled:** 0.45 opacity with the reason in text nearby, never in a tooltip alone. |
| **Input / select / textarea** | `--control-h` (textareas: min 2 lines), `--surface` background, `--border-strong` 1px, `--r-md`, `--fs-md` on a phone. The label sits above in `--fs-sm` 600, the hint below in `--text-muted`, the error below in `--danger` with an icon. A select has the same font and height as an input. |
| **File input** | Never shown natively. Use a button or a drop zone; the native input stays hidden. |
| **Focus** | `outline: 2px solid var(--accent); outline-offset: 2px` plus a 2px `--dot-ring` shadow on every focusable element, including tabs, chips and menu items. Amber alone is 1.4:1 on the page; the ring carries the 3:1. |
| **Chip / pill** | `--r-pill`, height `--control-h-sm` (desktop) or `--control-h` (phone). **Persona pill:** a 4 px left stripe in the persona colour, then the badge, the name and a state glyph (● ready, ◐ working, ○ offline, ⧗ queued, 🔒 not allowed) with a 3:1 ring. A tap asks the persona; `▾` opens its menu. **Mention chip:** `--primary-bg` with `--primary-text`, `@Name`, and the turn state as a word. |
| **Badge** | `--fs-xs`, `--r-sm`, `0 var(--sp-1)`, neutral (`--border`/`--text`), role (`--accent`/`--text`) or status (`*-bg`/`*` text). At most 2 per row; more become icons with tooltips. In People, every badge is a 20 × 20 icon square in a fixed column, so a badge lines up from row to row. |
| **Drawer header** | From 40rem up, a disclosure triangle first (▾ open, ▸ folded, turned in CSS, `aria-expanded`), the section's icon, the title, then its actions. The triangle and the actions are 24 px squares to the eye with a 44 px target from an `::after`; the whole header toggles the drawer. |
| **Icons** | ✕ close · ⋯ more actions · ▾ ▸ open, folded · ↔ text width · ⇥ fold the chat · 💬 chat, notes · 📄 documents · 👥 people · ● you · ⚙ AI host · ★ admin · ✦ can ask the AI · ✍ transcribed · 🎙 mic live · 🔇 muted · 🎧 listening only · 🔕 deafened · + add · external link (an SVG square with an arrow out of its top-right corner, `currentColor`) open in a new window. A copy glyph means copy, nothing else. |
| **Tabs** | 44 px targets, `--fs-sm` 600, a 3 px `--brand` underline on the current tab, `aria-current="page"`. They scroll sideways under 40rem (no wrapping) with a fade edge, and stay sticky in long pages. |
| **Sheet / dialog** | `--surface`, `--r-lg`, `--sh-2`, a backdrop `rgb(30 34 56 / .45)`. Close on Escape, on a backdrop tap and with an ✕ (44 px) in the top-right. Full screen under 40rem, with a drag handle for bottom sheets. One primary action at the bottom right (full width on a phone). |
| **Confirm** | A sheet with a title as a question ("Remove Grill?"), one line on the consequence, then [Cancel] and a [Remove] danger button. Reversible actions skip the confirm and show an undo toast. Typing the name is for room deletion only. Never use `confirm()` or `alert()`. |
| **Toast** | Bottom right on desktop, bottom on a phone above the tab bar. `--chrome` background, white text, `--r-md`, 4 s, at most one action ("Undo"), `role=status`. Its wording matches the button: "Rename" → "Renamed". |
| **Status strip** | One line under the header: the highest-priority state (running, paused, reconnecting) plus "+2 more". It never stacks bars. |
| **Empty state** | A sentence that says what goes here and one action button. "No documents yet. Drop files here or press +." Never a blank area. |
| **Table** | `--fs-sm`, rows at least 44 px, a `--border` bottom rule, numbers right-aligned, relative dates with an absolute-date tooltip, row actions in `⋯`. Under 40rem each row becomes a card. |
| **List** | Rows at least 44 px, primary text then meta in `--text-muted`, one trailing `⋯`. |
| **Header (all UIs)** | `--chrome` background, 56 px high, logo + title on the left, actions on the right, all at `--control-h`. Same on the room page, `/admin`, the profile and the daemon page. |

## 6. Layout

| surface | width |
|---|---|
| room | full width. Sidebar 16rem, chat min 22rem, viewer flexible, annotations a 20rem toggle panel |
| profile | `max-width: 64rem`, centred, a card grid `repeat(auto-fill, minmax(18rem, 1fr))` |
| `/admin`, daemon page | `max-width: 72rem` for forms and cards; tables may use the full width |
| join, sign-in | `max-width: 28rem` card |
| reading text (hints, docs) | `max-width: 70ch` |

**Breakpoints:**

| range | layout |
|---|---|
| < 40rem | phone: one column, a bottom tab bar in the room, 44 px controls, sheets full screen |
| 40–64rem | tablet: sidebar collapsible, annotations as a sheet |
| ≥ 64rem | desktop |
| ≥ 100rem | annotations may stay open next to the chat |

**Phone rules:**
- Every target is at least 44 × 44 px with at least 8 px between targets.
- No hover-only actions: long-press or `⋯` instead.
- Respect `env(safe-area-inset-*)`.
- Input font is at least 16 px.

## 7. Motion

- Use 150 ms ease-out, only in response to an action: opening a sheet, a toast entering, the upload progress.
- Respect `prefers-reduced-motion`: no transitions, and the speaking pulse becomes a static ring.

## 8. Voice and wording

- Talk to "you".
- Use sentence case, with no full stop on buttons.
- An error says what happened and what to do: "This file type isn't supported. Use PDF, Word, slides, images or Markdown."
- An action keeps its name all through a flow.

| now | say instead |
|---|---|
| Chain 0/25, chain budget, re-arm, suspend limit | auto turns 3/25 · auto-turn limit · Resume auto turns · Remove the limit |
| host, holds the agent(s), Host the agent | AI host · Alice runs the AI for this room · Run the AI on my computer |
| can ask (checkbox), agent button | Can ask the AI |
| turn, turn running/queued | answer · Grill is answering… / waiting |
| persona | AI persona in headings; the persona's name elsewhere |
| synthesis | Grill's answer |
| effort medium, sonnet, +model 6 % | "sonnet · effort medium" in the Personas list (009-AC1); the model mix in a tooltip only |
| Every N rounds | Speaks up every N answers |
| Self-call | Can follow up on its own |
| Autocompact (k tokens) | Memory size, on the persona sheet's Model tab |
| New session (with handoff) | Start fresh (keep a summary) |
| Publish as a template | Save to the library |
| Transcription unavailable / 2 voices transcribed | (audio menu) Voice-to-text is off · 2 voices are being written down |
| Deafen | Mute the room |
| Manage (member view) | Room settings |
| Volume, revisions, attachments (bytes) | Storage: 3.9 KB of 200 MB |
| Upload a document | Add documents |
| Download original | Download |
| Recovery link | Use on another device |
| Glossary | Words to recognise |
| Egress allowlist | Websites the AI may reach |
| Denied requests | Blocked websites |
| Sandbox CPUs, egress proxy memory | (Advanced) CPU and memory limits |
| daemon, supervisor, instance | AI host app · this computer · server |

## 9. One shared CSS file

- **Source:** `internal/uikit/kit/tokens.css` holds the custom properties, light and dark, and the component classes (`.btn`, `.btn-primary`, `.btn-secondary`, `.btn-ghost`, `.btn-danger`, `.btn-icon`, `.field`, `.chip`, `.badge`, `.tabs`, `.sheet`, `.menu`, `.toast`, `.empty`, `.table`). It is the reference for every value in §2 to §4.
- **No build step:** the room server and the daemon both import `internal/uikit`, which embeds the file and serves it at `/static/kit/tokens.css`. `go:embed` cannot reach a parent directory, hence one Go package for both binaries.
- **Loading:** each page links it before its own stylesheet. The vanilla room page and the Preact pages (htm, no build) use the same class names. A page stylesheet keeps layout only and holds no colour literal: `TestPageCSSHasNoColourLiterals` fails on one.
- **Contrast:** `TestTokenContrast` computes the WCAG ratio of each pair in §2, light and dark, from the file itself.
- **Theme switch:** `@media (prefers-color-scheme: dark)` overrides the tokens. The document frame follows the same media query, so a document always matches the page around it.
