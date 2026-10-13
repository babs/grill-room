#!/bin/sh
# Installs grill-room-daemon from the room server that serves this script, into ~/.local/bin (no
# sudo), then starts it paired to the room:
#
#   curl -fsSL https://<room-host>/install.sh | sh -s -- https://<room-host>/r/<room>
#
# Strict POSIX sh: dash, bash, busybox ash and the macOS sh all run it.
set -eu

readonly bin_name=grill-room-daemon
# The signer the supervisor accepts (003-AC2): this repository's release workflow on a vX.Y.Z tag.
readonly signer='^https://github\.com/babs/grill-room/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$'
readonly issuer=https://token.actions.githubusercontent.com

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

# verify_signature checks the project's keyless signature on the download in $tmp when cosign is
# installed; there are no minisign signatures to check.
verify_signature() {
	if ! command -v cosign >/dev/null 2>&1; then
		printf 'Signature not checked: cosign is not installed.\n'
		return
	fi
	bundle=$(mktemp "$dir/.$bin_name.XXXXXX")
	curl -fsSL -o "$bundle" "$base/download/$file.sigstore.json" \
		|| die "the room server serves no signature for $file and cosign is installed: refusing an unsigned build. Nothing was installed."
	# cosign 2.4 reads cosign 3's bundle format only with this flag; 2.5 and 3 detect it.
	format=
	case $(cosign version 2>&1) in *GitVersion:*v2.*) format=--new-bundle-format ;; esac
	# $format is empty or one flag: unquoted on purpose.
	# shellcheck disable=SC2086
	if ! out=$(cosign verify-blob $format --bundle "$bundle" --certificate-identity-regexp "$signer" \
		--certificate-oidc-issuer "$issuer" "$tmp" 2>&1); then
		die "signature check failed for $file: $out. Nothing was installed."
	fi
	printf 'Signature checked: built by the grill-room release workflow.\n'
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

	# Any link inside a room will do (its docs window, a query, a fragment): keep the room's own,
	# read as the daemon reads it (internal/daemon/room.go ParseRoomURL).
	room_url=${room_url%%#*}
	room_url=${room_url%%\?*}
	scheme=${room_url%%://*}
	rest=${room_url#*://}
	authority=${rest%%/*}
	# Security: user info would hide the real host behind an allowed name.
	case $authority in '' | *@*) usage ;; esac
	path=${rest#"$authority"}
	prefix=${path%%/r/*}
	[ "$prefix" != "$path" ] || usage
	room_id=${path#"$prefix"/r/}
	room_id=${room_id%%/*}
	case $room_id in '' | *[!a-z0-9]*) usage ;; esac
	[ "${#room_id}" -le 64 ] || usage
	base=$scheme://$authority$prefix
	room_url=$base/r/$room_id
	target=$(detect_target)
	if [ -r /proc/sys/kernel/osrelease ] && grep -qi microsoft /proc/sys/kernel/osrelease; then
		printf 'WSL detected: installing the Linux build.\n'
	fi
	file=$bin_name-$target
	dir=$HOME/.local/bin
	mkdir -p "$dir"

	# Downloaded next to its final place, so the move into place is an atomic rename.
	bundle=
	tmp=$(mktemp "$dir/.$bin_name.XXXXXX")
	trap 'rm -f "$tmp" ${bundle:+"$bundle"}' EXIT
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
	verify_signature

	chmod 755 "$tmp"
	mv -f "$tmp" "$dir/$bin_name"
	[ -z "$bundle" ] || rm -f "$bundle"
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
