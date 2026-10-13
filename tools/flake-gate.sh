#!/usr/bin/env bash
# CI's flake gate (test hygiene): the Go tests in the test files a change adds or edits run five
# times under the race detector, and the e2e specs it adds or edits run three times each. A test
# that passes once by luck fails here. CI only: never run it on a workstation.
set -euo pipefail

readonly usage="usage: tools/flake-gate.sh <base-ref> go|e2e"
if [[ $# -ne 2 ]]; then
	echo "$usage" >&2
	exit 2
fi
readonly base=$1 what=$2

if ! git rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
	echo "flake-gate: base $base is not a commit here: fetch it first" >&2
	exit 1
fi
mapfile -t changed < <(git diff --name-only --diff-filter=AM "$base"...HEAD)

go_gate() {
	local f dir names
	local -A tests=()
	for f in "${changed[@]}"; do
		[[ $f == *_test.go ]] || continue
		# A test file of helpers only has no Test function: grep's no-match is not an error here.
		names=$(grep -oE '^func Test[A-Za-z0-9_]+\(t \*testing\.T\)' "$f" | sed -E 's/^func (Test[A-Za-z0-9_]+).*/\1/' | paste -sd '|' -) || true
		[[ -n $names ]] || continue
		dir=./$(dirname "$f")
		tests[$dir]=${tests[$dir]:+${tests[$dir]}|}$names
	done
	if [[ ${#tests[@]} -eq 0 ]]; then
		echo "flake-gate: no Go test file changed since $base"
		return 0
	fi
	for dir in "${!tests[@]}"; do
		echo "flake-gate: $dir: ${tests[$dir]}"
		# A test of another OS (_windows_test.go on Linux) matches nothing here and passes.
		go test -race -count=5 -run "^(${tests[$dir]})\$" "$dir"
	done
}

e2e_gate() {
	local f
	local -a specs=()
	for f in "${changed[@]}"; do
		# The default config's specs only: voice/, lab/ and the others need their own stack.
		[[ $f =~ ^e2e/[^/]+\.spec\.js$ ]] && specs+=("${f#e2e/}")
	done
	if [[ ${#specs[@]} -eq 0 ]]; then
		echo "flake-gate: no e2e spec changed since $base"
		return 0
	fi
	echo "flake-gate: ${specs[*]}"
	make e2e-repeat SPECS="${specs[*]}"
}

case $what in
go) go_gate ;;
e2e) e2e_gate ;;
*)
	echo "$usage" >&2
	exit 2
	;;
esac
