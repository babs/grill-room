#!/usr/bin/env bash
# Runs bench.py over the S14 matrix: 3 timed runs of one voice per engine and language, then one
# run per sample voice writing its WAV. Usage: S14_MODELS=<dir> ./run_bench.sh <samples dir> <results.jsonl>
set -euo pipefail

samples=$1
results=$2
cores=${S14_CORES:-16-19}
mkdir -p "$samples"

bench() {
  systemd-run --user --scope -q -p MemoryMax=6G -p MemorySwapMax=0 \
    nice -n 19 ionice -c3 taskset -c "$cores" .venv/bin/python bench.py "$@" 2>/dev/null | tail -1 >> "$results"
}

primary=(
  "piper fr fr_FR-siwis-medium" "piper en en_US-bryce-medium"
  "kokoro fr ff_siwis" "kokoro en af_heart"
  "kokoro_int8 fr ff_siwis" "kokoro_int8 en af_heart"
  "pocket fr estelle" "pocket en alba"
  "supertonic fr F1" "supertonic en F1"
)
for _ in 1 2 3; do
  for p in "${primary[@]}"; do
    # shellcheck disable=SC2086 # each entry is "engine lang voice"
    bench $p
  done
done

sample_voices=(
  "piper fr fr_FR-siwis-medium" "piper fr fr_FR-mls-medium#84" "piper fr fr_FR-mls-medium#3"
  "piper fr fr_FR-mls-medium#40" "piper fr fr_FR-upmc-medium#0" "piper fr fr_FR-upmc-medium#1"
  "piper fr fr_FR-tom-medium"
  "piper en en_GB-cori-high" "piper en en_US-bryce-medium" "piper en en_US-norman-medium"
  "piper en en_US-ljspeech-high" "piper en en_US-john-medium"
  "kokoro fr ff_siwis" "kokoro fr am_michael" "kokoro fr bf_emma"
  "kokoro en af_heart" "kokoro en af_bella" "kokoro en am_michael" "kokoro en am_fenrir"
  "kokoro en bf_emma" "kokoro en bm_george"
  "pocket fr estelle" "pocket fr marius" "pocket fr alba" "pocket fr charles" "pocket fr eve"
  "pocket fr javert" "pocket fr giovanni"
  "pocket en alba" "pocket en marius" "pocket en charles" "pocket en vera" "pocket en peter_yearsley"
  "pocket en caro_davy"
  "supertonic fr F1" "supertonic fr F2" "supertonic fr M1" "supertonic fr M2" "supertonic fr M3"
  "supertonic en F1" "supertonic en F2" "supertonic en M1" "supertonic en M2" "supertonic en M3"
)
for s in "${sample_voices[@]}"; do
  read -r engine lang voice <<< "$s"
  bench "$engine" "$lang" "$voice" --out "$samples/$lang-$engine-${voice//#/-}.wav"
done
