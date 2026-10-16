#!/usr/bin/env bash
# Phase 3: the engines too slow for the full matrix (Kyutai TTS 1.6B, Chatterbox Multilingual) on the
# 6 key sentences as written, and Kokoro's first-audio delay with and without clause chunking.
# usage: S14_MODELS=<dir> ./extra.sh <work dir>
set -euo pipefail
cd "$(dirname "$0")"
w=$1
key=fr01,fr02,fr03,fr04,fr08,en01
S14B_MEM=14G ./run.sh "$w/matrix.jsonl" moshi raw "$w/matrix/kyutai-12080__raw" \
  --fr kyutai:cml-tts/fr/12080_11650_000047-0001_enhanced.wav --only "$key" >/dev/null
./run.sh "$w/matrix.jsonl" chatterbox raw "$w/matrix/chatterbox-12080__raw" \
  --fr "chatterbox:$w/voices/12080_11650_000047-0001_enhanced.wav" --only "$key" >/dev/null
for chunk in 0 1; do
  S14B_KOKORO_CHUNK=$chunk ./run.sh "$w/kokoro-chunk.jsonl" main raw "$w/kokoro-chunk-$chunk" \
    --en kokoro:bf_emma --only en01,en02,en03,en04 >/dev/null
done
