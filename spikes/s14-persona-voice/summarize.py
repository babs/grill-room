#!/usr/bin/env python3
"""Spike S14: medians of the timed runs (no WAV written) per engine and language, as a Markdown table."""

import json
import statistics
import sys
from collections import defaultdict

runs = defaultdict(list)
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r["wav"] is None:
        runs[(r["engine"], r["lang"], r["voice"])].append(r)

print(
    "| Engine | Lang | Voice | Runs | First chunk, short sentence (s) | First chunk, longest sentence (s) "
    "| RTF | Peak RSS (MB) |"
)
print("|---|---|---|---|---|---|---|---|")
for (engine, lang, voice), rs in runs.items():
    med = {k: statistics.median(r[k] for r in rs) for k in ("ttfa_first_s", "ttfa_long_s", "rtf", "rss_peak_mb")}
    rtfs = sorted(r["rtf"] for r in rs)
    print(
        f"| {engine} | {lang} | {voice} | {len(rs)} | {med['ttfa_first_s']:.2f} | {med['ttfa_long_s']:.2f} "
        f"| {med['rtf']:.3f} ({rtfs[0]:.3f}-{rtfs[-1]:.3f}) | {med['rss_peak_mb']:.0f} |"
    )
