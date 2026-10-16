# Spike S14b: French speech with English terms, on CPU

Asked 2026-10-15 after listening to S14: the French voices stumble when a sentence switches to English (tech terms, product names, English quotes). Find a CPU-only, self-hostable setup that sounds good in that case. Research and measurements only, no product code.

## Verdict

**Keep Pocket TTS 3.3.0 and put a rule-based "hybrid" normalisation layer in front of it.** Single English terms are respelled for the French voice ("ri-traï", "guite-heub", "cleuss-teur", "laïve-kit"), numbers and acronyms are said in the line's language, and only English clauses (quotes) switch to the same voice's English model. That setup meets the bar on 4 cores:

- French word error rate (STT round trip) falls from 0.244 to 0.109, the mean over 5 Pocket voices. On the 5 key French sentences estelle scores 0.057, against 0.083 for Kyutai TTS 1.6B, which needs 9x real time on the same cores.
- All 5 words you flagged come back in all 5 voices for GitHub, 502 and LiveKit. Before the layer the counts were 3, 0 and 1 out of 5.
- Speed: first audio 0.11 s, RTF 0.39, 1.15 core-s per audio second, 1.4 GB. That is the cost of plain Pocket.

**English rooms can ship now with Pocket as it is.** You found its English clean, and it scores like Kokoro on our STT (EN WER 0.08-0.12 against 0.095). French is the blocker, so spec 013 can ship English first and put French behind this layer.

| Rank | Setup | FR WER (17 sentences) | First audio | RTF, 4 cores | core-s per s | RSS | Licence | Verdict |
|---|---|---|---|---|---|---|---|---|
| 1 | **Pocket 3.3.0 + hybrid** | 0.109 (0.080-0.144, 5 voices) | 0.11 s | 0.39 | 1.15 | 1.4 GB | code MIT, weights CC-BY-4.0 | **recommended** |
| 2 | Pocket 3.3.0 + respelling only (a) | 0.117 (0.105-0.136) | 0.11 s | 0.39 | 1.15 | 1.4 GB | same | simplest; quotes stay French-accented |
| 3 | Sopro v2 Turbo + hybrid (estelle's source clip) | 0.107 | 0.85 s | 0.62 | 2.48 | 2.7 GB | Apache-2.0 | runner-up: keeps one voice across languages best, 2.2x the CPU |
| 4 | Supertonic 3 + hybrid | 0.125-0.127 | 3.9 s | 0.76 | 3.0 | 0.5 GB | OpenRAIL-M | too slow to start a line |
| 5 | Kokoro ff_siwis + Kokoro English | 0.127-0.139 | 0.64 s | 0.66 | 2.6 | 0.6 GB | Apache-2.0, data provenance unclear | one French voice |
| 6 | Piper siwis + Kokoro English (b) | 0.110 | 0.18 s | 0.59 | 2.4 | 0.9 GB | GPL runtime | intelligible, robotic French (S14) |
| 7 | MeloTTS French | 0.266 | 2.9 s | 0.58 | 2.1 | 2.2 GB | MIT | out: worst French, no English voice |
| 8 | Magpie TTS v2607 (NeMo-Speech.cpp) | 0.26-0.41 (5 sentences) | 1.2 s | 2.25 | n/m | n/m | NVIDIA Open Model License | out: slower than real time, accented, IPA path broken |
| 9 | Kyutai TTS 1.6B en_fr | 0.083 (5 sentences) | 16.8 s | 9.0 | 36 | 10.4 GB | CC-BY-4.0 | out on CPU |
| 10 | Chatterbox Multilingual (v2 weights) | 0.22 (5 sentences) | 43 s | 8.8 | 35 | 5.7 GB | MIT, audio watermark | out on CPU |

n/m: not measured. Every number is on 4 pinned E-cores, see Setup.

**Mixing engines does not pay.** Pocket for French with Kokoro bf_emma for the English spans sounds like two people (speaker similarity 0.06, see §4). It doubles the CPU (2.4 core-s per s) and adds 0.6 GB, for no gain in intelligibility (FR WER 0.22 for estelle with every term switched). The one place you may like it is a quote: "hybrid, quotes in bf_emma" is in the grid for your ear.

**Pocket from git `main`** (French retrained 2026-10-01, English `english_2026-09_2`) shows no measurable French gain on estelle, and one of its English lines was cut short. Wait for the PyPI release and listen again (§5).

## Setup

- Same box as S14: i7-14700F, every run on **4 pinned E-cores** (`taskset -c 16-19`), engine threads 4, one synthesis at a time, each in a memory-capped scope (`systemd-run --scope -p MemoryMax=10G`; 14G for Kyutai). The lab's i7-6700 workers were not measured.
- STT round trip: every sample through the room's own worker, parakeet-ultra (`grill_stt.replay`, image v0.0.21), 8 talkers at a time on other cores. WER is against the text as the persona wrote it, numbers compared as words on both sides. Each English term is checked separately against the spellings that count (`sentences.json`).
- Timbre: cosine similarity of ECAPA speaker embeddings (speechbrain `spkrec-ecapa-voxceleb`, Apache-2.0). It compares a voice's French line with its English line.
- **Noise.** Pocket samples at random. Two renders of the same text moved FR WER by up to 0.04 (estelle 0.255 and 0.260, clone 3267 0.228 and 0.265). One sentence rendered twice from identical text heard "retry" 5 times out of 5 the first time and 2 out of 5 the second. Trust the means over 5 voices, not single cells.
- Claude requests used: 0.

### Test set

21 sentences in the way personas speak (`sentences.json`). 17 are French: tech words (rollout, feature flag, sidecar, LiveKit, merge request, staging, p95, 502, Kubernetes, ArgoCD, sprint, backlog, retry, cluster, GitHub, GitLab, deploy, merge), mid-sentence switches, two English quotes, acronyms, numbers, a percentage and a version. 4 are English controls. 6 key sentences (fr01, fr02, fr03, fr04, fr08, en01) are rendered for every candidate.

## 1. Candidates and licences

Checked at the source on 2026-10-15/16.

| Candidate | Licence, from source | French and English | Status here |
|---|---|---|---|
| Pocket TTS 3.3.0 | code MIT (GitHub), weights CC-BY-4.0 (HF card); cloning weights gated, terms accepted by you | yes, separate model per language | full matrix |
| Pocket TTS git `main` `ec569b2` | same | French retrained, `english_2026-09_2` | estelle, 4 strategies |
| Pocket voices cloned from CML-TTS French | CC-BY-4.0 (`kyutai/tts-voices` README) | French readers | 8 screened, 4 in the matrix |
| Sopro v2 Turbo (`samuel-vitorino/sopro-v2-turbo` @ `c12d0dc`) | weights and code Apache-2.0 (HF card, PyPI 2.2.0) | fr, en, pt, de; per-call language tag; cloning only | 2 reference clips |
| Kyutai TTS 1.6B en_fr | weights CC-BY-4.0 (HF card), code MIT/Apache | built for both | 6 key sentences |
| Chatterbox Multilingual | MIT (HF and GitHub); every output carries the Perth watermark | 23 languages | 6 key sentences |
| MeloTTS French | MIT (HF card) | French only | full set |
| Supertonic 3 | OpenRAIL-M (S14) | per-call language | full matrix |
| Kokoro 82M | weights Apache-2.0. VOICES.md gives no data licence for bf_emma; the card says the data is "permissive/non-copyrighted" and includes synthetic audio from closed commercial TTS providers | 1 French voice (ff_siwis, CC-BY) | French row and English columns |
| Piper fr siwis | GPL-3.0 runtime, voice CC-BY (S14) | French | baseline |
| Magpie TTS Multilingual v2607 | NVIDIA Open Model License: commercial use allowed. Rights end if its guardrails are bypassed, and NVIDIA may update the terms. **Record as a risk** | 12 languages, 5 voices, all English speakers | 6 key sentences + fr16 |

Excluded on licence, per the 2026-10 release watch: OmniVoice (CC-BY-NC data), KittenTTS 2 (commercial grant ends at USD 1M revenue), NeuTTS (paid past USD 5M revenue), Fish Drama-3, Breeze-TTS-2, VoTSpeech, and S14's non-commercial list (XTTS, F5, Voxtral, Fish/OpenAudio).

**Reference clips for cloning.** Estelle's clip and Sopro's main reference (`unmute-prod-website/developpeuse-3.wav`) are Kyutai's own recordings, CC0 per the voices README. `Selfie.wav` is a voice donation. The donation terms (v1, read in full) let anyone use the clips "for any purpose (commercial or non-commercial)", subject to Kyutai's Acceptable Use Policy and data-protection law. **Donors may withdraw consent and ask for deletion**, so a shipped donated voice may have to be removed later. Pocket's gated cloning terms forbid impersonation without consent.

## 2. Normalisation layer

Engine-independent, rule-based, in `normalize.py` (self-check: `normalize.py --check`). Modes:

| Mode | What it does |
|---|---|
| raw | the text as written |
| num | numbers, percentages, versions (`2.3.1` → "deux point trois point un"), `p95` → "pé quatre-vingt-quinze", acronyms spelled with French or English letter names, HTTP codes after an article said digit by digit in English ("a five oh two"); a few names said the English way ("Live Kit", "Argo C D") |
| respell (a) | num, plus a 38-entry lexicon of English dev/ops terms respelled for a French voice |
| spans (b, c) | num, plus every lexicon term and English clause as an English span, read by an English voice |
| hybrid | respell for single terms, English span only for clauses with at least 2 English stopwords (quotes) |

The lexicon covers your list (GitHub, GitLab, retry, cluster, deploy, rollout, staging, merge, pull request, feature flag, sidecar, LiveKit, Kubernetes, ArgoCD, backlog, sprint) and about 20 more (webhook, backoff, endpoint, health check, load balancer, pipeline, code freeze, error budget, PostgreSQL, Redis, README, end-to-end, login, canary). Your respellings are used as given: "ri-traï", "cleuss-teur", "laïve-kit".

### Results, Pocket, mean of 5 voices

| Strategy | FR WER (range) | English terms heard (of 60) | EN WER | GitHub | 502 | retry | cluster | LiveKit |
|---|---|---|---|---|---|---|---|---|
| raw | 0.244 (0.216-0.265) | 28.8 | 0.201 | 3/5 | 0/5 | 0/5 | 4/5 | 1/5 |
| num | 0.172 (0.165-0.181) | 35.6 | 0.107 | 0/5 | 5/5 | 0/5 | 5/5 | 0/5 |
| (a) respell | 0.117 (0.105-0.136) | 46.6 | 0.126 | 5/5 | 5/5 | 5/5 | 3/5 | 5/5 |
| **hybrid** | **0.109 (0.080-0.144)** | 47.4 | 0.085 | 5/5 | 5/5 | 2/5 | 4/5 | 5/5 |
| hybrid, quotes in Kokoro bf_emma/bm_george | 0.141 | 44.6 | 0.102 | 4/5 | 5/5 | 5/5 | 5/5 | 5/5 |
| (c) every term switched, same voice | 0.150 | 47.0 | 0.099 | 5/5 | 5/5 | 0/5 | 2/5 | 2/5 |
| (b) every term in Kokoro GB | 0.161 | 43.8 | 0.102 | 5/5 | 5/5 | 5/5 | 1/5 | 5/5 |
| (b) every term in Kokoro US | 0.164 | 46.2 | 0.081 | 5/5 | 5/5 | 5/5 | 5/5 | 5/5 |
| (b) every term in Pocket's closest English voice | 0.171 | 44.4 | 0.143 | 5/5 | 5/5 | 0/5 | 0/5 | 4/5 |
| (b) every term in Supertonic | 0.193 | 42.2 | 0.118 | 5/5 | 5/5 | 0/5 | 3/5 | 5/5 |

hybrid and respell render the same text for fr08, so "retry" at 2/5 against 5/5 is sampling noise, not a difference between them.

What the numbers say:

- **Numbers first.** Saying numbers in words alone takes FR WER from 0.244 to 0.172 and fixes 502 everywhere ("cinquante-deux" is gone).
- **Respelling beats switching for single words.** Pocket's English model, given one word, often garbles or drops it. "Le" plus "cluster" came back as "Euh, je peux faire..."; "retry" as "Ray Tree" or "retree". Every one-word switch also adds a pause and a new prosody contour. Respelling keeps one French breath.
- **Switching is right for quotes.** On the two quote sentences the STT hears the English well in every mode (WER 0.01-0.05; parakeet itself handles code-switching). Your ear is the judge there: the hybrid reads "ship it and we'll fix it later" with estelle's English model and the same voice.
- **Kokoro US reads isolated terms best** (5/5 everywhere), but the timbre break costs more than it gains, see §4.

### A tiny LLM rewrite step?

**Not now.** The rules fixed every flagged word. The remaining misses are words outside the lexicon, plus Pocket's sampling. A local 0.5-1.5B rewrite would add a model and a second or more per line on 4 cores (not measured), and it can change what the persona said. Cheaper options, in order:

1. Grow the lexicon through the room glossary of spec 007, with a spoken form per term.
2. Ask the persona's own model to mark English spans in `spoken.md` (for example `<en>ship it</en>`). It already writes that file in the same turn, so it costs no extra call. The stopword detector stays as the fallback.
3. Auto-respell unknown English-looking words: English IPA from espeak, mapped to French spelling by rule.

## 3. Engines

### Pocket TTS 3.3.0

- **Cloned French voices against the catalogue.** 8 CML-TTS French readers were screened on the full set, chosen by pitch. Raw FR WER 0.223-0.283, estelle 0.260: within noise of each other. With the hybrid layer, the clones score 0.080 (2114, lower voice), 0.091 (3267), 0.107 (4193) and 0.125 (12080), estelle 0.144. They help a little, as French speakers with no foreign accent. Estelle stays the baseline you liked; the clones give 4 more French voices. Gender is inferred from pitch, not checked by ear.
- Cost is the same for every voice: one process holds one French and one English model (1.4-1.5 GB) whatever the number of voices.

### Sopro v2 Turbo

- Apache-2.0, 120M parameters, French and English with a per-call language tag, streaming, cloning only.
- On 4 E-cores: first audio 0.85-0.90 s (the authors report about 0.3 s on an Apple M3), RTF 0.62-0.68, 2.5 core-s per s, 2.7-3.7 GB. The int8 option did not help (RTF 0.64).
- **It handles English terms in French well even with no rewrite.** Estelle's source clip scores 0.119 raw and 0.107 hybrid. The authors call mixed-language text a weak spot, but here it did better raw than any other real-time engine.
- **It keeps one voice across languages best**: 0.81 similarity between its French and English lines. Pocket reaches 0.44-0.65.
- It depends on the reference clip: the voice donation `Selfie.wav` scores 0.22-0.27. A persona voice would need a vetted clip.
- Verdict: the alternative if you prefer its sound, at 2.2x Pocket's CPU and twice its memory.

### Kyutai TTS 1.6B and Chatterbox

- Kyutai: the best raw intelligibility (0.083 on the 5 key French sentences). In float32 on 4 E-cores it runs at RTF 5.8 with 16 codebooks and 9.0 with 32, needs 10.4 GB, and takes 16-19 s to first audio. bf16 has no fast CPU path here. Out on CPU.
- Chatterbox Multilingual: RTF 8.8 and 5.7 GB. Every output is watermarked. Out on CPU. PyPI 0.1.7 still loads the v2 weights.

### Magpie TTS v2607

- Built from NeMo-Speech.cpp v0.2.0 in a scratch container (preset `cpu-tts`). The host has no SentencePiece dev files, and installing them was not authorised.
- F16 GGUF, 4 E-cores: first audio 1.2 s, 0.44x real time (RTF 2.25). The published 2.7x real time is for Q8_0 on 8 P-core threads. Converting to Q8_0 needs NeMo's Python stack and was not tried.
- Its French goes through a byte tokenizer, so inline IPA cannot work in a French span. Strategy (e) therefore sends English terms to its English frontend as forced IPA from espeak. The runtime **spelled the IPA out letter by letter** (FR WER above 1.0). Strategy (e) fails as shipped.
- Its 5 voices are English speakers. In French they score 0.41 raw and 0.26 with English spans, and sound accented.
- Out.

### MeloTTS, Piper, Supertonic, Kokoro French

- MeloTTS French: 0.27-0.33. It has no English voice, so English lines come out unintelligible (EN WER 0.93). Out.
- Piper siwis: cheapest by far (0.23 core-s per s). With Kokoro US for the English terms it scores 0.110, but its French is robotic (S14).
- Supertonic 3: hybrid 0.125. But every call costs about 1.5 s up front: first audio 3.6-3.9 s, and per-span switching runs slower than real time (RTF 1.5-1.95).
- Kokoro ff_siwis: 0.32 raw, 0.13-0.14 with an English voice for the terms. It is Kokoro's only French voice.

## 4. Two engines, one persona: timbre

Speaker similarity between a voice's French line and the English line of the paired voice. For scale, the same voice on two French sentences scores 0.69-0.88.

| Pairing | Similarity |
|---|---|
| Sopro, same clip, both languages | 0.76-0.81 |
| Pocket git main, estelle in both models | 0.69 |
| Pocket 3.3.0, same voice in both models | 0.44-0.65 (estelle 0.56) |
| Supertonic, same style | 0.40-0.52 |
| Magpie, same voice | 0.42-0.50 |
| Pocket voice + its closest Pocket English catalogue voice (by ECAPA) | 0.11-0.25 |
| Any French voice + Kokoro, Supertonic or another engine | -0.07 to 0.21 |
| **estelle + Kokoro bf_emma** | **0.06** |

A cross-engine switch is a second person, whatever the level matching. Each span was levelled to -22 dBFS, so loudness does not jump. A same-voice switch keeps the speaker, more or less: Sopro and Pocket main best.

### Kokoro bf_emma

- **Licence**: weights Apache-2.0. bf_emma's voice pack has no stated data source or licence (VOICES.md grade B-). The model card says the training data is permissive and includes synthetic audio from closed commercial TTS providers, so its provenance is unclear. It is not a non-commercial licence, but it is a weaker footing than CC0 or CC-BY voices.
- **CPU**, English lines on 4 E-cores: RTF 0.55-0.59, 2.2-2.35 core-s per s, 0.62 GB. Kokoro renders a whole call before returning. First audio is 2.1-3.8 s for a whole line. Cutting at clause ends brings it to 1.1-1.3 s on lines with a comma and changes nothing on lines without one (2.9-4.0 s). S14's 4.4 s was a longer sentence. Cutting at a fixed word count would cut it further, at a prosody cost.
- **As the English room voice**: its STT score ties Pocket's (EN WER 0.095 against 0.08-0.12). The choice is your ear. It costs 2x Pocket's CPU per spoken second and 10-40x Pocket's first-audio delay.
- **As the English voice inside French**: it sounds like a second person (0.06). Use it for quotes only, if at all.
- **Mixed setup** (Pocket for French, Kokoro for English, picked per persona voice): one process holding both takes 2.0-2.1 GB against 1.4. Code-switched lines cost 2.2-2.9 core-s per s against 1.15. It adds a second engine to ship and patch. Worth it only if you rank bf_emma clearly above every Pocket English voice for English rooms. Then run it as that persona's English voice, never mid-sentence.

## 5. Pocket TTS git main against 3.3.0 (estelle)

| Strategy | FR WER 3.3.0 | FR WER main | EN WER 3.3.0 | EN WER main |
|---|---|---|---|---|
| raw | 0.255 | 0.321 | 0.199 | 0.272 |
| respell | 0.126 | 0.124 | 0.134 | 0.119 |
| hybrid | 0.144 | 0.157 | 0.080 | 0.095 |
| same-voice switch | 0.193 | 0.122 | 0.118 | 0.158 |

No difference beyond noise in French, except the per-term switch. Estelle's English model keeps her voice better (similarity 0.69 against 0.56). One English line stopped early: "The LiveKit sidecar returns a 502 whenever..." came back as "...returns a FIFO." The PR claims fewer repeated endings and more rising questions. Our set has one question, and one render per line cannot confirm either claim. Same CPU and memory as 3.3.0. **Listen to the `pocketmain-estelle` row, then re-test at the next PyPI release.**

## 6. Recommendation for spec 013

1. Engine: **Pocket TTS 3.3.0**, pinned. Voices: estelle plus the CML-TTS clones 2114, 3267, 4193 and 12080 (CC-BY-4.0, attribution in `scenarios/VOICES.md`) for French. English slots as in S14.
2. A **normalisation step before synthesis** (amends S14's draft AC3):
   - numbers, percentages, versions and acronyms said in the line's language;
   - a dev/ops lexicon of English terms with a French respelling, extendable per room through the spec 007 glossary;
   - English clauses of at least 2 English stopwords spoken by the same persona voice's English model, everything else in one French call.
   - It needs a test that fails without each rule, like the one `normalize.py --check` runs.
3. **Ship English first**; French follows with the layer, gated by your listening test of the grid.
4. Do not mix engines within a line.
5. Keep Sopro v2 Turbo as the named alternative, and re-check Pocket main at its release.
6. Number words in Go: `num2words` (LGPL-2.1, used here) is Python. The server needs its own fr/en number speller, about 100 lines.

## Samples

In the folder handed over with this report (not committed): `b/index.md` holds a grid of French voices by strategy and English voice, 6 key sentences per cell. It also has a "picks" section, before/after pairs for GitHub, 502, retry, cluster and LiveKit, and an English-room comparison including bf_emma. About 23 MB of Opus.

## Reproduce

Everything runs from `spikes/s14b-french-code-switching/`. Python 3.13 main env via `uv sync` (Pocket, Supertonic, Piper, Kokoro, num2words). Side envs under `envs/` (gitignored), created with `uv venv` plus `uv pip install` and the PyTorch CPU index: `chatterbox` (chatterbox-tts 0.1.7, torch 2.6, setuptools<81), `moshi` (moshi 0.2.13), `melo` (MeloTTS from git, setuptools<81, `python -m unidic download`), `timbre` (speechbrain 1.1.2), `pocketmain` (pocket-tts @ `ec569b2`), `sopro` (sopro 2.2.0).

```sh
uv run ./normalize.py --check && uv run ./normalize.py      # texts.json, every mode
uv run ./stt.py --check
export S14_MODELS=<S14 models dir> HF_TOKEN_FILE=<token file>
./screen.sh <work>                 # 8 cloned Pocket voices + estelle, raw
./matrix.sh <work>                 # every row x column, 21 sentences each
./extra.sh <work>                  # Kyutai, Chatterbox (6 key sentences), Kokoro chunking
./newcomers.sh <work>              # Pocket git main, Sopro
./magpie.sh <work>                 # needs <work>/nemo: NeMo-Speech.cpp v0.2.0 built (cpu-tts) and `pull magpie`
./stt_run.sh <work>/matrix <work>/stt-matrix
envs/timbre/bin/python timbre.py <fr.wav> <en.wav> ...
uv run ./build_index.py <work> <samples dir> results/tables.md
```

Results: `results/tables.md` (every combination: FR and EN WER, terms heard, flagged words, timbre, first audio, RTF, CPU, RSS), `results/scores.json` (per-sentence STT text), `results/synth.jsonl`, `results/timbre.jsonl`, `results/kokoro-chunk.jsonl`, `results/magpie-bench-fr.json`.

## Sources

- Pocket TTS: [GitHub](https://github.com/kyutai-labs/pocket-tts), [PR 344 (French)](https://github.com/kyutai-labs/pocket-tts/pull/344), [PR 353 (English)](https://github.com/kyutai-labs/pocket-tts/pull/353), [model card](https://huggingface.co/kyutai/pocket-tts), [voices and licences](https://huggingface.co/kyutai/tts-voices), [voice donation terms v1](https://kyutai.org/legal/Terms%20of%20Use%20-%20Unmute%20Voice%20Donation%20Project%20v1.pdf)
- Kyutai TTS 1.6B: [model card](https://huggingface.co/kyutai/tts-1.6b-en_fr), [delayed-streams-modeling](https://github.com/kyutai-labs/delayed-streams-modeling)
- Sopro: [model card](https://huggingface.co/samuel-vitorino/sopro-v2-turbo), [blog](https://research.haloneuro.ai/posts/sopro-v2), [code](https://github.com/samuel-vitorino/sopro)
- Magpie: [model card](https://huggingface.co/nvidia/magpie_tts_multilingual_357m), [NeMo-Speech.cpp](https://github.com/NVIDIA/NeMo-Speech.cpp), [NVIDIA Open Model License](https://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-license)
- Chatterbox: [GitHub](https://github.com/resemble-ai/chatterbox), [model card](https://huggingface.co/ResembleAI/chatterbox)
- MeloTTS: [French model card](https://huggingface.co/myshell-ai/MeloTTS-French)
- Kokoro: [model card](https://huggingface.co/hexgrad/Kokoro-82M), [VOICES.md](https://huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md)
- Speaker embeddings: [speechbrain/spkrec-ecapa-voxceleb](https://huggingface.co/speechbrain/spkrec-ecapa-voxceleb)
- Release watch 2026-07 to 2026-10: coordinator's notes (sources listed there).

## Self-check

- **What went wrong on the way:**
  - Three run chains hit the 2 h background limit; one STT pass was killed mid-run and rerun.
  - One STT pass slowed to about 10 minutes per group under load from other processes on the box. I stopped it and the rerun went at normal speed.
  - The first STT attempt failed on file permissions (container user); fixed with `--user`.
  - Installing speechbrain into the Chatterbox env upgraded its torch to a CUDA build. I restored torch 2.6 CPU and moved speechbrain to its own env.
- **What the measurements caught that I had not expected:**
  - Per-term switching garbles one-word spans. That led to the hybrid mode.
  - Pocket's sampling noise is as large as some strategy gaps.
  - Magpie's IPA path does not work on its CPU runtime.
- **Not done:**
  - a lab-node measurement;
  - Magpie Q8_0;
  - a fixed-word-count chunking test on Kokoro;
  - more than one render per cell;
  - gender and naturalness, which need your ear.
- Docs may go public: no paths, hosts or IPs in committed files. Raw results had local paths stripped before commit.
