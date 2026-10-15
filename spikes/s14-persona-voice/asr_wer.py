#!/usr/bin/env python3
"""Spike S14: intelligibility proxy. Each sample went through the STT worker's replay (parakeet);
this prints its WER against the text it read, and which tech words came back."""

import json
import re
import sys
import unicodedata
from collections import defaultdict

from bench import TEXTS

JARGON = [
    "spec",
    "webhook",
    "github",
    "502",
    "kubernetes",
    "retry",
    "backoff",
    "postgresql",
    "oauth",
    "token",
    "p95",
    "livekit",
]


def norm(s: str) -> list[str]:
    s = unicodedata.normalize("NFKC", s.lower()).replace("’", "'")
    return re.findall(r"[\w']+", s)


def wer(ref: list[str], hyp: list[str]) -> float:
    d = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        prev, d[0] = d[0], i
        for j, h in enumerate(hyp, 1):
            prev, d[j] = d[j], min(d[j] + 1, d[j - 1] + 1, prev + (r != h))
    return d[-1] / len(ref)


def main() -> None:
    text = defaultdict(list)
    for line in open(sys.argv[1]):
        r = json.loads(line)
        if "id" in r and r.get("text"):
            text[r["id"]].append((r["arrival"], r["text"]))
    print("| Sample | WER | Tech words heard |")
    print("|---|---|---|")
    for track in sorted(text):
        hyp = " ".join(t for _, t in sorted(text[track]))
        ref = " ".join(TEXTS[track[:2]])
        heard = [w for w in JARGON if w in norm(hyp)]
        print(f"| {track} | {wer(norm(ref), norm(hyp)):.2f} | {len(heard)}/{len(JARGON)} |")


if __name__ == "__main__":
    main()
