#!/usr/bin/env python3
"""Spike S14b: the listening folder and the result tables, from the matrix renders and scores.

usage: build_index.py <work dir> <samples out dir> <results md>
<work> holds matrix/<row>__<col>/<id>.wav, matrix.jsonl (synth.py lines), stt-matrix/scores.json
(stt.py) and timbre.jsonl (timbre.py pairs). Writes one Opus file per cell with the 6 key sentences,
the before/after pairs for the terms the user flagged, index.md, and the Markdown tables.
"""

import json
import statistics
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).parent
SENTENCES = json.loads((HERE / "sentences.json").read_text())["sentences"]
KEY = [s["id"] for s in SENTENCES if s.get("key")]
ROWS = [
    "pocket-estelle",
    "pocketmain-estelle",
    "pocket-3267",
    "pocket-12080",
    "pocket-2114",
    "pocket-4193",
    "sopro-developpeuse-3",
    "sopro-Selfie",
    "supertonic-F2",
    "supertonic-M2",
    "kokoro-ff_siwis",
    "piper-siwis",
    "melo-FR",
    "magpie-Sofia",
    "magpie-John",
    "kyutai-12080",
    "chatterbox-12080",
]
COLS = ["raw", "num", "respell", "hybrid", "hybridgb", "same", "gb", "us", "pocketen", "supertonicen", "ipaspans"]
COL_TITLE = {
    "raw": "as written",
    "num": "numbers said",
    "respell": "(a) French respelling",
    "hybrid": "(a)+(c) hybrid, same voice",
    "hybridgb": "(a)+(b) hybrid, Kokoro GB",
    "same": "(c) switch, same voice",
    "gb": "(b) Kokoro GB",
    "us": "(b) Kokoro US",
    "pocketen": "(b) Pocket EN",
    "supertonicen": "(b) Supertonic EN",
    "ipaspans": "(e) English IPA spans",
}
# The user's flagged words (S14 listening): the sentence and term that carries each.
FLAGGED = [
    ("GitHub", "fr08", "GitHub"),
    ("502", "fr02", "502"),
    ("retry", "fr08", "retry"),
    ("cluster", "fr16", "cluster"),
    ("LiveKit", "fr02", "LiveKit"),
]


def row_of(fr: str) -> str:
    engine, voice = fr.split(":", 1)
    if voice.endswith(".wav"):  # a cloned CML-TTS speaker: its id
        return f"{engine}-{voice.rsplit('/', 1)[1].split('_')[0]}"
    return f"{engine}-" + {"fr_FR-siwis-medium": "siwis"}.get(voice, voice)


def combo_name(fr: str, en: str, mode: str) -> str:
    row = row_of(fr)
    if mode not in ("spans", "hybrid"):
        return f"{row}__{mode}"
    if mode == "hybrid":
        return f"{row}__hybrid" if en == fr else f"{row}__hybridgb"
    if en == fr:
        return f"{row}__same"
    engine, voice = en.split(":", 1)
    if engine == "kokoro":
        col = "gb" if voice.startswith("b") else "us"
    else:
        col = f"{engine}en"
    return f"{row}__{col}"


def perf(work: Path) -> dict[str, dict[str, float]]:
    """Per combo: median and worst first-audio delay on French lines, RTF, CPU per audio second, peak RSS."""
    out: dict[str, dict[str, float]] = {}
    pending: list[dict[str, float]] = []
    for line in (work / "matrix.jsonl").open():
        if not line.startswith("{"):  # MeloTTS and Chatterbox print progress on stdout
            continue
        r = json.loads(line)
        if "id" in r:
            pending.append(r)
            continue
        fr_lines = [p for p in pending if str(p["id"]).startswith("fr")] or pending
        out[r.get("out") or combo_name(r["fr"], r["en"], r["mode"])] = {
            "ttfa": statistics.median(p["ttfa_s"] for p in fr_lines),
            "ttfa_max": max(p["ttfa_s"] for p in fr_lines),
            "rtf": r["rtf"],
            "cpu": r["cpu_per_audio_s"],
            "rss": r["rss_peak_mb"],
        }
        pending = []
    return out


def opus(src: list[Path], dst: Path) -> None:
    """Sentences joined by 0.7 s of silence, 48 kbit/s Opus: small enough for a LAN folder of ~80 cells."""
    dst.parent.mkdir(parents=True, exist_ok=True)
    inputs: list[str] = []
    for p in src:
        inputs += ["-i", str(p)]
    n = len(src)
    filt = "".join(f"[{i}:a]aresample=24000,apad=pad_dur=0.7[a{i}];" for i in range(n))
    filt += "".join(f"[a{i}]" for i in range(n)) + f"concat=n={n}:v=0:a=1[out]"
    subprocess.run(
        [
            "ffmpeg",
            "-loglevel",
            "error",
            "-y",
            *inputs,
            "-filter_complex",
            filt,
            "-map",
            "[out]",
            "-ac",
            "1",
            "-c:a",
            "libopus",
            "-b:a",
            "48k",
            str(dst),
        ],
        check=True,
    )


def main() -> None:
    work, out, results = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
    scores = {}
    for f in sorted(work.glob("stt-matrix*/scores.json")):
        scores |= json.loads(f.read_text())
    timbre = {}
    tf = work / "timbre.jsonl"
    if tf.exists():
        for line in tf.open():
            t = json.loads(line)
            timbre[Path(t["b"]).parent.name] = t["cos"]
    pf = perf(work)
    combos = sorted(p.name for p in (work / "matrix").iterdir() if p.is_dir())
    for c in combos:
        files = [work / "matrix" / c / f"{sid}.wav" for sid in KEY]
        if all(f.exists() for f in files) and not (out / f"{c}.ogg").exists():
            opus(files, out / f"{c}.ogg")
    for c in (
        "pocket-estelle__raw",
        "pocket-estelle__respell",
        "pocket-estelle__same",
        "pocket-estelle__gb",
        "pocket-estelle__hybrid",
        "pocket-estelle__hybridgb",
        "pocket-3267__raw",
        "pocket-3267__respell",
        "pocket-3267__same",
        "pocket-3267__gb",
        "pocket-3267__hybrid",
    ):
        for sid in sorted({f[1] for f in FLAGGED} | {"fr03"}):
            src = work / "matrix" / c / f"{sid}.wav"
            dst = out / "fix" / f"{sid}--{c}.ogg"
            if src.exists() and not dst.exists():
                opus([src], dst)

    def cell(c: str) -> str:
        s = scores.get(c)
        if not (out / f"{c}.ogg").exists():
            return ""
        txt = f"[play](./{c}.ogg)"
        if s:
            txt += f" {s.get('wer_fr', '')}"
        return txt

    grid = ["| French voice | " + " | ".join(COL_TITLE[c] for c in COLS) + " |", "|---|" + "---|" * len(COLS)]
    for r in ROWS:
        cells = [cell(f"{r}__{c}") for c in COLS]
        if any(cells):
            grid.append(f"| {r} | " + " | ".join(cells) + " |")

    table = [
        "| Combo | FR WER | FR terms | EN WER | EN terms | "
        + " | ".join(f[0] for f in FLAGGED)
        + " | Timbre FR/EN | 1st audio (s) | RTF | core-s per s | RSS MB |",
        "|---|---|---|---|---|" + "---|" * len(FLAGGED) + "---|---|---|---|---|",
    ]
    for c, s in sorted(scores.items(), key=lambda kv: kv[1].get("wer_fr", 9)):
        flags = []
        for _, sid, term in FLAGGED:
            hit = s["sentences"].get(sid, {}).get("terms", {}).get(term)
            flags.append("" if hit is None else ("yes" if hit else "no"))
        p = pf.get(c, {})
        table.append(
            f"| {c} | {s.get('wer_fr', '')} | {s.get('terms_fr', '')} | {s.get('wer_en', '')} | {s.get('terms_en', '')} | "
            + " | ".join(flags)
            + f" | {timbre.get(c, '')} | {p.get('ttfa', 0):.2f} | {p.get('rtf', '')} | {p.get('cpu', '')} | {p.get('rss', '')} |"
        )
    results.write_text(
        "# S14b results\n\nGenerated by build_index.py.\n\n## Grid (FR WER per cell)\n\n"
        + "\n".join(grid)
        + "\n\n## Every combination\n\n"
        + "\n".join(table)
        + "\n"
    )
    (out / "GRID.md").write_text("\n".join(grid) + "\n")
    print(results.read_text())


if __name__ == "__main__":
    main()
