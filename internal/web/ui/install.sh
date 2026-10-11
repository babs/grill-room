#!/bin/sh
# Installs grill-room-daemon from the room server that serves this script, into ~/.local/bin (no
# sudo), then starts it paired to the room:
#
#   curl -fsSL https://<room-host>/install.sh | sh -s -- https://<room-host>/r/<room>
#
# Strict POSIX sh: dash, bash, busybox ash and the macOS sh all run it.
set -eu

readonly bin_name=grill-room-daemon

die() {
	printf 'grill-room install: %s\n' "$1" >&2
	exit "${2:-1}"
}

usage() {
	die "usage: sh install.sh https://<room-host>/r/<room>" 2
}

# detect_target prints <os>-<arch> as the room server names its release files.
detect_target() {
	case $(uname -s) in
		Linux) os=linux ;;
		Darwin) os=darwin ;;
		*) die "unsupported OS $(uname -s): the daemon runs on Linux, macOS, and Windows inside WSL" ;;
	esac
	case $(uname -m) in
		x86_64 | amd64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) die "unsupported CPU $(uname -m): the daemon is built for amd64 and arm64" ;;
	esac
	# A shell under Rosetta reports x86_64 on Apple silicon; the native build is faster. Intel Macs
	# have no such sysctl, hence the silenced error.
	if [ "$os" = darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
	printf '%s-%s\n' "$os" "$arch"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1"
	else
		die "neither sha256sum nor shasum is installed: cannot verify the download"
	fi
}

main() {
	[ "$#" -eq 1 ] || usage
	room_url=$1
	# Security: the binary and its checksum come from this host, so plain http is only safe locally.
	case $room_url in
		https://*/r/*) ;;
		http://localhost/* | http://localhost:* | http://127.0.0.1/* | http://127.0.0.1:*)
			case $room_url in */r/*) ;; *) usage ;; esac
			;;
		*) usage ;;
	esac
	command -v curl >/dev/null 2>&1 || die "curl is not installed"
	[ -n "${HOME:-}" ] || die "HOME is not set"

	base=${room_url%/r/*}
	target=$(detect_target)
	if [ -r /proc/sys/kernel/osrelease ] && grep -qi microsoft /proc/sys/kernel/osrelease; then
		printf 'WSL detected: installing the Linux build.\n'
	fi
	file=$bin_name-$target
	dir=$HOME/.local/bin
	mkdir -p "$dir"

	# Downloaded next to its final place, so the move into place is an atomic rename.
	tmp=$(mktemp "$dir/.$bin_name.XXXXXX")
	trap 'rm -f "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM
	printf 'Downloading %s from %s\n' "$file" "$base"
	curl -fsSL -o "$tmp" "$base/download/$file" || die "download of $base/download/$file failed"
	want=$(curl -fsSL "$base/download/$file.sha256") || die "download of $base/download/$file.sha256 failed"
	want=${want%% *}
	got=$(sha256_of "$tmp")
	got=${got%% *}
	if [ -z "$want" ] || [ "$got" != "$want" ]; then
		die "sha256 mismatch for $file: the room server publishes $want, the download is $got. Nothing was installed."
	fi

	chmod 755 "$tmp"
	mv -f "$tmp" "$dir/$bin_name"
	trap - EXIT HUP INT TERM
	printf 'Installed %s\n' "$dir/$bin_name"
	case ":${PATH:-}:" in
		*":$dir:"*) ;;
		*) printf 'Note: %s is not in your PATH; add it to run %s by name.\n' "$dir" "$bin_name" ;;
	esac

	printf 'Starting %s for %s\n' "$bin_name" "$room_url"
	exec "$dir/$bin_name" "$room_url"
}

# Everything runs from here, on the last line: a download cut short runs nothing.
main "$@"
