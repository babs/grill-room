# 010 — On-device transcription (opt-in)

**Status**: in progress
**Requested by**: babs
**Date**: 2026-10-14
**Context**: spike S11 (browser STT), grill of 2026-10-14 (Q1 to Q5); builds on 005 (presence, AC6 and AC7), 007 (worker, queue, metrics, CC-BY credit), 008 and 009 (People details card)

## Problem

One STT pod transcribes every room. It holds about four people talking at once across the instance (007), and every voice costs the server CPU. Many participants sit at a desktop that could do the work itself.

S11 shows a desktop browser can run the same model at the server's level:
- `parakeet.js` (MIT), onnxruntime-web 1.30 (MIT), a port of the worker's Silero segmenter, parakeet-ultra w4a8 (CC-BY-4.0);
- end of speech to line p50 1.6 s, p95 2.2 s on pause cuts, 2.6 s with 15 s cuts, WASM on 4 threads, visible tab;
- talk WER 0.121 against 0.122 on the server, English leak 0.001;
- a 402 MB download, cached, and ready in about 5.6 s on a warm load;
- WebGPU works without `shader-f16`.

Two traps came out of it. A hidden, silent headless tab slowed decoding 2.5-4x after about 5 minutes. And upstream `parakeet.js` drops the space before accented words.

## Solution

A participant on a capable desktop can tick "Transcribe on this device" in the audio menu. It is off by default and the browser remembers the choice. The option only shows on a desktop that passes a capability check and a 10 s self-test. Phones never get it.

With it on, the browser cuts the participant's own speech into utterances, transcribes them and posts each line under the participant's name, like a chat message. The server's worker stops transcribing that person's track and picks it up again within about 5 s when the device stops, slows down or closes. When the device's latency degrades, the track goes back to the server on its own, and returns to the device once the device keeps up again.

The server serves the model from its own volume, which an init container fills at deploy. Without that volume the option never shows.

People can tell who transcribed a line: its tooltip says "Transcribed on <name>'s device" or "Transcribed by the server", and the People badge has a device variant. The agents are not told.

## Scope

- Capability detection, the 10 s self-test and the opt-in toggle in the audio menu.
- The device pipeline: segmenter port, `parakeet.js` engine, the 007 plausibility filter, line posting.
- A participant route for device lines, stored with `source=device|server`.
- The handover: a device flag in the presence heartbeat, relayed to the worker in its heartbeat answer; the worker skips flagged tracks.
- A server-side dedupe of near-identical lines across sources within 5 s.
- Fallback to the server on degradation, and the return.
- The model files: a pinned manifest, a fetch command, an init container, a dedicated volume, a `/models/` route.
- Page isolation headers so WASM gets threads.
- A real-Chrome background-tab check, a fake-mic e2e and a latency and accuracy bench.

## Out of scope

- Phones and tablets. Tablets come later, if a real self-test shows the load is sustainable.
- Chrome's on-device Web Speech (S11 §3.3). Quality unmeasured, closed model, Chromium only. A later tier after a hand check with real French voices.
- Trust checks on device text. A device line is text the participant writes under their own name, like chat (Q2). No rate limit beyond what chat has, which today is none.
- A room-level or moderator switch to forbid device mode.
- Telling personas which lines came from a device.
- Interim or streaming text.
- `zstd`-encoded model files (S11 §4, untested).
- The redux w2a8 model (meetings WER 0.262).

## Acceptance criteria

### Offer and self-test
- [ ] **AC1** The audio menu shows "Transcribe on this device" only when all hold:
  - the server offers device mode (AC5);
  - the page is `crossOriginIsolated`;
  - the device is a desktop: `navigator.userAgentData.mobile === false`, or, without `userAgentData`, `maxTouchPoints <= 1` and no `Mobi` in the user agent;
  - `hardwareConcurrency >= 4`, and `deviceMemory >= 8` when the browser reports it;
  - `navigator.storage.estimate()` leaves at least 1 GB free;
  - this device has not failed the self-test for the current model revision (AC2).

  Phones and tablets never see it. It is off by default. The choice is stored per device (browser storage), not per room.
- [ ] **AC2** Turning it on shows the download size, then fetches every model file with `integrity: sha256-…`, stores it in Cache Storage and asks `navigator.storage.persist()`. A hash mismatch stops with a message and caches nothing. Then the page decodes a bundled 10 s clip. It passes when RTFx ≥ 3 on WASM or ≥ 8 on WebGPU, and the text is within WER 0.2 of the clip's reference. A WebGPU failure retries on WASM before refusing. On a fail the toggle turns off and the option disappears for that model revision on this device, with the reason in text for that session. A new model revision offers it again.
- [ ] **AC3** With a WebGPU adapter the engine runs the w4a8 encoder on WebGPU, otherwise on WASM with 4 threads. `ort.env.wasm.wasmPaths` points at our own `/models/` path, so no runtime file comes from a CDN.

### Page and model files
- [ ] **AC4** The room page is served with `Cross-Origin-Opener-Policy: same-origin` and `Cross-Origin-Embedder-Policy` (`credentialless`, or `require-corp` if it fits), and its CSP adds `'wasm-unsafe-eval'` to `script-src` and `worker-src 'self'`. Every existing room flow still works under them: the document iframe, PDFs, uploads, push, LiveKit audio, the docs window. The e2e suite stays green.
- [ ] **AC5** `GRILL_ROOM_DEVICE_STT_MODEL_DIR` names the model directory. When it is unset, or any file of the pinned manifest is missing or fails its sha256 at server start, device mode is not offered: the `stt` event says so, `/models/` answers 404, and one log line names the variable or the file. With a complete directory, `GET /models/<revision>/<file>` serves only manifest files, with `Cache-Control: public, max-age=31536000, immutable`, `Cross-Origin-Resource-Policy: same-origin` and the right content type. Any other path is a 404.
- [ ] **AC6** `grill-room models fetch --dir <dir>` downloads every file of the manifest compiled into the binary: the w4a8 encoder, the int8 decoder, the vocabulary, the onnxruntime-web runtime files and the Silero v6 VAD. Each file is pinned by repository, revision and sha256. A file already present with the right hash is skipped. A download goes to a temporary file and is renamed after its hash checks. A mismatch exits non-zero and leaves nothing under the final name.
- [ ] **AC7** `deploy/k8s/room.yaml` runs that command in an init container of the server image, into a dedicated PVC, mounted read-only in the server container. The init container stays non-root with a read-only root filesystem. The NetworkPolicy lets the init container reach the model host and the npm registry over 443, and nothing else new. A deploy with the volume already full downloads nothing.
- [ ] **AC8** `parakeet.js` is vendored at a pinned version, with a header naming the upstream repository and version, and with the accented-word spacing fix (`/\s+(?=[^\p{L}\p{N}_\s])/gu`). The third-party notices list `parakeet.js`, onnxruntime-web and Silero VAD (MIT) and the model (CC-BY-4.0). The 007-AC13 credit in the audio menu also shows while device mode is on, whatever engine the server runs.

### Device pipeline and lines
- [ ] **AC9** With device mode on and the mic live, the page segments the participant's own microphone audio with the worker's parameters (thresholds 0.5 and 0.35, 800 ms end of speech, 400 ms minimum, 15 s maximum, 200 ms pad) and transcribes each utterance once, off the main thread. It drops empty lines, lines with a gzip ratio above 2.4, and lines with less than 90 % Latin letters, as 007-AC3 does. While the mic is muted it processes no audio. The LiveKit audio is unchanged.
- [ ] **AC10** `POST /api/rooms/{room}/voice/device-lines` takes `{body}` with the participant's session. The speaker is the session's participant, never a body field. The body goes through the same `cleanText` and `maxBodyLen` as a chat message. The line is stored as a voice message with `source=device` and counts for voice triggers like a worker line. Worker lines are stored with `source=server`. Without device mode offered (AC5) the route answers 404.
- [ ] **AC11** Exports carry `source` on voice lines in `messages.jsonl`; an import reads a missing `source` on a voice line as `server`. The agent workspace renders a voice line the same way for both sources.
- [ ] **AC12** A voice line's tooltip reads "Transcribed on <name>'s device" or "Transcribed by the server". In People, the "transcribed" badge has a device variant with its own glyph and tooltip, so the state is not colour alone (design rule). The People details card (009-AC36) says where that person's voice is transcribed: "Transcribed on their device" or "Transcribed by the server". Nothing else marks device lines.

### Handover
- [ ] **AC13** While device mode is on, the device pipeline sends a presence beat with `device_stt: true` every 1 s, from its worker, so a stalled pipeline stops the beats and main-thread timer throttling does not. The beat leaves the tab's audio state untouched. The server keeps a per-participant device flag. It lapses 3 s after the last device beat, and at once on a beat with `device_stt: false` or `gone`. The `presence` event carries the flag per participant.
- [ ] **AC14** `POST /api/stt/heartbeat` answers 200 with `{"device": {"<room>": [<participant ids>]}}` instead of 204. The worker keeps its subscription to a flagged track, discards its audio, and leaves the participant out of `voices`. An utterance already cut when the flag arrives still posts. The worker heartbeats every 2 s instead of 5. A worker that ignores the answer still transcribes everyone, and a 204 from an older server means it skips nobody.
- [ ] **AC15** After device transcription stops, the worker transcribes that participant again within about 5 s. Measured from the stop to the worker's first subscribed audio frame going to the VAD:
  - ≤ 3 s when the page announces the stop (toggle off, fallback, tab close);
  - ≤ 5 s when it goes silent (crash, frozen tab).
- [ ] **AC16** The server drops a voice line when the same participant has a line from the other source stored in the last 5 s whose text is near-identical: similarity ≥ 0.8 (1 minus Levenshtein distance over the longer length) after lowercasing, keeping letters and digits, and collapsing spaces. The drop answers 200 with `{"deduped": true}` and counts in `grill_room_voice_lines_deduped_total{source}`. Two lines from the same source are never deduped, so a person can repeat themselves.

### Fallback
- [ ] **AC17** The device hands the track back to the server, dropping its flag at once, when any of:
  - over the last 60 s and at least 3 utterances, end of speech to line p95 is above 4 s;
  - more than 2 utterances wait in the device queue;
  - the engine fails, or the model fails to load.

  After a latency or queue fallback the device keeps decoding without posting. It takes the track back when, over the last 60 s and at least 3 utterances, p95 is at most 3 s and the queue is empty, and at most once every 2 minutes. After an engine or model failure it stays on the server until the page reloads. The audio menu says where the voice goes: "Transcribed on this device" or "Transcribed by the server: this device is too slow right now".
- [ ] **AC18** On real Chrome, headed, with the room tab hidden for 10 minutes and the room audio muted, either p95 stays ≤ 4 s, or the fallback fires and the worker transcribes every utterance after the hand-back. If the hidden-tab slowdown shows, a mitigation ships (STT in a SharedWorker, or an audible-silent output) and the check runs again. The report says which.

### Measures and observability
- [ ] **AC19** A bench reruns the S11 harness on the shipped code, WASM on 4 threads, in a visible tab, live for 30 minutes on the S11 corpus:
  - end of speech to line p95 ≤ 3.0 s on pause cuts (S11: 2.15 s) and ≤ 3.5 s on all cuts (S11: 2.63 s);
  - talk WER ≤ the server's on the same run + 0.01, English leak ≤ 0.01;
  - peak tab memory ≤ 2.5 GB;
  - warm load to ready ≤ 10 s (S11: 5.6 s).

  The report lands in `spikes/`.
- [ ] **AC20** The server exports `grill_room_device_stt_participants` (flags held now), `grill_room_voice_lines_total{source}` and the AC16 counter. `docs/architecture.md` shows the device path, the handover and these metrics.

## Phases

### Phase 1 — Model volume, page isolation, offer and self-test
- Work: the manifest with its pins (repository, revision and sha256 checked against the model host's API, as 007 did), `grill-room models fetch`, the `/models/` route and its start check, the init container, PVC and NetworkPolicy, the isolation headers and CSP, the vendored `parakeet.js` with its fix, the engine load, capability detection, the self-test and the toggle, the notices and credit.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8
- **Data model impact**: none. A new PVC for the model files, outside SQLite.
- **DoD**:
  - Go tests break the `/models/` route in at least two shapes each: a missing file and a wrong hash (not offered), `..` and an unlisted name (404);
  - `models fetch` run twice downloads nothing the second time; a flipped byte exits non-zero and leaves no final file;
  - `make k8s-render` shows the init container, the PVC, the read-only mount and the egress rule; on the lab the init container fills the volume and `curl -I` on a model file shows the cache headers;
  - the full e2e suite is green in CI under the isolation headers;
  - Playwright tests with a stubbed `navigator` hide the toggle for a phone, an iPad, 2 cores and a page without isolation, and cover a self-test pass, a fail and a hash mismatch.

### Phase 2 — Device pipeline, handover, dedupe, fallback
- Work: the segmenter port and engine worker, line posting, the device-lines route and `source`, export and import, tooltips and badge, the device beat and flag, the heartbeat answer and the worker skip, the dedupe, the fallback and return, the metrics.
- **Covers**: AC9, AC10, AC11, AC12, AC13, AC14, AC15, AC16, AC17, AC20
- **Data model impact**: `messages.source TEXT` (`device`, `server`, or NULL for non-voice messages), with existing voice lines backfilled to `server`. In memory: the per-participant device flag.
- **DoD**:
  - e2e with a fake mic (`--use-file-for-fake-audio-capture`) and the real model in CI: a device line shows with `source=device` and the device tooltip; the worker's heartbeat answer lists the participant and the worker posts no line for that track; after the toggle goes off, and after the page is closed, a sentence spoken 5 s later posts as `source=server`;
  - Go tests for AC15 on an injected clock: an announced stop clears the flag at once, a silent one after 3 s;
  - dedupe tests: dropped for a case or accent change and for trailing punctuation; kept for the same source twice, for a different sentence, and for a 6 s gap;
  - fallback tests on injected latencies: degrade on p95, on queue and on an engine error; return after recovery; no second return within 2 minutes;
  - the migration applies on a fresh DB and on a copy of the lab DB; an export and import round trip keeps `source`; `/metrics` shows every AC20 series.

### Phase 3 — Real Chrome, bench, rollout
- Work: the background-tab run on real Chrome, the mitigation if needed, the bench, the report, the docs, the lab deploy and checklist step.
- **Covers**: AC18, AC19
- **Data model impact**: none
- **DoD**:
  - the AC18 run and the AC19 thresholds met, the report committed under `spikes/`;
  - a lab deploy with a DB backup; the lab checklist step passes on a desktop: toggle on, a sentence posts with the device tooltip, toggle off, the next sentence posts with the server tooltip.

## Data model impact (summary)

- `messages.source`, a new nullable column, backfilled to `server` on voice lines; exported and imported.
- In memory: a per-participant device flag on the server, and a `device` map in the STT heartbeat answer.
- Outside SQLite: a PVC holding the pinned model files.

## Open questions

- [ ] **COEP and the document iframe.** The iframe has an opaque origin. If neither `credentialless` nor `require-corp` keeps it working (AC4), the fallback is to send the isolation headers only when this device has the option on, read from a cookie, with a reload on toggle. To settle at the start of Phase 1.
- [ ] **Dedicated workers and background throttling.** AC13 assumes Chrome's intensive throttling does not slow a dedicated worker's timers. S11 did not test it. Checked in Phase 1 on real Chrome; if it fails, the beat moves to whatever AC18's mitigation uses.

## Decisions

- Opt-in, off by default, remembered per device (routine call).
- Any desktop browser that passes AC1 and the self-test gets the option, not only Chromium. The self-test is the gate.
- The worker keeps its subscription to a skipped track and discards the audio, so the resume does not wait on a LiveKit resubscribe.
- Device beats every 1 s with a 3 s lapse, and a 2 s worker heartbeat, so a silent stop resumes within 5 s (Q5).
- Fallback uses a 4 s p95 to leave and 3 s to come back, a gap so the track does not flap; S11's p95 is 2.2 s. During fallback the device keeps decoding without posting, because it cannot measure a recovery otherwise.
- The onnxruntime-web runtime and Silero VAD live in the model volume next to the model, so a server without the volume ships none of it and the binary stays small.
- The fetch runs as a subcommand of the server image, not a separate image: one image to pin and scan.
- Dedupe compares only across sources and keeps the first line stored: a duplicate beats a loss (Q5).
- No chat-style rate limit is added: chat has none today, and Q2 says device lines get the same limits as chat.
- `parakeet.js` is pinned with the spacing fix applied locally; the fix goes upstream as a PR. The 007 CC-BY credit covers device mode.
- The People details card carries the device or server line (user, 2026-10-14), next to the 009 "transcribed" line.
