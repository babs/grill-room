# 002 — Room and agent: personas, skills, profiles, richer chat

**Status**: shipped
**Requested by**: babs
**Date**: 2026-10-11
**Context**: spec 001 (shipped as PoC), `INTENT.md`, `docs/architecture.md`, `docs/agent-workspace.md`, spike S5

## Problem

The PoC has one agent per room with one hidden role, one model set by an env var on the host, and no skills. Users want several agents with distinct jobs in the same grill, for example one that challenges and one that takes notes. They want to steer each agent's model and effort, and give agents reusable skills. Today a person exists only inside one room, so nothing personal follows them across rooms. The chat is plain text that cannot be fixed after the fact, including voice lines that the STT got wrong. Every turn re-downloads the whole room workspace, and the sandbox has no Python, Office libraries or network, so most useful skills cannot run.

## Solution

A room can hold several personas, for example "Grill" and "Secretary". Each one has its own Claude session, model, effort, persona text, skills and trigger. Personas are created from a shared library of templates, and anyone in the room can add or edit them between turns. Each persona can run on demand and/or every N rounds, and can call another persona with `@name`. Chains of agent turns stop after a budget that a human can re-arm or suspend. The host's daemon runs every persona, within a model and effort cap that the host controls. A request above the cap runs clamped until the host approves it. Each agent sees who else is in the room: the other personas and what they did, and the humans present.

A person gets a lightweight profile on first join. It follows them across rooms and holds their personal skills and persona templates. They can recover it with a secret link or bind it to an email with a magic link. The chat renders Markdown. People can edit and soft-delete their messages, and correct their own voice lines; the agent is told about edits. Members can rename the room and create invites.

The sandbox gets Python, uv, Office libraries and common CLI tools. It reaches the network only through an allowlist proxy that the host manages, and never reaches Anthropic directly. Turns fetch only the workspace files that changed.

## Scope

- Profiles: an anonymous profile created at first join, with a long-lived cookie and a secret recovery link. An optional email magic link binds a profile to an address. A profile holds a display name, a rooms list, personal skills and personal persona templates.
- Rooms: any member renames, any member creates invites and revokes their own; the admin can do both on every room.
- Chat: server-rendered, sanitised Markdown. Edit with kept versions, soft delete, correction of your own voice lines, and edits flagged to the agent.
- Persona template library: shipped presets (Grill, Secretary, Rodin) and templates published by profiles, with immutable versions, author, and removal by the admin.
- Room personas: instances of templates with per-room overrides, editable between turns: text, model, effort, triggers, skills, self-call.
- One Claude session per room persona, all run by the room's host daemon, one turn at a time per room with a queue.
- Triggers: a button per persona, every N rounds per persona, and `@persona` calls from humans and personas. Self-call is off by default.
- Chain budget: default 25 links, re-armable, suspendable, with an unlimited mode behind a permanent warning.
- Host cap: maximum model and effort. Above-cap requests run clamped, the host gets an approval banner, and the host can turn on an "approve everything" mode.
- Presence preamble in every prompt, from a single template.
- Skills: the Claude Code skill format, a shared versioned library, and personal skills in the profile. Each persona has its active set, injected as a plugin dir, and rooms pin versions. A one-time risk disclaimer.
- Sandbox image: python3, uv, a venv with openpyxl, pandas, python-docx, python-pptx, pypdf and matplotlib, plus pandoc, node, jq, yq, ripgrep, git and curl.
- Sandbox egress through a squid sidecar managed by the daemon: CONNECT 443 to an allowlist (PyPI, GitHub read hosts, npm by default), Anthropic hosts denied, denied requests logged.
- Daemon page: minimal controls for the host cap, the approval mode and the egress allowlist. The full settings and monitoring UI is spec 003.
- Differential workspace sync: a manifest, a per-room daemon cache, and a tar of only the missing or changed paths, with a full fallback.
- Mobile layout and live reconnection, already fixed in d4e5068, now held by ACs.

## Out of scope

- Admin UI, export/import, daemon settings and monitoring UI, Piper scenarios: spec 003.
- OIDC accounts and passkeys.
- A host per persona; all personas run on the room host.
- Retention and archiving.
- LibreOffice in the sandbox.
- TLS interception in the egress proxy.
- Reactions, threads in chat, mentions of humans (only `@persona` triggers).

## Acceptance criteria

### Profiles and identity
- [x] **AC1** A guest joining a room without a profile gets one: a long-lived HttpOnly cookie, and a recovery link shown once with a copy button.
- [x] **AC2** Joining another room with the same browser attaches the new participant to the same profile, and the profile lists both rooms.
- [x] **AC3** Opening the recovery link on another device signs that device into the profile.
- [x] **AC4** With SMTP configured, a profile can bind an email. A magic link is single use, expires after 15 minutes, and is rate-limited per address and per IP. Signing in with it from another device opens the profile. Without SMTP the option is hidden.
- [x] **AC5** Participants never see another profile's email. The admin can.

### Rooms
- [x] **AC6** Any member can rename the room, and a system line in the chat records who renamed it from what to what.
- [x] **AC7** Any member can create an invite link. A member can revoke the invites they created, and the admin can revoke any.

### Chat
- [x] **AC8** A message renders Markdown (emphasis, lists, code, links, tables). Raw HTML and script content are not rendered, and links get `rel="noopener noreferrer"`.
- [x] **AC9** An author can edit their message. It shows "(edited)", and earlier versions stay readable.
- [x] **AC10** The speaker can correct their own voice line. Others cannot.
- [x] **AC11** An author can delete their message. It shows "message deleted", and the content stays in the store for the agent and for export.
- [x] **AC12** A message edited or deleted after an agent turn has seen it appears in that persona's next prompt as edited (before → after) or deleted.

### Personas
- [x] **AC13** The template library ships "Grill", "Secretary" and "Rodin" (user, 2026-10-18; "Default" renamed "Grill", user, 2026-10-22). A profile can publish a template, and each version is immutable with its author. The admin can remove a template.
- [x] **AC14** Any member can add a persona to a room from a template. The room copy keeps its own overrides, and a later template version does not change it. The room is offered the update instead.
- [x] **AC15** Any member can edit a room persona's text, model, effort, triggers, skills and self-call. A change made during a turn applies to the next turn, and every change is recorded in the chat.
- [x] **AC16** Each room persona has its own Claude session. Persona A's turn never resumes persona B's session, and a takeover by another host resumes each persona's own session.
- [x] **AC17** Only one turn runs at a time in a room. A press while a turn runs is queued and shown as queued.
- [x] **AC18** Every synthesis message shows the persona and the model and effort actually used.
- [x] **AC42** The default persona is always identified by its own name and badge ("Grill" unless renamed), never as an anonymous "Agent": synthesis messages, revisions, annotations and replies all carry it. Agent output from before 002 is attributed to the migrated Default persona.

### Host cap and approvals
- [x] **AC19** A persona asking for more than the host cap runs at the cap. Its synthesis shows "requested X → ran Y (host cap)", and the host sees an approval banner.
- [x] **AC20** When the host approves, that persona in that room runs as requested from the next turn, and the daemon's global cap is unchanged. When the host refuses, it keeps running at the cap.
- [x] **AC21** With "approve everything" on, a new persona and every persona change wait for the host's approval before taking effect.

### Triggers and chains
- [x] **AC22** Each persona has its own button in the room. Pressing it runs that persona, and "every N rounds" makes it run after N completed agent turns of any persona.
- [x] **AC23** A human message mentioning `@persona` queues a turn for that persona.
- [x] **AC24** A persona's synthesis mentioning `@other` queues a turn for `other`. Mentioning itself does nothing unless that persona has self-call on.
- [x] **AC25** Agent-triggered turns (every N rounds and `@persona` from a persona) count as chain links. At the budget (default 25, configurable per room), the chain pauses with a banner offering "re-arm" and "suspend limit".
- [x] **AC26** A human pressing a persona button, the re-arm button, or a human message mentioning a persona resets the chain count. Voice lines and plain text do not.
- [x] **AC27** Suspending the limit shows a permanent warning while it is off, and the host cap still applies.

### Presence
- [x] **AC28** Every prompt starts with a presence preamble from one template:
  - the room name;
  - the persona's own name and model/effort;
  - each other persona with model/effort, state, turns, trigger and the first line of its last synthesis;
  - the humans present, each by the `@name` a mention of them takes, the host and the current speaker;
  - the chain budget left;
  - to address a human by their `@name`, which notifies them, and to keep `@agent` for giving a turn.

  Each line is capped at about 100 characters.

### Skills
- [x] **AC29** A skill is a Claude Code skill folder (`SKILL.md` with frontmatter, optional files). The shared library accepts a zip upload, keeps immutable versions with their author, and lets the admin remove one.
- [x] **AC30** A profile can upload personal skills. They are only offered in rooms that profile is in, and they never come from the host's disk.
- [x] **AC31** A persona's active skills reach `claude -p` through `--plugin-dir` as the version pinned in the room. A skill update is offered, not applied.
- [x] **AC32** The first time a profile activates a skill, a disclaimer warns about scripts and data exposure, including the egress allowlist. It is shown once per profile.

### Sandbox and egress
- [x] **AC33** The sandbox image runs `python3`, `uv`, `node`, `jq`, `yq`, `rg`, `git`, `curl` and `pandoc`. A script reading and writing an `.xlsx` with openpyxl and a `.docx` with python-docx succeeds offline.
- [x] **AC34** From the sandbox, `uv pip install` of a PyPI package and a `git clone` from GitHub work through the egress proxy, and a request to a host outside the allowlist is denied.
- [x] **AC35** From the sandbox, any Anthropic host is denied by the egress proxy, and `claude -p` still works through the bearer-swap proxy.
- [x] **AC36** The host can edit the allowlist, the cap and the approval mode from the daemon page. Denied egress requests are listed there with host and time.

### Workspace sync
- [x] **AC37** The daemon fetches a manifest of paths, sha256 and sizes, then a tar of only the paths missing from or changed in its per-room cache. A second turn with no room change transfers no upload bytes.
- [x] **AC38** A corrupt or missing cache falls back to the full workspace, and the turn still succeeds.

### Mobile and live
- [x] **AC39** On a phone, the message input stays visible above the on-screen keyboard and the message list stays scrollable. Tested by `e2e/mobile.spec.js` and checked by hand on Android.
- [x] **AC40** A dropped live stream reconnects and resumes after the last message without duplicates. Returning to the page from the background reloads the room state. Tested by `e2e/mobile.spec.js`.

### End to end
- [x] **AC41** On the lab, a simulated session with two personas (Grill on demand, Secretary every 2 rounds) runs: Grill calls `@secretary` once, the chain count is shown, and both syntheses name the model and effort. The presence preamble is captured and reviewed in the report.

## Phases

### Phase 1 — Profiles, rename, member invites
- Work: profiles, recovery link, magic link (SMTP optional), linking participants to profiles, room rename, member invites.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7
- **Data model impact**: `profiles`, `profile_sessions`, `magic_links`; `participants.profile_id`; `invites.created_by`
- **DoD**: `go test -race` green with the cascade and purge invariants extended; Playwright flow join → second room → recovery link on a new context; magic link tested against a local SMTP sink (e.g. mailpit in compose).

### Phase 2 — Chat Markdown, edit, delete
- Work: server-side Markdown rendering for messages, versions, soft delete, voice line correction, edits flagged in prompts.
- **Covers**: AC8, AC9, AC10, AC11, AC12, AC39, AC40
- **Data model impact**: `message_versions`; `messages.edited_at`, `messages.deleted_at`
- **DoD**: XSS table test on the renderer; e2e editing and deleting messages; a prompt golden test with an edited message; `e2e/mobile.spec.js` green.

### Phase 3 — Persona templates and room personas
- Work: template library with presets, room instances, edit between turns, one session per persona, turn queue, model and effort per turn (`--model`, `--effort`, `--append-system-prompt-file`).
- **Covers**: AC13, AC14, AC15, AC16, AC17, AC18, AC42
- **Data model impact**: `persona_templates`, `persona_template_versions`, `room_personas`; `turns.persona_id`, `turns.effort`; `agent_sessions` keyed by room persona
- **DoD**: migration of existing rooms to one "Default" persona holding their current session; a live e2e with two personas on the compose stack (haiku); takeover resumes each persona's session.

### Phase 4 — Host cap and approvals
- Work: cap and approval mode in the daemon config and on its page, clamping, approval banner, per-persona exceptions.
- **Covers**: AC19, AC20, AC21
- **Data model impact**: `room_personas.cap_decision`, `cap_daemon_id`, `cap_model`, `cap_effort` (the host's last answer, approved or refused, per host daemon), `room_personas.pending`, `persona_change_requests`; `daemons.max_model`, `max_effort`, `approve_all`; `messages.req_model`, `req_effort`
- **DoD**: e2e covering above-cap clamp → banner → approve → next turn as requested; "approve everything" holds a new persona.

### Phase 5 — Triggers, chains, presence
- Work: per-persona buttons, every N rounds, `@persona` from humans and personas, self-call option, chain budget, re-arm/suspend/unlimited, presence preamble template.
- **Covers**: AC22, AC23, AC24, AC25, AC26, AC27, AC28
- **Data model impact**: `rooms.chain_budget`, `rooms.chain_count`, `rooms.chain_suspended`; `room_personas.every_n`, `room_personas.self_call`; `turn_queue.source`, and `turn_queue.participant_id` nullable for turns an agent queued
- **DoD**: unit tests for the chain counter state machine (every reset and link source); a golden test of the preamble; e2e chain pause → re-arm.

### Phase 6 — Skills
- Work: shared library (zip upload, versions, removal), personal skills in profiles, per-persona activation, pinning, plugin dir injection, disclaimer.
- **Covers**: AC29, AC30, AC31, AC32
- **Data model impact**: `skills`, `skill_versions` (blobs), `room_persona_skills`, `profiles.skills_disclaimer_at`; `turns.skills` (the versions a turn loads, pinned when it starts)
- **DoD**: a skill with a script runs in the sandbox during a live haiku turn and its output lands in a doc; pinning holds across a library update.

### Phase 7 — Sandbox image and egress
- Work: image with the tool list, squid sidecar per room sandbox, allowlist, Anthropic deny, denied log on the daemon page.
- **Covers**: AC33, AC34, AC35, AC36
- **Data model impact**: none (daemon local config)
- **DoD**: in-sandbox probes for each tool, uv install, git clone, denied host, denied Anthropic host; `claude -p` haiku still works; image size recorded.

### Phase 8 — Differential workspace sync
- Work: manifest endpoint, per-room daemon cache, tar of requested paths, fallback.
- **Covers**: AC37, AC38
- **Data model impact**: none
- **DoD**: transferred bytes measured on two consecutive turns (second turn: no upload bytes); corrupted cache test; existing turn e2e green.

### Phase 9 — Lab rollout and simulated session
- Work: deploy to the lab; extend the S5 scenario with two personas, a chain and the presence preamble; report.
- **Covers**: AC41
- **Data model impact**: none
- **DoD**: report `spikes/S6-personas.md`; at most 15 real Claude requests.

## Data model impact (summary)

New tables:
- `profiles`, `profile_sessions`, `magic_links`
- `message_versions`
- `persona_templates`, `persona_template_versions`, `room_personas`, `persona_change_requests`
- `skills`, `skill_versions`, `room_persona_skills`

New columns:
- `participants.profile_id`, `invites.created_by`
- `messages.edited_at`, `messages.deleted_at`
- `turns.persona_id`, `turns.effort`
- `rooms.chain_budget`, `rooms.chain_count`, `rooms.chain_suspended`
- `turn_queue.source`
- `profiles.skills_disclaimer_at`, `turns.skills`

Changed: `agent_sessions` is keyed by room persona, and `turn_queue.participant_id` is empty for a turn an agent queued. Existing rooms migrate to one "Default" persona that keeps their current session. Every new room-owned table cascades from `rooms`; profile-owned tables cascade from `profiles`.

## Personal data

- A profile links a person's rooms. The admin can see which rooms a profile is in.
- An email bound by magic link is personal data: visible to the admin only, never to participants.
- Personal skills and templates belong to their profile. They reach the host's sandbox only while that profile's persona is active, and the agent can then read them.
- Any member can invite, so anyone holding an invite can widen the room. That matches the PoC rule that a link holder can read the room.
- Deleting a room still removes everything in it. Deleting a profile removes its skills, templates and email, and detaches its participants, which keep their display name.

## Open questions

- [x] 1. SMTP relay for the lab magic links: a mailpit in the lab, reached through the `room-smtp` NetworkPolicy; local dev and `make e2e` use mailpit too.

## Decisions

- One host runs every persona of a room (grill Q2). Hosts are stateless, so per-persona hosts stay possible later without a data model change.
- A round is a completed agent turn of any persona.
- Default chain budget is 25 (the user offered 25 or 50).
- The host's cap and egress allowlist get minimal controls on the existing daemon page; the full settings and monitoring UI is spec 003.
- The skill format is Claude Code's own, so skills written for Claude Code work unchanged.
- Personal skills come only from the profile, never from the host's disk, to avoid accidental leaks (grill Q7).
- Squid allows CONNECT on 443 without TLS interception; the residual risk of writable allowed domains is stated in the disclaimer.
- The sandbox stays a single image without LibreOffice.
- Models rank haiku < sonnet < opus < fable; a full id ranks with the family it names, Mythos with fable, and an unknown id above all. An empty model or effort is the host's default and is never capped.
- A cap answer is per persona and host daemon, for the exact level asked: after a hand-over the new host's cap applies and the new host is asked again. A refusal is kept so the banner does not ask again for that same level.
- "Approve everything" holds a member's new persona and changes, not the host's own and not removals. Held requests survive a hand-over; the host at decision time decides.
