#!/usr/bin/env bash
# S18: join a Teams meeting with a native (no browser) client and capture what it receives.
# See ../S18-teams-native-client.md. Everything it writes stays under .scratchpad/s18/.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
readonly HERE REPO
readonly WORK="$REPO/.scratchpad/s18"
readonly OST_URL="https://github.com/eisbaw/ost"
readonly OST_REV="0892144"
readonly OST_DIR="$WORK/ost"
readonly BIN="$OST_DIR/target/release/teams-cli"
readonly RUST_IMAGE="rust:1.99.1-bookworm"
readonly STT_IMAGE="${STT_IMAGE:-grill-stt:local}"
# Tokens land in $WORK/home/teams-cli/config.toml (0600), never in ~/.config.
export XDG_CONFIG_HOME="$WORK/home"

usage() {
    cat >&2 <<EOF
usage: $0 <command>
  build                      clone ost @$OST_REV, apply the S18 patch, build in Docker (4 CPUs)
  login [--force]            device-code sign-in, once: join and meetings renew the tokens themselves
  meetings                   calendar meetings, 2 h ago to 24 h ahead, with their join links (Graph)
  check <meetup-join-url>    parse a link offline: what the join would use, no network
  join <meetup-join-url> [minutes]
                             join the meeting (default 30 min, Ctrl-C hangs up), capture, analyze
  analyze <run-dir>          rerun the analysis of a capture
  transcribe <run-dir> [set] transcribe each WAV through grill_stt.replay (set: s18-csrc or s18)
  logout                     delete the stored tokens
EOF
    exit 2
}

die() {
    echo "error: $*" >&2
    exit 1
}

need() {
    command -v "$1" >/dev/null || die "$1 is required"
}

cmd_build() {
    need git
    need docker
    mkdir -p "$WORK"
    if [[ ! -d "$OST_DIR/.git" ]]; then
        git clone -q "$OST_URL" "$OST_DIR"
    fi
    git -C "$OST_DIR" fetch -q origin
    git -C "$OST_DIR" checkout -q --force "$OST_REV"
    git -C "$OST_DIR" clean -qfd src
    git -C "$OST_DIR" apply "$HERE/ost-meeting-capture.patch"
    mkdir -p "$WORK/cargo"
    docker run --rm --cpus 4 --memory 8g -u "$(id -u):$(id -g)" \
        -e CARGO_HOME=/cargo -e CARGO_BUILD_JOBS=4 \
        -v "$WORK/cargo":/cargo -v "$OST_DIR":/src -w /src \
        "$RUST_IMAGE" bash -c 'set -o pipefail; cargo build --release --quiet && cargo test --release --quiet 2>&1 | tail -3'
    echo "built $BIN"
}

need_bin() {
    [[ -x "$BIN" ]] || die "no client yet: run '$0 build' first"
}

cmd_login() {
    need_bin
    "$BIN" login "$@"
    "$BIN" whoami
}

cmd_join() {
    local url="${1:-}" minutes="${2:-30}"
    [[ -n "$url" ]] || usage
    [[ "$minutes" =~ ^[0-9]+$ ]] || die "minutes must be a whole number"
    need_bin
    local dir log rc=0
    dir="$WORK/runs/$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$dir"
    log="$dir/client.log"
    : >"$log"
    echo "capture: $dir"
    echo "Ctrl-C hangs up cleanly; the analysis runs after the call."
    # Live view of the call milestones; the full log stays in client.log. tail -F reports the
    # file being truncated on start, noise only.
    tail -F -n +1 --pid=$$ "$log" 2>/dev/null |
        grep --line-buffered -E 'call_(placed|accepted|ended)|rejection_reason|stopped_by|Phase [12] complete|Remote audio SSRC|Trouter closed|ERROR|Error' &
    "$BIN" call-test --meeting "$url" --duration "$((minutes * 60))" --capture "$dir" >>"$log" 2>&1 || rc=$?
    sleep 1
    if [[ "$rc" -ne 0 ]]; then
        echo "client exited with $rc, last lines of $log:" >&2
        tail -20 "$log" >&2
        # Nothing captured means the join itself failed: the log above is the finding.
        [[ -s "$dir/rtp.jsonl" || -s "$dir/trouter.jsonl" ]] || exit 1
    fi
    cmd_analyze "$dir"
}

cmd_meetings() {
    need_bin
    "$BIN" meetings
}

cmd_check() {
    [[ -n "${1:-}" ]] || usage
    need_bin
    "$BIN" meeting-check "$1"
}

cmd_analyze() {
    local dir="${1:-}"
    [[ -n "$dir" ]] || usage
    [[ -d "$dir" ]] || die "no such run directory: $dir"
    python3 -I "$HERE/analyze.py" "$dir"
}

cmd_transcribe() {
    local dir="${1:-}" set="${2:-}"
    [[ -n "$dir" ]] || usage
    local data="$dir/analysis"
    [[ -f "$data/manifest.json" ]] || die "no $data/manifest.json: run '$0 analyze $dir' first"
    need docker
    docker image inspect "$STT_IMAGE" >/dev/null 2>&1 || die "no $STT_IMAGE image: run 'make stt-image'"
    if [[ -z "$set" ]]; then
        set="s18"
        if grep -q '"s18-csrc"' "$data/manifest.json"; then
            set="s18-csrc"
        fi
    fi
    echo "transcribing set $set in real time (one replay pass per group of 4 tracks)"
    # Replay needs the worker's settings to load; the room and LiveKit ones are never contacted.
    docker run --rm --network none --cpus 4 -u "$(id -u):$(id -g)" -v "$data":/data \
        -e APP_ROOM_URL=http://unused -e APP_ROOM_TOKEN=unused -e APP_LIVEKIT_URL=ws://unused \
        -e APP_LIVEKIT_API_KEY=unused -e APP_LIVEKIT_API_SECRET=unused \
        "$STT_IMAGE" python -m grill_stt.replay /data/manifest.json "/data/lines-$set.jsonl" --set "$set" --talkers 4
    python3 -I - "$data/lines-$set.jsonl" "$data/speakers.json" <<'EOF' | tee "$data/transcript-$set.md"
import json, sys
from pathlib import Path
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
names = json.loads(Path(sys.argv[2]).read_text()) if Path(sys.argv[2]).exists() else {}
# arrival is the end of the line on the track's own clock, which analyze.py starts at the capture start.
for r in sorted((r for r in rows if "text" in r), key=lambda r: r["arrival"]):
    m, s = divmod(int(r["arrival"]), 60)
    print(f"- [{m:02d}:{s:02d}] **{names.get(r['id'], r['id'])}**: {r['text']}")
EOF
}

cmd_logout() {
    rm -rf "${WORK:?}/home"
    echo "tokens deleted"
}

main() {
    [[ $# -ge 1 ]] || usage
    local cmd="$1"
    shift
    case "$cmd" in
        build) cmd_build ;;
        login) cmd_login "$@" ;;
        meetings) cmd_meetings ;;
        check) cmd_check "$@" ;;
        join) cmd_join "$@" ;;
        analyze) cmd_analyze "$@" ;;
        transcribe) cmd_transcribe "$@" ;;
        logout) cmd_logout ;;
        *) usage ;;
    esac
}

main "$@"
