#!/usr/bin/env python3
"""Scores /data/out/*.jsonl: WER per corpus, term hits, English retention, latency, RTFx, 2-stream queue.

usage: score.py [--md]
"""
import json
import os
import re
import sys
import unicodedata
from collections import defaultdict
from pathlib import Path

D = Path(os.environ.get("D", "/data"))
VAD_CUT_S = 0.8
# Hesitations: SUMM-RE transcribes them, most engines drop them.
FILLERS = {"euh", "heu", "mh", "hm", "hum", "mmh", "bah", "ben"}
EN_W = set(["the", "and", "is", "are", "was", "were", "we", "they", "that", "this", "with", "you", "but", "so", "have", "it's", "there", "what", "of", "to"])
FR_W = set(["le", "la", "les", "et", "est", "sont", "on", "nous", "ils", "que", "qui", "pas", "un", "une", "c'est", "donc", "ça", "des", "du", "en", "je", "tu", "il"])


def words(s: str) -> list[str]:
    s = unicodedata.normalize("NFKC", s).lower().replace("’", "'")
    s = re.sub(r"\[[^\]]*\]|<[^>]*>", " ", s)  # annotations such as [rire]
    s = s.replace("_", " ")  # SUMM-RE joins multiword units: tout_le monde
    return [w for w in re.sub(r"[^\w]+", " ", s).split() if w not in FILLERS]


def edits(ref: list[str], hyp: list[str]) -> int:
    prev = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        cur = [i]
        for j, h in enumerate(hyp, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r != h)))
        prev = cur
    return prev[-1]


def count(hay: list[str], needle: list[str]) -> int:
    n = len(needle)
    return sum(hay[i : i + n] == needle for i in range(len(hay) - n + 1))


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


def queue2(rows: list[dict], items: list[str]) -> list[float]:
    """Two speaker tracks talking at once into the worker's single queue (one consumer, every thread).

    Both tracks start at t=0; arrival = when the Segmenter emits the utterance; service = measured infer.
    Returns end-of-speech-to-line latency per utterance.
    """
    evs = sorted((r["arrival"], r["infer"]) for r in rows if r["item"] in items)
    free, lat = 0.0, []
    for arr, svc in evs:
        free = max(free, arr) + svc
        lat.append(free - arr + VAD_CUT_S)
    return lat


def posted(r: dict) -> bool:
    """The worker's plausibility filter (pipeline.implausible), Whisper rows only; repetition for all."""
    import zlib

    t = r["hyp"].encode()
    if t and len(t) / len(zlib.compress(t)) > 2.4:
        return False
    if "avg_logprob" not in r:
        return True
    unsure = r.get("lang_prob", 1.0) < 0.5
    return not (r["avg_logprob"] < -1.0 or (r["no_speech"] > 0.6 and unsure) or (unsure and r["dur"] >= 2.0))


def score(path: Path, prod: bool = False) -> dict:
    tag, cset = path.name.removesuffix(".jsonl").rsplit(".", 1)
    rows = [json.loads(x) for x in path.read_text().splitlines()]
    meta = json.loads((D / "corpus" / f"{cset}.json").read_text())
    terms = json.loads((D / "corpus" / "glossary.json").read_text())[cset]
    m = json.loads(path.with_suffix(".meta").read_text()) if path.with_suffix(".meta").exists() else {}
    truth = {u["id"]: u["lang"] for u in meta["utts"]}
    for r in rows:
        r["lang"] = truth[r["id"]]  # engines overwrote it with their own guess
        if prod and not posted(r):
            r["hyp"] = ""
    by = defaultdict(list)
    for r in rows:
        by[r["item"]].append(r)
    err = defaultdict(int)
    n = defaultdict(int)
    th = defaultdict(int)
    tn = defaultdict(int)
    en_leak_w = en_leak_n = 0
    hw = rw = 0
    for item, rs in by.items():
        lang = rs[0]["lang"] if cset == "piper" else "fr"
        ref, hyp = words(meta["items"][item]), words(" ".join(r["hyp"] for r in rs))
        err[lang] += edits(ref, hyp)
        hw += len(hyp)
        rw += len(ref)
        n[lang] += len(ref)
        for t in terms:
            tw = words(t)
            k = count(ref, tw)
            if k:
                tn[lang] += k
                th[lang] += min(k, count(hyp, tw))
        if lang == "fr":
            hy = " ".join(r["hyp"] for r in rs)
            en_leak_w += en_share(hy) * len(hyp)
            en_leak_n += len(hyp)
    lat = [r["infer"] + VAD_CUT_S for r in rows]
    out = {
        "tag": tag, "set": cset, "n": len(rows),
        "wer": sum(err.values()) / max(1, sum(n.values())),
        "wer_fr": err["fr"] / n["fr"] if n["fr"] else None,
        "wer_en": err["en"] / n["en"] if n["en"] else None,
        "terms_fr": th["fr"] / tn["fr"] if tn["fr"] else None,
        "terms_en": th["en"] / tn["en"] if tn["en"] else None,
        "en_leak": en_leak_w / en_leak_n if en_leak_n else 0.0,
        "p50": pct(lat, 0.5), "p95": pct(lat, 0.95),
        "rtfx": sum(r["dur"] for r in rows) / max(1e-9, sum(r["infer"] for r in rows)),
        "rss_mb": m.get("rss_mb"), "len_ratio": hw / max(1, rw),
        "terms_n": sum(tn.values()), "prod": prod, "errors": sum(1 for r in rows if r.get("error")),
    }
    if cset == "summre":
        # The two tracks with the most speech, as if both talked at the same time.
        speech = sorted(by, key=lambda i: -sum(r["dur"] for r in by[i]))[:2]
        q = queue2(rows, speech)
        out |= {"q2_p50": pct(q, 0.5), "q2_p95": pct(q, 0.95), "q2_max": max(q)}
    return out


def main() -> None:
    ps = [p for p in sorted((D / "out").glob("*.jsonl")) if p.stat().st_size]
    res = [score(p) for p in ps] + [score(p, True) for p in ps if p.name.startswith("fw_")]
    f = lambda v, p=2: "-" if v is None else f"{v:.{p}f}"
    print("| run | set | n | WER | WER fr | WER en | terms fr (n) | terms en | EN leak | hyp/ref words | lat p50 | lat p95 | RTFx | 2-stream p95 | RSS MB |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in res:
        print(f"| {r['tag']}{' +filter' if r['prod'] else ''} | {r['set']} | {r['n']} | {f(r['wer'], 3)} | {f(r['wer_fr'], 3)} | {f(r['wer_en'], 3)} | "
              f"{f(r['terms_fr'])} ({r['terms_n']}) | {f(r['terms_en'])} | {f(r['en_leak'])} | {f(r['len_ratio'])} | {f(r['p50'])} | {f(r['p95'])} | "
              f"{f(r['rtfx'], 1)} | {f(r.get('q2_p95'))} | {f(r['rss_mb'], 0)} |")
    if "--json" in sys.argv:
        print(json.dumps(res))


if __name__ == "__main__":
    main()
