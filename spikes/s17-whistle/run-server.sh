#!/usr/bin/env bash
# S17 server bench: every run of bench.py, one at a time, in the STT worker image on 4 pinned P-cores.
# usage: run-server.sh <data-dir> <engine-dir> [run...]
#   data-dir:   corpus/ from prep.py; results go to out/
#   engine-dir: needle (the Cactus CLI, linux-x86_64), whistle.cact, orukeet.gguf, kroko/ (sherpa-onnx export),
#               pydeps/ (sherpa-onnx 1.13.8 for Python 3.14, --target install)
#   run:        one of the names below (default: all)
# Env: CPUS (default 8,10,12,14: four distinct P-cores), STT_IMAGE (default grill-stt:local).
set -euo pipefail

[[ $# -ge 2 ]] || { sed -n 2,9p "$0" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
data=$(cd "$1" && pwd)
eng=$(cd "$2" && pwd)
shift 2
readonly here data eng
readonly image=${STT_IMAGE:-grill-stt:local} cpus=${CPUS:-8,10,12,14}
IFS=, read -r -a core <<<"$cpus"
readonly core
readonly py="/app/.venv/bin/python3 /bench/bench.py"

dock() {
	local name="grill-room-test-s17-$$"
	# Security: no network, so neither engine can phone home during the runs.
	docker run --rm --name "$name" --network none --cpuset-cpus="$cpus" --memory 6g --memory-swap 6g \
		--user "$(id -u):$(id -g)" -e HOME=/tmp -e D=/data -e PYTHONPATH=/app:/eng/pydeps -e PYTHONUNBUFFERED=1 \
		-e NEEDLE_CLI=/eng/needle -e WHISTLE_CACT=/eng/whistle.cact -e ORUKEET_MODEL=/eng/orukeet.gguf \
		-v "$data":/data -v "$eng":/eng:ro -v "$here":/bench:ro --entrypoint bash "$image" -c "$1" 2>&1 |
		grep -vE 'transcribe_cpp|telemetry' || true
}

# c<N>: N processes at once on the N longest meeting tracks, 4/N threads each on their own cores, paced.
conc() {
	local e=$1 n=$2 per=$((4 / $2)) cmd="" k c
	for ((k = 0; k < n; k++)); do
		c=$(IFS=,; echo "${core[*]:k*per:per}")
		cmd+="$py $e summre --track $k --threads $per --cpus $c --paced --tag $e.c$n & "
	done
	dock "${cmd} wait"
}

run() {
	case $1 in
	parakeet | orukeet | whistle | kroko)
		for s in piper devoxx summre; do dock "$py $1 $s"; done ;;
	whistle-kw)
		for s in piper devoxx summre; do dock "$py whistle $s --keywords piper --tag whistle.kw-piper"; done
		dock "$py whistle devoxx --keywords own --tag whistle.kw-own" ;;
	whistle-fr)
		for s in devoxx summre; do dock "$py whistle $s --language fr --tag whistle.lang-fr"; done ;;
	whistle-summre)
		# Reruns the meeting set after the WAV overflow fix (27 utterances held full-scale samples).
		dock "$py whistle summre"
		dock "$py whistle summre --keywords piper --tag whistle.kw-piper"
		dock "$py whistle summre --language fr --tag whistle.lang-fr"
		dock "$py whistle summre --depth 4 --tag whistle.depth4" ;;
	whistle-d4)
		for s in piper devoxx summre; do dock "$py whistle $s --depth 4 --tag whistle.depth4"; done ;;
	c2 | c4)
		for e in parakeet orukeet whistle; do conc "$e" "${1#c}"; done ;;
	*) echo "unknown run $1" >&2; return 1 ;;
	esac
}

for r in "${@:-parakeet orukeet whistle kroko whistle-kw whistle-fr whistle-d4 c2 c4}"; do
	for x in $r; do
		echo "=== $(date -u +%T) $x"
		run "$x"
	done
done
