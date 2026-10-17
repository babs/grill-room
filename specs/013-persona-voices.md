# 013 — Personas speak in the call

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-16, notes folded 2026-10-17
**Context**: spikes S14 (persona voice, prototype) and S14b (French with English terms), both on branch `spike/persona-voice`; grill notes of 2026-10-14 to 2026-10-16 (Q1 to Q5, lexicon); builds on 006 (starters), 007 (STT pod, pinned model build, notices), 009 (persona sheet tabs), 010 (device STT) and `docs/agent-workspace.md` (output contract)

## Problem

Personas only write. In a call, people have to look at the chat to know that Grill answered, and a reply lands while the room has moved on. The people talking cannot hear the personas, so the call and the chat drift apart.

S14 and S14b showed that a CPU-only engine can give each persona its own voice in the LiveKit call, at about 0.1 s to first audio, without touching the STT worker's budget. They also found the traps: French voices garble English terms and numbers, a model does not keep a spoken summary short on its own, and LiveKit reports nobody else speaking until the bot subscribes to the humans' audio.

## Solution

Every persona speaks in the call by default; a room admin turns "Speaks in the call" off for a persona and picks its voice in the persona sheet. The Spec grill starter keeps Secretary silent.

After each turn, a speaking persona says a short first-person summary of what it did: "I changed the payment spec. About Alice's note on the budget, I replied that the figure is monthly. Bob, which timeout do we keep?" It never reads the chat text, a document or its synthesis. A persona that writes no spoken line but changed something says a short summary the server builds from its actions. It speaks only while at least one person is in the audio, never over a human, stops when someone starts talking and picks up the sentence again once they stop. Personas take turns. A line that waited too long is dropped from speech; the chat keeps the text.

French and English rooms both ship. A rule-based text layer reads numbers, acronyms and English tech words the way a French speaker says them: tech terms English-style, naturalised words like "bug" or "parking" French-style. The server admin can fix a pronunciation, or flip a term between the two styles, in `/admin`. Each persona has its own speaking rate, and the pod shortens the long pauses some voices leave. Each listener can mute a persona for themselves from its pill.

## Scope

- `spoken.md`, a new workspace output, 40 to 60 words (a room setting, default 60), sanitised and cut at a sentence end by the server; without it, a server-built summary of the turn's actions.
- A text layer before synthesis: language per sentence, numbers, units and acronyms in that language, a pronunciation lexicon, English clauses through the same voice's English model.
- A `tts` pod running Pocket TTS 3.3.0, weights pinned and fetched at build with the Hugging Face token as a build secret only.
- One LiveKit bot per speaking persona.
- The STT worker skips bot tracks; device mode stays on the local mic.
- A speaking rate per persona (0.9 to 1.3) and silence gaps shortened to 300 ms.
- Turn-taking: never over a human, stop within 300 ms, resume or drop, one persona at a time, stale lines dropped, timeouts per room.
- The "Speaks in the call" toggle, the voice picker with a sample, the rate, default voices, the persona pill (a click opens the persona card, ▶ runs the persona, 🔊 mutes it for this listener) and its speaking states.
- The Spec grill starter turns on Grill's voice.
- A pronunciation lexicon in `/admin`, each term English-style or French-style, seeded with about 130 terms (Appendix A), each seed term checked by an STT round trip in CI.
- Scale to zero as an option, echo verification on a real audio loop.

## Out of scope

- Voice cloning from user audio, uploaded voices, and voices from Kyutai's voice-donation project (donors may withdraw consent, S14b). Pocket's terms forbid impersonation without consent.
- A second engine, in a line or per persona (Kokoro, Sopro, Supertonic). Two engines in one line sound like two people (S14b §4).
- Speaking the chat text, the synthesis or a document.
- A small LLM rewrite step before synthesis (S14b: not needed now).
- Personas marking English spans in `spoken.md` (`<en>…</en>`, S14b option 2). A later step if the stopword rule falls short.
- A per-room lexicon or spoken forms in the 007 glossary. The lexicon is instance-wide.
- Personas hearing the call live or answering by voice in real time. Turns still start from text and triggers.
- Pronunciation preview in `/admin`. The admin hears a change in a room.
- GPU synthesis. Echo checks in Firefox and Safari.
- Exporting the lexicon with a room.

## Acceptance criteria

### Spoken output
- [ ] **AC1** The prompt asks for `spoken.md`, with the rules of Appendix C and the room's word cap (AC3), only when the server offers voice (AC13), the persona has "Speaks in the call" on, and at least one human is in the room's audio when the turn starts. A human is in the audio when one of their tabs is in the call and not deafened. Otherwise the prompt is byte-identical to today's.
- [ ] **AC2** The daemon sends `spoken.md` content (`NeedsContent`). The server keeps it when it is valid UTF-8 of at most 4 KB, else it ignores it and lists why in the turn's system message, as for the other refused outputs. `spoken.md` is never a document, a chat message or part of an export, and no log carries its words. `docs/agent-workspace.md` lists it in "What the agent produces".
- [ ] **AC3** The server sanitises `spoken.md` before queueing it: it removes Markdown marks, code spans, link targets, bare URLs, emoji, short ids (`Alice ·a1b2` becomes `Alice`, `#14` becomes `14`) and a mention's `@` (`@Ana_María` becomes `Ana María`), and folds whitespace. Then it keeps whole sentences while the total stays at or under the room's word cap (40 to 60, default 60, AC25). When the first sentence alone is longer, it cuts at the last comma at or before the cap, else at the cap, and ends with a full stop. Text empty after this is not spoken.
- [ ] **AC4** A turn without `spoken.md`, or with nothing left after AC3, speaks a summary the server builds from the turn's actions, in the first person and in the synthesis's language (AC5's count): the documents it changed and wrote, by name said as words, the notes it opened and the threads it answered ("I changed the payment spec. I opened 2 notes."). A turn that did none of these speaks nothing. The synthesis, the chat text and documents are never spoken.

### Text layer
- [ ] **AC5** Each sentence gets a language, `fr` or `en`, from a function-word count. A tie takes the previous sentence's language; a tie on the first sentence takes the whole line's count; a tie there gives `en`.
- [ ] **AC6** Numbers are said in the sentence's language: integers, decimals (French with "virgule"), percentages, versions (`2.3.1`, `v2`), a letter with digits (`p95`), and units (Appendix A, rule cases). In English, an HTTP status after an article is said digit by digit ("a five oh two"); in French as a number ("cinq cent deux"). An acronym of 2 to 5 capitals is spelled with the language's letter names unless the lexicon has it. A run of 10 digits or more (an id, a phone number) is said digit by digit, and a line says at most 150 words once numbers and terms are words, whole sentences first.
- [ ] **AC7** In a French sentence, each lexicon term takes the French form of its style: an English-style term (the default for tech terms) its English-style respelling ("ri-traï"), a French-style term (naturalised words: bug, web, email, parking, week-end) its French-style form ("beugue"); an empty form leaves the term to the language's other rules (an acronym spelled, a word as written). In an English sentence, a term takes its English form when it has one. Longest term first, whole words only; a term holding a capital matches its exact case, an all-lowercase term matches any case. A comma-separated clause of a French sentence with at least 2 English stopwords, and more English than French ones, goes to the same voice's English model; everything else in the sentence stays one French call. A line never uses two engines or two voices.
- [ ] **AC8** Each rule of AC3, AC4, AC5, AC6, AC7 and AC38 has a test that fails without it, on at least two input shapes it did not come from (S14b's `normalize.py --check` is the starting set).

### TTS pod, build and licences
- [ ] **AC9** A `tts` Deployment runs Pocket TTS 3.3.0, one process holding the French and the English model. The image is non-root with a read-only root filesystem; the Deployment uses `Recreate`, `/healthz` and `/metrics` on its own port, 2.5 GiB memory as request and limit, a 2 CPU limit and a CPU request set from the Phase 1 lab measurement. Its sizing comment cites S14 and S14b. The NetworkPolicy lets it reach the room server and LiveKit only, and lets only the metrics scrape in.
- [ ] **AC10** The build fetches every weight and voice file by repository, revision and sha256; a mismatch fails the build. The Hugging Face read token reaches the build only as a BuildKit secret: no layer, history entry, label, environment variable or file of the image contains it. At run time the pod loads everything offline.
- [ ] **AC11** The voice catalogue is a pinned file in the repo: id, display name, native language, licence, source, credit. It holds only CC0, CC-BY-4.0 or public-domain voices from `kyutai/tts-voices` (Appendix B), none from a non-commercial set and none from voice donations. The server refuses any other voice id with 400. No route accepts audio for a voice.
- [ ] **AC12** The image carries under `/licenses` the Pocket TTS code licence (MIT), the weights notice (CC-BY-4.0), each shipped voice's licence and source, and the licences of num2words (LGPL-2.1, unmodified), the LiveKit SDK and torch. The third-party notices section of the docs credits Kyutai Pocket TTS and each voice.

### Server and pod
- [ ] **AC13** `GRILL_ROOM_TTS_TOKEN` turns voice on for the server. Unset: no prompt change, every `/api/tts/*` route answers 404, and the persona sheet shows no voice control. A wrong token gets 401.
- [ ] **AC14** The pod takes lines from an SSE stream, `GET /api/tts/events`, and resumes after a reconnect from `Last-Event-ID`. A line carries its sequence, room, persona id and name, voice, speaking rate, sanitised text, the room's timeouts (AC25) and its queue time. The server keeps a line for its room's queue maximum while no pod is connected, then drops it (reason `no_service`). The lexicon reaches the pod on the same stream, whole at connect and again on each change. The server sends on the same stream, at connect and on each change, the rooms with at least one human in the audio and their speaking personas (id, name, voice), which the bots follow (AC15). The pod heartbeats every 2 s with the rooms and bots it holds, and reports each persona's state: `waiting`, `speaking`, `idle`, or `dropped` with a reason.
- [ ] **AC15** Each speaking persona is its own LiveKit participant: identity `persona-<id>`, kind `agent`, the persona's name, allowed to publish microphone audio only. It subscribes to the humans' audio tracks and to no agent's, so LiveKit reports the humans as active speakers (S14). It joins while the persona has "Speaks in the call" on and at least one human is in the room's audio, and leaves 60 s after either stops. It never joins a LiveKit room with no human in it. Each sentence is levelled to about −22 dBFS with a peak cap.
- [ ] **AC16** When a persona speaks and has no voice yet (created, migrated, or toggled on), the server stores a default: the first catalogue voice, in catalogue order, native to the room's language and used by no other persona of the room. The room's language is guessed from its last 50 messages with AC5's count, or the browser's language when the room has none. With no unused voice in that language it takes the next unused voice in any language; with none at all, the first voice of the language, and logs it. A stored voice never changes when another persona joins or leaves.
- [ ] **AC17** On a lab node, with the bot joined and nobody speaking, the first audio frame is published at most 0.3 s after the pod receives the line (p95 over 20 lines). Two rooms speaking at once both play without a gap over 100 ms (`grill_tts_underruns_total` stays 0).
- [ ] **AC18** Through the shipped text layer, the pinned engine and the room's own STT (`grill_stt.replay`), the S14b French set (17 sentences) scores a mean WER of at most 0.12 over the 5 French-native voices (S14b: 0.109), and the English set (S14b's 4 controls plus S14's 4 sentences) at most 0.12 over 5 English voices. GitHub, 502 and LiveKit come back in at least 4 of 5 French voices.
- [ ] **AC19** The STT worker skips participants of kind `agent` before subscribing: no subscription, no `track_ignored` warning, one debug line per participant. It leaves a room where only agents remain. Device mode taps only the local microphone track (010-AC9), and a test asserts it. Persona bots never appear in People, and the audio menu's count of voices written down counts humans only.
- [ ] **AC20** Metrics, listed in `docs/architecture.md`: lines queued, spoken, interrupted and resumed, dropped by reason (`interrupted`, `stale`, `no_listener`, `no_service`, `error`), queue wait, first-audio seconds, stop seconds (AC22), synthesis real-time factor, underruns, bots joined.

### Call behaviour
- [ ] **AC21** A persona starts a line only after the room's quiet time (default 1 s) with no human active speaker.
- [ ] **AC22** When a human starts speaking, the persona's audio stops at a listener within 300 ms of the speech onset (p95 over 20 interruptions), and the pod clears its buffered audio. CI measures the stop from when the listener hears the human; the full 300 ms from the onset in the human's file is checked by hand from a workstation (lab checklist).
- [ ] **AC23** After an interruption, if the room is quiet again within the resume window (default 20 s), the persona says the interrupted sentence again from its start and goes on. Otherwise it drops the rest of the line (reason `interrupted`). The chat keeps the synthesis in both cases.
- [ ] **AC24** One persona speaks at a time per room, in queue order; rooms do not wait on each other. A line not started within the room's queue maximum (default 40 s) is dropped (reason `stale`). A line whose room has no human in the audio when its turn comes is dropped (reason `no_listener`). A dropped line posts nothing in the chat.
- [ ] **AC25** Room settings has the three timeouts and the word cap, edited by room admins: quiet before speaking (0.5 to 5 s, default 1), resume window (0 to 60 s, default 20), queue maximum (10 to 300 s, default 40), spoken words (40 to 60, default 60). A value out of range gets 400. A change applies to the next line. Room export and import carry them.

### Persona settings and UI
- [ ] **AC26** The persona sheet's Identity tab (009-AC2) has "Speaks in the call", on for every existing and new persona (user, 2026-10-17). Whoever may edit the persona may change it. Save stays the sheet's only primary button.
- [ ] **AC27** The Identity tab has a voice picker: each catalogue voice with its name, native language, and a ▶ button that plays a sample in the room's language (AC16). The samples are rendered at build time and served by the room server, so the picker works while the `tts` pod is down or scaled to zero. A voice's licence credit shows on hover and on focus, and on a tap on touch. Voices used by another persona of the room are marked; picking one anyway is allowed.
- [ ] **AC28** The pill of a persona that speaks in the call has a speaker icon button (speaker, or speaker-off when muted), and the persona card a "Mute for me" switch. Muting unsubscribes that persona's track for this participant only; unmuting subscribes again. The choice is kept per room in browser storage and survives a reload.
- [ ] **AC29** The persona pill shows "speaking" and "waiting to speak" with an SVG icon from `kit/ui.js` (speaking: the speaking pulse, static under `prefers-reduced-motion`; waiting: the same icon, outlined), the word on a desktop, and the meaning in `aria-label` and the tip. Never colour alone. A state clears within 10 s after the pod stops heartbeating.
- [ ] **AC30** Starters carry "Speaks in the call" and the voice per persona; Save as starter and room export and import carry them too, and an older starter or export reads them as on and empty (default voice per AC16). The built-in Spec grill gets a new version with Grill's voice on and Secretary's off; Retro and Doc review keep every persona on.

### Lexicon
- [ ] **AC31** `/admin` has a Pronunciation page for the server admin: list, search, add, edit, delete. An entry is a term (1 to 40 characters, unique ignoring case), a style (English or French) and three spoken forms: English-style French, French-style French and English, at least one set. The list shows each term's style and flips it in one click. At most 1000 entries. Each change writes an audit entry (004-AC6) and reaches the pod for the next line, with no restart. Anyone else is refused, as on every `/api/admin` route: 401 without the admin's token or session, 403 with an API token.
- [ ] **AC32** A migration inserts Appendix A (about 130 terms) once. Later edits and deletions stand; the server never re-seeds at start.
- [ ] **AC33** A CI job renders each Appendix A entry in a carrier sentence of each language it has a form for, 10 times with the first default voice of that language, and passes the entry when the room's STT hears one of its accepted spellings in at least 6 of 10 renders. The job fails when more than 5 entries fail; its report lists every entry's hits and names every failing one.

### Scale to zero and echo
- [ ] **AC34** The room server exports `grill_room_tts_demand`, the number of rooms with a speaking persona and at least one human in the audio. A KEDA `ScaledObject`, shipped apart from the app like the 007 alert rules and off by default, scales `tts` between 0 and 1 on it. A line queued while the pod starts waits within the queue maximum (AC24), and is dropped `stale` past it. The cold start (scale-up to first audio) is measured and written in `docs/architecture.md`.
- [ ] **AC35** On CI or a lab node, Chrome in a container with its own PipeWire loop (a null sink whose monitor is the capture source, never the host's audio session) joins as a silent human while a persona says 10 lines. With `echoCancellation` on, the STT worker posts no line for that human. With it off, the persona's own echo cuts it off at least once (`grill_tts_lines_interrupted_total` rises), which proves the loop carries audio. If neither CI nor the lab can host the loop, this AC is struck through with the reason in Decisions.
- [ ] **AC36** `docs/lab-checklist.md` gains a voice step: two speaking personas, a human cutting one off, the persona resuming, the second waiting its turn, nothing transcribed from a persona.

### Rate, pauses and the persona pill
- [ ] **AC37** Each persona has a speaking rate, 0.9 to 1.3 (default 1.0), next to its voice in the Identity tab; out of range gets 400, and whoever may edit the persona may change it. The pod changes the tempo, not the pitch: at 1.2 a sentence lasts 1/1.2 of its length at 1.0, within 5 %. Starters, exports and imports carry it; older ones read 1.0.
- [ ] **AC38** The pod shortens every silence inside a line, within a sentence or between two, to at most 300 ms; speech is never cut. A test feeds the audio chain a line with 1 s gaps and finds no gap over 300 ms in what it publishes.
- [ ] **AC39** A click on a persona pill opens the persona card: its state, model and effort, triggers, voice and "Speaks in the call", "Mute for me", and Edit. A ▶ icon button on the pill, tip and `aria-label` "Ask <persona> now", runs the persona, for whoever may press the agent button. The icon comes from one constant.

## Phases

### Phase 1 — TTS pod and pipeline
- Work: `spoken.md` in `internal/turn` and the prompt, the sanitiser and the word cut (60 until the room setting lands), the action summary, the SSE stream, heartbeat and state routes with their token, the `tts` image (Pocket, pinned weights, build secret, catalogue, licences, samples), the text layer with the S14b lexicon compiled in, the bots, plain FIFO playback per room, default voices, the rate and silence trimming, the STT worker's agent skip, metrics, the Deployment and NetworkPolicy. The toggle has no UI yet: voice is off unless the lab sets `GRILL_ROOM_TTS_TOKEN` and turns a persona on through the API.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC17, AC18, AC19, AC20, AC38, and AC37's pipeline (the rate set through the API)
- **Data model impact**: `personas.speaks` (0 or 1, default 1; the migration sets existing personas to 1), `personas.voice` (text, empty by default), `personas.voice_rate` (default 1.0). In memory: the speech queue and each persona's voice state.
- **DoD**:
  - Go tests on the sanitiser and the cut, each rule broken in two shapes: Markdown and a URL, an id with and without a dot, a 600-word line without punctuation, a 65-word first sentence with and without a comma, the action summary in both languages;
  - Python tests on the text layer per AC8; prompt test: with the flag conditions off, the prompt is byte-identical;
  - building the image twice gives the same weight hashes; a flipped sha256 fails the build; a scan of the image's layers, history and config for the token value finds nothing;
  - first audio p95 ≤ 0.3 s and no underrun with two rooms on a lab node, via `/metrics`;
  - the AC18 replay on a lab node: FR mean WER ≤ 0.12, EN ≤ 0.12, the three flagged terms in ≥ 4 of 5 voices, report under `spikes/` or `docs/`;
  - a voice e2e (fake mic, LiveKit, real worker, the `tts` pod with a fake engine): a persona's line plays, the worker logs no subscription to `persona-*`, no voice line carries a persona's words; one smoke run with the real engine;
  - `make k8s-render` shows the `tts` Deployment, its limits and the policy;
  - a re-test of the Appendix C prompt on haiku, at most 6 requests, checked by S14's `prompt_test.py`;
  - `make lint test` green.

### Phase 2 — Call behaviour and UI
- Work: the samples handed to the server and the room language from the browser for AC16 when the room has no line yet, quiet time, interruption, resume and drop, the per-room queue rules, room timeouts and word cap in settings and export, the toggle, the voice picker and the rate in the Identity tab, the pill (card, ▶, mute) and its states, the starter change, the lab checklist step.
- **Covers**: AC21, AC22, AC23, AC24, AC25, AC26, AC27, AC28, AC29, AC30, AC36, AC37 (UI), AC39
- **Data model impact**: `tts_samples` (the voice samples the pod renders at build and hands over, AC27), `rooms.voice_quiet_ms`, `rooms.voice_resume_s`, `rooms.voice_queue_s`, `rooms.voice_words` (defaults 1000, 20, 40, 60); starter manifests and room exports gain `speaks`, `voice` and `voice_rate` per persona and the four room values. Built-in Spec grill, new version.
- **DoD**:
  - pod tests on an injected clock: start after quiet, stop and clear on a speaker update, resume within the window, drop past it, stale drop, `no_listener` drop, FIFO across two personas;
  - e2e with a fake mic (fake engine): a listener page measures each remote track's level; no persona audio starts while the human's level is up; the human interrupts until 20 interruptions have their own audio reaching the listener within 250 ms (at most 30), and the persona stops within 300 ms of the onset in the human's file (p95 over those 20; in CI, from when the listener hears the human); the persona resumes after the human stops; a second persona waits for the first. If LiveKit's speaker updates alone miss 300 ms, the pod adds its own VAD on the subscribed human tracks and the test runs again;
  - Playwright at 390 and 1440 px: the toggle, picker and rate on the Identity tab, ▶ plays a sample with the pod stopped, the credit tip on hover and tap, the pill click opens the card, the pill ▶ runs a turn, the pill mute unsubscribes and survives a reload, the pill shows both states with icon and word;
  - starter tests: a room from Spec grill has Grill speaking and Secretary not; an export and import round trip keeps voices and timeouts;
  - the lab check (AC36) with 2 personas and a human interrupting, passed by hand; the end-to-end AC22 run (lab checklist, real engine, p95 at most 300 ms from the onset in the human's file);
  - `make lint test` green.

### Phase 3 — Lexicon and polish
- Work: the `/admin` Pronunciation page and its routes, the lexicon table and seed, the lexicon on the pod's stream, the CI round-trip job, the demand gauge and the KEDA manifest, the cold-start measurement, the echo loop test, the docs.
- **Covers**: AC31, AC32, AC33, AC34, AC35
- **Data model impact**: `pronunciations` (term, style, fr_en, fr_fr, en, by, at), seeded once by migration; audit actions `tts.lexicon.add`, `tts.lexicon.edit`, `tts.lexicon.delete`.
- **DoD**:
  - Go tests: anyone but the admin is refused (401, 403 for an API token); a duplicate term ignoring case, an entry with no form and the 1001st entry get 400; an edit reaches a connected test pod on the stream;
  - the migration applies on a fresh DB and on a copy of the lab DB, and a second start inserts nothing;
  - the AC33 job green in CI, its report attached;
  - on the lab: with the KEDA object applied, the pod goes to 0 with no demand, and a line queued from cold is spoken within the queue maximum or dropped `stale`; the cold start is written down;
  - the AC35 run, or the struck AC with its reason;
  - `make lint test` green.

## Data model impact (summary)

- `personas.speaks`, `personas.voice`, `personas.voice_rate` (Phase 1).
- `tts_samples`, `rooms.voice_quiet_ms`, `rooms.voice_resume_s`, `rooms.voice_queue_s`, `rooms.voice_words` (Phase 2).
- `pronunciations`, seeded once (Phase 3).
- Starter manifests and room exports gain per-persona `speaks`, `voice` and `voice_rate` and the four room values; older ones read as on and defaults.
- In memory only: the speech queue, the pod's heartbeat and persona states. Nothing spoken is stored.

## Open questions

None open. The draft's three were answered on 2026-10-16 and moved to Decisions.

## Decisions

From the notes (user):

- Engine: Pocket TTS 3.3.0, one engine, no Kokoro mix, with the rule-based hybrid text layer (S14b).
- Option B: one LiveKit bot per persona, server-side synthesis, in a dedicated `tts` pod sized 2 CPU and 2.5 GiB to start, able to scale to zero.
- Spoken: the persona-written `spoken.md`, first person, 40 to 60 words (a room setting, default 60), cut by the server at a sentence end; never the documents, never the full chat text. Without `spoken.md`, a short server-built summary of the turn's actions (2026-10-16).
- The `tts` image holds the gated weights: it goes to the private lab registry only, never to a public one (2026-10-16).
- Whole English sentences in a French-native voice use that voice's English model: one timbre per persona (2026-10-16).
- Speaking rate per persona, 0.9 to 1.3; silence gaps capped at 300 ms (2026-10-16).
- Anglicisms: tech terms English-style by default (respelled); each lexicon term carries an English or French style the admin flips; naturalised words (bug, web, email, parking, week-end) seeded French-style (2026-10-16).
- Persona pill: a click opens the persona card; ▶ "Ask <persona> now" runs it; 🔊 mutes it for this listener. Tentative, the icon sits behind one constant (2026-10-16).
- "Speaks in the call" on by default for every persona, existing ones included (user, 2026-10-17, overturns 2026-10-16 "off by default"); speaks only with at least one human in the audio; the Spec grill starter keeps Secretary off.
- Mute both ways: per listener (local) and per persona (the setting).
- Voice picker in the Identity tab, ▶ sample in the room's language, distinct defaults by order, catalogue voices only, licence credit on hover; no user-uploaded voices.
- Turn-taking: never start over a human, stop within 300 ms, resume within 20 s or drop, one persona at a time, queued line dropped past 40 s, pill "speaking" and "waiting to speak", timeouts per room.
- French ships enabled, no gate; language detected per sentence. TTS is on by default in the lab deploy (2026-10-17).
- Lexicon editable by the server admin in `/admin`, seeded with the user's cases and a broad dev, ops and product set, each verified by STT round trip in a build test.
- Gated weights fetched at build with a read-only token as a build secret, never committed, never in the image. CC-BY credit in the third-party notices.

Made while drafting (overturnable):

- The text layer runs in the `tts` pod, in Python, from S14b's `normalize.py` and num2words. The Go server would need its own number speller (S14b §6).
- The pod has its own token, not the STT token: least privilege for a new service.
- Lines reach the pod on an SSE stream, not by polling: a poll interval would eat into the 0.3 s first-audio budget.
- Bots join when the room has a human in the audio, not when a line arrives, so a LiveKit join never sits in front of the first audio.
- Each bot subscribes to human tracks only, never to other bots; LiveKit needs the subscription to report active speakers (S14).
- The default voice is stored when the toggle turns on, so it never shifts as personas come and go.
- Voice donations are out (Out of scope), so marius (`voice-donations/Selfie.wav`) and javert (`Butter.wav`) are out of the catalogue even though their licence is CC0; Appendix B's English defaults start with alba and charles. Voices whose source the `kyutai/tts-voices` README does not list (giovanni, lola, juergen, rafael, daan) are out too.
- Samples (AC27) are rendered when the image builds; the pod hands them to the room server at connect, which keeps them in its database. Committing synthetic audio to the public repo is avoided.
- A server-built summary does not override a persona that chose silence when it did nothing: a turn with no action and no `spoken.md` says nothing.
- Lexicon forms: an empty form leaves the term to the language's other rules, so flipping a term to French-style with no French-style form lets the French voice read the word itself, and an acronym with no form is spelled by the acronym rule.
- Samples are rendered at build and served by the room server: they work while the pod is scaled to zero.
- The per-listener mute unsubscribes rather than setting volume 0: it saves the bandwidth.
- A dropped line posts nothing in the chat: the synthesis is already there.
- The lexicon round trip passes a term on 6 of 10 renders: Pocket samples at random and one render moves a term's result (S14b, Setup). At 2 of 3, two runs on the same seed failed different sets of 7 and 9 entries (user, 2026-10-17: 6 of 10, no seeding). At 6 of 10, two CI runs again failed 9 entries each, a different set each time; pooled over the 20 renders, rows other than the user's cases under 60% lost their French form (Terraform, SLO, branch, rebase, changelog, Stripe, mail, DNS, A/B test; those with no other form left the seed, which holds 129 terms), and the job fails only past 5 failing entries (user, 2026-10-17).
- The turn-taking e2e uses a fake engine; the real engine gets one smoke run and the lab replay. Turn-taking does not depend on the engine, and CI stays light.
- Risk: S14 saw LiveKit report a human about 0.25 s after the speech began, which leaves about 50 ms for the 300 ms stop. Phase 2 measures it and adds a VAD in the pod if needed.
- Measured in Phase 2's e2e: LiveKit names a talking human to a bot about 400 ms after their onset, too late for AC22. The pod also reads the humans' tracks its bots subscribe to, with an energy onset detector (20 ms above −42 dBFS, 400 ms hang): the stop then lands 30 to 110 ms after the onset at a listener. The room hears a human while any of its bots, or detectors, does; a bot that has not subscribed to someone yet names nobody and must not silence the others.
- AC22's CI sample: on a shared runner the human's own audio sometimes reached the listener 0.4 to 1.5 s after its file onset, and the persona's stop came right behind it. No pod can beat a stalled media path, and with 10 samples one such stall is the p95. The test leaves out interruptions whose human audio took over 250 ms to reach the listener and interrupts until 20 stay, at most 30: one runner stall spanned three interruptions in a row (CI, 2026-10-17). Even then, interruptions next to a stall reached the listener 130 to 230 ms late and pushed the p95 to 300-313 ms while the persona stopped about 100 ms after the listener heard the human: CI measures from that moment, the lab keeps the end-to-end bound (user, 2026-10-17).
- AC33's first round trip (estelle and alba, the room's STT) passed 137 of 227 entry-languages. Candidate respellings were rendered 3 times each; the best one at 2 of 3 or better replaced the form: cluster is now "clusteur" (the user's "cleuss-teur" was heard 0 of 3, "clusteur" 3 of 3). A language with no passing form lost its forms, and the voices read the term as written there; 35 entries with none left were removed (sidecar, webhook, deploy, commit, JSON, SQL and others). Most acronyms lost their French form: the STT does not write letter-by-letter French back as the acronym, though the voices still spell them by the acronym rule. The seed held 137 terms; flaky, OKR and WebRTC then lost their French forms, never heard back across runs, and flaky, with no English form, left the seed (user, 2026-10-17).
- AC35 runs in CI (`persona-echo`): Chrome's microphone is a remap of a PipeWire null sink's monitor (Chrome lists no monitor source). With echo cancellation on, 10 lines pass with no STT line and no interruption; with it off the persona's own echo cuts it off and resumes it in a loop, but the STT posts nothing: each echo fragment is under its 400 ms of voice, so the cut itself proves the loop (user, 2026-10-17). A listener with echo cancellation off on speakers stalls the persona: out of scope, the room keeps it on.
- A bot's track sends silence between lines: a stream that restarts at each line makes the listener's jitter buffer grow again.
- The per-listener mute lives on the pill and in the card (notes, 2026-10-16), not in the audio menu.
- One torch thread per synthesis: two under a 2-CPU limit starved LiveKit's media threads and left gaps in Phase 1's voice e2e.
- The CPU request comes from the Phase 1 lab run: S14 and S14b measured E-cores, not the lab's nodes, and 007 found a 4-CPU node cannot always grant 2 CPUs.

## Appendix A — Pronunciation lexicon seed

How to read it: every table but "Naturalised" is English-style, its "FR" being the English-style form. "FR" is what a French voice says inside a French sentence, "EN" what an English voice says inside an English sentence; "as written" means no entry for that language. The respellings follow S14b's and the user's ("ri-traï", "guite-heub", "clusteur", "laïve-kit"). Every row other than the user's cases is a candidate until AC33 passes it; a row other than the user's cases heard back under 60%, pooled over the CI runs, is respelled or removed before the Phase 3 merge. Accepted spellings for the round trip are the term itself, its spaced form ("live kit"), the name it is said by where the STT writes that name (k8s: Kubernetes, kubectl: kube control, PostgreSQL: Postgres, Nginx: engine x, OK: okay, B2B and B2C: B to B, B to C) and, for numbers, the digits or the words.

### User cases

| Term | FR | EN |
|---|---|---|
| GitHub | guite-heub | as written |
| retry | ri-traï | as written |
| cluster | clusteur | as written |
| LiveKit | laïve-kit | Live Kit |

502 is a rule case (below).

### From S14b

| Term | FR | EN |
|---|---|---|
| rollout | rol-aoute | as written |
| rollback | rol-bak | as written |
| feature flag | fi-tcheur flag | as written |
| staging | stéïdjinng | as written |
| Kubernetes | koubeurnétisse | as written |
| merge request | meurdje riquouest | as written |
| pull request | poule riquouest | as written |
| ArgoCD | argo cédé | Argo C D |
| backlog | bak-logue | as written |
| sprint | sprinnte | as written |
| OAuth | as written | O Auth |
| pipeline | païpe-laïne | as written |
| code freeze | code frize | as written |
| GitLab | guite-lab | as written |
| merge | meurdje | as written |
| review | ri-viou | as written |
| PostgreSQL | postgrèsse | Postgres |
| Redis | rédisse | as written |
| canary | canari | as written |
| install | instol | as written |
| login | loguine | as written |
| npm | ène pé èm | N P M |
| pnpm | pé ène pé èm | P N P M |

### Cloud, containers, operations

| Term | FR | EN |
|---|---|---|
| k8s | koubeurnétisse | Kubernetes |
| kubectl | as written | kube control |
| pod | pode | as written |
| namespace | néïme-spéïsse | as written |
| container | conn-téï-neur | as written |
| Docker | doc-keur | as written |
| cloud | claoude | as written |
| AWS | as written | A W S |
| GCP | gé cé pé | G C P |
| Azure | azur | as written |
| S3 | èss trois | as written |
| serverless | seur-veur-lesse | as written |
| autoscaling | auto-squéïlingue | as written |
| ingress | inn-grèsse | as written |
| Nginx | as written | engine X |
| Prometheus | prométhéusse | as written |
| monitoring | monitorinngue | as written |
| dashboard | dache-borde | as written |
| uptime | eupe-taïme | as written |
| downtime | daoune-taïme | as written |
| timeout | taïme-aoute | as written |
| on-call | onne-col | as written |
| runbook | reune-bouk | as written |
| post-mortem | poste mortème | as written |
| vCPU | as written | V C P U |
| CPU | as written | C P U |
| GPU | as written | G P U |
| RAM | rame | ram |

### Git and CI/CD

| Term | FR | EN |
|---|---|---|
| CI/CD | as written | C I C D |
| CI | as written | C I |
| build | bilde | as written |
| hotfix | hotte-fixe | as written |
| push | pouche | as written |
| checkout | tchèque-aoute | as written |
| linter | linn-teur | as written |
| PR | pé èr | P R |
| repo | répo | as written |
| monorepo | mono-répo | as written |

### Web and code

| Term | FR | EN |
|---|---|---|
| API | as written | A P I |
| SDK | èss dé ka | as written |
| URL | as written | U R L |
| YAML | ya-meul | yammel |
| HTTP | ache té té pé | H T T P |
| HTTPS | as written | H T T P S |
| DNS | as written | D N S |
| TLS | té èl èss | T L S |
| SSH | èss èss ache | as written |
| VPN | vé pé ène | V P N |
| JWT | as written | J W T |
| token | tau-keune | as written |
| WebSocket | ouèbe-soquette | web socket |
| WebRTC | as written | web R T C |
| full-stack | foule-stak | as written |
| framework | frème-oueurk | as written |
| React | ri-act | as written |
| TypeScript | taïpe-scripte | as written |
| JavaScript | java-scripte | as written |
| Python | païe-sonne | as written |
| MongoDB | mongo dé bé | Mongo D B |
| debug | di-beugue | as written |
| refactor | ri-facteur | as written |
| legacy | lèga-ci | as written |
| open source | o-peune source | as written |
| Jira | dji-ra | as written |

### Product and team

| Term | FR | EN |
|---|---|---|
| roadmap | rode-mape | as written |
| feature | fi-tcheure | as written |
| user story | you-zeur stori | as written |
| stakeholder | stéïk-holdeur | as written |
| onboarding | onne-bordinngue | as written |
| feedback | fide-bak | as written |
| pricing | praïe-sinng | as written |
| A/B test | as written | A B test |
| benchmark | bènnche-mark | as written |
| deadline | dède-laïne | as written |
| meeting | mitinngue | as written |
| daily | déïli | as written |
| standup | stannde-eupe | as written |
| scrum | skreume | as written |
| workshop | oueurk-chope | as written |
| brainstorming | brène-stormingue | as written |
| KPI | ka pé i | K P I |
| OKR | as written | O K R |
| MVP | èm vé pé | as written |
| POC | as written | P O C |
| UX | as written | U X |
| UI | as written | U I |
| B2B | bé tou bé | B to B |
| B2C | as written | B to C |

### Naturalised anglicisms (French-style, AC7)

"FR" here is the French-style form.

| Term | FR | EN |
|---|---|---|
| bug | beugue | as written |
| web | ouèbe | as written |
| email | imèle | as written |
| parking | parkinng | as written |
| week-end | ouik-ènde | as written |
| startup | starte-eupe | as written |
| manager | manadjeur | as written |
| marketing | markètinng | as written |
| business | biznèss | as written |
| design | dizaïne | as written |
| podcast | podcaste | as written |
| smartphone | smarte-faune | as written |
| wifi | oui-fi | as written |
| OK | as written | O K |

### Rule cases (AC6, not lexicon rows, in the same round trip)

| Written | FR | EN |
|---|---|---|
| 502 (HTTP status) | cinq cent deux | five oh two |
| 404 | quatre cent quatre | four oh four |
| 503 | cinq cent trois | five oh three |
| p95 | pé quatre-vingt-quinze | P ninety-five |
| 99,9 % / 99.9% | quatre-vingt-dix-neuf virgule neuf pour cent | ninety-nine point nine percent |
| 2.3.1 | deux point trois point un | two point three point one |
| v2 | version deux | version two |
| 300 ms | trois cents millisecondes | three hundred milliseconds |
| 2 Go / 2 GB | deux gigaoctets | two gigabytes |
| 512 Mo / 512 MB | cinq cent douze mégaoctets | five hundred twelve megabytes |
| 2,5 Gio / 2.5 GiB | deux virgule cinq gibioctets | two point five gibibytes |
| 300 € | trois cents euros | three hundred euros |
| $20 | vingt dollars | twenty dollars |

## Appendix B — Voice catalogue, starting point

Pinned in Phase 1 from the `kyutai/tts-voices` README at a revision, with each voice's licence read there.

- French-native: estelle (CC0), and the CML-TTS French readers 2114, 3267, 4193 and 12080 (CC-BY-4.0), which S14b scored best with the hybrid layer. Gender is inferred from pitch and still needs an ear.
- English-native: alba (CC-BY-4.0), the VCTK set (anna, vera, fantine, charles, paul, eponine, azelma, george, mary, jane, michael, eve, CC-BY-4.0) and the Voice-Zero LibriVox set (bill_boerst, peter_yearsley, stuart_bell, caro_davy, CC0).
- Excluded: cosette (Expresso) and jean (EARS), both non-commercial; marius and javert, from the voice-donation project; giovanni, lola, juergen, rafael and daan, neither French nor English and not listed in the `kyutai/tts-voices` README.
- Catalogue order for defaults (AC16): French-native first in S14b's order (estelle, 2114, 3267, 4193, 12080); English-native starting with alba and charles, then the rest of the VCTK set, then Voice-Zero.

## Appendix C — Prompt rules for `spoken.md`

S14's v3 with the room's word cap (`<cap>`, default 60). To re-test in Phase 1 (v3 itself was never run on a model).

```
- The room is on an audio call. Write spoken.md: what you say out loud in the call after this turn. A speech synthesizer reads it in your voice, word for word, so write it the way you would say it.
  - Say what you did, in the first person: "J'ai commenté la section budget." "I changed the spec. Retries now stop after three tries." When you answered someone, say whose point it was and what you answered: "About Alice's note on the budget, I replied that the figure is monthly."
  - Say only what your files and your synthesis say. Give a number only when you wrote it.
  - One question at most, at the end, addressed by name to the person who can answer it.
  - <cap> words at most, in the language of your synthesis. Short sentences, most under 15 words, ended by full stops. No colons, dashes or brackets.
  - Plain words only. No Markdown, list, link, file name, path, number sign, id, code, quotation marks or emoji. Write symbols and units as words: "300 euros a month", not "300 €/month". Say "the payment spec", not "docs/spec-paiement.md", and "Alice", not "Alice ·a1b2".
  - Never read a document or your synthesis out. Say what changed and why it matters.
  - No greeting, thanks, recap phrase or filler.
  - Nothing worth saying aloud: leave spoken.md out.
```
