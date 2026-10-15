#!/usr/bin/env bash
# Starts voicebot.py against the loopback stack of spikes/S14-persona-voice.md.
# Usage: ./run_bot.sh <demo.env> <log file> [engine]; demo.env holds LK_KEY, LK_SECRET, STT_TOKEN.
set -euo pipefail
set -a
# shellcheck source=/dev/null
. "$1"
set +a
export S14_ROOM_URL=${S14_ROOM_URL:-http://127.0.0.1:7695} S14_STT_TOKEN=$STT_TOKEN
export S14_LIVEKIT_URL=${S14_LIVEKIT_URL:-ws://127.0.0.1:7890} S14_LIVEKIT_KEY=$LK_KEY S14_LIVEKIT_SECRET=$LK_SECRET
export S14_ENGINE=${3:-pocket} S14_THREADS=${S14_THREADS:-2}
exec systemd-run --user --scope -q -p MemoryMax=4G -p MemorySwapMax=0 \
  nice -n 10 taskset -c "${S14_CORES:-16-19}" .venv/bin/python voicebot.py > "$2" 2>&1
