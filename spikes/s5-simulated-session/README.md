# Spike S5: a simulated grill session

Three synthetic voices (Piper) hold a short FR/EN grill on `plan.md` in the lab deployment, with a real daemon running the agent. The report is `../S5-simulated-session.md`.

| File | |
|---|---|
| `session.json` | the script: speakers, voices and licenses, lines with text, language and timings, the annotation |
| `make_audio.py` | one 48 kHz WAV per speaker and round on a shared timeline; fills in the timings |
| `s5.spec.js` | the session: room, 3 browsers with fake mics, upload, daemon pairing, 2 rounds, 2 agent turns, and the AC39 loop: Emma's thread, her follow-up, the agent's own annotations, a close and a reopening reply |
| `analyze.py` | WER, attribution, latency, turn times, models, threads and doc diffs from the run's `result.json` |
| `offline.py` | the audio through the STT worker's own VAD and model, no browser: cuts, WER, model time, and the latency a single queue gives |

Rerun from the repo root (needs `.env` with the lab keys, `make build`, `e2e/` installed, Docker, a Claude Code login; spends up to 14 Claude requests, Opus by default, `S5_MODEL` to change, `S5_AGENT=0` for none, `S5_AUDIO` for another audio dir):

```sh
cd spikes/s5-simulated-session && ln -sfn ../../e2e/node_modules node_modules
UV_PROJECT_ENVIRONMENT=../../.scratchpad/s5/venv uv run ./make_audio.py ../../.scratchpad/s5/audio
cd ../../e2e && pnpm exec playwright test -c ../spikes/s5-simulated-session/playwright.config.js
cd ../spikes/s5-simulated-session && UV_PROJECT_ENVIRONMENT=../../.scratchpad/s5/venv uv run ./analyze.py ../../.scratchpad/s5/run/result.json
```

Offline, from `stt/` with the worker's venv, on the CPU you want to size (a pod with the STT image works: copy the script, `analyze.py`, `session.json` and the audio in, and point `--model` at `/models/whisper`):

```sh
PYTHONPATH=. uv run python ../spikes/s5-simulated-session/offline.py ../.scratchpad/s5/audio --threads 3 --min-silence-ms 800
# --two-pass and --fallback give the transcriber as it was before S5, --json writes every utterance
```

The generated audio is not committed: 6 WAVs, 41 MB, plus 250 MB of voice models.
