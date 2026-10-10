# Spike S5: a simulated grill session

Three synthetic voices (Piper) hold a short FR/EN grill on `plan.md` in the lab deployment, with a real daemon running the agent. The report is `../S5-simulated-session.md`.

| File | |
|---|---|
| `session.json` | the script: speakers, voices and licenses, lines with text, language and timings, the annotation |
| `make_audio.py` | one 48 kHz WAV per speaker and round on a shared timeline; fills in the timings |
| `s5.spec.js` | the session: room, 3 browsers with fake mics, upload, daemon pairing, 2 rounds, annotation, 2 agent turns |
| `analyze.py` | WER, attribution, latency, turn times and doc diffs from the run's `result.json` |

Rerun from the repo root (needs `.env` with the lab keys, `make build`, `e2e/` installed, Docker, a Claude Code login; spends up to 8 Claude requests, Opus by default, `S5_MODEL` to change, `S5_AGENT=0` for none):

```sh
cd spikes/s5-simulated-session && ln -sfn ../../e2e/node_modules node_modules
UV_PROJECT_ENVIRONMENT=../../.scratchpad/s5/venv uv run ./make_audio.py ../../.scratchpad/s5/audio
cd ../../e2e && pnpm exec playwright test -c ../spikes/s5-simulated-session/playwright.config.js
cd ../spikes/s5-simulated-session && UV_PROJECT_ENVIRONMENT=../../.scratchpad/s5/venv uv run ./analyze.py ../../.scratchpad/s5/run/result.json
```

The generated audio is not committed: 6 WAVs, 41 MB, plus 250 MB of voice models.
