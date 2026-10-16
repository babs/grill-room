#!/usr/bin/env python3
"""Scores $D/out/*.jsonl with S10's score.py (same normalisation, WER, terms, English leak), plus what S17 adds:
paced latency (queue wait + inference + 0.8 s cut) for the concurrency runs, the speed of the utterances that
carry the glossary, and engine memory.

usage: D=<data-dir> score.py [--json]
"""

import importlib.util
import json
import os
import sys
from collections import defaultdict
from pathlib import Path

D = Path(os.environ.get("D", "/data"))
spec = importlib.util.spec_from_file_location(
    "s10score", Path(__file__).resolve().parents[1] / "s10-validation" / "score.py"
)
s10 = importlib.util.module_from_spec(spec)
assert spec.loader
spec.loader.exec_module(s10)
s10.D = D


def concurrency(paths: list[Path]) -> dict:
    """A c2/c4 run: one file per stream; latency counts the wait behind the stream's own earlier utterances."""
    rows = [json.loads(x) for p in paths for x in p.read_text().splitlines()]
    lat = [r["wait"] + r["infer"] + s10.VAD_CUT_S for r in rows]
    per = [
        sum(r["dur"] for r in rows if r["item"] == i) / sum(r["infer"] for r in rows if r["item"] == i)
        for i in {r["item"] for r in rows}
    ]
    return {
        "n": len(rows),
        "p50": s10.pct(lat, 0.5),
        "p95": s10.pct(lat, 0.95),
        "max": max(lat),
        "rtfx_per_stream": min(per),
        "streams": len(paths),
    }


def main() -> None:
    out = sorted(p for p in (D / "out").glob("*.jsonl") if p.stat().st_size)
    single = [p for p in out if ".c2." not in p.name and ".c4." not in p.name]
    res = []
    for p in single:
        r = s10.score(p)
        meta = p.with_suffix(".meta")
        r["rss_mb"] = json.loads(meta.read_text())["rss_mb"] if meta.exists() else None
        res.append(r)
    groups: dict[str, list[Path]] = defaultdict(list)
    for p in out:
        for c in (".c2.", ".c4."):
            if c in p.name:
                groups[p.name.split(".k")[0]].append(p)
    conc = {k: concurrency(v) for k, v in sorted(groups.items())}
    f = lambda v, p=2: "-" if v is None else f"{v:.{p}f}"  # noqa: E731
    print(
        "| run | set | n | WER | WER fr | WER en | terms fr (n) | terms en | EN leak | hyp/ref | p50 | p95 | RTFx | RSS MB |"
    )
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in res:
        print(
            f"| {r['tag']} | {r['set']} | {r['n']} | {f(r['wer'], 3)} | {f(r['wer_fr'], 3)} | {f(r['wer_en'], 3)} | "
            f"{f(r['terms_fr'])} ({r['terms_n']}) | {f(r['terms_en'])} | {f(r['en_leak'], 3)} | {f(r['len_ratio'])} | "
            f"{f(r['p50'])} | {f(r['p95'])} | {f(r['rtfx'], 1)} | {f(r['rss_mb'], 0)} |"
        )
    print("\n| concurrency run | streams | n | p50 | p95 | max | slowest stream RTFx |")
    print("|---|---|---|---|---|---|---|")
    for k, c in conc.items():
        print(
            f"| {k} | {c['streams']} | {c['n']} | {f(c['p50'])} | {f(c['p95'])} | {f(c['max'])} | {f(c['rtfx_per_stream'], 1)} |"
        )
    if "--json" in sys.argv:
        print(json.dumps({"single": res, "concurrency": conc}))


if __name__ == "__main__":
    main()
