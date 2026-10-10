#!/usr/bin/env python3
"""Build the FR/EN speech fixture for the voice e2e tests from Google FLEURS (CC-BY-4.0).

Writes fr-en.wav (16 kHz mono PCM16, alternating FR and EN read sentences separated by pauses) and
fr-en.json (reference text, language, start and end of each utterance in the file, attribution).
Usage: uv run ./scripts/make_fixture.py <output dir>
"""

import csv
import io
import json
import sys
import tarfile
import urllib.request
import wave
from pathlib import Path

import numpy as np
from faster_whisper import decode_audio

SR = 16000
FLEURS = "https://huggingface.co/datasets/google/fleurs/resolve/main/data/{lang}/{path}"
# Leading silence covers the time the page takes to publish its microphone to LiveKit.
LEAD_S, GAP_S, TAIL_S = 4.0, 2.0, 3.0
MIN_S, MAX_S = 3.5, 7.0
PEAK = 0.5


def fleurs(lang: str, n: int) -> list[tuple[str, np.ndarray, str]]:
    """The first n distinct dev sentences between MIN_S and MAX_S long, without proper nouns."""
    with urllib.request.urlopen(FLEURS.format(lang=lang, path="dev.tsv"), timeout=60) as r:
        rows = csv.reader(io.TextIOWrapper(r, "utf-8"), delimiter="\t", quoting=csv.QUOTE_NONE)
        refs = {row[1]: row[2] for row in rows}
    out: list[tuple[str, np.ndarray, str]] = []
    with (
        urllib.request.urlopen(FLEURS.format(lang=lang, path="audio/dev.tar.gz"), timeout=60) as r,
        tarfile.open(fileobj=r, mode="r|gz") as tar,
    ):
        for m in tar:
            name = Path(m.name).name
            text = refs.get(name)
            if not m.isfile() or text is None or any(text == t for _, _, t in out):
                continue
            f = tar.extractfile(m)
            assert f is not None
            audio = decode_audio(io.BytesIO(f.read()), sampling_rate=SR)
            # No proper nouns: the tests check the words, and Whisper spells rare names freely.
            if MIN_S <= len(audio) / SR <= MAX_S and not any(w[0].isupper() for w in text.split()[1:]):
                out.append((name, audio, text))
            if len(out) == n:
                return out
    raise SystemExit(f"{lang}: fewer than {n} sentences between {MIN_S} and {MAX_S} s")


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    out = Path(sys.argv[1])
    fr, en = fleurs("fr_fr", 2), fleurs("en_us", 2)
    parts = [np.zeros(int(LEAD_S * SR), np.float32)]
    utts, t = [], LEAD_S
    for lang, (name, audio, text) in zip(["fr", "en", "fr", "en"], [fr[0], en[0], fr[1], en[1]], strict=True):
        # Some FLEURS takes peak at -40 dBFS; browsers apply AGC, so normalise like a live mic.
        audio = audio * (PEAK / max(float(np.abs(audio).max()), 1e-6))
        # Trim the take's own edge silence: "end" must be the end of speech, which latency counts from.
        loud = np.flatnonzero(np.abs(audio) > PEAK * 0.05)
        audio = audio[loud[0] : loud[-1] + 1]
        parts += [audio, np.zeros(int(GAP_S * SR), np.float32)]
        utts.append(
            {
                "lang": lang,
                "text": text,
                "start": round(t, 3),
                "end": round(t + len(audio) / SR, 3),
                "source": f"google/fleurs {'fr_fr' if lang == 'fr' else 'en_us'} dev {name}",
            }
        )
        t += len(audio) / SR + GAP_S
    parts.append(np.zeros(int(TAIL_S * SR), np.float32))
    pcm = (np.clip(np.concatenate(parts), -1, 1) * 32767).astype("<i2")
    with wave.open(str(out / "fr-en.wav"), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())
    meta = {
        "source": "Google FLEURS, https://huggingface.co/datasets/google/fleurs",
        "license": "CC-BY-4.0, https://creativecommons.org/licenses/by/4.0/",
        "changes": "sentences cut from the dev split, peak-normalised, joined with silence",
        "utterances": utts,
    }
    (out / "fr-en.json").write_text(json.dumps(meta, ensure_ascii=False, indent=1) + "\n")
    print(f"fr-en.wav: {len(pcm) / SR:.1f} s, {len(utts)} utterances")


if __name__ == "__main__":
    main()
