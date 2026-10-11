#!/usr/bin/env bash
# S8 test 1: one haiku turn with the setup token handed straight to Claude Code, egress allowed.
# Usage: direct.sh [token-file]   (default: <repo>/.scratchpad/token)
# Output: <repo>/.scratchpad/s8/direct/{out.json,debug.log}, tokens redacted from the debug log.
set -euo pipefail
G=$(cd "$(dirname "$0")/../.." && pwd)
readonly G
readonly TOKEN_FILE=${1:-$G/.scratchpad/token}
readonly IMAGE=${IMAGE:-grill-room-sandbox:local}
readonly OUT=$G/.scratchpad/s8/direct
readonly NAME=grs-s8-direct

[[ -r "$TOKEN_FILE" ]] || { echo "token file $TOKEN_FILE not readable" >&2; exit 2; }
tok=$(tr -d '[:space:]' <"$TOKEN_FILE")
[[ -n "$tok" ]] || { echo "token file $TOKEN_FILE is empty" >&2; exit 2; }
mkdir -p "$OUT"
envfile=$(mktemp "$G/.scratchpad/s8/env.XXXXXX")
trap 'rm -f "$envfile"; docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
chmod 600 "$envfile"
# Security: the token goes through a 0600 env file, never argv.
printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$tok" >"$envfile"

docker run -d --name "$NAME" --label grill-room-spike=1 --env-file "$envfile" \
  --entrypoint sleep "$IMAGE" infinity >/dev/null
rm -f "$envfile"
set +e
docker exec "$NAME" claude -p --model haiku --output-format json --debug-file /tmp/debug.log "say ok" >"$OUT/out.json" 2>&1
echo "exit=$?" >>"$OUT/out.json"
set -e
docker exec "$NAME" cat /tmp/debug.log | sed -E 's/sk-ant-[A-Za-z0-9_-]+/<redacted>/g' >"$OUT/debug.log"
