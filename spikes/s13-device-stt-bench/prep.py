#!/usr/bin/env python3
# /// script
# requires-python = ">=3.12"
# dependencies = ["numpy", "pyarrow", "soundfile", "soxr", "huggingface_hub", "piper-tts"]
# ///
"""Rebuilds the S11 corpus: <out>/all.wav (talk clips, Piper lines, SUMM-RE tracks masked to the track's
own speaker) and <out>/manifest.json. The AC18 run takes its sentences from the Piper lines.

usage: prep.py <out> <refs> <bench.py>
  refs: the devoxx.json and summre.json that ../s10-validation/prep.py writes under corpus/;
  bench.py: ../s10-stt-models/bench.py, whose `audio` step renders script.json with Piper.
  The talk audio needs yt-dlp and ffmpeg on the PATH.
Same order, pauses and noise floor as S11, so the first 30 minutes are the same audio.
"""

import importlib.util
import io
import json
import subprocess
import sys
from pathlib import Path

import numpy as np
import pyarrow.parquet as pq
import soundfile as sf
import soxr
from huggingface_hub import hf_hub_download

SR = 16000
D, REFS_DIR, BENCH = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
REFS = json.loads((REFS_DIR / "summre.json").read_text())["items"]
TALK = json.loads((REFS_DIR / "devoxx.json").read_text())["items"]
LEAD, PAUSE, PIPER_PAUSE, GAP_CAP, PAD, WIN = 2.0, 10.0, 2.0, 3.0, 0.2, 300.0
rng = np.random.default_rng(0)


def noise(sec: float) -> np.ndarray:
    # A real mic never sends digital zero: ~-60 dBFS floor.
    return (rng.standard_normal(int(sec * SR)) * 1e-3).astype(np.float32)


def talk() -> dict:
    m4a = D / "k8s_ops.m4a"
    if not m4a.exists():
        subprocess.run(
            [
                "yt-dlp",
                "--js-runtimes",
                "node",
                "-q",
                "-f",
                "bestaudio[ext=m4a]/bestaudio",
                "-o",
                str(m4a),
                "https://youtu.be/36iJtvJmqhc",
            ],
            check=True,
        )
    out = {}
    for name, a0 in (("k8s1", 600), ("k8s2", 1500)):
        raw = subprocess.run(
            [
                "ffmpeg",
                "-loglevel",
                "error",
                "-ss",
                str(a0),
                "-t",
                "180",
                "-i",
                str(m4a),
                "-ar",
                str(SR),
                "-ac",
                "1",
                "-f",
                "f32le",
                "-",
            ],
            check=True,
            capture_output=True,
        ).stdout
        out[name] = np.frombuffer(raw, dtype=np.float32).copy()
    return out


def summre() -> dict:
    out = {}
    for shard in range(3):
        f = hf_hub_download(
            "linagora/SUMM-RE",
            f"data/test/test-{shard:05d}-of-00028.parquet",
            repo_type="dataset",
        )
        pf = pq.ParquetFile(f)
        for rg in range(pf.num_row_groups):
            for row in pf.read_row_group(
                rg, columns=["meeting_id", "speaker_id", "audio", "segments"]
            ).to_pylist():
                item = f"{row['meeting_id']}_{row['speaker_id']}"
                if item not in REFS or not row["segments"]:
                    continue
                segs = row["segments"]
                mid = (segs[0]["start"] + segs[-1]["end"]) / 2
                inside = [
                    s
                    for s in segs
                    if mid - WIN / 2 <= s["start"] and s["end"] <= mid + WIN / 2
                ]
                audio, sr = sf.read(io.BytesIO(row["audio"]["bytes"]), dtype="float32")
                if audio.ndim > 1:
                    audio = audio.mean(axis=1)
                if sr != SR:
                    audio = soxr.resample(audio, sr, SR).astype(np.float32)
                # Own speech only, gaps capped: the raw track also carries the other people in the room.
                parts, prev_end = [], None
                for s in inside:
                    b, e = (
                        max(0, int((s["start"] - PAD) * SR)),
                        int((s["end"] + PAD) * SR),
                    )
                    if prev_end is not None:
                        gap = (b - prev_end) / SR
                        if gap > 0:
                            parts.append(noise(min(gap, GAP_CAP)))
                        else:
                            b = prev_end
                    parts.append(audio[b:e])
                    prev_end = e
                out[item] = np.concatenate(parts)
                print(item, f"{len(out[item]) / SR:.0f}s", flush=True)
    return out


def piper() -> tuple[dict, dict]:
    p = D / "piper"
    spec = importlib.util.spec_from_file_location("bench", BENCH)
    lb = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(lb)
    if not (p / "audio.npz").exists():
        p.mkdir(exist_ok=True)
        lb.cmd_audio(p)
    z = np.load(p / "audio.npz")
    lines = lb.load_script()["lines"]
    return (
        {ln["id"]: z[f"{ln['id']}_16k"] for ln in lines},
        {ln["id"]: {"ref": ln["text"], "lang": ln["lang"]} for ln in lines},
    )


def main() -> None:
    pclips, pmeta = piper()
    clips = talk() | pclips | summre()
    order = ["k8s1", "k8s2"] + list(pclips) + [i for i in REFS if i in clips]
    parts, spans, pauses, t = [noise(LEAD)], [], [], LEAD
    for name in order:
        a = clips[name]
        pause = PIPER_PAUSE if name in pmeta else PAUSE
        sset = "piper" if name in pmeta else "talk" if name in TALK else "summre"
        ref = pmeta[name]["ref"] if name in pmeta else TALK.get(name) or REFS[name]
        lang = pmeta[name]["lang"] if name in pmeta else "fr"
        spans.append(
            {
                "item": name,
                "set": sset,
                "lang": lang,
                "start": t,
                "end": t + len(a) / SR,
                "ref": ref,
            }
        )
        parts += [a, noise(pause)]
        t += len(a) / SR
        pauses.append([t, t + pause])
        t += pause
    sf.write(D / "all.wav", np.concatenate(parts), SR, subtype="PCM_16")
    (D / "manifest.json").write_text(
        json.dumps({"spans": spans, "pauses": pauses, "total": t}, ensure_ascii=False)
    )
    print(f"total {t / 60:.1f} min, {len(spans)} items", flush=True)


if __name__ == "__main__":
    main()
