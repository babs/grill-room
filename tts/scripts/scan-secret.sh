#!/usr/bin/env bash
# 013-AC10: an image holds no trace of a secret: not in a layer's files, the history or the config.
# A known string of the image (a pinned file name) must be found, so a scan that reads nothing fails.
# usage: scan-secret.sh <image> <secret file>
set -euo pipefail

if [[ $# -ne 2 || ! -r $2 ]]; then
    echo "usage: $0 <image> <readable secret file>" >&2
    exit 2
fi
readonly image=$1 secret=$2
tmp=$(mktemp -d "${SCAN_TMP:-$PWD}/scan.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

docker save "$image" -o "$tmp/image.tar"
mkdir "$tmp/raw" "$tmp/fs"
tar -xf "$tmp/image.tar" -C "$tmp/raw"
n=0
while IFS= read -r -d '' blob; do
    if tar -tf "$blob" >/dev/null 2>&1; then
        mkdir "$tmp/fs/$n" && tar -xf "$blob" -C "$tmp/fs/$n" 2>/dev/null || true
        n=$((n + 1))
    fi
done < <(find "$tmp/raw" -type f -print0)
docker history --no-trunc "$image" > "$tmp/history.txt"
docker image inspect "$image" > "$tmp/inspect.json"

if ! grep -rqF 'models.lock.json' "$tmp/fs"; then
    echo "control failed: the scan read no layer content ($n layers)" >&2
    exit 1
fi
if grep -rlF -f "$secret" "$tmp/raw" "$tmp/fs" "$tmp/history.txt" "$tmp/inspect.json" | sed "s|$tmp/||" | grep .; then
    echo "the secret is in the files above" >&2
    exit 1
fi
echo "no trace of the secret in $n layers, the history or the config"
