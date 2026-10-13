# 007 — STT engine: parakeet-ultra

**Status**: shipped (AC18: the phone check is manual and open)
**Requested by**: babs
**Date**: 2026-10-13
**Context**: spikes S10 (models), S10b (workstation bench), S10c (browser) and S10d (lab validation at the 4-CPU limit)

## Problem

The STT worker runs faster-whisper `small`. On real French speech it is the weak link of the room:
- WER 0.327 on real meetings and 0.257 on a real tech talk (lab, S10d).
- p95 latency 5.7 s for one talker. With two people talking at once it reaches 15.5 s, so a pod holds one continuous talker.
- The room glossary (005-AC23) made it worse on real speech: meetings 0.327 → 0.386, talk 0.257 → 0.515. It is now off by default.

S10d measured parakeet-ultra (transcribe.cpp, Q8_0, greedy) on a lab node at the same limit: meetings 0.232, talk 0.121, no English leak, p95 2.8 s for one stream and 3.9 s for two.

## Solution

The worker transcribes with parakeet-ultra. Lines arrive in about half the time, with fewer wrong words, and the one STT pod keeps up with about four people talking at once across the whole instance. That is the documented ceiling of this spec. Past it, utterances wait in a bounded queue of 64, the metrics show the wait, and an alert fires when an utterance waits more than 10 s. A full queue never loses a line silently: the room gets a system line naming the speaker. The engine ignores the room glossary, so Room settings hides the glossary tab while the running engine does not use it; the stored glossaries stay. For one release, a single setting switches the worker back to faster-whisper.

## Scope

- A transcribe.cpp engine in the worker, behind the existing per-track LiveKit subscription and Silero cuts (800 ms end of speech, 15 s max).
- The plausibility filter without the language label, for the new engine.
- Two engine processes in the one pod, a bounded queue of 64, per-speaker ordering, queue and latency metrics, and an alert on queue wait.
- A visible room line when the queue drops an utterance.
- An English-leak metric and a leak test line.
- Packaging: transcribe.cpp built in the image at a pinned commit, the model pinned by revision and sha256, license notices.
- An engine switch with faster-whisper as the fallback for one release, a rollback path, and a lab A/B check on the S10 corpora.

## Out of scope

- Hotwords and phrase boosting (sherpa-onnx beam search). S10d: Piper term hits 0.31 → 0.40, real speech flat, at half the greedy throughput (RTFx 3.0 vs 6.2). A later spec, gated on a real-voice term test.
- Client-side STT in the browser. S10c measured parakeet-ultra in WASM: same accuracy (talk WER 0.133 live, 0.111 file), but words freeze about 8.4 s after speech (p95 11.5 s), a 722 MB download, a 2-3 GB tab, desktop only, and the server cannot verify client text. A later spec, as an opt-in mode beside the worker.
- Streaming or interim text. A line still posts once per utterance.
- More than one STT pod, or sharding rooms across pods. A later spec, "STT on demand": the room server assigns rooms to STT workers through the heartbeat (presence knows who is in the audio), and KEDA or an HPA scales the replicas on a server metric (active talkers, rooms with audio).
- Removing faster-whisper. That follows one release after this one.
- Deleting stored glossaries. They stay for a later engine that uses them.

## Acceptance criteria

### Engine
- [x] **AC1** `APP_ENGINE` selects the engine: `parakeet` (default) or `whisper`. Any other value stops the worker at start with a message that names the allowed values. The start log and a `grill_stt_engine_info{engine,model,revision}` gauge name the running engine.
- [x] **AC2** With `parakeet`, the worker transcribes each utterance with transcribe.cpp, the parakeet-ultra Q8_0 GGUF, greedy decoding, no language argument. The per-track subscription, the Silero cuts, the `voices` heartbeat and the line API are unchanged.
- [x] **AC3** With `parakeet`, a transcript is dropped when it is empty, when its gzip compression ratio is above 2.4 (repetition, computed on the text as Whisper does), or when less than 90 % of its letters are Latin (reason `script`). The language probability and language-script checks do not apply. The `IMPLAUSIBLE` reasons keep their names. With `whisper`, the filter is today's, unchanged.
- [x] **AC4** Each posted line gets a language guess, `fr`, `en` or `unknown`, from a function-word count. It replaces the engine's language label in the logs and feeds `grill_stt_lines_language_total{engine,guess}`. Nothing reaches the room server.
- [x] **AC5** An English leak shows in that metric. A test fixture of French dense in English terms (a Piper voice allowed by `scenarios/VOICES.md`) comes out guessed `fr` through parakeet, and an English fixture `en`. The A/B corpus reports the leak per set (AC16).

### Capacity
- [x] **AC6** The worker runs `APP_ENGINE_PROCESSES` engine processes (default 2) with `APP_CPU_THREADS` threads each (default 2). Each takes the next utterance from the one queue when it is free. A process that dies is restarted. Its utterance counts as `error`, and the other process carries on. `/healthz` fails when no engine process has been up for 60 s.
- [x] **AC7** Lines of one speaker post in speech order. An utterance is not started while the same participant's previous one is still in an engine. Other speakers' utterances pass it.
- [x] **AC8** The queue stays bounded (`APP_QUEUE_SIZE`, default 64). When it is full, the new utterance is dropped, never silently:
  - `grill_stt_utterances_dropped_total` counts it;
  - a log names the room and participant, never the words;
  - the room gets a system line, "Transcription overloaded: one line lost for <name>.", posted by the worker through a new STT route. At most one such line per speaker every 30 s, so a burst cannot flood the chat.

  A queued utterance is never dropped for its age: a late line still goes to the transcript.
- [x] **AC9** Metrics, in `docs/architecture.md`:
  - `grill_stt_queue_depth` (exists);
  - `grill_stt_queue_wait_seconds`, from the VAD cut to an engine taking the utterance, with a bucket at 10 s;
  - `grill_stt_transcribe_seconds` and `grill_stt_line_latency_seconds` (exist), labelled by engine;
  - `grill_stt_engines_busy`;
  - the drop counter (AC8).
- [x] **AC10** The manifest keeps the 4-CPU limit and the `Recreate` strategy. It requests 2 CPUs, sets memory to 3Gi for request and limit, and sets `APP_ENGINE`, `APP_ENGINE_PROCESSES` and `APP_CPU_THREADS`. Its sizing comment cites S10d and states the ceiling: one pod, about four continuous talkers across the instance.
- [x] **AC19** An utterance that waited more than 10 s in the queue logs a `queue_wait_slow` warning (room, participant, wait). An alert rule fires when any utterance waited more than 10 s in the last 5 minutes, from the `grill_stt_queue_wait_seconds` buckets. The rule and the scrape config ship as manifests applied apart from the app (the lab runs the VictoriaMetrics operator), and `docs/architecture.md` gives the expression for any other Prometheus.

### Packaging and licenses
- [x] **AC11** A build stage compiles transcribe.cpp from a commit pinned by full SHA. The build targets the AVX2 baseline: never `GGML_NATIVE`, so an image built on a newer CPU runs on the lab nodes. The final image gets only the runtime artefacts. It stays non-root, with a read-only root filesystem.
- [x] **AC12** The build fetches `parakeet-ultra-Q8_0.gguf` from `handy-computer/parakeet-ultra-gguf` at revision `39eeb55181f0d354fd934f06e92fd8d5037fed8e` and checks its sha256, `007a59761e9258779f189df396b50d12b83bb5b79e66df4b955c230d2f2a0a59`. A mismatch fails the build. At run time the worker loads it offline, as it does the Whisper model today.
- [x] **AC13** The image carries the license texts of transcribe.cpp and ggml (MIT) and the model's notice (CC-BY-4.0) under `/licenses`. The docs credit parakeet-ultra in a third-party notices section: Moondream's post-train of NVIDIA Parakeet TDT 0.6B v3, CC-BY-4.0, converted to Q8_0 GGUF by the transcribe.cpp authors (`handy-computer`). The room's audio menu shows the same credit, with a link to the license, while the worker reports `parakeet`.

### Glossary
- [x] **AC14** The glossary stays stored, exported and imported, as in 005-AC23. With `parakeet` the worker never reads the room prompt. The worker's heartbeat carries its engine and whether it uses the glossary. While no live worker uses it, Room settings hides the "Words to recognise" tab and its deep link opens the first tab instead, and a plain member, whose only tab it was, no longer gets the Room settings button. With `whisper` and `APP_GLOSSARY_PROMPT` on, the tab is back with today's sentence.

### Rollout
- [x] **AC15** With `APP_ENGINE=whisper`, the same image runs today's faster-whisper `small` path, `APP_GLOSSARY_PROMPT` included, and its tests stay green. It is kept for one release.
- [x] **AC16** A lab A/B run replays the S10 corpora (SUMM-RE meetings, the tech talk, the 38 Piper lines) through the worker's own segmenter, engines and filter, in a Job at the 4-CPU limit, with S10b scoring. With `parakeet`:
  - meetings WER ≤ 0.25 and talk ≤ 0.15, each at or below `whisper` on the same run; Piper English lines ≤ 0.08;
  - English leak ≤ 0.01 on meetings and talk;
  - p95 latency ≤ 4 s for one talker, ≤ 5 s for two, and for four a target of 8 s with 10 s as the failure bound, with no drop;
  - peak pod memory ≤ 2.7 GiB.

  The report lands in `spikes/`.
- [x] **AC17** Rollback is one step in the release notes and `docs/lab-checklist.md`: set `APP_ENGINE=whisper` and restart the Deployment. No data changes either way. Redeploying the previous image tag is the second way out.
- [ ] **AC18** The lab checklist's voice step passes with `parakeet`. A sentence on the phone shows as a line within 5 s, and two people speaking at once both get their lines.

## Phases

### Phase 1 — Engine, filter, packaging
- Work: the transcribe.cpp `Transcriber`, the engine switch, the parakeet plausibility rules, the language guess, the image build with pinned runtime and model, license files and notices, the hidden glossary tab and the heartbeat engine fields.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC11, AC12, AC13, AC14, AC15
- **Data model impact**: none. The STT heartbeat gains `engine` and `glossary` fields, held in memory.
- **DoD**:
  - unit tests break each new filter rule in at least two shapes: empty and whitespace; a looped phrase and a repeated token; Cyrillic and CJK output;
  - the e2e fixture test passes with both engines in the built image, under `--network none`;
  - building twice gives the same GGUF sha256, and a wrong sha256 fails the build;
  - an e2e test checks the glossary tab for each heartbeat state;
  - the `whisper` suite stays green.

### Phase 2 — Two engines, queue, metrics, alert
- Work: the engine process pool, per-speaker ordering, the bounded queue, the drop line and its route, the metrics, the alert manifests and the Deployment.
- **Covers**: AC6, AC7, AC8, AC9, AC10, AC19
- **Data model impact**: none. The drop line is an ordinary system message.
- **DoD**:
  - tests with a fake engine: two speakers run in parallel, one speaker never posts out of order, a full queue drops, counts and posts one rate-limited line, and a killed process restarts while the other finishes its utterance;
  - `/metrics` shows every AC9 series;
  - `make k8s-render` output has 3Gi, 4 CPUs and the three env vars;
  - the alert rule loads in the lab's vmalert and the stt pod is a scrape target.

### Phase 3 — Lab A/B and rollout
- Work: a replay mode that feeds WAV tracks in real time to the worker's segmenter and pipeline, the A/B Job, the report, the docs and the release notes, and the lab rollout.
- **Covers**: AC16, AC17, AC18
- **Data model impact**: none
- **DoD**:
  - the AC16 thresholds met on a lab node, the report committed under `spikes/`;
  - a lab redeploy, the lab checklist's voice step passed with `parakeet`;
  - a rollback drill: `whisper` set, a line posts, `parakeet` set back, a line posts.

## Data model impact (summary)

None. The STT heartbeat gains in-memory `engine` and `glossary` fields; the drop line is a system message.

## Decisions

- Engine: parakeet-ultra through transcribe.cpp, Q8_0, greedy (S10d go). Qwen3-ASR-0.6B was as accurate but missed latency (p95 9.7 s).
- Two processes with 2 threads each, not one with 4: 8.4x real time in total against 6.2x (S10d).
- The `script` check counts Latin letters: French and English both use them, and a non-Latin transcript means the engine went astray.
- The language guess only feeds metrics and logs, never the room server, which never received the language before either.
- One image holds both engines for one release, so rollback is a setting and not a rebuild. It costs about 0.5 GB of image size.
- Utterances are never dropped for their age: a late line beats a missing one in a transcript.
- CPU request stays 2. S10d scheduled with 900m, which is the fallback if 2 does not fit.

## Decisions (user, 2026-10-13)

- Integration: try the transcribe.cpp Python binding first in Phase 1; if it fails on the worker's Python, fall back to one long-lived CLI process per engine.
- Model: pin the GGUF conversion validated in S10d by revision and sha256; verify its provenance and license chain in Phase 1 (the upstream author named in AC13 is to be checked, not assumed).
- Attribution: the room's audio menu plus a third-party notices section in the docs.
- Four-talker latency: p95 8 s is the target, 10 s (the product limit) is the failure bound.
- Memory: a 3 Gi request fits the lab worker nodes (24-43 % of memory requested at the time of writing).

## Decisions (A/B review, user, 2026-10-13)

- The worker dated every cut 800 ms early, as if a pause had ended it. A 15 s forced cut has no pause, so its latency was overstated, in `grill_stt_line_latency_seconds` too. Fix that, then measure again with the one-talker bound unchanged (≤ 4 s).
- Piper English lines need only WER ≤ 0.08, no longer "at or below whisper": the set is synthetic, and parakeet wins on real speech (spikes/S007-ab.md).

## Follow-ups (not built in 007)

- A babble filter on per-token confidence: parakeet gives no confidence the AC3 filter can use, and it turns TTS glitches into Latin-letter babble that posts (`stt/tests/fixtures/voices.json`). transcribe.cpp exposes a probability per token.
- AC18's manual phone check stays open for the user. Its automated equivalent (`e2e/voice/talkers.spec.js` on the lab) passed.

## Decisions (grill, user, 2026-10-13)

- Capacity: one pod. One STT worker joins every room, so the ceiling is about four simultaneous talkers instance-wide. The spec documents it and the saturation metrics show it. Scaling out is the later "STT on demand" spec (Out of scope).
- Overload: never drop silently. The queue holds 64 utterances; an alert fires when an utterance waits more than 10 s; past 64, the utterance is dropped with a visible system line in the room. This replaces the draft's queue of 8, which assumed one room per pod.
- Glossary: this spec ships without glossary use. The tab is hidden while the engine does not use it, and the stored data stays. S10 and S10d show parakeet-ultra handles French with English jargon without biasing (talk terms 0.94 with no list), and Claude reads the transcript afterwards, which absorbs the remaining term errors. Hotwords through sherpa-onnx beam search stay a later option.
- Model provenance, verified on the Hugging Face API on 2026-10-13: the GGUF `handy-computer/parakeet-ultra-gguf` comes from the transcribe.cpp authors, license `cc-by-4.0`, `base_model: moondream/parakeet-ultra`, `base_model_relation: quantized`, revision `39eeb55181f0d354fd934f06e92fd8d5037fed8e`. The build pins that revision and the Q8_0 file's sha256.
