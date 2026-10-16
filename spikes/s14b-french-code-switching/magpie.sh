#!/usr/bin/env bash
# Phase 5: Magpie TTS v2607 (NeMo-Speech.cpp, CPU) on the key sentences plus fr16, two of its five voices.
# Strategy (e), ipaspans: English terms switch to Magpie's English frontend as forced IPA from espeak.
# usage: ./magpie.sh <work dir>   (needs <work>/nemo built and pulled, image grill-room-test-s14b-nemo-dev)
set -euo pipefail
cd "$(dirname "$0")"
w=$1
ids=fr01,fr02,fr03,fr04,fr08,fr16,en01
for v in Sofia John; do
  for mode in raw respell spans ipaspans; do
    [[ $v == John && $mode =~ ^(respell|spans)$ ]] && continue
    out=$w/matrix/magpie-${v}__$mode
    [[ $mode == spans ]] && out=$w/matrix/magpie-${v}__same
    [[ -f $out/en01.wav ]] && continue
    nice -n 19 .venv/bin/python magpie.py "$w/nemo" grill-room-test-s14b-nemo-dev "$mode" "$v" "$out" --only "$ids"
  done
done
