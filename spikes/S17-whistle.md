# S17: Cactus Whistle as an STT engine, server and browser

Run on 2026-10-16 on the workstation. Whistle is `Cactus-Compute/whistle` at revision `ca528760`, on the Cactus engine from `Cactus-Compute/needle3` at revision `93e88ede`. The reference is parakeet-ultra, the engine the worker ships (spec 007), scored on the same audio. At the coordinator's request, two more engines got a server row: Orukeet and Kroko FR. Scripts and result summaries are in `s17-whistle/`.

Tags: **[M]** measured here, **[S10]** / **[S13]** from those spikes, **[P]** public claim, **[E]** estimate, **[U]** unverified.

## 1. Verdict

- **Whistle does not replace parakeet-ultra on the server.** On our French it makes nearly twice the errors. Meetings WER is 0.394 against 0.233, the talk 0.218 against 0.121 and the Piper lines 0.392 against 0.232. It is also slower per utterance: RTFx 6.1 against 12.1, talk p95 4.6 s against 2.5 s [M]. The 5-beam search is the reason. Its one strong point is memory: 43 MB per process against 1.24 GB.
- **It does not complement it either.** No server role needs a 43 MB engine that is half as accurate. Memory is not what limits the STT pod. CPU is, and Whistle uses more CPU per audio second than parakeet does.
- **Keyword biasing works, but it does not bring the glossary back.** With the 68 Piper room terms, Piper term hits go from 0.15 to 0.44 and Piper WER from 0.392 to 0.278. Real speech stays flat: meetings 0.394 to 0.395, talk 0.218 to 0.215. That is the opposite of Whisper's prompt, which made real speech worse. Even with the glossary, though, Whistle stays behind parakeet-ultra without one (Piper 0.278 against 0.232, meetings 0.395 against 0.233). The glossary needs a better engine underneath it, not this one.
- **In the browser it fails the accuracy bar.** Live for 30 minutes on desktop, the latency matches parakeet w4a8: p95 2.16 s on pause cuts, the same as S13. The download is tiny: 17.9 MB against 402 MB. The tab peaks at 0.37 GB against 1.34 GB [M]. But talk WER is 0.221 against 0.120 for parakeet w4a8 on the device. Spec 010 AC19 allows the server's WER plus 0.01 (0.137). Whistle misses it by 0.08.
- **Phones: it fails on latency and on accuracy** (emulated, §5.1). With the CPU cut to a quarter of one desktop core, decoding runs at RTFx 1.2 and the queue builds on continuous talk. End of speech to line has a p95 of 12.5 s on pause cuts and 45 s on all cuts [M, emulated]. Even the optimistic bound, desktop decode times times 4 with no contention, gives p95 7.2 s on pause cuts, above spec 010's 4 s fallback trigger. The official WASM build has one thread, so a phone's other cores cannot help. The text is the desktop's (talk WER 0.21-0.22). Whistle does not make device mode viable on phones.
- **Orukeet is worse than parakeet-ultra on our audio.** Meetings 0.244 against 0.233. Talk 0.265 against 0.121, with an English leak of 0.087 on that French talk. Same speed, same memory. Not a swap.
- **Risks.** The engine is a closed binary, published without source under an Apache-2.0 licence file. Its default thread count breaks in a CPU-limited container. The Python package sends telemetry by default.

## 2. What Whistle is (verified)

| Claim on the card | Finding |
|---|---|
| One 16.9 MB file, 2-4 bit | `whistle.cact` is 16,919,407 bytes [M]. The config says Cactus Quants, group size 128. A `checkpoints/whistle.safetensors` sits beside it. |
| Apache-2.0 | The model repo and the engine repo both carry the Apache-2.0 text, and so does the GitHub repo `cactus-compute/needle` [M]. See §7 for the source gap. |
| en/de/fr/es/it/nl/pl with language detection | Detection held up on our sets: English leak 0.010 on meetings and 0 on the talk. Forcing `fr` brings meetings to 0.390 and the leak to 0 [M]. |
| Keyword biasing during beam search | Works through `--audio-keywords` (CLI) or `keywords` (C API) [M], §4. |
| Word timestamps | In the API and the CLI. Not used here. |
| C++ engine and WebAssembly | A prebuilt `needle` CLI, `libneedle.a`, `needle.h` and an emscripten `needle.js`/`needle.wasm` per platform. The source of that engine is not public (§7). |
| 5 beams by default | Not configurable. No flag, no API call and no field in the `.cact` sets the beam width. A beam 1 run was only possible through the community ONNX port (§3.2). |
| Community ONNX and GGUF ports | `mrfakename/whistle-ONNX` (Apache-2.0) is a PyTorch reimplementation with a JS pipeline and greedy decoding. It has a WebGPU demo. `ManniX-ITA/whistle-GGUF` needs the opencoti llamafile fork. Stock ggml, whisper.cpp and transcribe.cpp do not load it. |

**Server runtimes:**
- **CLI**: `needle --model whistle.cact --audio x.wav --threads N` prints JSON. It has `--audio-language`, `--audio-keywords`, `--audio-depth` and `--audio-stream`. `--serve` exists, but its HTTP route takes text only.
- **Python**: `pip install cactus-needle`, a pure-Python package. At first use it downloads the engine wheel from the Hub and calls `libneedle3.so` through ctypes.
- **Go**: none. It would mean cgo against `libneedle.a`.
- **C API** (`needle.h`): one global, not thread-safe model per process, and no thread setting.

**Engine threads.** The engine picks its own thread count ("fast cores, at most 4"). In a 4-core cpuset it started 12 threads and ran about 100x slower: 35 s for a 4 s line, 0.7 tokens/s [M]. `--threads 4` fixes it. The C API has no such setting, so the library route (Python package or ctypes) cannot be bounded in a CPU-limited container. The bench drives the CLI, one process per utterance. That costs 30 ms per call, load included [M].

**Instruction sets.** The x86-64 binary picks a code path at run time from `cpuid`. It contains AVX-512 and VNNI code. It ran on this AVX-VNNI CPU. The AVX2-only path, the one the lab's i7-6700 nodes would take, was not tested [U].

## 3. Server bench

**Setup:**
- Workstation i7-14700F. Each run is a container of the worker image, pinned to 4 distinct P-cores (8, 10, 12, 14), with no network, 6 GB, `nice 19`. One run at a time. Other work ran on the host (load average 5-8).
- Corpora rebuilt with S10's cuts (worker Segmenter, Silero v6, 800 ms):
  - SUMM-RE meetings, 224 utterances / 16.7 min, gold reference. Its items and cuts are identical to the S10d copy;
  - the French talk, 27 / 6.0 min, silver reference;
  - the 38 Piper lines S13 rendered.
- Scoring is S10's `score.py`, unchanged: normalisation, WER, term hits and the English-leak metric.
- Latency is inference plus the 0.8 s cut, as in S10. RTFx is speech seconds over inference seconds. RAM is the peak RSS of the engine process.
- Engines:
  - parakeet-ultra: the shipped `grill_stt.parakeet` binding, transcribe.cpp 0.4.0, Q8_0, greedy, 4 threads;
  - Whistle: the official CLI, `--threads 4`, language detected, 5 beams, one process per utterance with its 30 ms start counted;
  - Orukeet: `oruk/orukeet` at `b59c13a7`, its transcribe.cpp Q8_0 GGUF (SHA-256 matches its card), through the same shipped binding;
  - Kroko FR community: the k2-fsa sherpa-onnx export of 2025-08-06, streaming zipformer, greedy, 4 threads.
- Concurrency: c2 runs 2 processes of 2 threads, c4 runs 4 processes of 1 thread. Each process has its own cores and plays one of the longest meeting tracks. Each utterance is submitted at its real arrival time, so latency includes waiting behind the stream's earlier utterances.

### 3.1 Results

| Engine | Meetings WER | Talk WER | Piper WER (fr / en) | Piper terms fr | Talk terms | EN leak, talk / meetings | Meetings p50 / p95 | Talk p95 | RTFx (meetings) | RAM | 2 streams p95 | 4 streams p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **parakeet-ultra** (shipped) | **0.233** | **0.121** | **0.232** (0.27 / 0.05) | 0.31 | **0.94** | 0 / 0.002 | 1.05 / 1.84 s | 2.50 s | 12.1 | 1.24 GB | **2.32 s** | **4.02 s** |
| Whistle, detected language | 0.394 | 0.218 | 0.392 (0.45 / 0.09) | 0.15 | 0.63 | 0 / 0.010 | 1.30 / 2.98 s | 4.60 s | 6.1 | **43 MB** | 3.21 s | 4.62 s |
| Whistle + Piper keywords | 0.395 | 0.215 | 0.278 (0.32 / 0.03) | **0.44** | 0.55 | 0 / 0.009 | 1.30 / 3.40 s | 2.71 s | 5.5 | 44 MB | | |
| Whistle + the talk's own keywords | | 0.222 | | | 0.76 | 0 / - | | 2.94 s | 8.9 | 45 MB | | |
| Whistle, `--audio-language fr` | 0.390 | 0.218 | | | 0.63 | 0 / 0 | 1.22 / 2.66 s | 3.97 s | 6.8 | 43 MB | | |
| Whistle, `--audio-depth 4` | 0.414 | 0.252 | 0.421 (0.47 / 0.14) | 0.08 | 0.49 | 0 / 0.010 | 1.14 / 2.52 s | 6.33 s | 8.0 | 43 MB | | |
| Whistle ONNX port, greedy, fp32 | 0.435 | 0.229 | 0.404 (0.46 / 0.09) | 0.10 | 0.59 | 0 / 0.008 | 1.02 / 1.61 s | 1.94 s | 14.6 | ~0.6 GB (Node) | | |
| Orukeet Q8_0 | 0.244 | 0.265 | 0.268 (0.30 / 0.09) | 0.33 | 0.86 | **0.087** / 0.011 | 1.07 / 1.90 s | 2.21 s | 11.2 | 1.24 GB | 2.42 s | 3.99 s |
| Kroko FR community | 0.292 | 0.176 | 0.530 (0.44 / **1.00**) | 0.14 | 0.57 | 0 / 0 | 0.96 / 1.59 s | 1.94 s | 18.5 | 188 MB | | |

Notes:
- **parakeet reproduces S10**: meetings 0.233 against 0.232, talk 0.121 against 0.121. This workstation is about twice as fast as a lab node (RTFx 12.1 here, 6.2 there [S10]), so the latencies here are not lab latencies.
- **Why Whistle is slow**: the beam search over long transcripts. Its p95 is worst on the talk, whose 15 s cuts carry the most words. Greedy decoding through the ONNX port runs 2.4x faster (RTFx 14.6), even in fp32.
- **Concurrency**: at 4 streams every engine sits near p95 4 s on these 4 cores. With 2 streams, Whistle's slowest stream runs at RTFx 5.5 against 7.9 for parakeet.
- **Kroko** is a French-only model. The English Piper lines come out as French (WER 1.0), and English words are part of every grill-room room. It is not a candidate. Its talk WER (0.176) is the only number here between Whistle and parakeet.
- **Defect found and fixed during the run.** The first Whistle meeting runs wrote full-scale samples as `-2^31` because a float32 product overflowed (27 of 224 utterances, 1,270 samples). The four single-stream Whistle meeting rows were run again after the fix. The concurrency runs started after it.

### 3.2 Beam 1 against the default 5 beams

The official engine has no beam setting, so the comparison uses the ONNX port's greedy decoder. That port runs fp32 weights dequantized from the same `.cact`, so it is a proxy, not the engine at beam 1.

| | Meetings | Talk | Piper | Meetings p95 | Talk p95 | RTFx |
|---|---|---|---|---|---|---|
| Engine, 5 beams (quantized kernels) | 0.394 | 0.218 | 0.392 | 2.98 s | 4.60 s | 6.1 |
| Port, greedy (fp32) | 0.435 | 0.229 | 0.404 | 1.61 s | 1.94 s | 14.6 |

Greedy costs 1-4 points of WER and halves the latency. Neither setting comes near parakeet-ultra.

## 4. Keyword biasing and the room glossary

- **Synthetic speech, the glossary on its own topic**: Piper term hits go from 0.15 to 0.44, French WER from 0.45 to 0.32, English from 0.09 to 0.03 [M].
- **Real speech, the same 68 terms off topic**: meetings 0.394 to 0.395, talk 0.218 to 0.215. There is no harm [M]. Meeting latency gets a little worse: p95 2.98 to 3.40 s.
- **Real speech, on topic**: with the talk's own 31 terms, talk term hits go from 0.63 to 0.76 and WER from 0.218 to 0.222 [M].
- **Against what we have**: S10d's sherpa hotwords on parakeet-ultra took Piper terms from 0.31 to 0.40, also with no harm on real speech, at half the throughput [S10]. Whistle's biasing is the strongest boost measured so far (0.15 to 0.44). It starts from a much lower base, though, and the terms that matter on real speech (talk terms 0.76) stay below parakeet with no glossary (0.94).

So keyword biasing is the right mechanism: it helps where the terms appear and leaves the rest alone, unlike Whisper's prompt. It would revive the glossary on an engine as accurate as parakeet-ultra. On Whistle, the glossary only buys back part of the accuracy the engine loses.

## 5. Browser

**Setup.** The S11/S13 approach: Playwright 1.64 drives Google Chrome 155 (`channel: 'chrome'`), headed under Xvfb, in a container. The "laptop" profile pins cores 0, 2, 4 and 6, with `nice 10` and a 16 GB cap. The page is S11's design:
- an AudioWorklet feeds 512-sample windows to a VAD worker running Silero v6 on single-thread onnxruntime-web, with the port of the worker's Segmenter (800 ms cut);
- the STT worker loads the official `needle.js`/`needle.wasm` with `whistle.cact`, both fetched with a pinned SHA-256 into Cache Storage, and decodes each utterance once;
- the corpus is S13's: the first 30 minutes of `all.wav` through Chrome's fake microphone.
- Latency runs from the receive time of the last voiced VAD window to the end of the decode, as in S11 and S13 (S13 adds the post to the server, a few ms on loopback).
- RAM is the RSS of the largest renderer, sampled every 2 s, as in S13.

The official WASM build is single-threaded, SIMD and CPU only: no pthreads, no WebGPU. The container has no WebGPU adapter. WebGPU exists only through the community ONNX port, and was not measured.

| | parakeet-ultra w4a8, WASM 4 threads [S13] | Whistle, official WASM, 1 thread [M] |
|---|---|---|
| Download | 402 MB | **17.9 MB** (`whistle.cact` 16.9 MB, `needle.wasm` 0.93 MB, `needle.js` 63 KB). The Silero VAD still needs onnxruntime-web |
| Cold load, loopback | 6.51 s, self-test included | 0.29 s (fetch and hash 0.24 s, load 0.03 s) |
| Warm load | 2.09-2.73 s | 0.05-0.07 s (3 reloads) |
| End of speech to line, p50 / p95, pause cuts | 1.44 / 2.16 s | 1.23 / **2.16 s** (n = 186) |
| p95, all cuts | 2.19 s | 2.49 s (15 s cuts: p50 2.18, p95 2.89 s) |
| Queue | | never waited |
| Talk WER (spec 010 limit 0.137) | **0.120** | 0.221 |
| Meetings WER (masked tracks in the first 30 min) | **0.153** | 0.370 |
| Piper WER fr / en | 0.260 / 0.094 | 0.456 / 0.062 |
| English leak | 0.001 | 0 talk, 0.007 meetings |
| Peak tab memory (largest renderer) | 1.34 GB | **0.37 GB** live, 0.21 GB just after load |
| CPU while playing | 0.54 core | 0.13 core |
| Whole corpus in batch (43 min) | RTFx 5.05 | RTFx 7.0. Talk 0.213, meetings 0.362 |
| Lines in pauses | none | none |

Whistle in the browser gives the same text as the native engine: the same transcript on the check clip, and talk WER 0.213-0.221 against 0.218. Latency and memory are the best measured in any device run. Accuracy is the blocker.

### 5.1 Phone feasibility (emulated)

**This is an emulation, not a phone.** The profile is a 390x844 viewport at DPR 3, a mobile Android user agent and touch, with CPU cut 4x.

- **DevTools throttling does not work here.** `Emulation.setCPUThrottlingRate` slows only the page's main thread. Sent to a worker target, Chrome answers "Operation is only supported for pages, not workers". A decode in the worker did not slow down at all (54.7 s against 61.4 s for the same 30 utterances).
- **The CPU cut is a cgroup quota instead**: one core at 25% (5 ms every 20 ms). The same 30 utterances then took 247.7 s to decode, 4.03x the desktop [M].
- **A live run under that quota was unusable.** The quota also starved Chrome's fake-microphone capture: lines drifted and broke apart, and three appeared in pauses. That run is discarded as an artifact of the emulation.
- **Latency comes from a replay instead.** The first 30 minutes were decoded in batch under the quota (measured decode times). `replay.py` then feeds each utterance to a FIFO decoder at its real cut time. Replaying the desktop batch the same way gives 1.26 / 2.18 s on pause cuts against 1.23 / 2.16 s live, and 2.66 against 2.49 s p95 on all cuts. The method is close, and slightly pessimistic.

| Emulated phone | Quota run (measured decode) | Desktop decode x4 (bound) | Desktop decode x3 |
|---|---|---|---|
| Decode speed | RTFx 1.22 (5.7x slower: VAD and decode share the quarter core) | RTFx 1.75 | RTFx 2.3 |
| End of speech to line, pause cuts p50 / p95 | 3.33 / **12.5 s** | 2.65 / 7.22 s | 2.14 / 5.25 s |
| All cuts p50 / p95, max | 3.65 / 45.0 s, max 73.6 s | 2.88 / 10.3 s, max 13.1 s | 2.29 / 7.75 s, max 9.8 s |
| Queue p95 / max | 27.9 / 69.8 s | 2.1 / 6.4 s | 0 / 3.9 s |
| Talk only, p50 / p95 | 38.2 / 73.6 s | | |
| Piper lines only (short, 2 s apart), p50 / p95 | 3.29 / 4.84 s | | |
| Cold load, loopback | 2.04 s | | |
| Tab memory | 0.81 GB in batch (the page holds the 83 MB WAV); live, the desktop's 0.37 GB [E] | | |

- **Continuous speech drowns it.** The talk's 15 s cuts each take longer to decode than they last (RTFx 1.2), so the queue grows for as long as the talker talks. Short turns with pauses (the Piper lines) stay near 3-5 s.
- **Every column misses spec 010's limits.** AC17 hands the track back above a 4 s p95; AC19 asks for 3.0 s on pause cuts. A phone 3x slower than this desktop core still misses (5.25 s).

parakeet-ultra w4a8 under the same cut would decode at about RTFx 1.3 (5.05 / 4) [E] and fall behind on continuous speech. Spec 010 keeps phones out anyway.

Not covered: a real phone, mobile Chrome, thermal throttling, battery, a backgrounded mobile tab, and big.LITTLE scheduling.

## 6. Recommendations

| Question | Answer |
|---|---|
| Replace parakeet-ultra on the server? | **No.** Meetings WER is +0.16 and talk WER +0.10, and it is half as fast. |
| Complement it on the server, for example as a low-memory fallback? | **No.** The pod is limited by CPU, not memory, and Whistle uses more CPU per audio second. |
| Replace w4a8 on the device? | **No.** Talk WER 0.221 misses AC19's 0.137. Keep parakeet-ultra w4a8. |
| Complement it on the device, for example on desktops that fail the RTFx 3 self-test? | Possible, but not worth it today. Its lines would be half as accurate as the server fallback those desktops already get. |
| Revive the room glossary through keyword biasing? | **Not with Whistle.** The mechanism is right (+29 points of Piper terms, no harm on real speech). Pursue it on parakeet-ultra: S10d's sherpa hotwords, or a transcribe.cpp phrase-boost if one appears. |
| Make device mode viable on phones? | **No.** Emulated, p95 is 7-12 s on pause cuts with a single-threaded engine, and the accuracy is the desktop's 0.22 talk WER. Phones stay on the server. |
| Orukeet instead of parakeet-ultra? | **No.** It loads unchanged in our binding, but talk WER 0.265 and an English leak of 0.087 rule it out. |
| CoSE-E as a 4th corpus? | **Skipped.** See §8. |
| Look at again when | Cactus publishes the engine source, a larger Whistle rung appears, or a beam setting lands. The cost to re-run is `run-server.sh` plus `run-browser.sh`, about 2 hours. |

## 7. Licence and provenance

| Item | Licence | Risk |
|---|---|---|
| `whistle.cact` and `whistle.safetensors` (Cactus Compute, Inc.) | Apache-2.0 (LICENSE in the repo) | Low on the terms. The training data is not listed. The card says only that no test audio is in it. |
| Cactus engine binaries (`needle`, `libneedle.a`, `needle.wasm`, `libneedle3.so` in the wheels) | Apache-2.0 file in `needle3` | **High.** No source anywhere public. GitHub `cactus-compute/needle` holds Python only, and its release workflow says the engine wheels "are uploaded separately". We could not audit the binary, rebuild it for the AVX2 lab nodes, or fix the thread-count bug. Upstream controls every update. |
| `cactus-needle` Python package | Apache-2.0 | **Telemetry is on by default**: a usage event to a Supabase endpoint on each `transcribe`/`stream` call, with an anonymous id, version, OS and arch. `NEEDLE_TELEMETRY=0` or `DO_NOT_TRACK=1` turns it off. The engine library imports no network client: the CLI only `bind`s/`listen`s for `--serve`, and the library reads no environment variable. The repo README says "telemetry is turned on in the binary". The binaries do not show it, so it lives in the Python package [M on symbols; all runs used `--network none`]. |
| `mrfakename/whistle-ONNX` | Apache-2.0 | A one-person port. It reimplements the model, and its keyword bias "approximates the engine's". |
| `ManniX-ITA/whistle-GGUF` | Apache-2.0 | Needs a fork runtime (opencoti llamafile). |
| Orukeet weights | **CC-BY-SA-4.0** (adaptation of NVIDIA Parakeet TDT v3, CC-BY-4.0); code MIT | See below. |
| Kroko FR community | CC-BY-SA, stated in prose; the model's LICENSE file is empty and the card metadata says "other" | Unclear; not a candidate anyway. |

**CC-BY-SA for us (Orukeet).** Running the weights unmodified on our server, and even shipping them unmodified in an image or to a browser, is fine. ShareAlike applies to adaptations we distribute: a fine-tune, a requantisation or a merge that we publish would have to be CC-BY-SA too. Transcripts are output, not an adaptation. What we must do is attribution: name Orukeet, Oruk AI and NVIDIA's Parakeet, link the licence, and keep `NOTICE.md`, the same way we credit parakeet-ultra (CC-BY-4.0) today. Do not requantise Orukeet ourselves for the browser and then serve that file, unless we accept CC-BY-SA on it. This is a reading of the licence, not legal advice. Since Orukeet loses on accuracy, the question is moot today.

**Orukeet in the browser.** An ONNX build exists: `onnx/combined-v0.1.0-int8`, in the same layout parakeet.js loads, with a 653 MB int8 encoder. There is no w4a8 build, so it would be 1.6x our download. It was not run in the browser.

## 8. CoSE-E

ServiceNow's CoSE-E (arXiv 2609.35645) has an `en_fr` subset with 298 code-switched utterances, on `ServiceNow-AI/asr_codeswitched`. Skipped:
- the dataset card has no licence: no `license` field, and the README is YAML only. The paper states none either. With no licence, using it is not clearly permitted.
- all its audio is ElevenLabs TTS output, so the terms of a third-party service apply on top. The paper itself warns that the voices are synthetic and may favour some engines.
Ask ServiceNow for a licence if it is wanted. It would be a 4th corpus for `prep.py`, using the existing scoring.

## 9. Integration cost, if it were wanted [E]

| Piece | Work |
|---|---|
| Server Transcriber | A subprocess per utterance with `--threads`, about 80 lines plus tests. The C API cannot bound threads, and a long-lived process needs HTTP audio, which the engine does not have. |
| Image | A stage that fetches the pinned `needle` binary and `whistle.cact`, plus licence files. A test on the AVX2 lab nodes first. |
| Glossary | Pass the room terms as `--audio-keywords`, and show the glossary tab again for this engine (007-AC14 already switches on the heartbeat's `glossary` field). |
| Device | Swap the STT worker for `needle.wasm` (about 100 lines, done here in `web/stt.js`), new manifest entries and a new self-test bar. The threads and COOP/COEP stop mattering for the engine. |
| Total | About 3-4 days for server and device. Owning a closed engine adds work on every update. |

## 10. Harness

`s17-whistle/` holds no audio, no model and no secret.
- `prep.py`: rebuilds S10's three corpora with the worker's Segmenter, inside the worker image.
- `bench.py` and `run-server.sh`: one engine over one corpus, and every server run of §3.
- `score.py`: S10's scoring plus the concurrency latency. `results/server.md` and `results/server.json` hold the scores.
- `onnx-greedy.mjs`: the greedy run through the ONNX port.
- `web/` and `run-browser.sh`: the browser page, the workers and the Playwright driver. `PHONE=1` sets the phone quota.
- `analyze-browser.py`: scores a browser run. `replay.py`: replays a batch run at real-time arrivals.
- `results/browser.json`: the browser summaries.

```bash
./run-server.sh <data> <engines>                 # data: corpus/ from prep.py; engines: needle, whistle.cact, orukeet.gguf, kroko/, pydeps/
D=<data> ./score.py
CHROME_IMAGE=<image> ./run-browser.sh <web> --tag live-desktop --mode live --secs 1800
CHROME_IMAGE=<image> PHONE=1 ./run-browser.sh <web> --tag bench-phone --mode bench --secs 1800 --phone
./analyze-browser.py <web>/runs <web>/data/manifest.json live-desktop
./replay.py <web>/runs 1800 bench-phone
```
