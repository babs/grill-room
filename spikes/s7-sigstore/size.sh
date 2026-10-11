#!/usr/bin/env bash
# S7: size the daemon binary with and without a verifier linked in.
# Builds HEAD's cmd/grill-room-daemon in a scratch copy, so the repo's go.mod stays untouched.
# Usage: size.sh <scratch-dir>
set -euo pipefail
# Every variant must build with the same toolchain, or the size delta is meaningless.
export GOTOOLCHAIN=local

readonly TARGETS=(linux/amd64 darwin/arm64 windows/amd64)
readonly SIGSTORE=github.com/sigstore/sigstore-go@v1.3.0
# The version sigstore-go v1.3.0 requires; tidy would otherwise pick the latest, which needs go 1.27.
readonly SIGSTORE_LIB=github.com/sigstore/sigstore@v1.10.8
readonly MINISIGN=github.com/jedisct1/go-minisign@a09352b57a22

if [[ $# -ne 1 ]]; then
	echo "usage: $0 <scratch-dir>" >&2
	exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
mkdir -p "$1"
work=$(mktemp -d "$(cd "$1" && pwd)/size.XXXXXX")
trap 'rm -rf "$work"' EXIT

# variant <name> <hook.go content or empty> <module to add...>
variant() {
	local name=$1 hook=$2
	shift 2
	local tree="$work/$name"
	mkdir -p "$tree"
	git -C "$repo" archive HEAD | tar -x -C "$tree"
	if [[ -n "$hook" ]]; then
		if [[ "$hook" == *s7verifier* ]]; then
			mkdir -p "$tree/internal/s7verifier"
			cp "$here/verifier/verifier.go" "$here/verifier/trusted_root.json" "$tree/internal/s7verifier/"
		fi
		printf '%s\n' "$hook" >"$tree/cmd/grill-room-daemon/s7hook.go"
		(cd "$tree" && go get "$@" && go mod tidy) || {
			echo "$name: go get/tidy failed" >&2
			exit 1
		}
	fi
	local t f
	for t in "${TARGETS[@]}"; do
		f="$work/$name-${t%/*}-${t#*/}"
		(cd "$tree" && CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "-s -w" -o "$f" ./cmd/grill-room-daemon) || {
			echo "$name $t: build failed" >&2
			exit 1
		}
		printf '%s %s %s gz=%s\n' "$name" "$t" "$(stat -c %s "$f")" "$(gzip -9c "$f" | wc -c)"
	done
}

# read -d '' returns 1 at end of input, which is the normal case here.
# The hooks only run when GRILL_S7 is set, but the linker cannot know that, so the code is kept.
read -r -d '' HOOK_SIGSTORE <<'EOF' || true
package main

import (
	"os"

	s7verifier "github.com/babs/grill-room/internal/s7verifier"
)

func init() {
	if os.Getenv("GRILL_S7") == "" {
		return
	}
	tr, err := s7verifier.EmbeddedRoot()
	if err != nil {
		panic(err)
	}
	f, _ := os.Open(os.Args[1])
	_, err = s7verifier.VerifyKeyless(tr, os.Args[2], f, s7verifier.ReleaseSAN)
	println(err)
	println(s7verifier.VerifyKey([]byte(os.Args[3]), os.Args[2], f))
}
EOF

HOOK_TUF="${HOOK_SIGSTORE/println(err)/println(err)
	_, err = s7verifier.FetchRoot(os.Args[4])
	println(err)}"

read -r -d '' HOOK_MINISIGN <<'EOF' || true
package main

import (
	"os"

	"github.com/jedisct1/go-minisign"
)

func init() {
	if os.Getenv("GRILL_S7") == "" {
		return
	}
	pk, err := minisign.NewPublicKey(os.Args[1])
	if err != nil {
		panic(err)
	}
	sig, err := minisign.NewSignatureFromFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	println(pk.VerifyFromFile(os.Args[3], sig))
}
EOF

variant base ""
variant sigstore "$HOOK_SIGSTORE" "$SIGSTORE" "$SIGSTORE_LIB"
variant sigstore-tuf "$HOOK_TUF" "$SIGSTORE" "$SIGSTORE_LIB"
variant minisign "$HOOK_MINISIGN" "$MINISIGN"
