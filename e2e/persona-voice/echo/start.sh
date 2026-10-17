#!/usr/bin/env bash
# Starts PipeWire with the loop, then Playwright's browser server on $1 (default 3000), under Xvfb:
# headed Chrome, whose audio goes to the default sink.
set -euo pipefail
export XDG_RUNTIME_DIR=/tmp/run
mkdir -p -m 700 "$XDG_RUNTIME_DIR"
eval "$(dbus-launch --sh-syntax)"
pipewire &
wireplumber &
pipewire-pulse &
for _ in $(seq 50); do pactl info >/dev/null 2>&1 && break; sleep 0.2; done
pactl load-module module-null-sink sink_name=loop sink_properties=device.description=loop >/dev/null
pactl set-default-sink loop
# Chrome lists no monitor source: a remap of the monitor is a plain one.
pactl load-module module-remap-source master=loop.monitor source_name=loopmic source_properties=device.description=loopmic >/dev/null
pactl set-default-source loopmic
Xvfb :99 -screen 0 1280x800x24 -nolisten tcp &
export DISPLAY=:99
exec playwright run-server --port "${1:-3000}" --host 127.0.0.1
