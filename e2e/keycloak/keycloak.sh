#!/usr/bin/env bash
# Keycloak for the e2e keycloak stack (spec 027): dev mode, realm/ imported at start, published on
# 127.0.0.1 only. A test fixture, never a real provider.
#   keycloak.sh start <port> <server-url>   starts it and waits for the realm's discovery document
#   keycloak.sh run <port> <server-url>     start, then hold until a signal or the container ends; removes it on exit
#   keycloak.sh stop <port>                 removes it
set -euo pipefail

readonly IMAGE='quay.io/keycloak/keycloak:26.8.1@sha256:a78d6b79572168cb25886b80a8b5b8d4ed4390743e3eecb810538683e209dd6b'
readonly REALM='grill-room'
readonly START_TIMEOUT=180

usage() {
  echo "usage: $0 start|run <port> <server-url> | stop <port>" >&2
  exit 2
}

container() {
  echo "grill-room-e2e-keycloak-$1"
}

start() {
  local port="$1" url="$2" name realm_dir discovery deadline
  name="$(container "$port")"
  realm_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/realm"
  discovery="http://127.0.0.1:$port/realms/$REALM/.well-known/openid-configuration"
  # A container left by a killed run holds the name and the port.
  docker rm -f "$name" >/dev/null
  # Pulled first so START_TIMEOUT counts Keycloak's start only.
  docker pull -q "$IMAGE" >/dev/null
  # --hostname pins the issuer to localhost: the server and the browser both reach it there.
  docker run -d --name "$name" -p "127.0.0.1:$port:8080" --memory 1g \
    -e GRILL_ROOM_E2E_URL="$url" -v "$realm_dir:/opt/keycloak/data/import:ro" \
    "$IMAGE" start-dev --import-realm --hostname "http://localhost:$port" >/dev/null
  deadline=$((SECONDS + START_TIMEOUT))
  until curl -sf --max-time 5 -o /dev/null "$discovery"; do
    if [[ "$(docker inspect -f '{{.State.Running}}' "$name")" != true || $SECONDS -ge $deadline ]]; then
      echo "keycloak.sh: $discovery did not answer within ${START_TIMEOUT}s; last logs:" >&2
      docker logs --tail 40 "$name" >&2
      docker rm -f "$name" >/dev/null
      exit 1
    fi
    sleep 1
  done
}

stop() {
  docker rm -f "$(container "$1")" >/dev/null
}

run() {
  local name parent="$PPID"
  name="$(container "$1")"
  # Global: the EXIT trap may run outside this function.
  run_port="$1"
  trap 'stop "$run_port"' EXIT
  trap 'exit 143' TERM
  trap 'exit 130' INT
  start "$1" "$2"
  echo "keycloak.sh: $name ready on http://localhost:$1" >&2
  # Ends with its parent too: a runner killed outright (pnpm exec on Ctrl-C) sends no signal, and
  # this script is then reparented.
  while (( $(ps -o ppid= -p "$$") == parent )); do
    if [[ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]]; then
      echo "keycloak.sh: $name stopped by itself" >&2
      exit 1
    fi
    # wait, unlike a foreground command, returns as soon as a trapped signal arrives.
    sleep 2 &
    wait "$!"
  done
  echo "keycloak.sh: parent $parent gone, removing $name" >&2
}

main() {
  [[ $# -ge 2 && $2 =~ ^[0-9]+$ ]] || usage
  if ! command -v docker >/dev/null; then
    echo "keycloak.sh: docker is not installed; the Keycloak e2e stack needs it" >&2
    exit 1
  fi
  if ! docker info >/dev/null 2>&1; then
    echo "keycloak.sh: cannot reach the Docker daemon; the Keycloak e2e stack needs it" >&2
    exit 1
  fi
  case "$1" in
    start) [[ $# -eq 3 ]] || usage; start "$2" "$3" ;;
    run) [[ $# -eq 3 ]] || usage; run "$2" "$3" ;;
    stop) [[ $# -eq 2 ]] || usage; stop "$2" ;;
    *) usage ;;
  esac
}

main "$@"
