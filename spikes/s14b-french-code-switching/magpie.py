#!/usr/bin/env python3
"""Spike S14b: Magpie TTS Multilingual v2607 through NeMo-Speech.cpp (CPU build in a scratch container).

The CPU preset has no server, so one container runs the CLI once per span; timing comes from
`nemo-speech bench tts`, not from here. Spans are trimmed, levelled and joined like synth.py does.

usage: magpie.py <nemo dir> <image> <mode> <voice> <out dir> [--only id,id]
<nemo dir> holds src/ (built, preset cpu-tts) and home/ (the pulled models).
"""

import argparse
import json
import os
import subprocess
from pathlib import Path

import numpy as np
import soundfile as sf

from synth import level, trim

HERE = Path(__file__).parent
LANG = {"fr": "fr-FR", "en": "en-US"}


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("nemo", type=Path)
    p.add_argument("image")
    p.add_argument("mode")
    p.add_argument("voice")
    p.add_argument("out", type=Path)
    p.add_argument("--only", default="")
    a = p.parse_args()
    texts = json.loads((HERE / "texts.json").read_text())[a.mode]
    ids = a.only.split(",") if a.only else list(texts)
    work = a.out / "spans"
    work.mkdir(parents=True, exist_ok=True)
    jobs = []
    for sid in ids:
        for i, (lg, t) in enumerate(texts[sid]):
            jobs.append(f"{LANG[lg]}\t/out/spans/{sid}-{i}.wav\t{t}")
    (work / "jobs.tsv").write_text("\n".join(jobs) + "\n")
    script = (
        "while IFS=$'\\t' read -r lang out text; do "
        '/src/build/cpu-tts/bin/nemo-speech synthesize "$text" --language "$lang" '
        f'--voice {a.voice} --device cpu --seed 1 --force -o "$out" >/dev/null 2>&1 || echo "failed $out"; '
        "done < /out/spans/jobs.tsv"
    )
    uid = f"{os.getuid()}:{os.getgid()}"
    subprocess.run(
        [
            "docker",
            "run",
            "--rm",
            "--name",
            f"grill-room-test-s14b-magpie-{os.getpid()}",
            "--user",
            uid,
            "--cpuset-cpus",
            "16-19",
            "--memory",
            "6g",
            "-e",
            "HOME=/home/u",
            "-v",
            f"{a.nemo / 'home'}:/home/u",
            "-v",
            f"{a.nemo / 'src'}:/src",
            "-v",
            f"{a.out.resolve()}:/out",
            a.image,
            "bash",
            "-c",
            script,
        ],
        check=True,
    )
    for sid in ids:
        pieces = []
        n = len(texts[sid])
        for i in range(n):
            audio, sr = sf.read(work / f"{sid}-{i}.wav", dtype="float32")
            if n > 1:
                audio = np.concatenate([level(trim(audio, sr)), np.zeros(int(sr * 0.04), np.float32)])
            pieces.append(audio)
        sf.write(a.out / f"{sid}.wav", np.clip(level(np.concatenate(pieces)), -1, 1), sr, subtype="PCM_16")
    print(json.dumps({"out": a.out.name, "mode": a.mode, "voice": a.voice, "n": len(ids)}))


if __name__ == "__main__":
    main()
