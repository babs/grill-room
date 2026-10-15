#!/usr/bin/env bash
# Runs the host's codex binary (static musl) in the grill-room sandbox image: no network at all
# (--network none), read-only root, no capability. The only way out is a unix socket to the spike
# proxy, which socat inside the container exposes as 127.0.0.1:8787 for both the model base URL and
# HTTP(S)_PROXY, so every other host codex tries shows up in the proxy log as a refused CONNECT.
set -euo pipefail

usage() { echo "usage: $0 up|down <scratch-dir>" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
readonly cmd="$1" dir="$2"
readonly name="grill-room-test-s15-codex"
readonly image="${S15_IMAGE:-grill-room-sandbox:local}"

case "$cmd" in
  up)
    codex_dir="${S15_CODEX_DIR:?set S15_CODEX_DIR to the codex vendor/<triple> directory}"
    mkdir -p "$dir/sock"
    docker run -d --name "$name" --init \
      --network none --cap-drop ALL --security-opt no-new-privileges --read-only \
      --memory 2g --memory-swap 2g --cpus 2 --pids-limit 512 \
      --tmpfs /tmp --tmpfs /home/agent:uid=1000,gid=1000,mode=0700 \
      --tmpfs /workspace:uid=1000,gid=1000,mode=0755 \
      -v "$codex_dir:/opt/codex:ro" -v "$dir/sock:/run/s15" \
      -e CODEX_HOME=/home/agent/.codex \
      -e GRILL_ROOM_CODEX_BEARER="$(cat "$dir/bearer")" \
      -e HTTP_PROXY=http://127.0.0.1:8787 -e HTTPS_PROXY=http://127.0.0.1:8787 \
      -e http_proxy=http://127.0.0.1:8787 -e https_proxy=http://127.0.0.1:8787 \
      -e NO_PROXY=127.0.0.1,localhost -e no_proxy=127.0.0.1,localhost \
      -w /workspace "$image" sleep infinity >/dev/null
    docker exec -d "$name" socat TCP-LISTEN:8787,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/s15/proxy.sock
    ;;
  down)
    docker rm -f -v "$name" >/dev/null 2>&1 || true
    ;;
  *) usage ;;
esac
