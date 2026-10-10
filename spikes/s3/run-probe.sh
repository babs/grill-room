#!/usr/bin/env bash
# Runs one claude command in a sandbox container with a tcpdump sidecar in its netns.
# Usage: [SEED=dir] [EXTRA_ENV="-e K=V"] [HOME_DIR=/home/agent] [WORKDIR=/workspace] run-probe.sh <name> <network> <home-volume> <claude args...>
# Output: $OUT/<name>.out (claude stdout+stderr), $OUT/<name>.pcap.txt (non-proxy packets).
set -euo pipefail
name=$1 net=$2 vol=$3; shift 3
G=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${OUT:-$G/.scratchpad/logs}
HOME_DIR=${HOME_DIR:-/home/agent}
WORKDIR=${WORKDIR:-/workspace}
# shellcheck disable=SC2086
docker run -d --name "$name" --label grill-room-spike=1 --network "$net" \
  --add-host=host.docker.internal:172.24.0.1 \
  -e HOME="$HOME_DIR" -w "$WORKDIR" -v "$vol:$HOME_DIR" -v "${SEED:-$G/.scratchpad/seed}:/seed:ro" \
  -e ANTHROPIC_BASE_URL=http://host.docker.internal:8787 ${EXTRA_ENV:-} \
  --entrypoint sh grs-claude:spike -c "mkdir -p '$WORKDIR' 2>/dev/null; exec /usr/local/bin/entrypoint.sh sleep infinity" >/dev/null
docker run -d --name "$name-dump" --label grill-room-spike=1 --cap-add NET_ADMIN --network "container:$name" \
  grs-tcpdump:spike -i any -n -p -l -tttt -v -A -s 0 'not (host 172.24.0.1 and port 8787)' >/dev/null
sleep 2
set +e
docker exec "$name" claude "$@" >"$OUT/$name.out" 2>&1
echo "exit=$?" >>"$OUT/$name.out"
set -e
sleep 3
docker logs "$name-dump" >"$OUT/$name.pcap.txt" 2>&1
docker rm -f "$name" "$name-dump" >/dev/null
