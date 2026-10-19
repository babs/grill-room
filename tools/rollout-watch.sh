#!/usr/bin/env bash
# 017-AC19: posts one line per second into a fresh room while a stream stays open on it, as a
# browser's would, then reports lines posted against lines received. Run it through the ingress
# during a rollout or the loss of one pod (docs/rollout.md). Exit 1 on any loss, reorder or duplicate.
set -euo pipefail

readonly usage="usage: GRILL_ROOM_URL=<base url> GRILL_ROOM_ADMIN_TOKEN=<token> tools/rollout-watch.sh
optional: ROLLOUT_WATCH_LINES=<lines to post, one per second, default 300>
          ROLLOUT_WATCH_SETTLE=<seconds to wait for the last lines after posting, default 15>
Ctrl-C stops posting early and reports."

for c in curl jq awk; do
	if ! command -v "$c" >/dev/null; then
		echo "rollout-watch: $c is missing" >&2
		exit 1
	fi
done
if [[ -z ${GRILL_ROOM_URL:-} || -z ${GRILL_ROOM_ADMIN_TOKEN:-} ]]; then
	echo "$usage" >&2
	exit 2
fi
readonly lines=${ROLLOUT_WATCH_LINES:-300} settle=${ROLLOUT_WATCH_SETTLE:-15}
if [[ ! $lines =~ ^[1-9][0-9]*$ || ! $settle =~ ^[0-9]+$ ]]; then
	echo "$usage" >&2
	exit 2
fi
readonly base=${GRILL_ROOM_URL%/}

tmpdir=$(mktemp -d)
room=""
stream_pid=""
cleanup() {
	touch "$tmpdir/stop"
	if [[ -n $stream_pid ]]; then
		stop_stream
	fi
	if [[ -n $room ]]; then
		# Best effort: a room left behind is only a test room, and the report is already out.
		curl -sS --max-time 10 -o /dev/null -X DELETE -H @"$tmpdir/admin" "$base/api/admin/rooms/$room" ||
			echo "rollout-watch: room $room not deleted" >&2
	fi
	rm -rf "$tmpdir"
}
trap cleanup EXIT

# Security: secrets reach curl through header files, never its command line, which ps shows.
(
	umask 077
	printf 'Authorization: Bearer %s\n' "$GRILL_ROOM_ADMIN_TOKEN" >"$tmpdir/admin"
	: >"$tmpdir/none"
)

t0=${EPOCHREALTIME/./}
# elapsed prints whole seconds since the start, for the event log.
elapsed() { printf 't+%ds' $(((${EPOCHREALTIME/./} - t0) / 1000000)); }
log() { echo "$(elapsed) $*" >&2; }

# api POSTs JSON from stdin to path with the headers in file hdr, writes the answer to
# $tmpdir/answer and prints the HTTP status, 000 when no answer came.
api() {
	local path=$1 hdr=$2 status
	if ! status=$(curl -sS --max-time 5 -o "$tmpdir/answer" -w '%{http_code}' -H @"$tmpdir/$hdr" \
		-H 'Content-Type: application/json' --data-binary @- "$base$path" 2>>"$tmpdir/curl.err"); then
		status=000
	fi
	echo "$status"
}

# setup creates the room, an invite, and joins it; the participant's cookie goes to a header file.
setup() {
	local status invite
	status=$(api /api/rooms admin <<<'{"name":"rollout watch"}')
	[[ $status == 201 ]] || { echo "rollout-watch: create room: HTTP $status" >&2; return 1; }
	room=$(jq -r '.id' "$tmpdir/answer")
	status=$(api "/api/rooms/$room/invites" admin <<<'{}')
	[[ $status == 201 ]] || { echo "rollout-watch: invite: HTTP $status" >&2; return 1; }
	invite=$(jq -r '.token' "$tmpdir/answer")
	status=$(jq -cn --arg i "$invite" '{invite: $i, name: "Rollout watch"}' | api /api/join none)
	[[ $status == 201 ]] || { echo "rollout-watch: join: HTTP $status" >&2; return 1; }
	(
		umask 077
		printf 'Cookie: grill_room_session=%s\n' "$(jq -r '.token' "$tmpdir/answer")" >"$tmpdir/member"
	)
}

# events prints "id<TAB>data" for each complete event with an id in an SSE capture: an event
# cut by a dropped connection has no closing blank line, and a browser drops it too.
events() {
	awk '/^id: /{id=substr($0,5)} /^data: /{data=substr($0,7)} /^$/{if(id!=""&&data!="")print id "\t" data; id="";data=""}' "$@"
}

# stream keeps the room's event stream open like EventSource: each connection is captured to
# conn.N, and a dropped one reconnects after 1 s with Last-Event-ID set to the last complete event.
stream() {
	local n=0 last="" rc id
	local -a resume=()
	while [[ ! -e $tmpdir/stop ]]; do
		n=$((n + 1))
		# A refused connection writes no file; events still reads one.
		: >"$tmpdir/conn.$n"
		# --speed-time: the server pings every 20 s, so a silent minute is a dead connection.
		curl -sSN --fail --speed-limit 1 --speed-time 60 -H @"$tmpdir/member" "${resume[@]}" \
			-o "$tmpdir/conn.$n" "$base/api/rooms/$room/events" 2>>"$tmpdir/stream.err" &
		echo "$!" >"$tmpdir/curl.pid"
		rc=0
		wait "$!" || rc=$?
		[[ -e $tmpdir/stop ]] && break
		id=$(events "$tmpdir/conn.$n" | tail -n1 | cut -f1)
		last=${id:-$last}
		if [[ -n $last ]]; then
			resume=(-H "Last-Event-ID: $last")
		fi
		log "stream closed (curl exit $rc), reconnect $n with Last-Event-ID ${last:-none}"
		echo "$rc" >>"$tmpdir/reconnects"
		sleep 1
	done
}

stop_stream() {
	touch "$tmpdir/stop"
	# The curl of the open connection, not the loop: its capture must end on a whole line.
	if [[ -e $tmpdir/curl.pid ]]; then
		kill "$(cat "$tmpdir/curl.pid")" 2>/dev/null || true # already gone when it just dropped
	fi
	wait "$stream_pid" || true                             # the loop's status says nothing more
	stream_pid=""
}

setup
readonly prefix="rollout-watch $room"
stream &
stream_pid=$!
for _ in {1..50}; do
	[[ -s $tmpdir/conn.1 ]] && break
	sleep 0.2
done
if [[ ! -s $tmpdir/conn.1 ]]; then
	echo "rollout-watch: the event stream did not open: $(cat "$tmpdir/stream.err")" >&2
	exit 1
fi
log "room $room, stream open, posting $lines lines"

stop=0
trap 'stop=1' INT
: >"$tmpdir/posted"
: >"$tmpdir/failed"
for ((n = 1; n <= lines && !stop; n++)); do
	status=$(jq -cn --arg b "$prefix $n" '{body: $b}' | api "/api/rooms/$room/messages" member)
	if ((stop)); then
		break
	fi
	if [[ $status == 201 ]]; then
		echo "$n" >>"$tmpdir/posted"
	else
		echo "$n" >>"$tmpdir/failed"
		log "post $n failed: HTTP $status"
	fi
	# Paced on the start time, so a slow POST does not push every later line back.
	wait_us=$((t0 + n * 1000000 - ${EPOCHREALTIME/./}))
	if ((wait_us > 0)); then
		# Ctrl-C interrupts it with a non-zero status; the loop then stops on $stop.
		sleep "$(printf '%d.%06d' $((wait_us / 1000000)) $((wait_us % 1000000)))" || true
	fi
done
trap - INT
attempted=$((n - 1))

# received prints the line numbers of this run's lines, in arrival order over all connections.
received() {
	local f
	for f in $(find "$tmpdir" -maxdepth 1 -name 'conn.*' -printf '%f\n' | sort -t. -k2n); do
		events "$tmpdir/$f"
	done | cut -f2- | jq -r --arg p "$prefix " '.body // empty | select(startswith($p)) | ltrimstr($p)'
}
deadline=$((SECONDS + settle))
while ((SECONDS < deadline)); do
	if [[ -z $(comm -23 <(sort -u "$tmpdir/posted") <(received | sort -u)) ]]; then
		break
	fi
	sleep 1
done
stop_stream
received >"$tmpdir/received"

posted=$(wc -l <"$tmpdir/posted")
failed=$(wc -l <"$tmpdir/failed")
lost=$(comm -23 <(sort -u "$tmpdir/posted") <(sort -u "$tmpdir/received") | wc -l)
failed_delivered=$(comm -12 <(sort -u "$tmpdir/failed") <(sort -u "$tmpdir/received") | wc -l)
read -r reordered duplicates < <(awk '{if($1 in seen)d++; else if($1<max)o++; seen[$1]=1; if($1>max)max=$1} END{print o+0, d+0}' "$tmpdir/received")
reconnects=0
if [[ -e $tmpdir/reconnects ]]; then
	reconnects=$(wc -l <"$tmpdir/reconnects")
fi

echo "lines: $attempted attempted, $posted posted, $failed not posted (POST failed; $failed_delivered of them delivered anyway)"
echo "stream: $((reconnects + 1)) connection attempts, $reconnects reconnects with Last-Event-ID"
echo "received: $((posted - lost)) of $posted posted, $lost lost, $reordered out of order, $duplicates duplicated"
if ((lost > 0)); then
	echo "lost lines: $(comm -23 <(sort -u "$tmpdir/posted") <(sort -u "$tmpdir/received") | sort -n | paste -sd' ')"
fi
if ((lost + reordered + duplicates > 0)); then
	echo "result: FAIL"
	exit 1
fi
echo "result: PASS"
