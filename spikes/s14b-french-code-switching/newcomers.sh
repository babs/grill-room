#!/usr/bin/env bash
# Phase 4, from the 2026-10 release watch: Pocket TTS from git main (French retrained 2026-10-01,
# English english_2026-09_2) next to the PyPI 3.3.0 estelle, and Sopro v2 Turbo cloning CC0 clips.
# usage: S14_MODELS=<dir> ./newcomers.sh <work dir>
set -euo pipefail
cd "$(dirname "$0")"
w=$1
render() { # combo venv mode synth args...
  local combo=$1 venv=$2 mode=$3
  shift 3
  [[ -f $w/matrix/$combo/en04.wav ]] && return 0
  ./run.sh "$w/matrix.jsonl" "$venv" "$mode" "$w/matrix/$combo" "$@" >/dev/null
}
export S14B_POCKET_EN=english_2026-09_2
for mode in raw respell hybrid; do
  render "pocketmain-estelle__$mode" pocketmain "$mode" --fr pocket:estelle
done
render "pocketmain-estelle__same" pocketmain spans --fr pocket:estelle
unset S14B_POCKET_EN
# Kyutai's own recording behind estelle (CC0), and a voice donation (CC0, consented terms v1).
for ref in developpeuse-3 Selfie; do
  for mode in raw respell hybrid; do
    render "sopro-${ref}__$mode" sopro "$mode" --fr "sopro:$w/voices/ref-$ref.wav"
  done
  render "sopro-${ref}__same" sopro spans --fr "sopro:$w/voices/ref-$ref.wav"
done
S14B_SOPRO_INT8=1 render "sopro-developpeuse-3-int8__raw" sopro raw --fr "sopro:$w/voices/ref-developpeuse-3.wav"
