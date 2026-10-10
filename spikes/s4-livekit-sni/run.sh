#!/usr/bin/env bash
# S4 end to end: LiveKit up, temporary traefik rule, Playwright cases, then full teardown.
# First run needs DEV_DOMAIN and TRAEFIK_RULES_DIR (exported, or in the repo's .env) to write ./.env.
# Usage: ./run.sh [playwright args], S4_SECONDS=75 ./run.sh --grep A-relay
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
readonly here
readonly logs="$here/../../.scratchpad/s4"
readonly rule_name="zz-s4-livekit-spike.yaml"
cd "$here"

if [[ ! -f .env ]]; then
  # shellcheck disable=SC1091
  [[ -f ../../.env ]] && set -a && . ../../.env && set +a
  : "${DEV_DOMAIN:?set DEV_DOMAIN}" "${TRAEFIK_RULES_DIR:?set TRAEFIK_RULES_DIR}"
  bind="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')"
  umask 077
  cat > .env <<EOF
DEV_DOMAIN=$DEV_DOMAIN
TRAEFIK_RULES_DIR=$TRAEFIK_RULES_DIR
LK_BIND=$bind
LIVEKIT_API_KEY=API$(openssl rand -hex 6)
LIVEKIT_API_SECRET=$(openssl rand -hex 32)
EOF
fi
set -a
# shellcheck disable=SC1091
. ./.env
set +a
readonly rule="$TRAEFIK_RULES_DIR/$rule_name"

cleanup() {
  mkdir -p "$logs"
  docker compose logs --no-color > "$logs/livekit.log" 2>&1 || echo "no livekit logs" >&2
  rm -f "$rule"
  docker compose down --remove-orphans
}
trap cleanup EXIT

docker compose up -d --wait
# shellcheck disable=SC2016 # literal: envsubst substitutes only this variable
envsubst '${DEV_DOMAIN}' < traefik-rule.yaml > "$rule"

for _ in $(seq 30); do
  curl -fsS -o /dev/null "https://livekit.$DEV_DOMAIN/" && break
  sleep 1
done
if ! curl -fsS -o /dev/null "https://livekit.$DEV_DOMAIN/"; then
  echo "livekit.$DEV_DOMAIN not reachable through traefik" >&2
  exit 1
fi

pnpm exec playwright test --reporter=list "$@"
