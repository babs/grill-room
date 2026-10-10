# S1-bis: faster-whisper int8 on CPU, FR/EN/mixed

## Verdict
CPU STT is viable with `small` int8. On 2-5 s chunks, one continuous speaker needs about 1.1 P-cores; one P-core alone runs at RTF 1.08, just short of real time. 2 P-cores give about 2.9 s latency, 4 give about 2.1 s. `medium` and `large-v3-turbo` are not real-time on 4 cores with short chunks. Mixed FR/EN works only with per-chunk language detection. Every chunk pays for a full 30 s Whisper encoder pass, because faster-whisper pads to 30 s. With auto language it pays for two, because faster-whisper 1.2.1 throws away the encoder output of the detection pass. Cost per chunk is flat, so a 2 s chunk costs the same as a 15 s one.

## Setup
- Host: i7-14700F desktop. P-cores are CPUs 0-15 (8 physical cores with SMT), E-cores are CPUs 16-27. Shared with the user's workload, runs under `nice -n 19`, so treat timings as upper bounds.
- faster-whisper 1.2.1, ctranslate2 4.8.2, Python 3.14, `compute_type=int8`, `beam_size=1`. Threads pinned with `taskset`, one thread per physical core: P = `8`, `8,10`, `8,10,12,14`; E = `16`, `16,17`, `16-19`. Distil models skipped: every distil-whisper model in faster-whisper is English-only, per the model cards.
- Audio: Google FLEURS dev split, CC-BY-4.0, read speech. `fr`: 4 sentences, 46 s. `en`: 4 sentences, 37 s. `mixed`: FR/EN/FR/EN/FR concatenated, 40 s. Resampled to 16 kHz mono and peak-normalised per sentence; some EN takes peak at -40 dBFS and silero dropped them. Reference: FLEURS raw transcripts.
- Earlier N-parallel concurrency numbers are discarded. They ran unpinned across mixed P/E cores on the shared host, so "streams per 28 cores" meant nothing.

## Results: one 3.6 s chunk (VAD-cut, auto language), single stream
| Model | Cores | Threads | Infer / chunk | Chunk RTF | Cores per real-time stream | Latency (0.5 s VAD + infer) | RAM steady / peak |
|---|---|---|---|---|---|---|---|
| small | P | 1 | 3.87 s | 1.08 | 1.1 | 4.4 s | 0.73 / 1.25 GB |
| small | P | 2 | 2.44 s | 0.68 | 1.4 | 2.9 s | |
| small | P | 4 | 1.63 s | 0.45 | 1.8 | 2.1 s | |
| small | E | 1 / 2 / 4 | 10.95 / 6.72 / 3.67 s | 3.04 / 1.87 / 1.02 | 3.0 / 3.7 / 4.1 | 11.5 / 7.2 / 4.2 s | |
| medium | P | 1 * / 4 | 11.4 / 6.15 s | 3.2 / 1.71 | 3.2 / 6.8 | 11.9 / 6.7 s | 1.95 / 3.2 GB |
| large-v3-turbo | P | 1 * / 2 / 4 | 19.1 / 13.4 / 7.8 s | 5.3 / 3.71 / 2.18 | 5.3 / 7.4 / 8.7 | 19.6 / 13.9 / 8.3 s | 1.88 / 2.2 GB |
| large-v3-turbo | E | 4 | 18.8 s | 5.22 | 20.9 | 19.3 s | |

\* p50 over 31 chunks from the `stream` run on CPU 8 (run before the switch to nice 19). Cores per stream = threads × chunk RTF. Steady RAM is RSS after warm-up; peak includes the fp16-to-int8 load and onnxruntime VAD. Whole-file RTF on one P-core, 30 s windows, no chunking: small 0.21, medium 0.63, turbo 0.87. That is about 5x cheaper than 2-5 s chunks and sets the ceiling for longer chunks.

## Accuracy (rough WER, 80-120 words per clip, about ±0.05 noise)
| Mode | small fr / en / mixed | medium | large-v3-turbo |
|---|---|---|---|
| Whole file | 0.13 / 0.19 / 0.34 | 0.13 / 0.16 / 0.27 | 0.13 / 0.13 / 0.28 |
| VAD chunks ≤5 s | 0.26 / 0.21 / 0.08 | 0.20 / 0.25 / 0.06 | 0.17 / 0.16 / 0.03 |
| VAD chunks cut on pauses, ≤30 s | 0.15 / 0.21 / 0.04 | not run | 0.06 / 0.14 / 0.01 |

Hard 5 s cuts split sentences and roughly double French WER for `small`. Cutting on pauses (median chunk 7 s here) keeps whole-file accuracy at the same per-chunk latency, and drops small's RTF on one P-core to 0.56.

## Mixed-language behaviour
- Whole file: Whisper detects one language for the first 30 s window (`fr`) and keeps it. `small` drops the English sentences. `medium` and `turbo` translate them into French ("The major religion in Moldova..." became "La religion majoritaire en Moldova...").
- Per chunk with `language=None`: 8/8 chunks correct for all three models at ≤5 s, and 5/5 for small and turbo at ≤30 s. This is the mode to use (D16).
- Forcing `language="fr"` cuts small's p50 (4 threads) from 2.17 s to 1.23 s per chunk but makes English unusable (WER 0.89). The fix for that cost is to encode once and reuse the encoder output for detection and decoding, which needs a patch or direct ctranslate2 calls. Not tested: code-switching inside a sentence (no freely licensed sample found).
- Seen hallucinations: a 0.6 s noise chunk gave "En tout cas, merci d'avoir regardé cette vidéo.", and a whole-file run gave "Sous-titres réalisés par la communauté d'Amara.org". Needs a minimum chunk length and a `no_speech_prob` filter.

## Recommendation and sizing
- `small`, int8, auto language per chunk, VAD `min_silence_duration_ms=500`, chunks cut on pauses (max about 15-30 s), not at a fixed 5 s. One model per room pod, shared by every speaker through a queue.
- Threads per stream: 2 to 4 for about 2-3 s latency. 1 thread is real-time only with pause-cut chunks (RTF 0.56, about 5 s latency).
- Sizing rule on this CPU, per continuously talking speaker with 2-5 s chunks: about 1.1-1.8 P-cores or 3-4 E-cores. With pause-cut chunks, about 0.6-1 P-core.
- Size for simultaneous speech, not for participants. A grill room usually has one talker at a time, so start with 4 P-class cores, a 1.5 GiB memory limit, and a queue for overlaps.
- These numbers come from one desktop CPU. Redo the sizing on the target kube node CPU (server SKU, SMT, AVX-512/VNNI), where one kube CPU is usually one SMT thread, not one physical core.

## Reproduce
```bash
cd spikes/s1bis
export UV_PROJECT_ENVIRONMENT=$PWD/../../.scratchpad/s1bis/venv UV_CACHE_DIR=$PWD/../../.scratchpad/s1bis/uv-cache
uv sync && uv run ./bench.py fetch          # HF_HOME defaults to .scratchpad/s1bis/hf
nice -n 19 taskset -c 8 uv run ./bench.py rtf --threads 1
nice -n 19 taskset -c 8 uv run ./bench.py stream --threads 1                # add --max-chunk 30 for pause cuts, --lang fr to force
for m in small medium large-v3-turbo; do nice -n 19 taskset -c 8,10,12,14 uv run ./bench.py chunk --model $m --threads 4; done
# results: .scratchpad/s1bis/results-{rtf,stream,chunk}.jsonl
```

## Disk
4.4 GB in `.scratchpad/s1bis/`: models 3.4 GB (`hf/`), venv 0.4 GB, uv cache 0.6 GB, audio 4 MB. Delete with `rm -rf .scratchpad/s1bis`. Code lives in `spikes/s1bis/`.
