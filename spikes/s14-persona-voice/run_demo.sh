#!/usr/bin/env bash
# Runs demo.spec.js against the loopback stack. Usage: ./run_demo.sh <demo dir holding demo.env,
# silent.wav, human.wav and bot.log>; it writes demo.webm and test-results there.
set -euo pipefail
d=$1
set -a
# shellcheck source=/dev/null
. "$d/demo.env"
set +a
S14_BASE=http://127.0.0.1:7695 S14_ADMIN_TOKEN=$ADMIN_TOKEN S14_SILENT_WAV=$d/silent.wav S14_HUMAN_WAV=$d/human.wav \
  S14_BOT_LOG=$d/bot.log S14_RECORDING=$d/demo.webm S14_OUT=$d/test-results \
  systemd-run --user --scope -q -p MemoryMax=6G -p MemorySwapMax=0 nice -n 10 \
  ./node_modules/.bin/playwright test --workers=1 --reporter=line
