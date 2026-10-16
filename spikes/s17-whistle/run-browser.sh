#!/usr/bin/env bash
# One S17 browser session on the S11 "laptop" profile: cores 0,2,4,6 pinned, nice 10, 16 GB cap, headed Chrome under Xvfb.
# usage: run-browser.sh <web-dir> <run.mjs args...>
#   web-dir: eng/ (needle.js, needle.wasm, whistle.cact, silero_vad_v6.onnx, SHA256SUMS.json), ort/ (onnxruntime-web
#   1.30 dist, VAD only), data/ (all.wav and manifest.json from ../s13-device-stt-bench/prep.py, glossary.json); runs/ gets the results.
# PHONE=1: one core at a 25% quota (5 ms per 20 ms), every thread 4x slower, for the --phone emulation.
# Env: CHROME_IMAGE (the Playwright 1.64 image with `npm i @playwright/test` in /w and `playwright install chrome`).
set -euo pipefail
[[ $# -ge 2 ]] || { sed -n 2,8p "$0" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
web=$(cd "$1" && pwd)
shift
readonly here web image=${CHROME_IMAGE:?set CHROME_IMAGE}
# shellcheck disable=SC2054 # the commas belong to the cpuset value
cpu=(--cpuset-cpus=0,2,4,6) pin=0,2,4,6
if [[ ${PHONE:-0} == 1 ]]; then cpu=(--cpuset-cpus=0 --cpu-period=20000 --cpu-quota=5000) pin=0; fi
readonly cpu pin
mkdir -p "$web/runs"
docker run --rm --init --name "grill-room-test-s17-chrome-$$" "${cpu[@]}" --memory=16g --memory-swap=16g --shm-size=2g \
	-v "$here/web":/app:ro -v "$web/eng":/eng:ro -v "$web/ort":/ort:ro -v "$web/data":/data:ro -v "$web/runs":/out \
	"$image" bash -c 'xvfb-run -a nice -n 10 taskset -c '"$pin"' node /app/run.mjs "$@"; rc=$?; chown -R '"$(id -u):$(id -g)"' /out; exit $rc' _ "$@"
