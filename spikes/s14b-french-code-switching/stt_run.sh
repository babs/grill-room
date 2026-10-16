#!/usr/bin/env bash
# STT round trip of every <samples>/<combo>/<id>.wav through the worker image's replay (parakeet-ultra).
# usage: ./stt_run.sh <samples dir> <stt work dir> [image]
set -euo pipefail
cd "$(dirname "$0")"
samples=$1
work=$2
image=${3:-grill-stt:local}
rm -rf "$work"
mkdir -p "$work"
.venv/bin/python stt.py prep "$samples" "$work"
# Replay needs no room: the settings below only satisfy the worker's config.
docker run --rm --name "grill-room-test-s14b-stt-$$" --user "$(id -u):$(id -g)" --cpuset-cpus 20-27 --memory 6g \
  -e APP_ROOM_URL=http://127.0.0.1 -e APP_ROOM_TOKEN=x -e APP_LIVEKIT_URL=ws://127.0.0.1 \
  -e APP_LIVEKIT_API_KEY=x -e APP_LIVEKIT_API_SECRET=x \
  -v "$(realpath "$work")":/data --entrypoint python "$image" \
  -m grill_stt.replay /data/manifest.json /data/out.jsonl --set s14b --talkers 8 | tail -1
.venv/bin/python stt.py score "$work/out.jsonl" "$work/scores.json"
