# Design rules

These rules apply to the room page, the docs window, the profile, the join page, `/admin` and the daemon's local page. They share one tokens file and one set of component classes. Anything not covered here follows the closest existing component.

## 1. Principles

- **Plain words for a PM.** Name things by what people do, not by how the system works (§8).
- **One control scale.** Every button, input, select and tab uses the same height. Hierarchy comes from fill, not size.
- **Brand from the logo, kept quiet.** Ink chrome, a teal primary and an amber focus or highlight. The flame orange marks the brand and nothing else; it never carries a state, so it can't be confused with danger.
- **No emoji or arrow glyph as a UI icon.** Every icon comes from the monochrome inline SVG set in `kit/ui.js` (stroke, `currentColor`, text size), or there is no icon. Emoji appear only where people put them: persona badges and messages.
- **No native `title` tooltip.** A hint shows in the styled tip (§5). The one `title` left in the room is the document frame's, which names the frame for screen readers.
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
| `--chrome` | `#1e2238` | `#2a3054` | `--chrome-text` `#fff` 15.66 / 12.73; lighter than `--bg` in dark, so the header stands apart |
| `--text` | `#1e2238` | `#e6e7ee` | see above |
| `--text-muted` | `#6b6f80` | `#9a9fb3` | on bg 4.61 / 6.55; on surface 4.99 / 5.77 |
| `--border` | `#dedbd2` | `#3a4062` | decorative |
| `--border-strong` | `#8e8b80` | `#7a809c` | on bg 3.16 / 4.43; on surface 3.41 / 3.90 (≥ 3:1 non-text) |
| `--brand` | `#159f8c` | `#3fc2ad` | on chrome 4.75 / 5.78 |
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
| `--backdrop` | `rgb(30 34 56 / .45)` | `rgb(0 0 0 / .6)` | behind a sheet; the light value barely dims a dark page |

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
| **Button** | One height (`--control-h`), padding `0 var(--sp-4)`, `--fs-sm` (desktop) or `--fs-md` (phone), weight 600, `--r-md`, with an optional 16 px icon. **Primary:** `--primary` fill, white text, one per view. **Secondary:** `--surface` with a `--border-strong` outline and `--text`. **Ghost:** transparent with `--primary-text`, for toolbars. **Danger:** `--danger` fill. A danger action that is not yet armed uses the danger outline, never a pale primary. **Disabled:** whatever its kind, no fill, a `--border` outline (none for ghost) and `--text-muted` text, so it reads at 4.5:1 or more on the page and on a card, in both themes (a faded fill fell under 2:1 in dark mode). The reason sits in text nearby, never in a tooltip alone. |
| **Input / select / textarea** | `--control-h` (textareas: min 2 lines), `--surface` background, `--border-strong` 1px, `--r-md`, `--fs-md` on a phone. The label sits above in `--fs-sm` 600, the hint below in `--text-muted`, the error below in `--danger` with an icon. A select has the same font and height as an input. |
| **File input** | Never shown natively. Use a button or a drop zone; the native input stays hidden. A form that takes attachments shows a paperclip icon button, then a chip naming the files chosen (or "3 images") with an ✕ icon that drops them. |
| **Focus** | `outline: 2px solid var(--accent); outline-offset: 2px` plus a 2px `--dot-ring` shadow on every focusable element, including tabs, chips and menu items. Amber alone is 1.4:1 on the page; the ring carries the 3:1. |
| **Chip / pill** | `--r-pill`, height `--control-h-sm` (desktop) or `--control-h` (phone). **Persona pill:** a 4 px bar in the persona colour inside the pill's left end (a border on a round end draws a crescent), then the badge, the name and a state shape (filled ready, half working, ring offline, dashed queued, a lock icon when not allowed) with a 3:1 ring. A tap on the pill opens the persona's card (state, model, triggers, voice, Mute for me, Edit); a long press or a right-click opens its sheet. Beside it, `▶` (an icon button) asks the persona now, and when the persona speaks in the call a speaker button mutes it for this listener. A `▶` that cannot ask keeps the pointer cursor: a press says why. While the persona speaks in the call, or waits to, the pill shows the speaking icon (filled, waves pulsing; static under reduced motion) or its outline, with the word on a wide screen (spec 013). **Mention chip:** `--primary-bg` with `--primary-text`, `@Name`, and the turn state as a word. |
| **Badge** | `--fs-xs`, `--r-sm`, `0 var(--sp-1)`, neutral (`--border`/`--text`), role (`--accent`/`--text`) or status (`*-bg`/`*` text). At most 2 per row; more become icons, each with its meaning in a tip. In People, every badge is a 20 × 20 square holding a 14 px block SVG, in a fixed column, so a badge lines up from row to row; its meaning shows in the styled tip on hover and stays in its `aria-label`, never in a native `title`. |
| **Drawer header** | From 40rem up, a disclosure triangle first (▾ open, ▸ folded, a clipped CSS square turned in CSS, never a text glyph, `aria-expanded`; its side is the `--fold-order` token), the section's SVG icon, the title, then its actions. The triangle and the actions are 24 px squares to the eye with a 44 px target from an `::after`; the whole header toggles the drawer. |
| **Icons** | One set, in `kit/ui.js`: 24-unit strokes in `currentColor`, 16 px. Markup names one with `data-icon="<name>"`; scripts call `grillIcon(name)`, Preact `grillIconHTML(name)`. close · more (three dots) · send · width · fold (the chat) · chat (also Notes) · docs · people · back · mic · mic-off · down (audio options) · clip (attach) · lock · eye (watches the notes) · check · alert · external (open in a new window) · plus · invite · settings · personas · host · rename · bell · device · pulse (Diagnostics) · profile · download · trash · play (runs a persona, plays a sample) · volume and volume-off (hear or mute a persona for me) · speaking (filled speaker, waves pulsing) and speaking-wait (outlined, dashed waves: waiting to speak) · info (a credit) · menu (three lines: a folded tab bar). The People badges are SVGs too: a gear for the AI host, a pen for transcribed, a screen for transcribed on their device, a mic for mic live, a crossed speaker for muted, headphones for listening only, a crossed bell for deafened. An icon-only control names itself with `aria-label`. |
| **Menu row** | Every `.menu` item (link, button or label) has one style: `--fs-control`, line height 1.2, `--control-h` high, `0 var(--sp-3)` padding, no border, weight 400. An item may lead with a 16 px `.ico` SVG and an 8 px gap. Once one item of a menu has an icon, the other items and the header indent by that column, so every text starts at the same x (±1 px); the header menus (⋯, you, the document's) give every item an icon. The header (`.menu-head`) is a block in `--fs-sm` `--text-muted`, its rule the menu's full width. A menu stays 8 px inside the viewport. Do not style a menu item one by one. |
| **Icon on a text line** | Rows and headers are flex or grid rows with `align-items: center` and a fixed line height (People: 20 px, the badge height). Icons are block SVGs or plain boxes in fixed squares, centred on the text's cap-height middle within ±1 px, at 390 and 1440 px. No `vertical-align` offsets, no text glyph as an icon. |
| **Tabs** | 44 px targets, `--fs-sm` 600, a 3 px `--brand` underline on the current tab, `aria-current="page"`. One bar, `.tabs.sticky`, on /admin, the daemon page, /profile and the persona sheet: it sits at the top of the content column and sticks there, on `--bg` in a page and on `--surface` in a sheet. Under 40rem the tabs take `0 var(--sp-3)` padding with no gap; a bar that overflows folds into one `.tabs-menu` button, the `menu` icon and the current tab's name over the `--brand` underline, that opens the tabs as a list under it (`aria-expanded`, the current tab marked by a 3 px `--brand` bar on its left). A page's own bar (`data-header`: /admin, /profile, the daemon page) folds into an icon-only ☰ at the left edge of the app header instead, before the logo, named after the current tab: the bar leaves no row in the content, where the section heading already names the tab. The header then sticks at the top and keeps one row, its title cut first; the list opens under it. A sheet's bar (the persona sheet, Room settings, the /admin room panel) has no header and keeps its button. On the daemon page a `--danger` dot on the ☰ says a tab has alerts. Picking a tab, Escape or a tap outside closes the list; focus goes back to the button. No sideways scroll on a phone: a cut label is the only hint that more tabs exist. `grillShowTab(nav)` (`kit/ui.js`) folds the bar, or else brings the current tab in whole, never a cut label ("ooms"); every page calls it when its tab changes. |
| **Sheet / dialog** | `--surface` for every sheet, wide ones included, `--r-lg`, `--sh-2`, a `--backdrop`. Close on Escape, on a backdrop tap and with a close icon (44 px) in the top-right. A tabbed sheet (`.sheet.wide`) hangs from a fixed top, `min(10dvh, 48px)`, so its head stays put when a tab changes its height. Full screen under 40rem, with a drag handle for bottom sheets. One primary action at the bottom right (full width on a phone). |
| **Confirm** | A sheet with a title as a question ("Remove Grill?"), one line on the consequence, then [Cancel] and a [Remove] danger button. Reversible actions skip the confirm and show an undo toast. Typing the name is for room deletion only. Never use `confirm()` or `alert()`. |
| **Toast** | Bottom right on desktop, bottom on a phone above the tab bar. `--chrome` background, white text, `--r-md`, 4 s, at most one action ("Undo"), `role=status`. Its wording matches the button: "Rename" → "Renamed". `grillToast(text)` (`kit/ui.js`) shows one in the page's `#toasts` region; the room page's own `toast()` adds the Undo action. |
| **Status pill** | `.status` plus one kind: `ok` (`--success`, filled dot), `busy` (`--primary-text`, half dot), `idle` (`--text-muted`, ring), `off` (`--text-muted`, dashed ring and outline), `failed` (`--danger`, square). A 1 px outline in the same colour, `--r-pill`, `--fs-xs` 600, then the word: Running is `ok`. On the header's chrome, `ok` takes `--success-dot` and the others `--chrome-muted`. A state is never a whole row in red, and never coloured text alone. |
| **Value** | `.value` for a value people copy (a link, a key, a fingerprint, a signer identity): `.value-text` on one line with an ellipsis, the whole value in its `data-tip` and in `data-value`, then a ghost `.copy` icon button named "Copy the …". `.copyable` sets the text in mono. `kit/ui.js` copies `data-value` and toasts "Copied". |
| **List row** | `.list-rows` > `.list-row`: a `.main` block (`.name` 600, then `.meta` in `--text-muted`), then `.actions`: icon buttons (`trash` to remove) and at most one ⋯ (`more`) that opens a `.menu.row-menu` popover through `popovertarget`. `kit/ui.js` hangs the menu under its button, right edges aligned. 44 px rows with a `--border` rule. In columns: wrap the list in `.cols` with `--cols` (a grid template, the last column `var(--control-h)` for the ⋯), put a `.list-row.head` naming the columns before it, and the row's values in `.cells` > `.cell`. One primary action per view; a row's actions go in its ⋯, at most one inline secondary. |
| **Settings group** | One `form.settings-group` per topic: a grid of label, `.control` and a full-width `.hint`, one column under 40rem. Its `.settings-foot` sticks to the bottom and holds the status line, "N unsaved changes" and the only Save, disabled while nothing changed. A toggle outside a form applies at once and says so with `grillToast`; it never waits for a Save. |
| **Status strip** | One line under the header: the highest-priority state (running, paused, reconnecting) plus "+2 more". It never stacks bars. An error strip (`role=alert`, `--danger-bg`, `--danger` text) leads with the `alert` icon and ends with a ✕ icon button named "Dismiss"; it goes when the state it reports is gone (the voice error on a live mic). |
| **People row** | One line: the audio dot, a sigil column, the name, the badges, then `⋯`. The sigil column is one glyph wide (1em, in the name's font, so it shares the name's baseline), right-aligned and empty without a sigil, so every name starts at the same x; `@` marks a room admin, as in IRC. You are your name in bold and `--primary-text`, no badge. The name truncates first, down to 5ch; only then do the badges give way. When someone was last seen, when they joined and whether they can ask the AI are in the details card, not in the row. |
| **Empty state** | One component, `.empty`, wherever a list or section has nothing: a dashed `--border-strong` box, `--text-muted`, `--fs-sm`, centred, `var(--sp-3) var(--sp-4)` padding. A sentence that says what goes here, and at most one action button. "No documents yet. Drop files here or press +." on a desktop; a phone, which cannot drop, gets the sentence and an Add documents button. Never a blank area, never a bare hint line. |
| **Hover card / tip** | One tip element per page, `#person-tip`, drawn by `kit/ui.js`; no page adds its own. A popover in `--surface` with a `--border` outline, `--sh-2`, `--fs-sm`: the card at `--r-lg` and `var(--sp-2) var(--sp-3)`, the one-line tip at `--r-md`, `--fs-xs` and `var(--sp-1) var(--sp-2)`. It opens after 300 ms of hover, at once on keyboard focus (the card on a tap too), sits below its anchor or above it at the bottom edge, shifts left at the right edge, and never covers a row's `⋯`. The pointer can move onto it; leaving it or Escape closes it. A control's tip (a button or a link) lets the pointer through instead: under a row, it would otherwise take the next row's click. Any element gets the tip from a `data-tip` attribute (`kit/ui.js`, every page); the People list and chat author names get the card. Never a native `title` tooltip. |
| **Date** | One format, from `kit/ui.js`: `grillDate` writes a date with its time ("Oct 14, 2026, 8:12 PM" in the reader's locale), `grillTime` the time of a line ("08:12 PM"), `grillAgo` a coarse age for lists ("5 min ago"; in words for a card, "5 minutes ago"), whose tip is the `grillDate`. No page formats a date itself. |
| **Table** | `--fs-sm`, rows at least 44 px, a `--border` bottom rule, numbers right-aligned, relative dates with the absolute date in the tip, row actions in `⋯`. Columns size to their content; long text ends in an ellipsis and its full value shows in a styled tip, never a native `title`. The header row sticks. A table scrolls sideways only when it cannot fit. Under 40rem each row becomes a card. |
| **List** | Rows at least 44 px, primary text then meta in `--text-muted`, one trailing `⋯`. The side panel's People and Documents rows are both `--control-h` high. A list with actions per row lines them up in columns: Room settings › Admins puts the name, the role, the admin button and Rename at the same x on every row. |
| **Fieldset** | No native frame: the legend reads as a `--fs-sm` 600 heading over its fields. |
| **Header (all UIs)** | `--chrome` background, 56 px high, logo + title on the left, actions on the right, all at `--control-h`. Same on the room page, the docs window (whose room name is a link back, underlined on hover only), `/admin`, the profile and the daemon page. |

## 6. Layout

| surface | width |
|---|---|
| room | full width. Sidebar 16rem, chat min 22rem, viewer flexible, annotations a 20rem toggle panel |
| profile | `.page-wide`, left-aligned, as `/admin`'s data views (user, 2026-10-18: 72rem left a wide screen half empty). The title and the identity strip, then sticky tabs (Rooms, Starters, Skills, API tokens, Account); the rooms in a card grid `repeat(auto-fill, minmax(18rem, 1fr))`, the lists full width. Prose keeps its measure: hints, the status line, the confirm card and empty states (but Starters') at 70ch, the Account facts at 48rem |
| `/admin`, daemon page | the tabs, then the view, in one column: `.page` (`max-width: 72rem`) for forms and prose, `.page-wide` (the full width less 2 × 16 px) for data views (/admin Rooms, Profiles, Libraries, Pronunciation, Sessions, Audit and a room's tabs; the daemon's Rooms and Activity). A data list is `.cols` rows: under 40rem each row is a card, its name and ⋯ on the first line, its cells on one muted line, each after its `data-label` |
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
- Every target is at least 44 × 44 px of its own box, with at least 8 px between targets. The 24 px drawer-header actions grow to 44 px; a row of People is 44 px high.
- An open document's name gets its own row under 40rem; its tools sit on the next one.
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

- **Source:** `internal/uikit/kit/tokens.css` holds the custom properties, light and dark, and the component classes (`.btn`, `.btn-primary`, `.btn-secondary`, `.btn-ghost`, `.btn-danger`, `.btn-icon`, `.field`, `.chip`, `.badge`, `.tabs`, `.page`, `.page-wide`, `.sheet`, `.menu`, `.toast`, `.empty`, `.table`, `.status`, `.value`, `.list-row`, `.cols`, `.meter`, `details.disclosure`, `.settings-group`). It is the reference for every value in §2 to §4.
- **No build step:** the room server and the daemon both import `internal/uikit`, which embeds the file and serves it at `/static/kit/tokens.css`. `go:embed` cannot reach a parent directory, hence one Go package for both binaries.
- **Loading:** each page links it before its own stylesheet. The vanilla room page and the Preact pages (htm, no build) use the same class names. A page stylesheet keeps layout only and holds no colour literal: `TestPageCSSHasNoColourLiterals` fails on one.
- **Contrast:** `TestTokenContrast` computes the WCAG ratio of each pair in §2, light and dark, from the file itself.
- **Theme switch:** `@media (prefers-color-scheme: dark)` overrides the tokens. The document frame follows the same media query, so a document always matches the page around it.
