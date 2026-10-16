#!/usr/bin/env python3
"""Live latency from a bench-mode run: each utterance reaches a FIFO decoder when the VAD cuts it on the corpus
clock (real time), and takes the decode time the bench measured. Used for the phone emulation, where throttling
the whole tab also starved Chrome's audio capture; replaying the desktop bench checks the method against the
desktop live run.

usage: replay.py <runs-dir> <secs> <tag>...
  Replays the utterances cut within the first <secs> s; one JSON line per run, latency as analyze-browser.py defines it.
"""

import json
import sys
from pathlib import Path

SR = 16000


def pct(xs: list[float], q: float) -> float:
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(q * len(xs)))], 2)


def replay(lines: list[dict]) -> dict:
    free = 0.0
    pause, maxlen, wait = [], [], []
    for ln in sorted(lines, key=lambda x: x["cutAt"]):
        arrival = ln["cutAt"] / SR
        start = max(free, arrival)
        free = start + (ln["decodeEnd"] - ln["decodeStart"]) / 1000
        wait.append(start - arrival)
        if ln["cutAt"] - ln["lastVoiced"] >= 0.7 * SR:
            pause.append(free - ln["lastVoiced"] / SR)
        else:
            maxlen.append(free - arrival)
    allc = pause + maxlen
    return {
        "n": len(allc),
        "pause_p50": pct(pause, 0.5),
        "pause_p95": pct(pause, 0.95),
        "all_p50": pct(allc, 0.5),
        "all_p95": pct(allc, 0.95),
        "max": round(max(allc), 2),
        "queue_p95": pct(wait, 0.95),
        "queue_max": round(max(wait), 2),
    }


if __name__ == "__main__":
    runs = Path(sys.argv[1])
    secs = float(sys.argv[2])
    for tag in sys.argv[3:]:
        ev = [json.loads(x) for x in (runs / tag / "events.jsonl").read_text().splitlines()]
        print(
            json.dumps(
                {"tag": tag} | replay([e for e in ev if e["ev"] == "line" and e["cutAt"] <= secs * SR])
            )
        )
