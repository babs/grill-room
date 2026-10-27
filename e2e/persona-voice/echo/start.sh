#!/usr/bin/env bash
# Starts PipeWire with the loop, then Playwright's browser server on $1 (default 3000), under Xvfb:
# headed Chrome, whose audio goes to the default sink.
set -euo pipefail

readonly ready_timeout_s=30

die() {
	echo "start.sh: $*" >&2
	exit 1
}

pulse_up() {
	pactl info >/dev/null 2>&1
}

# pactl set-default-* answers "Not supported" until WirePlumber publishes this metadata.
default_metadata_up() {
	[[ $(timeout 2 pw-metadata -n default 2>/dev/null) == *'Found "default" metadata'* ]]
}

# wait_for waits for "$@" to succeed, until the shared deadline.
wait_for() {
	local what=$1
	shift
	until "$@"; do
		((SECONDS < deadline)) || die "$what not ready after ${ready_timeout_s} s"
		sleep 0.2
	done
}

export XDG_RUNTIME_DIR=/tmp/run
mkdir -m 700 "$XDG_RUNTIME_DIR"
eval "$(dbus-launch --sh-syntax)"
pipewire &
wireplumber &
pipewire-pulse &
deadline=$((SECONDS + ready_timeout_s))
wait_for "pipewire-pulse" pulse_up
wait_for "WirePlumber's default metadata" default_metadata_up
pactl load-module module-null-sink sink_name=loop sink_properties=device.description=loop >/dev/null
pactl set-default-sink loop
# Chrome lists no monitor source: a remap of the monitor is a plain one.
pactl load-module module-remap-source master=loop.monitor source_name=loopmic source_properties=device.description=loopmic >/dev/null
pactl set-default-source loopmic
# -noreset: X resets when its last client leaves, and a Chrome launched during the reset finds no display.
Xvfb :99 -screen 0 1280x800x24 -nolisten tcp -noreset &
export DISPLAY=:99
exec playwright run-server --port "${1:-3000}" --host 127.0.0.1
