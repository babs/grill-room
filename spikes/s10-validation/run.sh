#!/usr/bin/env bash
# Starts one Job, waits for it and saves its log. Needs KUBECONFIG, NS and OUT (log dir).
# usage: run.sh <job name> <cpu request> <memory> setup|score|run <spec>...
#   setup needs 8Gi (SUMM-RE parquet row groups); e.g. run.sh ultra 900m 3Gi run "tcpp:parakeet-ultra-Q8_0.gguf summre none" "c2 tcpp:parakeet-ultra-Q8_0.gguf summre none"
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
readonly HERE

if (($# < 4)); then
    sed -n 3,4p "$0" >&2
    exit 2
fi
: "${NS:?}" "${OUT:?}" "${KUBECONFIG:?}"
name="$1" req="$2" mem="$3"
shift 3
# Everything below is pasted into sed and YAML.
for x in "$name" "$req" "$mem" "$@"; do
    if [[ ! "$x" =~ ^[A-Za-z0-9._:\ -]+$ ]]; then
        echo "bad argument: $x" >&2
        exit 2
    fi
done
args="$(printf '"%s", ' "$@")"

kubectl -n "$NS" create configmap s10d-bench \
    --from-file="$HERE/job.sh" --from-file="$HERE/run.py" --from-file="$HERE/prep.py" --from-file="$HERE/score.py" \
    --from-file="$HERE/glossary.json" --from-file="$HERE/../../stt/grill_stt/vad.py" \
    --from-file=labbench.py="$HERE/../s10-stt-models/bench.py" --from-file="$HERE/../s10-stt-models/script.json" \
    --dry-run=client -o yaml | kubectl -n "$NS" apply -f -
kubectl -n "$NS" delete job "$name" --ignore-not-found
sed -e "s/__NAME__/$name/; s/__REQ__/$req/; s/__MEM__/$mem/g; s/__ARGS__/${args%, }/" "$HERE/job.yaml" | kubectl -n "$NS" apply -f -

until status="$(kubectl -n "$NS" get job "$name" -o jsonpath='{.status.succeeded}{.status.failed}')" && [[ -n "$status" ]]; do
    sleep 30
done
status="$(kubectl -n "$NS" get job "$name" -o jsonpath='succeeded={.status.succeeded} failed={.status.failed}')"
mkdir -p "$OUT"
kubectl -n "$NS" logs "job/$name" >"$OUT/$name.log"
echo "$name done ($status), log in $OUT/$name.log"
