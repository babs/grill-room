#!/usr/bin/env bash
# The video e2e (spec 030): LiveKit alone on the host network, on loopback ports apart from the
# other e2e stacks (server 7721-7722, LiveKit 7901-7903). Playwright then starts the e2e server
# against it and drives two Chromes. Keys are generated per run and only ever passed through the
# environment. LiveKit goes with the script, a failure or an interrupt included; its log stays
# under test-results/.
# Usage, from e2e/: video/run.sh [playwright args...]
set -euo pipefail

readonly image_livekit=livekit/livekit-server:v1.13.9
run_id="$(date +%s)-$$"
readonly run_id
readonly livekit="grill-room-test-video-livekit-$run_id"
readonly out=test-results/video

export VIDEO_PORT=7721
export VIDEO_METRICS_PORT=7722
export VIDEO_LIVEKIT_PORT=7901

die() {
	echo "video: $*" >&2
	exit 1
}

cleanup() {
	mkdir -p "$out"
	docker logs "$livekit" >"$out/livekit.log" 2>&1 || true
	docker rm -f "$livekit" >/dev/null 2>&1 || true
}

# wait_http polls url until it answers 2xx, for at most $2 seconds.
wait_http() {
	local url=$1 deadline=$((SECONDS + $2))
	until curl -fs -o /dev/null --max-time 2 "$url"; do
		((SECONDS < deadline)) || return 1
		sleep 1
	done
}

[[ -f video/playwright.config.js ]] || die "run from e2e/"
[[ -n ${GRILL_ROOM_TEST_DATABASE_URL:-} ]] || die "GRILL_ROOM_TEST_DATABASE_URL must name a PostgreSQL 16 server (make pg-up prints one)"
for cmd in docker curl openssl pnpm; do
	command -v "$cmd" >/dev/null || die "$cmd is missing"
done

LIVEKIT_API_KEY="vd$(openssl rand -hex 8)"
LIVEKIT_API_SECRET=$(openssl rand -hex 32)
export LIVEKIT_API_KEY LIVEKIT_API_SECRET
export LIVEKIT_KEYS="$LIVEKIT_API_KEY: $LIVEKIT_API_SECRET"
export LIVEKIT_CONFIG="port: $VIDEO_LIVEKIT_PORT
bind_addresses: [127.0.0.1]
log_level: info
rtc:
  tcp_port: $((VIDEO_LIVEKIT_PORT + 1))
  udp_port: $((VIDEO_LIVEKIT_PORT + 2))
  use_external_ip: false"

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker run -d --name "$livekit" --network host --memory 1g -e LIVEKIT_KEYS -e LIVEKIT_CONFIG "$image_livekit" >/dev/null
wait_http "http://127.0.0.1:$VIDEO_LIVEKIT_PORT/" 60 || die "LiveKit did not come up"

pnpm exec playwright test -c video/playwright.config.js "$@"
