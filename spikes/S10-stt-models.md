# S10: CPU speech-to-text for French with English technical terms

## Verdict
**Ship `small` + the room glossary as `initial_prompt` now.** On 38 Piper lines it cuts WER from 0.32 to 0.21 (French 0.37 to 0.25) and doubles French term accuracy, 0.25 to 0.53, against a ceiling of about 0.68 set by Piper's French voices. It costs about 10% more per line: p95 4.6 s on a 4-CPU limit, p95 latency 5.4 s, so AC7 holds. Do not use `hotwords`. Alone it does worse than the prompt. Added to the prompt (399 tokens), WER goes back to the baseline 0.32 and 4 lines get dropped as loops. `medium` + glossary is only 0.02 WER better at 3x the cost (RTF 2.5) and misses AC7. `large-v3-turbo` is no better than `small` + glossary and costs 4x. Parakeet v3 is 4-6x cheaper than `small` at baseline quality but has no biasing hook, so it is a capacity fallback, not a quality fix. Kyutai and Granite run 7-16x slower than real time on these CPUs. I skipped Voxtral without a run (see Skipped).

## Setup
- Lab nodes have 4 CPUs (i7-6700, AVX2, no AVX-512 or VNNI) and run other tenants. A 2-CPU request fit on no worker (57-84% already requested), so Jobs requested 1400m with limits of 4 and 2 CPUs. A limit above 4 is impossible on 4-CPU nodes, so no bigger reference run. Threads = CPU limit.
- Audio: `script.json` has 32 French lines dense in English terms and 6 English lines, 60 terms in all. Piper 1.x rendered it in the Job: French alternates `fr_FR-siwis-medium` and `fr_FR-mls-medium` speaker 84, English uses `en_US-joe-medium` and `en_GB-cori-medium` (all allowed by `scenarios/VOICES.md`). One line is one utterance with the worker's 0.2 s padding: 3.0-7.2 s, median 4.3 s.
- Whisper runs the worker's path: int8, `beam_size=1`, T=0, one encoder pass for language detection and decoding. Glossary = the 60 terms + 8 unused distractors, sorted, as `Glossaire : a, b, ...` (202 tokens; faster-whisper keeps the last 223). Others: Parakeet and Canary int8 through `onnx-asr` (ONNX Runtime), Kyutai fp32 and Granite bf16 through transformers on torch CPU.
- Scoring approximates what the worker would post: its plausibility filter (log-prob, gzip repetition, language confidence, on the joined text) blanks a line, and the repetition check applies to every model. WER is word-level after lowercasing and stripping punctuation. A term counts only if its exact words appear, case-insensitive ("Argo CD" misses "ArgoCD", "P95" hits "p95"). Latency = 0.8 s VAD cut + inference. AC7 "yes" needs p95 latency ≤ 10 s and RTF < 1, so back-to-back speech does not queue.

## Results, 4-CPU limit, 38 lines
| Model | Glossary | WER all | WER fr siwis / mls | WER en | Terms fr / en | Infer p50 / p95 s | RTF | RAM GB | AC7 |
|---|---|---|---|---|---|---|---|---|---|
| small | none | 0.32 | 0.24 / 0.49 | 0.06 | 0.25 / 0.92 | 3.41 / 4.30 | 0.78 | 1.3 | yes |
| small | hotwords | 0.27 | 0.19 / 0.44 | 0.03 | 0.44 / 1.00 | 3.66 / 4.56 | 0.84 | 1.2 | yes |
| small | prompt | **0.21** | 0.15 / 0.34 | 0.03 | **0.53** / 1.00 | 3.74 / 4.64 | 0.83 | 1.2 | yes |
| small | both | 0.32 | 0.24 / 0.51 | 0.03 | 0.42 / 1.00 | 4.22 / 5.17 | 0.93 | 1.3 | yes |
| medium | none | 0.29 | 0.22 / 0.44 | 0.06 | 0.32 / 0.92 | 10.07 / 10.59 | 2.15 | 2.7 | no |
| medium | prompt | 0.19 | 0.17 / 0.27 | 0.05 | 0.58 / 1.00 | 11.41 / 13.06 | 2.50 | 2.4 | no |
| large-v3-turbo | none | 0.25 | 0.17 / 0.42 | 0.03 | 0.28 / 1.00 | 14.60 / 16.25 | 3.16 | 2.5 | no |
| large-v3-turbo | prompt | 0.20 | 0.15 / 0.35 | 0.03 | 0.43 / 1.00 | 15.11 / 16.71 | 3.23 | 2.3 | no |
| parakeet-tdt-0.6b-v3 | n/a | 0.28 | 0.21 / 0.42 | 0.11 | 0.22 / 0.77 | 0.83 / 1.25 | 0.19 | 1.0 | yes |
| canary-1b-v2 * | n/a | 0.28 | 0.21 / 0.44 | 0.05 | 0.25 / 0.85 | 1.82 / 3.18 | 0.42 | 2.1 | yes |

\* Canary has no language detection; it was given each line's true language. Whisper detected the language right on 38/38 lines in every run. English terms in English lines are solved by every Whisper variant (0.92-1.00).

Slow models ran on a subset, so compare them with `small` on the same lines:
- Kyutai `stt-1b-en_fr`, every 2nd line (19, SIWIS voice + 3 English): WER 0.26 against 0.21 for `small` and 0.13 for `small` + prompt. Terms fr 0.21. Infer p50 26 s, RTF 7.6, 5.6 GB. The transformers reference code; faster runtimes untested.
- Granite Speech 4.0 1B, every 4th line (10): WER 0.19, or 0.12 with its `Keywords:` list (`small`: 0.25, + prompt 0.13). Terms fr 0.25 to 0.50. Infer p50 47 s (64 s with keywords), RTF 12-16, 4.5 GB. fp32 would need about 8 GB, more than a node has free.

## Results, 2-CPU limit (every 2nd line, 19)
| Model | Infer p50 / p95 s at 2 CPUs | RTF at 2 | Same lines at 4 CPUs | WER |
|---|---|---|---|---|
| small, none | 3.58 / 4.27 | 0.96 | 3.34 / 4.00 | 0.21 |
| small, prompt | 4.13 / 4.48 | 1.08 | 3.81 / 4.32 | 0.13 |
| medium, prompt | 12.76 / 13.95 | 3.34 | 11.41 / 13.06 | 0.16 |
| large-v3-turbo, prompt | 19.64 / 21.18 | 5.18 | 14.80 / 15.68 | 0.13 |
| parakeet v3 | 0.63 / 0.71 | 0.16 | 0.78 / 0.99 | 0.19 |
| canary-1b-v2 * | 1.45 / 1.75 | 0.39 | 1.57 / 2.44 | 0.18 |

`small` gains only 0.2-0.3 s from 2 to 4 CPUs on these shared nodes.

## 005-AC23: the room server's sentence prompt
The shipped prompt is one French sentence built by the room server, glossary last: `prompt.txt` is what it served for these 68 terms in a room with Alice and its Grill persona (206 tokens). Same Job, 4-CPU limit, 38 lines:

| Glossary | WER all / fr / en | Terms all / fr / en | Infer p50 / p95 s | Dropped |
|---|---|---|---|---|
| none | 0.32 / 0.37 / 0.05 | 0.33 / 0.24 / 0.85 | 3.53 / 4.03 | 0 |
| prompt (comma list) | 0.23 / 0.27 / 0.02 | 0.55 / 0.47 / 1.00 | 3.91 / 4.67 | 1 |
| sentence (shipped) | **0.19** / 0.22 / 0.02 | **0.65** / 0.58 / 1.00 | 3.88 / 4.79 | 0 |

The sentence beats the comma list on every column and turns no line into a list. `./run.sh glossary-4 4 3Gi 1 whisper:small:none whisper:small:prompt whisper:small:sentence`.

## Biases
- Piper speaks English terms with French phonetics. 23 of 72 French term occurrences were missed by every model in every configuration: "rollout" came out as "relou" from all of them, "l'ingress" as "Aline KS". So French term accuracy tops out near 0.68 here. A real speaker who says the terms the English way should score higher; no recording exists in the repo to check.
- The MLS voice (speaker 84) doubles French WER against SIWIS in every model. Some of its lines are garbled at the source.
- The glossary is an oracle: it holds every term the script uses, plus 8 unused ones. None of the 8 leaked into a transcript, but the comma-list prompt sometimes turns output into a comma list ("Cache, redis, satur, ..."). A sentence-style prompt is untested.

## Skipped
- Voxtral Mini 3B (Apache-2.0) needs about 9.5 GB in bf16, and Voxtral Mini 4B Realtime (Apache-2.0) is 8.9 GB in bf16 with a 16 GB GPU stated. Both exceed the RAM free on a lab node, and the 2B Granite already runs at RTF 12-16 here.
- Canary 180M-flash and 1B-flash: same family and runtime path as the tested 1B v2, which is the newer multilingual one.
- NeMo itself: `onnx-asr` runs the same checkpoints without torch.

## Recommendation and sizing
- Now: per-room glossary as faster-whisper `initial_prompt` on `small`, no `hotwords`, capped near 200 tokens. Measure again on real voices before tuning further.
- Sizing on these nodes: `small` + glossary needs about 1 s of compute per 1 s of speech at 2 CPUs (RTF 1.08 on the subset) and 0.83 at 4 CPUs (full set). Plan **2 CPUs per concurrently talking speaker** when people take turns with pauses, 4 for one long monologue, and one talker per pod at a time with overlaps queued. A 4-CPU limit does not buy a second speaker.
- If rooms need several simultaneous talkers per pod: Parakeet v3 handles about 5 per 2 CPUs (RTF 0.16) at `small`-baseline quality, without the glossary gain.
- `medium` or `turbo` only on a faster CPU (AVX-512/VNNI) or a GPU.

## Reproduce
Files in `spikes/s10-stt-models/`: `script.json`, `bench.py` (audio, run, report, selftest), `job.sh` (in-pod venvs, runs), `job.yaml` (PVC + Job), `run.sh` (renders, applies, waits, saves the log).
```bash
export KUBECONFIG=<lab kubeconfig> NS=grill-room-stt-bench STORAGE_CLASS=<rwo class> OUT=<log dir>
./run.sh small-4 4 3Gi 1 whisper:small:none whisper:small:hot whisper:small:prompt whisper:small:both
./run.sh onnx-4 4 4Gi 1 onnx:nemo-parakeet-tdt-0.6b-v3:none onnx:nemo-canary-1b-v2:none
./run.sh kyutai-4 4 6Gi 2 kyutai:kyutai/stt-1b-en_fr-trfs:none
./run.sh granite-4 4 6Gi 4 granite:ibm-granite/granite-4.0-1b-speech:none granite:ibm-granite/granite-4.0-1b-speech:prompt
./run.sh whisper-4 4 3Gi 1 whisper:{small:none,small:hot,small:prompt,small:both,medium:none,medium:prompt,large-v3-turbo:none,large-v3-turbo:prompt}
./run.sh cpu-2 2 3Gi 2 whisper:small:none whisper:small:prompt whisper:medium:prompt whisper:large-v3-turbo:prompt onnx:nemo-parakeet-tdt-0.6b-v3:none onnx:nemo-canary-1b-v2:none
./bench.py report "$OUT"/*.log [--every 2] [--misses]
```
Sources: model cards on Hugging Face for `kyutai/stt-1b-en_fr` (CC-BY-4.0), `nvidia/parakeet-tdt-0.6b-v3` and `nvidia/canary-1b-v2` (CC-BY-4.0), `ibm-granite/granite-4.0-1b-speech` (Apache-2.0), `mistralai/Voxtral-Mini-3B-2507` and `mistralai/Voxtral-Mini-4B-Realtime-2602` (Apache-2.0); `istupakov/onnx-asr` on GitHub.
