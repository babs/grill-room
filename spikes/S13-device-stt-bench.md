# S13: on-device transcription, background tab and 30-minute bench

Spec 010 Phase 3 covers AC18 (real Chrome, room tab hidden 10 minutes) and AC19 (latency and accuracy bench on the S11 corpus). Both ran on 2026-10-15 against the shipped code of `feat/device-stt` (Phases 1 and 2). The harness and the result summaries are in `s13-device-stt-bench/`.

Tags: **[M]** measured here, **[S11]** from the S11 spike, **[U]** unverified.

## 1. Verdict

- **AC18 passes on the latency path, no mitigation needed.** The room tab stayed hidden 600.2 s with the room muted. The device posted all 38 sentences, 33 of them while hidden, with an end-of-speech-to-line p95 of 1.65 s. It never fell back and the worker posted nothing for that voice [M].
- **The S11 slowdown does not show in a room tab.** The page's own 1 s main-thread timer kept firing every second for the whole 10 minutes. A page without WebRTC, in the Phase 1 run, fell to one tick a minute after 5 minutes. The LiveKit connection most likely exempts the tab from intensive throttling [U on the mechanism]. Device mode only runs while the tab is in the audio, so it always holds that connection.
- **AC19 is met with the AC21 reload delay.** Every threshold passes on the 30-minute live run (peak 1.34 GB), on fresh-tab loads, and on a reload once the 12 s delay has run (peak 1.85-2.01 GB over 9 reloads). Without the delay a reload peaked at 2.2-4.0 GB.
  - p95 2.16 s on pause cuts and 2.19 s on all cuts, and no 5-minute slice above 2.43 s;
  - talk WER 0.120 on the device against 0.127 for the server on the same audio, English leak 0.001;
  - the tab peaked at 1.34 GB during the 30 minutes and at 1.54-1.96 GB while loading the model in a fresh tab;
  - warm load to ready took 2.09-2.73 s.
- **Why a reload peaked.** Chrome keeps the same renderer across a reload. With a new engine loading at once, the old page's memory stayed until about 9 s after the unload, so both models sat in one tab. Ending the workers on `pagehide` did not change it. With the engine held back 12 s (AC21), the old page's memory goes 2.0-2.3 s after the reload and the engine starts on a 224-257 MB tab (§4.1).

## 2. Setup

- **Stack**, all on the host network:
  - LiveKit 1.13.9 in Docker;
  - the STT worker image built from the branch, with the compose settings (2 engine processes of 2 threads, 3 GiB);
  - the e2e server binary, given the device model directory;
  - Google Chrome 155.0.8059.79 in the Playwright 1.64 image, headed under Xvfb.
- **CPU.** Alice's Chrome gets the S11 "laptop" profile. Other work ran on the host (load average about 5).

  | Process | Cores |
  |---|---|
  | Alice's Chrome | 4 P-cores pinned, `nice 10`, 16 GB cap |
  | STT worker | 4 other P-cores |
  | Bob's Chrome, LiveKit, server | E-cores |

- **Engine.** WASM with 4 threads and the w4a8 encoder; the container has no WebGPU adapter. The self-test ran against the real bar of RTFx 3, not the `grillSelfTestBar` seam, and passed at RTFx 5.6-6.3 with WER 0 on the clip.
- **Scripted mic.** A `getUserMedia` override plays 16 kHz PCM through WebAudio, scheduled on the mic's own `AudioContext` clock. AC18 schedules every sentence at the start, so the audio keeps its timing with no driver attached.
- **Recorder.** An init script keeps, inside the page:
  - `visibilitychange` events;
  - a 1 s main-thread tick;
  - every end-of-speech-to-line latency the pipeline worker reports in its stats;
  - its mode changes.

  In AC19 a probe in the pipeline worker also logs each utterance's length as it goes to the engine. An utterance of 451 windows or more is a 15 s cut, 450 or fewer is a pause cut. `analyze.py` pairs the n-th probe with the n-th latency and fails if a probe comes after its latency.
- **Latency** is the figure the shipped pipeline computes for the AC17 fallback. It runs from the receive time of the last voiced VAD window to the server's answer to the line post. S11 stopped at the decode end; the post on loopback adds a few milliseconds. An utterance the plausibility filter drops has no post, so its latency ends at the decode.

## 3. AC18: hidden room tab

**Method.**
1. Plain `google-chrome` with `--remote-debugging-port`, without the flags Playwright adds to its own launches.
2. A CDP client sets up the run. Alice joins, ticks "Keep my mic on while the room is muted", mutes the room, turns device mode on (cold load and self-test), and schedules the 38 Piper lines of the corpus, one every 18.5 s. Then the client process exits, which drops CDP.
3. 60 s later, a second plain `google-chrome --user-data-dir=<same>` invocation opens a foreground tab over the room tab.
4. After 600 s, a new CDP client closes that tab, waits for the schedule to end, and reads the page's record. Lines come from the server database.

**Visibility proof** [M], times from the second invocation:

| Event | Time |
|---|---|
| `visibilityState` hidden | +0.04 s |
| `visibilityState` visible | +600.26 s |
| Main-thread 1 s timer while hidden | 599 ticks, longest gap 1.0 s, 300 ticks in the last 5 minutes |
| Pipeline worker beats | 711 over the run, one a second |

**Results** [M]:

| Measure | Hidden (33 sentences) | Whole run (38) |
|---|---|---|
| End of speech to line, device figure, p50 / p95 / max | 1.48 / **1.65** / 1.77 s | 1.48 / 1.69 / 1.77 s |
| Clip end to stored line, p50 / p95 | 1.07 / 1.24 s | |
| Lines posted by the device | 33 / 33 | 38 / 38 |
| Lines posted by the worker | 0 | 0 |
| Mode changes | none | `device` from start to end |

- The clip-end figure runs below the device figure because a clip ends after its last word. Two clips with long trailing silence give negative values (-0.59 and -0.82 s).
- **Pass.** p95 is 1.65 s against the 4 s limit, a 2.35 s margin. The fallback had nothing to do, so this run never exercised the hand-back branch of AC18. The Phase 2 e2e covers it.

## 4. AC19: 30-minute bench

The run plays the first 1790.6 s of the S11 corpus:
- the 2 talk clips (silver reference);
- the 38 Piper lines;
- 8 SUMM-RE tracks masked to their own speaker (gold reference).

Alice (device mode on) and Bob (device mode off, so the server worker transcribes him) play the same audio at the same time, in the same room. S10's `score.py` scores the lines.

| Threshold | Limit | Measured | Margin |
|---|---|---|---|
| End of speech to line p95, pause cuts (n=187) | ≤ 3.0 s | 2.16 s (S11: 2.15) | 0.84 s |
| End of speech to line p95, all cuts (n=219) | ≤ 3.5 s | 2.19 s (S11: 2.63) | 1.31 s |
| Talk WER ≤ server + 0.01 | ≤ 0.137 | **0.120** (server 0.127) | 0.017 |
| English leak, French items | ≤ 0.01 | 0.001 | 0.009 |
| Peak tab memory, 30 minutes live | ≤ 2.5 GB | 1.34 GB (median 1.28) | 1.16 GB |
| Peak tab memory, model load in a fresh tab (2 cold, 3 warm) | ≤ 2.5 GB | 1.54-1.96 GB | 0.54 GB |
| Peak tab memory, reload with the AC21 delay (9 reloads, §4.1) | ≤ 2.5 GB | 1.85-2.01 GB | 0.49 GB |
| Peak tab memory, reload without the delay (21 loads, §4.1) | no longer the shipped path | 2.22-4.04 GB | |
| Warm load to ready (3 reloads, 3 new tabs, no delay) | ≤ 10 s | 2.09-2.73 s (S11: 5.6) | 7.27 s |

Other figures [M]:

| | Device (Alice) | Server (Bob) |
|---|---|---|
| Lines | 217 (2 dropped by the filter, 0 lost) | 219 |
| Talk WER | 0.120 | 0.127 |
| SUMM-RE WER (8 tracks) | 0.153 | 0.160 |
| Piper WER fr / en | 0.260 / 0.094 | 0.251 / 0.031 |
| English leak, all French items | 0.001 | 0.001 |

- **Cuts.** p50 / p95 1.44 / 2.16 s on pause cuts, 1.83 / 2.32 s on the 32 cuts at 15 s.
- **No drift.** The p95 per 5-minute slice reads 2.43, 1.89, 1.95, 1.91, 2.23, 2.32 s. The first slice is the continuous talk, mostly 15 s cuts.
- **CPU.** Alice's Chrome averaged 0.54 core while playing.
- **Cold load,** on a loopback network, took 6.51 s from the tick to "Transcribed on this device", self-test included (7.01 s in the new-tab run). Fetch and hash check 2.46 s, sessions 0.82 s, warm-up 1.31 s.
- **Warm load by reload.** Fetch from Cache Storage with the hash check 0.543-0.593 s, sessions 0.51-0.61 s, warm-up 1.00-1.37 s. A device that passed the self-test skips it on later loads.
- **Memory** is the RSS of the largest renderer, which holds the page and its workers, sampled every 2 s. A shorter spike can escape the sampling.
- **Piper English** (64 reference words) is too small a sample to read the 0.094 against 0.031.

### 4.1 Reload peak

Each row is 3 runs of 3 warm loads by `page.reload()`, on the same profile, with nothing played. The sampler reads the RSS of the largest renderer every 2 s, every 250 ms for the AC21 row. The second row ends the engine and pipeline workers synchronously on `pagehide` (`Worker.terminate()`, skipped for a back/forward cache entry); it was reverted. The AC21 row is the code the user chose: on a reload the engine starts 12 s after the page loads (`RELOAD_DELAY_MS`, production value, no test override).

| | Peak per warm load | 20 s after the third load | Settled |
|---|---|---|---|
| Shipped code | 2.30-4.04 GB (median 3.09) | 2.31-2.35 GB | 1.19 GB |
| Workers ended on `pagehide` | 2.22-4.04 GB (median 3.25) | 2.31-2.35 GB | 1.19 GB |
| New tab instead of reload (one run) | 1.63-1.96 GB | 1.58 GB | 1.22 GB |
| **AC21: engine 12 s after a reload** | **1.85-2.01 GB** (median 1.87) | | |

- **The 250 ms trace,** with the change, starts at the third reload. The renderer spikes to 3.4 GB at 1.6 s. The page is ready at 3.3 s. The renderer then holds a 2.2-2.3 GB plateau until 8.2 s and falls to 1.25 GB by 9.7 s.
- **About 0.7 GB of the plateau is the old page.** A load in a fresh tab ends at 1.6 GB, and the old page's share goes some 9 s after it unloaded.
- **Ending the workers changes nothing.** The browser already ends a page's dedicated workers when the page goes, so `terminate()` adds nothing. The delay is in Chrome giving the memory back, not in workers outliving the page [U on Chrome's internals]. The change was reverted.
- **With the AC21 delay**, on all 9 reloads:
  - the engine worker started 12.0 s after the page's time origin;
  - the old page's memory was gone first: the tab fell from 1.57-1.76 GB just after the reload to under 600 MB by 2.0-2.3 s, and held 224-257 MB in the 2 s before the engine started;
  - the engine then loaded in 3.2-4.8 s (fetch, sessions, warm-up), and Join audio to ready took 14.8-16.5 s, the delay included.
- **Untested.** Chrome may reclaim the old page's memory sooner under memory pressure on an 8 GB machine.

## 5. Gaps

- **Not a laptop.** 4 pinned desktop P-cores in a container stand in for one, as in S11. A laptop on battery and a WebGPU iGPU stay unmeasured.
- **The hand-back branch of AC18 did not fire**, because nothing degraded. The cause of the missing throttling is inferred, not proven. A room tab that leaves the audio stops device mode anyway.
- **Piper lines differ from S11's.** Synthesised again, each line is up to 1.3 s shorter or longer. The talk and SUMM-RE audio is identical. The 30-minute window holds 8 of the corpus's 14 SUMM-RE tracks.
- **One run per configuration** for AC18 and AC19, with other work on the host.
- **The AC21 runs shared the host** with another worker's STT stack. The self-test fell to RTFx 3.9-4.2, and one run failed it at 2.8 and was run again. This slows the engine load, not the memory figures.
- **Playwright did the reloads.** `page.reload()` stood in for a user's F5; Chrome keeping the renderer for a same-site reload is not verified for F5 [U].

## 6. Harness

`s13-device-stt-bench/` holds no audio and no secrets:
- `prep.py <out> <refs> <bench.py>` rebuilds the S11 corpus. It takes the talk from YouTube, the SUMM-RE test shards from Hugging Face, the references that `../s10-validation/prep.py` writes and the Piper lines that `../s10-stt-models/bench.py audio` renders.
- `run.sh ac18|ac19 <data> <out>` brings up the stack and the browser container, and removes them on exit. Set `MODEL_DIR`, `SERVER_BIN` and `CHROME_IMAGE` first.
- `ac18.sh` and `ac18.mjs` drive the hidden-tab run (`VISIBLE_S=60 HIDDEN_S=600 EVERY_S=18.5`).
- `ac19.mjs` drives the bench, one process per participant (`BENCH_SECS=1800`).
- §4.1 reruns `run.sh ac19` with `BENCH_SECS=1`, 3 times per build. `WARM_NEW_TAB=1` gives the new-tab row, and `SAMPLE_MS=250` gives the fine trace.
- `lib.mjs` holds the scripted mic, the recorder, the pipeline probe and the memory sampler.
- `analyze.py ac18|ac19 <out> <data>` scores a run; `analyze.py reload <out> <data>` gives the per-reload peak, engine start and memory before it.
- `results/` holds the summaries: `ac18.json`, `ac19.json`, `ac19-warm-new-tab.json`, and `ac19-reload.json` for §4.1.

The corpus audio, the model files and the run directories stay outside the repository.
