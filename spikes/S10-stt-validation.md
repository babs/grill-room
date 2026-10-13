# S10d: parakeet-ultra validated on the lab at the 4-CPU limit

## Verdict
**Go for a spec "STT engine: parakeet-ultra"** (transcribe.cpp Q8_0, greedy). On a real lab node it reproduces the workstation accuracy exactly and meets the S10 exit criterion with margin:
- meetings WER 0.232 (criterion ≤ 0.25), talk 0.121, no English leak;
- 1 stream: p95 2.8 s, RTFx 6.2; 2 concurrent streams: p95 3.9 s (criterion ≤ 5 s);
- `small` today: meetings 0.327, p95 5.7 s, and 15 s p95 once two people talk.

Hotwords work on ultra through sherpa-onnx, but they are not worth beam search in the first version: they gain Piper terms (0.31 → 0.40) and leave real speech flat, at half the throughput of greedy transcribe.cpp. Make them a follow-up phase. Qwen3-ASR-0.6B is as accurate but misses latency (p95 9.7-10.7 s). The `small` glossary prompt again hurts real speech (meetings 0.327 → 0.428, talk 0.254 → 0.507).

## Setup
- Lab workers: 4 vCPU i7-6700 (AVX2), other tenants on them. Each Job: limit 4 CPUs, **request 900m** (the smallest that placed one Job on each of four workers, which had 0.6-1.7 CPUs unrequested), one bench per node (pod anti-affinity). The nodes ran at about 3.9 of 4 cores while a bench ran, so tenant noise is in these timings. One run per configuration.
- Same corpora, segmentation and scoring as the S10b workstation bench. The in-cluster prep reproduced its cuts: SUMM-RE meetings 224 utterances / 16.7 min (gold ref), talk 27 / 6.0 min (silver ref, YouTube auto-captions), Piper 38 lines. Cuts come from the worker's own `stt/grill_stt/vad.py` (Silero v6, 800 ms end of speech, 15 s max).
- Latency = inference + 0.8 s VAD cut. RTFx = speech seconds / inference seconds. RAM = peak RSS of the engine process.
- **2 streams**: two processes with 2 threads each, run at the same time on the two longest meeting tracks (66 utterances). Each stream's latency includes waiting behind its own earlier utterances at their real arrival times (`small` and Qwen3 queue up; ultra does not). The two processes run back to back, so this is the worst case for contention. "Queue" = the worker's current design (one queue, one 4-thread engine) simulated from the 1-stream service times.
- Glossary "on": room sentence as Whisper `initial_prompt`; `--vocabulary` for Qwen3; sherpa hotwords (score 1.5, the export's `bpe.vocab`). Meetings get the 68 unrelated tech terms, which tests for harm.
- The sherpa ultra export (community int8, CC-BY-4.0) loads through sherpa's NeMo transducer path with `feature_dim=128`. Its SHA-256 sums match its model card, and its WER is within 0.01 of the GGUF on every set, so stock v3 was not needed.

## Results (lab node, 4-CPU limit)
| Engine | Gloss. | Meetings WER | Talk WER | Piper WER (fr / en) | Piper terms fr | Talk terms | EN leak | 1-stream p50 / p95 | RTFx | Queue p95 | 2-stream p95 / WER | RTFx per stream | RAM |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **ultra, tcpp greedy** | n/a | **0.232** | **0.121** | 0.224 (0.26 / 0.03) | 0.35 | 0.94 | 0 | **1.3 / 2.8 s** | **6.2** | **2.8 s** | **3.9 s** / 0.209 | **4.2** | 1.2 GB |
| ultra, sherpa beam 4 | off | 0.241 | 0.118 | 0.246 (0.28 / 0.09) | 0.31 | 0.90 | 0 | 1.9 / 5.2 s | 3.0 | 5.2 s | 4.8 s / 0.231 | 2.8 | 1.3 GB |
| ultra, sherpa beam 4 | hotwords | 0.233 | 0.117 | **0.202** (0.24 / 0.02) | 0.40 | 0.94 | 0 | 1.9 / 4.6 s | 3.1 | 4.5 s | 4.0 s / 0.225 | 3.2 | 1.3 GB |
| Qwen3-ASR-0.6B, tcpp | off | 0.227 | 0.130 | 0.296 (0.33 / 0.09) | 0.22 | 0.88 | 0 | 3.0 / 9.7 s | 1.5 | 9.8 s | 9.0 s / 0.183 | 1.5 | 1.6 GB |
| Qwen3-ASR-0.6B, tcpp | vocab | 0.231 | 0.131 | 0.559 (0.65 / 0.08) | 0.40 | 0.88 | 0 | 3.9 / 10.7 s | 1.1 | 12.5 s | 10.5 s / 0.185 | 1.0 | 1.6 GB |
| `small` int8 (+filter) | off | 0.326 | 0.257 | 0.313 (0.36 / 0.06) | 0.22 | 0.84 | 0 | 4.3 / 5.7 s | 1.2 | 9.3 s | 15.5 s / 0.301 | 0.7 | 1.2 GB |
| `small` int8 (+filter) | prompt | 0.386 | 0.515 | 0.209 (0.24 / 0.03) | 0.57 | 0.63 | 0 | 4.7 / 6.3 s | 1.1 | 13.9 s | 20.1 s / 0.381 | 0.6 | 1.2 GB |

Notes:
- Without the plausibility filter, `small` + prompt is worse still: meetings 0.428, talk 0.507.
- **ultra, lab vs workstation:** WER is identical (meetings 0.232 both, talk 0.121 vs 0.122). p95 2.8 s vs 1.7 s, RTFx 6.2 vs 14.6, which is at the low end of the S10 estimate of 2.7-4 s.
- **Two 2-thread processes beat one 4-thread process for ultra:** 2 × 4.2 = 8.4x real time against 6.2x. Most utterances in one stream fit in the gap before the next one, so its own queue stays short.
- **Hotwords on ultra:** Piper terms 0.31 → 0.40 and WER 0.246 → 0.202; talk terms 0.90 → 0.94; no harm on the off-topic meeting glossary (0.241 → 0.233). They cost nothing on top of beam search, but beam search itself halves throughput against greedy tcpp (RTFx 3.0 vs 6.2).
- **Qwen3 vocabulary:** one Piper line out of 38 printed the whole vocabulary list as its transcript. That drives its Piper WER to 0.56 (0.24 on the workstation). Real speech was unaffected.
- **No English leak** on any engine. With ultra the English Piper lines stay English (WER 0.03).

## Go / no-go: spec "STT engine: parakeet-ultra"
**Go.** Scope for the spec:
- A transcribe.cpp `Transcriber` (Q8_0 GGUF, 0.74 GB, greedy) behind the existing Silero 800 ms cuts.
- The plausibility rework from the S10 analysis, section 2.3: drop the language checks, keep the repetition and empty checks, add a Latin-script check.
- `small` stays as a fallback behind one setting.
- Hotwords (sherpa-onnx, beam 4, per-room glossary) in a later phase, gated on a real-voice term test. Piper is the only set where they help.

**Sizing (lab i7-6700 nodes):**
- **CPU per pod:** keep the 4-CPU limit. Request 2 CPUs where a node can give it; 900m schedules today.
- **Engine processes:** run 2 processes with 2 threads each, rather than 1 with 4 threads. That gives 8.4x real time in total, with p95 3.9 s at two simultaneous talkers.
- **Talkers per pod:**
  - 4 continuous talkers [E], keeping each process at most 50% busy;
  - 8-12 people in a turn-taking room [E];
  - one talker per pod today with `small`.
- **Memory:** 1.2 GB RSS per engine process. 1 process fits the current 1536 Mi limit with little margin; 2 processes need about 3 Gi.

## Reproduce
Files in `spikes/s10-validation/`:
- `job.yaml`: RWX PVC + Job, one bench per node.
- `run.sh`: renders, applies, waits, saves the log.
- `job.sh`, in-pod:
  - `setup`: venv, transcribe.cpp v0.4.0 AVX2 build, models, corpora;
  - `run`: the bench runs;
  - `score`: the scoring.
- `prep.py`, `run.py`, `score.py`: the S10b scripts, adapted to the cluster.
- `glossary.json`: the room glossaries.

The talk audio and its caption reference are copied into the PVC (`real/`) before `setup`.
```bash
export KUBECONFIG=<lab kubeconfig> NS=grill-room-stt-bench OUT=<log dir>
./run.sh setup 900m 8Gi setup
./run.sh ultra 900m 3Gi run "tcpp:parakeet-ultra-Q8_0.gguf summre none" "c2 tcpp:parakeet-ultra-Q8_0.gguf summre none"   # also devoxx, piper
./run.sh sherpa 900m 3Gi run "sherpa:ultra summre on" ...   # Qwen3: tcpp:Qwen3-ASR-0.6B-Q8_0.gguf (4Gi); control: fw:small
./run.sh score 100m 1Gi score
```
