#!/usr/bin/env python3
"""STT round trip (013-AC18): the shipped text layer and audio chain, the pinned engine, then the
room's own STT worker, which tells whether a listener would understand the line.

    roundtrip.py render <dir>        16 kHz tracks of each set sentence per voice, and the
                                     manifest the STT worker's replay reads
    roundtrip.py score <out.jsonl>   WER per voice and language, terms heard; exits 1 below the bar

Then, in the STT image: python -m grill_stt.replay <dir>/manifest.json <out.jsonl> --set rt
"""

import json
import os
import re
import sys
import threading
import unicodedata
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf
from num2words import num2words
from scipy.signal import resample_poly

from .audio import SilenceCap
from .engine import Engine, catalogue
from .room import Framer
from .textlayer import TextLayer, load_seed

SET = json.loads(Path(__file__).with_name("roundtrip.json").read_text())["sentences"]
VOICES = 5
# 013-AC18: mean WER bar per language, and the flagged French terms heard in 4 of 5 voices.
BAR = 0.12
FLAGGED = {"GitHub": "fr08", "502": "fr02", "LiveKit": "fr02"}


def voices(lang: str) -> list[str]:
    return [v["id"] for v in catalogue() if v["lang"] == lang][:VOICES]


def render_one(engine: Engine, layer: TextLayer, voice: str, text: str) -> np.ndarray:
    """One line as a listener gets it, 48 kHz from the chain, back to the STT's 16 kHz."""
    cap = SilenceCap(engine.sample_rate)
    frames: list[bytes] = []
    for spans in layer.plan(text):
        for span in spans:
            framer = Framer(engine.sample_rate, engine.gain(voice, span.lang), 1.0, cap)
            for chunk in engine.stream(voice, span.lang, span.text, threading.Event()):
                frames += framer.feed(chunk)
            frames += framer.flush()
    pcm = np.frombuffer(b"".join(frames), np.int16).astype(np.float32) / 32768
    out: np.ndarray = resample_poly(pcm, 1, 3).astype(np.float32)
    return out


def render(engine: Engine, out: Path) -> None:
    layer = TextLayer(load_seed())
    tracks = []
    for lang in ("fr", "en"):
        for voice in voices(lang):
            for s in (s for s in SET if s["lang"] == lang):
                rel = f"{voice}/{s['id']}.wav"
                (out / voice).mkdir(parents=True, exist_ok=True)
                audio = render_one(engine, layer, voice, s["text"])
                # A second of silence after the line, so the segmenter closes the utterance.
                sf.write(
                    out / rel, np.concatenate([audio, np.zeros(16000, np.float32)]), 16000, subtype="PCM_16"
                )
                tracks.append(
                    {"id": f"{voice}/{s['id']}", "set": "rt", "item": s["id"], "lang": lang, "wav": rel}
                )
                print(f"rendered {voice}/{s['id']}", flush=True)
    (out / "manifest.json").write_text(json.dumps({"tracks": tracks}) + "\n")


def norm(s: str, lang: str) -> list[str]:
    s = unicodedata.normalize("NFKC", s.lower()).replace("’", "'")
    s = re.sub(r"(\d)\s?%", r"\1 " + ("pour cent" if lang == "fr" else "percent"), s)
    # Both sides in words, so "502" and "cinq cent deux" compare equal.
    s = re.sub(r"\d+", lambda m: " " + num2words(int(m[0]), lang=lang) + " ", s)
    return re.findall(r"[\w']+", s.replace("-", " "))


def wer(ref: list[str], hyp: list[str]) -> float:
    d = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        prev, d[0] = d[0], i
        for j, h in enumerate(hyp, 1):
            prev, d[j] = d[j], min(d[j] + 1, d[j - 1] + 1, prev + (r != h))
    return d[-1] / max(len(ref), 1)


def heard(alts: list[str], hyp: list[str], lang: str) -> bool:
    h = " " + " ".join(hyp) + " "
    return any(" " + " ".join(norm(a, lang)) + " " in h for a in alts)


def score(rows: list[dict[str, Any]]) -> dict[str, Any]:
    """WER of each voice's lines against the text as written, and which terms came back."""
    said: dict[str, list[tuple[float, str]]] = {}
    for r in rows:
        if "id" in r and r.get("text"):
            said.setdefault(r["id"], []).append((r.get("arrival", 0.0), r["text"]))
    out: dict[str, Any] = {"voices": {}}
    for lang in ("fr", "en"):
        for voice in voices(lang):
            sents = {}
            for s in (s for s in SET if s["lang"] == lang):
                hyp_text = " ".join(t for _, t in sorted(said.get(f"{voice}/{s['id']}", [])))
                hyp = norm(hyp_text, lang)
                sents[s["id"]] = {
                    "wer": round(wer(norm(s["text"], lang), hyp), 3),
                    "terms": {t: heard(a, hyp, lang) for t, a in s["terms"].items()},
                    "hyp": hyp_text,
                }
            out["voices"][voice] = {
                "lang": lang,
                "wer": round(float(np.mean([v["wer"] for v in sents.values()])), 3),
                "sentences": sents,
            }
    for lang in ("fr", "en"):
        out[f"wer_{lang}"] = round(
            float(np.mean([v["wer"] for v in out["voices"].values() if v["lang"] == lang])), 3
        )
    out["flagged"] = {
        t: sum(v["sentences"][sid]["terms"][t] for v in out["voices"].values() if v["lang"] == "fr")
        for t, sid in FLAGGED.items()
    }
    out["pass"] = (
        out["wer_fr"] <= BAR and out["wer_en"] <= BAR and all(n >= 4 for n in out["flagged"].values())
    )
    return out


def main() -> None:
    if len(sys.argv) != 3 or sys.argv[1] not in ("render", "score"):
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    if sys.argv[1] == "render":
        from .engine import Pocket

        render(Pocket(Path(os.environ.get("APP_MODELS_DIR", "/models")), threads=2), Path(sys.argv[2]))
        return
    rows = [json.loads(line) for line in Path(sys.argv[2]).read_text().splitlines() if line.strip()]
    res = score(rows)
    print(json.dumps(res, ensure_ascii=False))
    print(
        f"wer fr {res['wer_fr']} en {res['wer_en']} flagged {res['flagged']} pass {res['pass']}",
        file=sys.stderr,
    )
    sys.exit(0 if res["pass"] else 1)


if __name__ == "__main__":
    main()
