#!/usr/bin/env bash
# Phase 1: Pocket on every sentence as written, the catalogue estelle against 8 voices cloned from
# CML-TTS French (picked by pitch: 4 higher, 4 lower). The STT round trip then picks the clones.
# usage: S14_MODELS=<dir> HF_TOKEN_FILE=<file> ./screen.sh <work dir>
set -euo pipefail
cd "$(dirname "$0")"
w=$1
cml=hf://kyutai/tts-voices/cml-tts/fr
for v in estelle 10179_11051_000005-0001 3267_1902_000075-0001 12080_11650_000047-0001 5476_3103_000072-0001 \
  2114_1656_000053-0001 1770_1028_000036-0002 4193_3103_000004-0001 7601_7727_000062-0001; do
  spec=pocket:$v
  [[ $v == estelle ]] || spec=pocket:$cml/${v}_enhanced.wav
  ./run.sh "$w/screen.jsonl" main raw "$w/screen/pocket-${v%%_*}" --fr "$spec" --en pocket:alba >/dev/null
done
