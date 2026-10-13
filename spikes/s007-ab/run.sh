#!/usr/bin/env bash
# Starts one A/B Job, waits for it and saves its log. Needs KUBECONFIG, NS, OUT (log dir) and IMAGE
# (the stt image by digest). NODE, optional, pins the Job to that node: to compare runs on one node.
# usage: run.sh <job name> <engine> <cpu request> <set:talkers>...
#   e.g. run.sh p1 parakeet 900m summre:1 devoxx:1 piper:1
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
readonly HERE

if (($# < 4)); then
    sed -n 4,5p "$0" >&2
    exit 2
fi
: "${NS:?}" "${OUT:?}" "${KUBECONFIG:?}" "${IMAGE:?}"
name="$1" engine="$2" req="$3"
shift 3
# Everything below is pasted into sed and YAML.
for x in "$name" "$engine" "$req" "$@"; do
    if [[ ! "$x" =~ ^[A-Za-z0-9.:-]+$ ]]; then
        echo "bad argument: $x" >&2
        exit 2
    fi
done
if [[ ! "$IMAGE" =~ ^[A-Za-z0-9./:_-]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "IMAGE must be pinned by digest" >&2
    exit 2
fi

kubectl -n "$NS" delete job "$name" --ignore-not-found
pin=""
if [[ -n "${NODE:-}" ]]; then
    [[ "$NODE" =~ ^[a-z0-9.-]+$ ]] || { echo "bad NODE: $NODE" >&2; exit 2; }
    pin="\n        kubernetes.io/hostname: $NODE"
fi
sed -e "s|__NAME__|$name|; s|__IMAGE__|$IMAGE|; s|__ENGINE__|$engine|; s|__REQ__|$req|; s|__SPECS__|$*|" \
    -e "s|kubernetes.io/arch: amd64|kubernetes.io/arch: amd64$pin|" "$HERE/job.yaml" |
    kubectl -n "$NS" apply -f -

until status="$(kubectl -n "$NS" get job "$name" -o jsonpath='{.status.succeeded}{.status.failed}')" && [[ -n "$status" ]]; do
    sleep 60
done
status="$(kubectl -n "$NS" get job "$name" -o jsonpath='succeeded={.status.succeeded} failed={.status.failed}')"
mkdir -p "$OUT"
kubectl -n "$NS" logs "job/$name" >"$OUT/$name.log"
echo "$name done ($status), log in $OUT/$name.log"
