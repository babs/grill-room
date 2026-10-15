#!/usr/bin/env bash
# Starts or stops the spike proxy; state (pid, log, bearer) lives in the scratch dir given.
set -euo pipefail
usage() { echo "usage: $0 start|stop <scratch-dir> [listen-addr]" >&2; exit 2; }
[[ $# -ge 2 ]] || usage
readonly cmd="$1" dir="$2" listen="${3:-127.0.0.1:18787}"
readonly dummy_account="00000000-0000-4000-8000-00000000a001"
case "$cmd" in
  start)
    [[ -s "$dir/bearer" ]] || { echo "no $dir/bearer" >&2; exit 1; }
    S15_DUMMY_BEARER="$(cat "$dir/bearer")" S15_DUMMY_ACCOUNT="$dummy_account" \
      nohup "$dir/s15-proxy" -listen "$listen" >> "$dir/proxy.log" 2>&1 &
    echo $! > "$dir/proxy.pid"
    ;;
  stop)
    if [[ -s "$dir/proxy.pid" ]]; then kill "$(cat "$dir/proxy.pid")" || true; rm -f "$dir/proxy.pid"; fi
    ;;
  *) usage ;;
esac
