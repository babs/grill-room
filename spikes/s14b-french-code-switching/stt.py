#!/usr/bin/env python3
"""Spike S14b: STT round trip through the room's own worker (parakeet-ultra, grill_stt.replay).

prep <samples dir> <stt dir>: each <samples>/<combo>/<id>.wav becomes a 16 kHz track of set s14b,
  plus <stt dir>/manifest.json.
score <out.jsonl> [<scores.json>]: WER per combo against the text as written, and which English
  terms came back, sentence by sentence.
"""

import json
import re
import sys
import unicodedata
from collections import defaultdict
from math import gcd
from pathlib import Path

import numpy as np
import soundfile as sf
from num2words import num2words
from scipy.signal import resample_poly

HERE = Path(__file__).parent
SENTENCES = {s["id"]: s for s in json.loads((HERE / "sentences.json").read_text())["sentences"]}


def prep(samples: Path, out: Path) -> None:
    tracks = []
    for wav in sorted(samples.glob("*/*.wav")):
        combo, sid = wav.parent.name, wav.stem
        audio, sr = sf.read(wav, dtype="float32")
        g = gcd(sr, 16000)
        audio = resample_poly(audio, 16000 // g, sr // g)
        rel = f"{combo}/{sid}.wav"
        (out / combo).mkdir(parents=True, exist_ok=True)
        # A second of silence after the line, so the segmenter closes the utterance.
        sf.write(out / rel, np.concatenate([audio, np.zeros(16000, np.float32)]), 16000, subtype="PCM_16")
        tracks.append({"id": f"{combo}/{sid}", "set": "s14b", "item": sid, "lang": SENTENCES[sid]["lang"], "wav": rel})
    (out / "manifest.json").write_text(json.dumps({"tracks": tracks}) + "\n")
    print(f"{len(tracks)} tracks")


def norm(s: str, lang: str) -> list[str]:
    s = unicodedata.normalize("NFKC", s.lower()).replace("’", "'")
    s = re.sub(r"(\d)\s?%", r"\1 " + ("pour cent" if lang == "fr" else "percent"), s)
    # Both sides in words, so "502" and "cinq cent deux" compare equal; versions digit by digit.
    s = re.sub(r"\d+", lambda m: " " + num2words(int(m[0]), lang=lang) + " ", s)
    s = s.replace("-", " ")
    return re.findall(r"[\w']+", s)


def wer(ref: list[str], hyp: list[str]) -> float:
    d = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        prev, d[0] = d[0], i
        for j, h in enumerate(hyp, 1):
            prev, d[j] = d[j], min(d[j] + 1, d[j - 1] + 1, prev + (r != h))
    return d[-1] / len(ref)


def heard(alts: list[str], hyp: list[str], lang: str) -> bool:
    h = " " + " ".join(hyp) + " "
    return any(" " + " ".join(norm(a, lang)) + " " in h for a in alts)


def score(out: Path) -> dict[str, dict[str, object]]:
    text: dict[str, list[tuple[float, str]]] = defaultdict(list)
    tracks = set()
    for line in out.open():
        r = json.loads(line)
        if "id" in r:
            tracks.add(r["id"])
            if r.get("text"):
                text[r["id"]].append((r["arrival"], r["text"]))
    if not tracks:
        # Rows exist only for lines the worker stored; take the track list from the manifest.
        tracks = {t["id"] for t in json.loads((out.parent / "manifest.json").read_text())["tracks"]}
    combos: dict[str, dict[str, object]] = defaultdict(lambda: {"sentences": {}})
    for track in sorted(tracks | set(text)):
        combo, sid = track.rsplit("/", 1)
        s = SENTENCES[sid]
        hyp_text = " ".join(t for _, t in sorted(text.get(track, [])))
        hyp = norm(hyp_text, s["lang"])
        terms = {t: heard(alts, hyp, s["lang"]) for t, alts in s["terms"].items()}
        combos[combo]["sentences"][sid] = {
            "wer": round(wer(norm(s["text"], s["lang"]), hyp), 3),
            "terms": terms,
            "hyp": hyp_text,
        }
    for c in combos.values():
        ss = c["sentences"]
        fr = [v for k, v in ss.items() if k.startswith("fr")]
        en = [v for k, v in ss.items() if k.startswith("en")]
        for name, group in (("fr", fr), ("en", en), ("all", list(ss.values()))):
            if group:
                c[f"wer_{name}"] = round(sum(g["wer"] for g in group) / len(group), 3)
                hits = [h for g in group for h in g["terms"].values()]
                c[f"terms_{name}"] = f"{sum(hits)}/{len(hits)}"
    return dict(combos)


def check() -> None:
    assert norm("Le 502 à 10 %", "fr") == ["le", "cinq", "cent", "deux", "à", "dix", "pour", "cent"]
    assert wer(["a", "b", "c"], ["a", "x", "c", "d"]) == 2 / 3
    assert heard(["feature flag"], norm("le feature-flag reste", "fr"), "fr")
    assert not heard(["fix"], norm("le fixe", "fr"), "fr")


def main() -> None:
    cmd = sys.argv[1]
    if cmd == "--check":
        check()
        print("ok")
    elif cmd == "prep":
        prep(Path(sys.argv[2]), Path(sys.argv[3]))
    elif cmd == "score":
        res = score(Path(sys.argv[2]))
        if len(sys.argv) > 3:
            Path(sys.argv[3]).write_text(json.dumps(res, ensure_ascii=False, indent=1) + "\n")
        print("| Combo | WER fr | terms fr | WER en | terms en |")
        print("|---|---|---|---|---|")
        for k, c in sorted(res.items(), key=lambda kv: kv[1].get("wer_fr", 9)):
            print(
                f"| {k} | {c.get('wer_fr', '')} | {c.get('terms_fr', '')} | {c.get('wer_en', '')} | {c.get('terms_en', '')} |"
            )


if __name__ == "__main__":
    main()
