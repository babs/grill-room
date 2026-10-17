#!/usr/bin/env python3
"""Lexicon round trip (013-AC33): each seed entry said in a carrier sentence of each language it has
a form for, 10 times by that language's first default voice, then heard by the room's STT worker.

    lexcheck.py render <dir>                 16 kHz tracks and the manifest the STT replay reads
    lexcheck.py score <out.jsonl> <report>   hits per entry; exits 1 when more than MAX_FAILED entries fail

Then, in the STT image: python -m grill_stt.replay <dir>/manifest.json <out.jsonl> --set lex
"""

import json
import os
import re
import sys
import unicodedata
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf
from num2words import num2words

from .engine import Engine
from .roundtrip import render_one
from .textlayer import Entry, Lang, TextLayer, load_seed

RENDERS = 10
NEEDED = 6
# The engine samples, so a few entries miss in any run; more than this is a regression.
MAX_FAILED = 5
# The first default voice of each language (013-AC16); both ship as Pocket's own states.
VOICE: dict[Lang, str] = {"fr": "estelle", "en": "alba"}
# The names a term is said by, which the STT writes out: they count as heard.
SAID_AS: dict[str, list[str]] = {
    "k8s": ["Kubernetes"],
    "kubectl": ["kube control", "cube control"],
    "PostgreSQL": ["Postgres"],
    "Nginx": ["engine x"],
    "OK": ["okay"],
    "B2B": ["B to B"],
    "B2C": ["B to C"],
}
CARRIER: dict[Lang, str] = {
    "fr": "Je pense que {} pose problème ici.",
    "en": "I think {} is the problem here.",
}


def langs(e: Entry) -> list[Lang]:
    out: list[Lang] = []
    if e.fr_en or e.fr_fr:
        out.append("fr")
    if e.en:
        out.append("en")
    return out


def tokens(s: str) -> list[str]:
    s = unicodedata.normalize("NFKC", s.lower()).replace("’", "'")
    return re.findall(r"[^\W_]+", s)


def accepted(term: str, lang: Lang) -> list[str]:
    """The term, its spaced form (camel case, letters of an acronym), the names it is said by and,
    for numbers, the digits or the words; each compared with spaces removed."""
    forms = {term, re.sub(r"(?<=[a-z])(?=[A-Z])", " ", term), *SAID_AS.get(term, [])}
    if re.fullmatch(r"[A-Z0-9/]{2,6}", term):
        forms.add(" ".join(term))
    for f in list(forms):
        forms.add(re.sub(r"\d+", lambda m: " " + num2words(int(m[0]), lang=lang) + " ", f))
    return sorted({"".join(tokens(f)) for f in forms})


def heard(term: str, lang: Lang, text: str) -> bool:
    """A run of up to 6 heard words that, joined, spells an accepted form: "git hub" is GitHub."""
    want = set(accepted(term, lang))
    toks = tokens(text)
    return any("".join(toks[i : i + n]) in want for n in range(1, 7) for i in range(len(toks) - n + 1))


def track_id(i: int, lang: Lang, n: int) -> str:
    return f"{lang}/{i:03d}-{n}"


def render(engine: Engine, out: Path) -> None:
    seed = load_seed()
    layer = TextLayer(seed)
    tracks = []
    for i, e in enumerate(seed):
        for lang in langs(e):
            text = CARRIER[lang].format(e.term)
            for n in range(RENDERS):
                tid = track_id(i, lang, n)
                (out / lang).mkdir(parents=True, exist_ok=True)
                audio = render_one(engine, layer, VOICE[lang], text)
                # A second of silence after the line, so the segmenter closes the utterance.
                sf.write(
                    out / f"{tid}.wav",
                    np.concatenate([audio, np.zeros(16000, np.float32)]),
                    16000,
                    subtype="PCM_16",
                )
                tracks.append({"id": tid, "set": "lex", "item": e.term, "lang": lang, "wav": f"{tid}.wav"})
            print(f"rendered {lang} {e.term}", flush=True)
    (out / "manifest.json").write_text(json.dumps({"tracks": tracks}) + "\n")


def score(rows: list[dict[str, Any]]) -> dict[str, Any]:
    said: dict[str, list[tuple[float, str]]] = {}
    for r in rows:
        if "id" in r and r.get("text"):
            said.setdefault(r["id"], []).append((r.get("arrival", 0.0), r["text"]))
    entries = []
    for i, e in enumerate(load_seed()):
        for lang in langs(e):
            hyps = [
                " ".join(t for _, t in sorted(said.get(track_id(i, lang, n), []))) for n in range(RENDERS)
            ]
            hits = sum(heard(e.term, lang, h) for h in hyps)
            entries.append(
                {"term": e.term, "lang": lang, "hits": hits, "pass": hits >= NEEDED, "heard": hyps}
            )
    failed = [f"{x['term']} ({x['lang']})" for x in entries if not x["pass"]]
    return {"entries": entries, "failed": failed, "pass": len(failed) <= MAX_FAILED}


def main() -> None:
    if len(sys.argv) < 3 or sys.argv[1] not in ("render", "score"):
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    if sys.argv[1] == "render":
        from .engine import Pocket

        threads = int(os.environ.get("APP_CPU_THREADS", "2"))
        render(Pocket(Path(os.environ.get("APP_MODELS_DIR", "/models")), threads=threads), Path(sys.argv[2]))
        return
    rows = [json.loads(line) for line in Path(sys.argv[2]).read_text().splitlines() if line.strip()]
    res = score(rows)
    Path(sys.argv[3]).write_text(json.dumps(res, ensure_ascii=False, indent=1) + "\n")
    ok = len(res["entries"]) - len(res["failed"])
    print(f"{ok} of {len(res['entries'])} passed, failed: {res['failed']}", file=sys.stderr)
    sys.exit(0 if res["pass"] else 1)


if __name__ == "__main__":
    main()
