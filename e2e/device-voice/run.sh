#!/usr/bin/env bash
# The device-voice e2e (010 Phase 2): LiveKit and the STT worker in Docker on the host network, on
# loopback ports apart from the main e2e (7690-7692), the mail e2e (7693-7696) and the compose
# stack's LiveKit (7880). Playwright then starts the e2e server against them and drives Chrome.
# Keys and tokens are generated per run and only ever passed through the environment.
# Every container goes with the script, a failure or an interrupt included; the worker's log is
# kept under test-results/ first, for the failure artifact.
# Usage, from e2e/: device-voice/run.sh [playwright args...]
set -euo pipefail

readonly image_livekit=livekit/livekit-server:v1.13.9
readonly image_stt=${DEVICE_VOICE_STT_IMAGE:-grill-stt:local}
run_id="$(date +%s)-$$"
readonly run_id
readonly livekit="grill-room-test-livekit-$run_id"
readonly stt="grill-room-test-stt-$run_id"
readonly out=test-results/device-voice

export DEVICE_VOICE_PORT=7697
export DEVICE_VOICE_METRICS_PORT=7698
export DEVICE_VOICE_STT_METRICS_PORT=7699
export DEVICE_VOICE_LIVEKIT_PORT=7887
export DEVICE_VOICE_STT_CONTAINER=$stt

die() {
	echo "device-voice: $*" >&2
	exit 1
}

cleanup() {
	mkdir -p "$out"
	# The worker may not have started: no log to keep then.
	docker logs "$stt" >"$out/stt.log" 2>&1 || true
	docker rm -f "$livekit" "$stt" >/dev/null 2>&1 || true
}

# wait_http polls url until it answers 2xx, for at most $2 seconds.
wait_http() {
	local url=$1 deadline=$((SECONDS + $2))
	until curl -fs -o /dev/null --max-time 2 "$url"; do
		((SECONDS < deadline)) || return 1
		sleep 1
	done
}

[[ -f device-voice/playwright.config.js ]] || die "run from e2e/"
[[ -d ${GRILL_ROOM_E2E_MODEL_DIR:-} ]] || die "GRILL_ROOM_E2E_MODEL_DIR must name the device model files (grill-room models fetch --dir <dir>)"
for cmd in docker curl openssl pnpm; do
	command -v "$cmd" >/dev/null || die "$cmd is missing"
done
docker image inspect "$image_stt" >/dev/null 2>&1 || die "no $image_stt image: run make stt-image first"

LIVEKIT_API_KEY="dv$(openssl rand -hex 8)"
LIVEKIT_API_SECRET=$(openssl rand -hex 32)
STT_TOKEN=$(openssl rand -hex 32)
export LIVEKIT_API_KEY LIVEKIT_API_SECRET STT_TOKEN
export LIVEKIT_KEYS="$LIVEKIT_API_KEY: $LIVEKIT_API_SECRET"
# Media on the host's own interfaces: the browser and the worker run on this host.
export LIVEKIT_CONFIG="port: $DEVICE_VOICE_LIVEKIT_PORT
bind_addresses: [127.0.0.1]
log_level: info
rtc:
  tcp_port: $((DEVICE_VOICE_LIVEKIT_PORT + 1))
  udp_port: $((DEVICE_VOICE_LIVEKIT_PORT + 2))
  use_external_ip: false"
export APP_ROOM_URL="http://127.0.0.1:$DEVICE_VOICE_PORT"
export APP_ROOM_TOKEN=$STT_TOKEN
export APP_LIVEKIT_URL="ws://127.0.0.1:$DEVICE_VOICE_LIVEKIT_PORT"
export APP_LIVEKIT_API_KEY=$LIVEKIT_API_KEY
export APP_LIVEKIT_API_SECRET=$LIVEKIT_API_SECRET
export APP_PORT=$DEVICE_VOICE_STT_METRICS_PORT

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker run -d --name "$livekit" --network host --memory 1g -e LIVEKIT_KEYS -e LIVEKIT_CONFIG "$image_livekit" >/dev/null
wait_http "http://127.0.0.1:$DEVICE_VOICE_LIVEKIT_PORT/" 60 || die "LiveKit did not come up"

# 4 CPUs and 3 GiB as compose gives it; one engine process is enough for one speaker.
docker run -d --name "$stt" --network host --cpus "${DEVICE_VOICE_STT_CPUS:-4}" --memory 3g --memory-swap 3g \
	-e APP_ROOM_URL -e APP_ROOM_TOKEN -e APP_LIVEKIT_URL -e APP_LIVEKIT_API_KEY -e APP_LIVEKIT_API_SECRET -e APP_PORT \
	-e APP_ENGINE_PROCESSES=1 -e APP_CPU_THREADS=2 "$image_stt" >/dev/null
wait_http "http://127.0.0.1:$DEVICE_VOICE_STT_METRICS_PORT/healthz" 180 || die "the STT worker did not come up"

pnpm exec playwright test -c device-voice/playwright.config.js "$@"
