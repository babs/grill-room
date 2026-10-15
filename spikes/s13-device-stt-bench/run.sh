#!/usr/bin/env bash
# One S13 run: LiveKit and the STT worker in Docker, the e2e server on the host, the browsers in a
# Playwright container with Google Chrome under Xvfb, all on the host network.
# CPU: Alice gets the S11 "laptop" profile (cores 0,2,4,6 pinned, nice 10); the worker gets 4 other
# P-cores as compose gives it 4 CPUs; Bob, LiveKit and the server stay on the E-cores.
# usage: run.sh ac18|ac19 <data-dir> <out-dir>
#   data-dir: all.wav and manifest.json from prep.py. Needs MODEL_DIR (grill-room models fetch),
#   SERVER_BIN (make e2e-build), CHROME_IMAGE (the Playwright image with `playwright install chrome`).
#   ac19: BENCH_SECS (default 1800), WARM_NEW_TAB=1 (warm loads in a new tab, not a reload),
#   SAMPLE_MS (memory sampling, default 2000). ac18: VISIBLE_S, HIDDEN_S, EVERY_S.
set -euo pipefail

die() {
	echo "run.sh: $*" >&2
	exit 1
}

[[ $# -eq 3 ]] || { echo "usage: run.sh ac18|ac19 <data-dir> <out-dir>" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd) || die "cannot resolve the script's directory"
modules=$(cd "$here/../../e2e/node_modules" && pwd) || die "no e2e/node_modules: run pnpm install in e2e/"
data=$(cd "$2" && pwd) || die "no data dir $2"
id=$(date +%s)-$$
readonly here modules data id
readonly scenario=$1 out=$3
readonly stt_image=${STT_IMAGE:-grill-stt:local}
readonly lk_image=livekit/livekit-server:v1.13.9
readonly livekit="grill-room-test-lk-$id" stt="grill-room-test-stt-$id" browser="grill-room-test-browser-$id"
export PORT=7710 METRICS_PORT=7711 STT_METRICS_PORT=7712 LK_PORT=7720 CDP_PORT=7730
server_pid=""

cleanup() {
	docker logs "$stt" >"$out/stt.log" 2>&1 || true
	docker rm -f "$browser" "$livekit" "$stt" >/dev/null 2>&1 || true
	if [[ -n $server_pid ]]; then kill "$server_pid" 2>/dev/null || true; fi
}

wait_http() {
	local url=$1 deadline=$((SECONDS + $2))
	until curl -fs -o /dev/null --max-time 2 "$url"; do
		((SECONDS < deadline)) || return 1
		sleep 1
	done
}

[[ $scenario == ac18 || $scenario == ac19 ]] || die "unknown scenario $scenario"
[[ -f $data/all.wav && -f $data/manifest.json ]] || die "$data has no all.wav or manifest.json"
[[ -d ${MODEL_DIR:-} && -x ${SERVER_BIN:-} && -n ${CHROME_IMAGE:-} ]] || die "MODEL_DIR, SERVER_BIN and CHROME_IMAGE must be set"
mkdir -p "$out"

LIVEKIT_API_KEY="s13$(openssl rand -hex 8)"
LIVEKIT_API_SECRET=$(openssl rand -hex 32)
STT_TOKEN=$(openssl rand -hex 32)
ADMIN_TOKEN=$(openssl rand -hex 32)
export LIVEKIT_KEYS="$LIVEKIT_API_KEY: $LIVEKIT_API_SECRET" ADMIN_TOKEN
export LIVEKIT_CONFIG="port: $LK_PORT
bind_addresses: [127.0.0.1]
rtc:
  tcp_port: $((LK_PORT + 1))
  udp_port: $((LK_PORT + 2))
  use_external_ip: false"

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker run -d --name "$livekit" --network host --cpuset-cpus=16-27 --memory 1g -e LIVEKIT_KEYS -e LIVEKIT_CONFIG "$lk_image" >/dev/null
wait_http "http://127.0.0.1:$LK_PORT/" 60 || die "LiveKit did not come up"

# The compose settings: 2 engine processes of 2 threads, 4 CPUs, 3 GiB.
docker run -d --name "$stt" --network host --cpuset-cpus=8,10,12,14 --memory 3g --memory-swap 3g \
	-e APP_ROOM_URL="http://127.0.0.1:$PORT" -e APP_ROOM_TOKEN="$STT_TOKEN" -e APP_LIVEKIT_URL="ws://127.0.0.1:$LK_PORT" \
	-e APP_LIVEKIT_API_KEY="$LIVEKIT_API_KEY" -e APP_LIVEKIT_API_SECRET="$LIVEKIT_API_SECRET" -e APP_PORT="$STT_METRICS_PORT" \
	-e APP_ENGINE_PROCESSES=2 -e APP_CPU_THREADS=2 "$stt_image" >/dev/null

GRILL_ROOM_ADDR="127.0.0.1:$PORT" GRILL_ROOM_METRICS_ADDR="127.0.0.1:$METRICS_PORT" GRILL_ROOM_DB="$out/server.db" \
	GRILL_ROOM_URL="http://127.0.0.1:$PORT" GRILL_ROOM_ADMIN_TOKEN="$ADMIN_TOKEN" \
	GRILL_ROOM_LIVEKIT_URL="ws://127.0.0.1:$LK_PORT" GRILL_ROOM_LIVEKIT_API_URL="http://127.0.0.1:$LK_PORT" \
	GRILL_ROOM_LIVEKIT_API_KEY="$LIVEKIT_API_KEY" GRILL_ROOM_LIVEKIT_API_SECRET="$LIVEKIT_API_SECRET" \
	GRILL_ROOM_STT_TOKEN="$STT_TOKEN" GRILL_ROOM_DEVICE_STT_MODEL_DIR="$MODEL_DIR" LOG_FORMAT=json \
	taskset -c 16-27 "$SERVER_BIN" >"$out/server.log" 2>&1 &
server_pid=$!
wait_http "http://127.0.0.1:$PORT/healthz" 60 || die "the server did not come up"
wait_http "http://127.0.0.1:$STT_METRICS_PORT/healthz" 180 || die "the STT worker did not come up"

if [[ $scenario == ac19 ]]; then
	cpus=0,2,4,6,20-23
	# Alice on the laptop profile, Bob on four E-cores of the same container.
	cmd='nice -n 10 taskset -c 20-23 node /h/ac19.mjs bob & nice -n 10 taskset -c 0,2,4,6 node /h/ac19.mjs alice; wait'
else
	cpus=0,2,4,6
	cmd='bash /h/ac18.sh'
fi
docker run --rm --init --name "$browser" --network host --cpuset-cpus="$cpus" --memory=16g --memory-swap=16g --shm-size=2g \
	-e PORT -e CDP_PORT -e ADMIN_TOKEN -e BENCH_SECS -e SAMPLE_MS -e WARM_NEW_TAB -e EVERY_S -e HIDDEN_S -e VISIBLE_S \
	-v "$here":/h:ro -v "$modules":/nm:ro -v "$data":/d:ro -v "$(cd "$out" && pwd)":/out "$CHROME_IMAGE" \
	bash -c "xvfb-run -a -s '-screen 0 1280x800x24' bash -c '$cmd'; rc=\$?; chown -R $(id -u):$(id -g) /out; exit \$rc"
