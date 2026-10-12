#!/usr/bin/env bash
# Starts one bench Job and saves its log. Needs KUBECONFIG, NS, STORAGE_CLASS and OUT (log dir).
# usage: run.sh <job name> <cpu limit> <memory> <every> <spec>...
#   e.g. run.sh small-4 4 3Gi 1 whisper:small:none whisper:small:both
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
readonly HERE

if (($# < 5)); then
    sed -n 3,4p "$0" >&2
    exit 2
fi
: "${NS:?}" "${STORAGE_CLASS:?}" "${OUT:?}" "${KUBECONFIG:?}"
name="$1" cpu="$2" mem="$3" every="$4"
shift 4
# Specs and the CPU limit are pasted into sed and YAML, and the limit becomes --threads.
if [[ ! "$cpu" =~ ^[0-9]+$ || ! "$every" =~ ^[0-9]+$ ]]; then
    echo "cpu limit and every must be integers" >&2
    exit 2
fi
for spec in "$@"; do
    if [[ ! "$spec" =~ ^[A-Za-z0-9._/-]+:[A-Za-z0-9._/-]+:(none|hot|prompt|both|sentence)$ ]]; then
        echo "bad spec: $spec" >&2
        exit 2
    fi
done
runs="$(printf '"%s", ' "$@")"

kubectl -n "$NS" create configmap s10-bench --from-file="$HERE/bench.py" --from-file="$HERE/script.json" \
    --from-file="$HERE/job.sh" --from-file="$HERE/prompt.txt" --dry-run=client -o yaml | kubectl -n "$NS" apply -f -
kubectl -n "$NS" delete job "$name" --ignore-not-found
sed -e "s/__NAME__/$name/; s/__CPU__/$cpu/; s/__MEM__/$mem/g; s/__THREADS__/$cpu/; s/__EVERY__/$every/" \
    -e "s|__RUNS__|${runs%, }|; s|__STORAGE_CLASS__|$STORAGE_CLASS|" "$HERE/job.yaml" | kubectl -n "$NS" apply -f -

until status="$(kubectl -n "$NS" get job "$name" -o jsonpath='{.status.succeeded}{.status.failed}')" && [[ -n "$status" ]]; do
    sleep 30
done
mkdir -p "$OUT"
kubectl -n "$NS" logs "job/$name" >"$OUT/$name.log"
echo "$name done (succeeded/failed: $status), log in $OUT/$name.log"
