#!/usr/bin/env bash
# Read-only summary of a room server through its admin API, one JSON object per line, sorted and
# free of engine-dependent or moving fields: the cutover diffs a run before against one after
# (spec 017, 017-AC9, docs/cutover.md).
set -euo pipefail

readonly usage="usage: GRILL_ROOM_URL=<base url> GRILL_ROOM_ADMIN_TOKEN=<token> tools/lab-check.sh
optional probes: GRILL_ROOM_CHECK_ROOM=<room id> with GRILL_ROOM_CHECK_COOKIE=<a member's room cookie>,
                 GRILL_ROOM_CHECK_SESSION=<a profile session cookie>"

for c in curl jq; do
	if ! command -v "$c" >/dev/null; then
		echo "lab-check: $c is missing" >&2
		exit 1
	fi
done
if [[ -z ${GRILL_ROOM_URL:-} || -z ${GRILL_ROOM_ADMIN_TOKEN:-} ]]; then
	echo "$usage" >&2
	exit 2
fi
if [[ -n ${GRILL_ROOM_CHECK_COOKIE:-} && -z ${GRILL_ROOM_CHECK_ROOM:-} ]]; then
	echo "lab-check: GRILL_ROOM_CHECK_COOKIE needs GRILL_ROOM_CHECK_ROOM" >&2
	exit 2
fi
readonly base=${GRILL_ROOM_URL%/}

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
# Security: secrets reach curl through header files, never its command line, which ps shows.
(
	umask 077
	printf 'Authorization: Bearer %s\n' "$GRILL_ROOM_ADMIN_TOKEN" >"$tmpdir/admin"
	printf 'Cookie: grill_room_session=%s\n' "${GRILL_ROOM_CHECK_COOKIE:-}" >"$tmpdir/room"
	printf 'Cookie: grill_room_profile=%s\n' "${GRILL_ROOM_CHECK_SESSION:-}" >"$tmpdir/profile"
)

# fetch GETs path with the headers in file hdr into $tmpdir/body and prints the HTTP status.
fetch() {
	local path=$1 hdr=$2
	if ! curl -sS --max-time 30 -o "$tmpdir/body" -w '%{http_code}' -H @"$tmpdir/$hdr" "$base$path"; then
		echo "lab-check: GET $path failed" >&2
		return 1
	fi
}

# get GETs an admin path, failing on any status but 200, and prints the body.
get() {
	local path=$1 status
	status=$(fetch "$path" admin)
	if [[ $status != 200 ]]; then
		echo "lab-check: GET $path: HTTP $status" >&2
		return 1
	fi
	cat "$tmpdir/body"
}

# Rooms: counts and byte volumes, not db size; host_online is the paired app's stream (017-AC9).
rooms=$(get /api/admin/rooms)
jq -cS '.rooms | sort_by(.id)[] | {room: .id, name, created_at, last_activity, participants, personas,
	host_participant_id: (.host_participant_id // 0), host_online, quota_bytes, quota, volume}' <<<"$rooms"
mapfile -t ids < <(jq -r '.rooms | sort_by(.id)[] | .id' <<<"$rooms")
for id in "${ids[@]}"; do
	# No client: its IP and user agent move with every request.
	get "/api/admin/rooms/$(jq -rn --arg s "$id" '$s|@uri')/participants" |
		jq -cS --arg room "$id" 'sort_by(.id)[] | {room: $room, participant: .id, name, admin, has_profile, host}'
done
# Profiles: no email, which the output files would keep, and no last activity.
get /api/admin/profiles | jq -cS 'sort_by(.id)[] | {profile: .id, uuid, created_at,
	rooms: ([.rooms[]?.room_id] | sort), skills, max_rooms, rooms_created}'
jq -cS -n --argjson t "$(get /api/admin/templates)" --argjson s "$(get /api/admin/skills)" \
	--argjson st "$(get /api/admin/starters)" --argjson p "$(get /api/admin/pronunciations)" \
	'{templates: ($t | length), skills: ($s | length), starters: ($st | length), pronunciations: ($p.entries | length)}'

failed=0
if [[ -n ${GRILL_ROOM_CHECK_ROOM:-} ]]; then
	status=$(fetch "/api/rooms/$(jq -rn --arg s "$GRILL_ROOM_CHECK_ROOM" '$s|@uri')" room)
	same=false
	if [[ $status == 200 ]] && jq -e --arg r "$GRILL_ROOM_CHECK_ROOM" '.room.id == $r' "$tmpdir/body" >/dev/null; then
		same=true
	else
		failed=1
	fi
	jq -cSn --arg room "$GRILL_ROOM_CHECK_ROOM" --argjson status "$status" --argjson same "$same" \
		'{probe: "room", room: $room, status: $status, same_room: $same}'
fi
if [[ -n ${GRILL_ROOM_CHECK_SESSION:-} ]]; then
	status=$(fetch /api/profile profile)
	profile=null
	if [[ $status == 200 ]]; then
		profile=$(jq -c '.profile.id // null' "$tmpdir/body")
	fi
	if [[ $profile == null ]]; then
		failed=1
	fi
	jq -cSn --argjson status "$status" --argjson profile "$profile" '{probe: "profile", status: $status, profile: $profile}'
fi
if ((failed)); then
	echo "lab-check: a probe failed" >&2
	exit 1
fi
