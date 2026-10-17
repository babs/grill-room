#!/usr/bin/env bash
# The persona voice e2e (spec 013): LiveKit, the STT worker and the tts pod on the host network, on
# loopback ports apart from the other e2e stacks (7701-7705, LiveKit 7891-7893). Playwright then
# starts the e2e server against them and drives Chrome. The tts pod runs from source on its fake
# engine, or on Pocket's ungated weights with PERSONA_VOICE_SOURCE_ENGINE=pocket, or, with
# PERSONA_VOICE_TTS_IMAGE set (a `make tts-image` build), on the image. PERSONA_VOICE_ECHO_IMAGE (a
# build of persona-voice/echo/) adds a Chrome with its own PipeWire loop on port 7705 (013-AC35).
# Keys and tokens are generated per run and only ever passed through the environment. Everything
# started goes with the script, a failure or an interrupt included; logs stay under test-results/.
# Usage, from e2e/: persona-voice/run.sh [playwright args...]
set -euo pipefail

readonly image_livekit=livekit/livekit-server:v1.13.9
readonly image_stt=${PERSONA_VOICE_STT_IMAGE:-grill-stt:local}
readonly image_tts=${PERSONA_VOICE_TTS_IMAGE:-}
readonly image_echo=${PERSONA_VOICE_ECHO_IMAGE:-}
readonly source_engine=${PERSONA_VOICE_SOURCE_ENGINE:-fake}
run_id="$(date +%s)-$$"
readonly run_id
readonly livekit="grill-room-test-livekit-$run_id"
readonly stt="grill-room-test-stt-$run_id"
readonly tts="grill-room-test-tts-$run_id"
readonly echo="grill-room-test-echo-$run_id"
readonly out=test-results/persona-voice
tts_pid=""

export PERSONA_VOICE_PORT=7701
export PERSONA_VOICE_METRICS_PORT=7702
export PERSONA_VOICE_STT_METRICS_PORT=7703
export PERSONA_VOICE_TTS_METRICS_PORT=7704
export PERSONA_VOICE_LIVEKIT_PORT=7891
export PERSONA_VOICE_STT_CONTAINER=$stt
export PERSONA_VOICE_ENGINE=${image_tts:+pocket}
export PERSONA_VOICE_ENGINE=${PERSONA_VOICE_ENGINE:-$source_engine}

die() {
	echo "persona-voice: $*" >&2
	exit 1
}

cleanup() {
	mkdir -p "$out"
	docker logs "$stt" >"$out/stt.log" 2>&1 || true
	if [[ -n $image_tts ]]; then
		docker logs "$tts" >"$PWD/$out/tts.log" 2>&1 || true
	elif [[ -n $tts_pid ]]; then
		kill "$tts_pid" 2>/dev/null || true
		wait "$tts_pid" 2>/dev/null || true
	fi
	if [[ -n $image_echo ]]; then
		docker logs "$echo" >"$out/echo.log" 2>&1 || true
	fi
	docker rm -f "$livekit" "$stt" "$tts" "$echo" >/dev/null 2>&1 || true
}

# wait_http polls url until it answers 2xx, for at most $2 seconds.
wait_http() {
	local url=$1 deadline=$((SECONDS + $2))
	until curl -fs -o /dev/null --max-time 2 "$url"; do
		((SECONDS < deadline)) || return 1
		sleep 1
	done
}

[[ -f persona-voice/playwright.config.js ]] || die "run from e2e/"
for cmd in docker curl openssl pnpm uv; do
	command -v "$cmd" >/dev/null || die "$cmd is missing"
done
docker image inspect "$image_stt" >/dev/null 2>&1 || die "no $image_stt image: run make stt-image first"
if [[ -n $image_tts ]]; then
	docker image inspect "$image_tts" >/dev/null 2>&1 || die "no $image_tts image: run make tts-image first"
fi

LIVEKIT_API_KEY="pv$(openssl rand -hex 8)"
LIVEKIT_API_SECRET=$(openssl rand -hex 32)
STT_TOKEN=$(openssl rand -hex 32)
TTS_TOKEN=$(openssl rand -hex 32)
export LIVEKIT_API_KEY LIVEKIT_API_SECRET STT_TOKEN TTS_TOKEN
export LIVEKIT_KEYS="$LIVEKIT_API_KEY: $LIVEKIT_API_SECRET"
export LIVEKIT_CONFIG="port: $PERSONA_VOICE_LIVEKIT_PORT
bind_addresses: [127.0.0.1]
log_level: info
rtc:
  tcp_port: $((PERSONA_VOICE_LIVEKIT_PORT + 1))
  udp_port: $((PERSONA_VOICE_LIVEKIT_PORT + 2))
  use_external_ip: false"
export APP_ROOM_URL="http://127.0.0.1:$PERSONA_VOICE_PORT"
export APP_LIVEKIT_URL="ws://127.0.0.1:$PERSONA_VOICE_LIVEKIT_PORT"
export APP_LIVEKIT_API_KEY=$LIVEKIT_API_KEY
export APP_LIVEKIT_API_SECRET=$LIVEKIT_API_SECRET

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker run -d --name "$livekit" --network host --memory 1g -e LIVEKIT_KEYS -e LIVEKIT_CONFIG "$image_livekit" >/dev/null
wait_http "http://127.0.0.1:$PERSONA_VOICE_LIVEKIT_PORT/" 60 || die "LiveKit did not come up"

docker run -d --name "$stt" --network host --cpus "${PERSONA_VOICE_STT_CPUS:-2}" --memory 3g --memory-swap 3g \
	-e APP_ROOM_URL -e APP_LIVEKIT_URL -e APP_LIVEKIT_API_KEY -e APP_LIVEKIT_API_SECRET \
	-e APP_ROOM_TOKEN="$STT_TOKEN" -e APP_PORT="$PERSONA_VOICE_STT_METRICS_PORT" \
	-e APP_ENGINE_PROCESSES=1 -e APP_CPU_THREADS=2 "$image_stt" >/dev/null
wait_http "http://127.0.0.1:$PERSONA_VOICE_STT_METRICS_PORT/healthz" 180 || die "the STT worker did not come up"

# The pod connects before the server is up and retries; the bots leave 5 s after their room empties.
if [[ -n $image_tts ]]; then
	docker run -d --name "$tts" --network host --cpus "${PERSONA_VOICE_TTS_CPUS:-2}" --memory 2560m --memory-swap 2560m --read-only --tmpfs /tmp \
		-e APP_ROOM_URL -e APP_LIVEKIT_URL -e APP_LIVEKIT_API_KEY -e APP_LIVEKIT_API_SECRET \
		-e APP_ROOM_TOKEN="$TTS_TOKEN" -e APP_PORT="$PERSONA_VOICE_TTS_METRICS_PORT" -e APP_LEAVE_AFTER_S=5 -e APP_CPU_THREADS="${PERSONA_VOICE_TTS_THREADS:-1}" -e APP_LOG_LEVEL="${PERSONA_VOICE_TTS_LOG_LEVEL:-info}" "$image_tts" >/dev/null
else
	mkdir -p "$out"
	APP_ROOM_TOKEN="$TTS_TOKEN" APP_PORT="$PERSONA_VOICE_TTS_METRICS_PORT" APP_ENGINE="$source_engine" APP_MODELS_DIR=/nonexistent APP_CPU_THREADS=1 APP_LEAVE_AFTER_S=5 APP_LOG_LEVEL="${PERSONA_VOICE_TTS_LOG_LEVEL:-info}" \
		uv run --directory ../tts --frozen python -m grill_tts >"$PWD/$out/tts.log" 2>&1 &
	tts_pid=$!
fi
wait_http "http://127.0.0.1:$PERSONA_VOICE_TTS_METRICS_PORT/healthz" 600 || die "the tts pod did not come up"

if [[ -n $image_echo ]]; then
	docker image inspect "$image_echo" >/dev/null 2>&1 || die "no $image_echo image: build persona-voice/echo/ first"
	docker run -d --name "$echo" --network host --memory 2g --shm-size 1g "$image_echo" 7705 >/dev/null
	export ECHO_BROWSER_WS=ws://127.0.0.1:7705/
	deadline=$((SECONDS + 60))
	until docker logs "$echo" 2>&1 | grep -q 'Listening on'; do
		((SECONDS < deadline)) || die "the echo browser did not come up"
		sleep 1
	done
fi

# A real Opus sample for the voice picker's ▶, written the way the image's build writes them.
mkdir -p "$out"
export PERSONA_VOICE_SAMPLE="$PWD/$out/sample.ogg"
uv run --directory ../tts --frozen python -c 'import sys, numpy as n, soundfile as f; t = n.arange(24000) / 24000; f.write(sys.argv[1], (0.1 * n.sin(2 * n.pi * 220 * t)).astype("float32"), 24000, format="OGG", subtype="OPUS")' "$PERSONA_VOICE_SAMPLE"

pnpm exec playwright test -c persona-voice/playwright.config.js "$@"
