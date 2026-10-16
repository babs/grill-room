#!/usr/bin/env bash
# One synth.py run in the chosen venv (main, chatterbox, moshi, melo: engines pin incompatible torch
# versions), pinned to 4 E-cores as in S14, memory-capped so a spike kills the run and not the
# session. Appends synth.py's JSON lines to <results.jsonl>.
# usage: S14_MODELS=<dir> [HF_TOKEN_FILE=<file>] ./run.sh <results.jsonl> <venv> <synth.py args...>
set -euo pipefail
cd "$(dirname "$0")"

results=$1
venv=$2
shift 2
py=.venv/bin/python
[[ $venv == main ]] || py=envs/$venv/bin/python

# The token is read at run time, never written anywhere: Pocket's cloning weights are gated.
if [[ -n ${HF_TOKEN_FILE:-} ]]; then
  HF_TOKEN=$(<"$HF_TOKEN_FILE")
  export HF_TOKEN
fi

systemd-run --user --scope -q -p MemoryMax="${S14B_MEM:-10G}" -p MemorySwapMax=0 \
  nice -n 19 ionice -c3 taskset -c "${S14_CORES:-16-19}" \
  "$py" synth.py "$@" 2> >(tail -5 >&2) | tee -a "$results"
