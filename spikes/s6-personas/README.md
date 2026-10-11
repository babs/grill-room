# Spike S6: two personas on the lab

The S5 session (`../s5-simulated-session/`: script, voices, plan) with Grill and Secretary, a chain link, a queued press and a shared skill. The report is `../S6-personas.md`.

| File | |
|---|---|
| `s6.spec.js` | room, 3 browsers with fake mics, daemon pairing, personas set in the panel, 2 rounds, 5 turns, the assertions, `result.json` |

Rerun from the repo root. It needs what S5 needs (`.env` with the lab keys, `make build`, `e2e/` installed, Docker, a Claude Code login) and the S5 audio. It spends up to 15 Claude requests, Opus. `S6_KEEP=1` keeps the room and the shared skill for reading the stored prompts; delete both with the admin API afterwards.

```sh
cd spikes/s6-personas && ln -sfn ../../e2e/node_modules node_modules
cd ../../e2e && S6_KEEP=1 pnpm exec playwright test -c ../spikes/s6-personas/playwright.config.js
cd ../spikes/s5-simulated-session && UV_PROJECT_ENVIRONMENT=../../.scratchpad/s5/venv uv run ./analyze.py ../../.scratchpad/s6/run/result.json
```

`analyze.py` gives the STT figures. The turns, requests and workspace bytes are in `result.json` under `agentTurns`.

The prompts are stored in the room server's SQLite only. Copy the database out of the volume (the server image has no shell, so mount the volume read-only in a throwaway pod), then:

```sh
sqlite3 grill-room.db "SELECT t.id, t.persona_name, t.participant_id IS NULL AS by_agent, t.prompt FROM turns t JOIN rooms r ON r.id = t.room_id WHERE r.name = 'S6 dark mode grill' ORDER BY t.id"
```

`by_agent` is 1 for a chain link: an `@name` call or "every N rounds".
