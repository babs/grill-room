#!/usr/bin/env bash
# 010-AC18 inside the browser container: plain headed Google Chrome on the laptop profile, no
# automation flags. CDP attaches only for ac18.mjs setup and collect; in between, a second plain
# invocation opens a foreground tab that hides the room tab for HIDDEN_S seconds.
set -euo pipefail

readonly prof=/prof/ac18
readonly chrome=(google-chrome --no-sandbox --user-data-dir="$prof" --no-first-run --no-default-browser-check
	--password-store=basic --use-fake-ui-for-media-stream --autoplay-policy=no-user-gesture-required)

nice -n 10 taskset -c 0,2,4,6 "${chrome[@]}" --remote-debugging-port="$CDP_PORT" about:blank >/out/chrome.log 2>&1 &
readonly pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT
up=""
for _ in $(seq 60); do
	if curl -fs -o /dev/null "http://127.0.0.1:$CDP_PORT/json/version"; then
		up=1
		break
	fi
	sleep 1
done
[[ -n $up ]] || { echo "ac18.sh: Chrome's CDP port did not open" >&2; exit 1; }

node /h/ac18.mjs setup
detached=$(date +%s%3N)
sleep "${VISIBLE_S:-60}"
hide=$(date +%s%3N)
"${chrome[@]}" "http://127.0.0.1:$PORT/healthz" >>/out/chrome.log 2>&1
sleep "${HIDDEN_S:-600}"
echo "{\"detached\": $detached, \"hide\": $hide, \"collect\": $(date +%s%3N)}" >/out/ac18-times.json
node /h/ac18.mjs collect
