#!/usr/bin/env python3
"""Scores the A/B replays against spec 007's AC16 thresholds, with spike S10's scoring.

usage: score.py <corpus dir with manifest.json> <dir of <engine>.<set>.x<talkers>.jsonl>
Prints one Markdown table per run and the AC16 verdict.
"""

import json
import re
import sys
import unicodedata
from collections import defaultdict
from pathlib import Path

# S10b scoring: hesitations out (SUMM-RE transcribes them, engines drop them), annotations out.
FILLERS = {"euh", "heu", "mh", "hm", "hum", "mmh", "bah", "ben"}
EN_W = {"the", "and", "is", "are", "was", "were", "we", "they", "that", "this", "with", "you", "but", "so",
        "have", "it's", "there", "what", "of", "to"}
FR_W = {"le", "la", "les", "et", "est", "sont", "on", "nous", "ils", "que", "qui", "pas", "un", "une",
        "c'est", "donc", "ça", "des", "du", "en", "je", "tu", "il"}
# The S10d bleed filter: an utterance mostly from another person in the room is not this track's.
TAIL_S = 0.6


def words(s: str) -> list[str]:
    s = unicodedata.normalize("NFKC", s).lower().replace("’", "'")
    s = re.sub(r"\[[^\]]*\]|<[^>]*>", " ", s)
    s = s.replace("_", " ")
    return [w for w in re.sub(r"[^\w]+", " ", s).split() if w not in FILLERS]


def edits(ref: list[str], hyp: list[str]) -> int:
    prev = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        cur = [i]
        for j, h in enumerate(hyp, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r != h)))
        prev = cur
    return prev[-1]


def en_share(text: str) -> float:
    tot = en = 0
    for sent in re.split(r"[.?!]+", text.lower()):
        w = re.findall(r"[\w']+", sent)
        tot += len(w)
        if sum(x in EN_W for x in w) > sum(x in FR_W for x in w):
            en += len(w)
    return en / tot if tot else 0.0


def pct(xs: list[float], q: float) -> float:
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else float("nan")


def own_speech(row: dict, own: list[list[float]]) -> bool:
    e = row["arrival"] - TAIL_S
    b = e - row["dur"]
    return sum(max(0.0, min(e, y) - max(b, x)) for x, y in own) >= 0.5 * (e - b)


def score(path: Path, m: dict) -> dict:
    lines = [json.loads(x) for x in path.read_text().splitlines()]
    summary = next(x for x in lines if x.get("summary"))
    rows = [x for x in lines if not x.get("summary")]
    cset = summary["set"]
    lang = {t["item"]: t["lang"] for t in m["tracks"]}
    by = defaultdict(list)
    for r in sorted(rows, key=lambda r: r["arrival"]):
        if cset != "summre" or own_speech(r, m["own"][r["item"]]):
            by[r["item"]].append(r)
    err: dict[str, int] = defaultdict(int)
    n: dict[str, int] = defaultdict(int)
    leak_w = leak_n = 0.0
    items = [t["item"] for t in m["tracks"] if t["set"] == cset]
    for item in items:
        hyp_text = " ".join(r["text"] for r in by[item])
        ref, hyp = words(m["items"][item]), words(hyp_text)
        err[lang[item]] += edits(ref, hyp)
        n[lang[item]] += len(ref)
        if lang[item] == "fr":
            leak_w += en_share(hyp_text) * len(hyp)
            leak_n += len(hyp)
    lat = [r["latency"] for r in rows]
    c = summary["counters"]
    return {
        "engine": summary["engine"], "set": cset, "talkers": summary["talkers"], "lines": len(rows),
        "wer": sum(err.values()) / max(1, sum(n.values())),
        "wer_en": err["en"] / n["en"] if n["en"] else None,
        "leak": leak_w / leak_n if leak_n else 0.0,
        "p50": pct(lat, 0.5), "p95": pct(lat, 0.95), "max": max(lat, default=float("nan")),
        "dropped": int(c.get("grill_stt_utterances_dropped_total", 0)),
        "implausible": int(c.get("grill_stt_lines_total.implausible", 0)),
        "errors": int(c.get("grill_stt_lines_total.error", 0)),
        "peak_gib": (summary.get("peak_working_set") or summary["peak_memory"] or 0) / 2**30, "wall_s": summary["wall_s"],
        "ws": bool(summary.get("peak_working_set")),
    }


def main() -> None:
    corpus, out = Path(sys.argv[1]), Path(sys.argv[2])
    m = json.loads((corpus / "manifest.json").read_text())
    res = {(r["engine"], r["set"], r["talkers"]): r for r in (score(p, m) for p in sorted(out.glob("*.jsonl")))}
    f = lambda v, d=3: "-" if v is None else f"{v:.{d}f}"  # noqa: E731
    print("| engine | set | talkers | lines | WER | WER en | EN leak | p50 s | p95 s | max s | dropped | implausible | errors | peak GiB |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in res.values():
        print(f"| {r['engine']} | {r['set']} | {r['talkers']} | {r['lines']} | {f(r['wer'])} | {f(r['wer_en'])} | {f(r['leak'], 2)} | "
              f"{f(r['p50'], 1)} | {f(r['p95'], 1)} | {f(r['max'], 1)} | {r['dropped']} | {r['implausible']} | {r['errors']} | {f(r['peak_gib'], 2)} |")
    get = lambda e, s, t, k: res.get((e, s, t), {}).get(k)  # noqa: E731
    checks = [
        ("meetings WER <= 0.25", get("parakeet", "summre", 1, "wer"), 0.25),
        ("talk WER <= 0.15", get("parakeet", "devoxx", 1, "wer"), 0.15),
        ("Piper English WER <= 0.08", get("parakeet", "piper", 1, "wer_en"), 0.08),
        ("meetings leak <= 0.01", get("parakeet", "summre", 1, "leak"), 0.01),
        ("talk leak <= 0.01", get("parakeet", "devoxx", 1, "leak"), 0.01),
        ("p95 one talker <= 4 s", get("parakeet", "summre", 1, "p95"), 4.0),
        ("p95 two talkers <= 5 s", get("parakeet", "summre", 2, "p95"), 5.0),
        ("p95 four talkers <= 10 s (target 8 s)", get("parakeet", "summre", 4, "p95"), 10.0),
        ("no drop at four talkers", get("parakeet", "summre", 4, "dropped"), 0),
        ("peak memory <= 2.7 GiB", max((r["peak_gib"] for r in res.values() if r["engine"] == "parakeet"), default=None), 2.7),
    ]
    for key, label in (("summre", "meetings"), ("devoxx", "talk")):
        checks.append((f"{label} WER at or below whisper", get("parakeet", key, 1, "wer"), get("whisper", key, 1, "wer")))
    print("\n| AC16 check | parakeet | bound | pass |\n|---|---|---|---|")
    for name, v, bound in checks:
        ok = v is not None and bound is not None and v <= bound
        print(f"| {name} | {f(v, 3) if isinstance(v, float) else v} | {f(bound, 3) if isinstance(bound, float) else bound} | {'yes' if ok else 'NO'} |")


if __name__ == "__main__":
    main()
