"""Counts proxy log events by (event, method, path or host, status), from a byte offset on."""
import collections
import json
import sys

path, offset = sys.argv[1], int(sys.argv[2]) if len(sys.argv) > 2 else 0
counts = collections.Counter()
with open(path, "rb") as f:
    f.seek(offset)
    for line in f:
        d = json.loads(line)
        counts[(d["msg"], d.get("method"), d.get("path") or d.get("host"), d.get("status"))] += 1
for key, n in sorted(counts.items(), key=str):
    print(n, *[k for k in key if k is not None])
