#!/usr/bin/env bash
# S8 test 2: one haiku turn through the bearer-swap proxy, the setup token as its token source.
# Mirrors the daemon sandbox: internal network, socat relay aliased "proxy", dummy credentials
# and oauthAccount stub in the container. Skips the squid egress proxy (tools only, not auth).
# Usage: recheck.sh [token-file]   (default: <repo>/.scratchpad/token)
# Env: DUMMY_EXPIRES_MS (expiry the dummy credentials claim; default now + 10 years), PORT, IMAGE.
# Output: <repo>/.scratchpad/s8/<UTC stamp>/{out.json,debug.log,proxy.log}; one line appended
# to <repo>/.scratchpad/s8/recheck.log. Exit 0 only if the turn succeeded (is_error false).
set -euo pipefail
G=$(cd "$(dirname "$0")/../.." && pwd)
readonly G
readonly TOKEN_FILE=${1:-$G/.scratchpad/token}
readonly IMAGE=${IMAGE:-grill-room-sandbox:local}
readonly PORT=${PORT:-8798}
readonly NAME=grs-s8
readonly LABEL=grill-room-spike=1
readonly S8=$G/.scratchpad/s8
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
readonly STAMP OUT=$S8/$STAMP

for c in docker go; do
  command -v "$c" >/dev/null || { echo "missing command: $c" >&2; exit 1; }
done
[[ -r "$TOKEN_FILE" ]] || { echo "token file $TOKEN_FILE not readable" >&2; exit 2; }
# Whitespace stripped: a trailing newline would break the JSON below.
tok=$(tr -d '[:space:]' <"$TOKEN_FILE")
[[ -n "$tok" ]] || { echo "token file $TOKEN_FILE is empty" >&2; exit 2; }
mkdir -p "$OUT" "$S8/bin"

proxy_pid=""
credsdir=$(mktemp -d "$S8/creds.XXXXXX")
# Cleanup runs after partial failures too: missing objects are expected.
cleanup() {
  if [[ -n "$proxy_pid" ]]; then kill "$proxy_pid" 2>/dev/null || true; fi
  docker rm -f "$NAME" "$NAME-relay" >/dev/null 2>&1 || true
  docker network rm "$NAME" >/dev/null 2>&1 || true
  rm -rf "$credsdir"
}
trap cleanup EXIT

if ! go build -C "$G/spikes/s3/proxy" -o "$S8/bin/grs-proxy" .; then
  echo "proxy build failed" >&2; exit 1
fi
gw=$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')

# Security: the proxy reads the token from a 0600 file in the shape it already parses; removed on exit.
# The 1-year expiry is nominal (setup-token lifetime) and only silences the proxy's expiry warning.
umask 077
printf '{"claudeAiOauth":{"accessToken":"%s","expiresAt":%s}}' \
  "$tok" "$(( $(date +%s) * 1000 + 365 * 86400000 ))" >"$credsdir/creds.json"
dummy="sk-ant-oat01-grill-room-dummy-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PROXY_LISTEN="$gw:$PORT" PROXY_EXPECTED_BEARER="$dummy" PROXY_CREDENTIALS_FILE="$credsdir/creds.json" \
  LOG_LEVEL=info "$S8/bin/grs-proxy" >"$OUT/proxy.log" 2>&1 &
proxy_pid=$!
sleep 1
if ! kill -0 "$proxy_pid" 2>/dev/null; then
  echo "proxy failed to start:" >&2; tail -n 5 "$OUT/proxy.log" >&2; exit 1
fi

hardening=(--label "$LABEL" --cap-drop ALL --security-opt no-new-privileges --read-only)
docker network create --internal --label "$LABEL" "$NAME" >/dev/null
docker run -d --name "$NAME-relay" "${hardening[@]}" --add-host "host.docker.internal:$gw" \
  --entrypoint socat "$IMAGE" "TCP-LISTEN:8787,fork,reuseaddr" "TCP:host.docker.internal:$PORT" >/dev/null
docker network connect --alias proxy "$NAME" "$NAME-relay"
docker run -d --name "$NAME" --network "$NAME" "${hardening[@]}" \
  --tmpfs /tmp --tmpfs /home/agent:uid=1000,gid=1000 --tmpfs /workspace:uid=1000,gid=1000 \
  -e ANTHROPIC_BASE_URL=http://proxy:8787 -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  -e ENABLE_CLAUDEAI_MCP_SERVERS=false --entrypoint sleep "$IMAGE" infinity >/dev/null

now_ms=$(( $(date +%s) * 1000 ))
expires_ms=${DUMMY_EXPIRES_MS:-$(( now_ms + 10 * 365 * 86400000 ))}
# Same stubs as the daemon's writeStubs: a dummy bearer with a refresh token, and a fake account.
docker exec -i "$NAME" sh -c 'mkdir -p ~/.claude && cat > ~/.claude/.credentials.json' <<JSON
{"claudeAiOauth":{"accessToken":"$dummy","refreshToken":"grill-room-dummy-refresh","expiresAt":$expires_ms,"subscriptionType":"max","scopes":["user:inference","user:profile","user:sessions:claude_code"]}}
JSON
docker exec -i "$NAME" sh -c 'cat > ~/.claude.json' <<JSON
{"oauthAccount":{"accountUuid":"00000000-0000-4000-8000-000000000001","emailAddress":"agent@grill-room.invalid","organizationUuid":"00000000-0000-4000-8000-000000000002","billingType":"stripe_subscription","accountCreatedAt":"2025-01-01T00:00:00Z","subscriptionCreatedAt":"2025-01-01T00:00:00Z","ccOnboardingFlags":{},"profileFetchedAt":$now_ms}}
JSON

set +e
docker exec "$NAME" claude -p --model haiku --output-format json --debug-file /tmp/debug.log "say ok" >"$OUT/out.json" 2>&1
rc=$?
set -e
# No debug log when claude died before writing one; the exit code already records it.
docker exec "$NAME" cat /tmp/debug.log | sed -E 's/sk-ant-[A-Za-z0-9_-]+/<redacted>/g' >"$OUT/debug.log" || true
sleep 1
statuses=$(grep -o '"path":"[^"]*","status":[0-9]*' "$OUT/proxy.log" | tr '\n' ' ' || true)
result=$(grep -o '"is_error":[a-z]*' "$OUT/out.json" | head -1 || true)
echo "$STAMP exit=$rc ${result:-is_error=unknown} $statuses" | tee -a "$S8/recheck.log"
[[ "$rc" -eq 0 && "$result" == '"is_error":false' ]]
