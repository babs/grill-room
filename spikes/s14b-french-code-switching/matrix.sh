#!/usr/bin/env bash
# Phase 2: every French voice (row) against every strategy and English voice (column), on all 21
# sentences, one synthesis at a time. Writes <work>/matrix/<row>__<column>/<id>.wav.
# Columns: raw (as written), num (numbers and acronyms said), respell (French respelling),
# same (per-span switch, same engine and voice), then per-span switch to another English voice:
# gb and us (Kokoro), pocketen (closest Pocket catalogue voice), supertonicen (Supertonic).
# hybrid and hybridgb: respelled terms, English clauses only in the same voice or Kokoro GB.
# usage: S14_MODELS=<dir> HF_TOKEN_FILE=<file> ./matrix.sh <work dir> [row filter regex]
set -euo pipefail
cd "$(dirname "$0")"
w=$1
filter=${2:-.}
cml=hf://kyutai/tts-voices/cml-tts/fr

# name|venv|French engine:voice|gender|closest Pocket English voice (timbre.py, ECAPA)
rows=(
  "pocket-estelle|main|pocket:estelle|f|vera"
  "pocket-3267|main|pocket:$cml/3267_1902_000075-0001_enhanced.wav|f|caro_davy"
  "pocket-12080|main|pocket:$cml/12080_11650_000047-0001_enhanced.wav|f|vera"
  "pocket-2114|main|pocket:$cml/2114_1656_000053-0001_enhanced.wav|m|peter_yearsley"
  "pocket-4193|main|pocket:$cml/4193_3103_000004-0001_enhanced.wav|m|peter_yearsley"
  "supertonic-F2|main|supertonic:F2|f|vera"
  "supertonic-M2|main|supertonic:M2|m|peter_yearsley"
  "kokoro-ff_siwis|main|kokoro:ff_siwis|f|vera"
  "piper-siwis|main|piper:fr_FR-siwis-medium|f|vera"
  "melo-FR|melo|melo:FR|f|"
)

render() { # combo venv mode synth args...
  local combo=$1 venv=$2 mode=$3
  shift 3
  [[ -f $w/matrix/$combo/en04.wav ]] && return 0 # resumable
  ./run.sh "$w/matrix.jsonl" "$venv" "$mode" "$w/matrix/$combo" "$@" >/dev/null
}

for r in "${rows[@]}"; do
  IFS='|' read -r name venv fr gender pen <<< "$r"
  [[ $name =~ $filter ]] || continue
  engine=${fr%%:*}
  for mode in raw num respell; do
    render "${name}__$mode" "$venv" "$mode" --fr "$fr"
  done
  [[ $venv == main ]] || continue
  case $engine in
    pocket|supertonic|kokoro)
      render "${name}__same" main spans --fr "$fr"
      # Hybrid: lexicon terms respelled in French, English clauses (quotes) in the same voice's English.
      render "${name}__hybrid" main hybrid --fr "$fr"
      ;;
  esac
  if [[ $gender == f ]]; then gb=bf_emma us=af_heart st=F2; else gb=bm_george us=am_michael st=M2; fi
  render "${name}__gb" main spans --fr "$fr" --en "kokoro:$gb"
  render "${name}__hybridgb" main hybrid --fr "$fr" --en "kokoro:$gb"
  render "${name}__us" main spans --fr "$fr" --en "kokoro:$us"
  render "${name}__pocketen" main spans --fr "$fr" --en "pocket:$pen"
  [[ $engine == supertonic ]] || render "${name}__supertonicen" main spans --fr "$fr" --en "supertonic:$st"
done
