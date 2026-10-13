#!/usr/bin/env python3
"""Builds the A/B corpus as whole speaker tracks, the worker's replay input (spec 007, AC16).

usage: prep.py <out dir> <talk.m4a> <devoxx_items.json>
       (run with: uv run --with pyarrow --with soundfile --with soxr --with huggingface_hub
                  --with numpy --with piper-tts python prep.py ...)

Same material as spike S10d, uncut: the Segmenter runs live in the replay instead.
- summre: SUMM-RE test meetings (CC-BY-SA-4.0), the S10d selection: one 5-min window per speaker
  track, until 15 min of speech; gold reference, and the speaker's own segments for the bleed filter.
- devoxx: two 3-min clips of a French Kubernetes talk; silver reference (YouTube auto-captions).
- piper: the lab's 38 Piper lines (spikes/s10-stt-models/script.json), one track each.
Writes <out>/<set>/<track>.wav (16 kHz mono PCM16) and <out>/manifest.json.
"""

import io
import json
import subprocess
import sys
import wave
from pathlib import Path

import numpy as np

SR = 16000
HERE = Path(__file__).resolve().parent
SUMMRE_REVISION = "47cb4ec3437ef5497b6092b8c6d1e0b5b1d86dc0"


def write(path: Path, audio: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes((np.clip(audio, -1, 1) * 32767).astype("<i2").tobytes())


def summre(out: Path, tracks: list, items: dict, own: dict, target_min: float = 15.0, window_s: float = 300.0) -> None:
    import pyarrow.parquet as pq
    import soundfile as sf
    import soxr
    from huggingface_hub import hf_hub_download

    speech = 0.0
    for shard in range(3):
        f = hf_hub_download("linagora/SUMM-RE", f"data/test/test-{shard:05d}-of-00028.parquet",
                            repo_type="dataset", revision=SUMMRE_REVISION)
        pf = pq.ParquetFile(f)
        for rg in range(pf.num_row_groups):
            for row in pf.read_row_group(rg, columns=["meeting_id", "speaker_id", "audio", "segments"]).to_pylist():
                segs = row["segments"]
                if not segs:
                    continue
                mid = (segs[0]["start"] + segs[-1]["end"]) / 2
                inside = [s for s in segs if mid - window_s / 2 <= s["start"] and s["end"] <= mid + window_s / 2]
                if len(inside) < 5:
                    continue
                w0, w1 = max(inside[0]["start"] - 0.3, 0), inside[-1]["end"] + 0.5
                audio, sr = sf.read(io.BytesIO(row["audio"]["bytes"]), dtype="float32")
                if audio.ndim > 1:
                    audio = audio.mean(axis=1)
                if sr != SR:
                    audio = soxr.resample(audio, sr, SR).astype(np.float32)
                item = f"{row['meeting_id']}_{row['speaker_id']}"
                write(out / "summre" / f"{item}.wav", audio[int(w0 * SR) : int(w1 * SR)])
                tracks.append({"id": item, "set": "summre", "item": item, "lang": "fr", "wav": f"summre/{item}.wav"})
                items[item] = " ".join(s["transcript"] if "transcript" in s else s.get("text", "") for s in inside)
                own[item] = [[round(s["start"] - w0, 3), round(s["end"] - w0, 3)] for s in inside]
                speech += sum(s["end"] - s["start"] for s in inside)
                print(f"summre {item}: speech so far {speech / 60:.1f} min", flush=True)
                if speech >= target_min * 60:
                    return


def devoxx(out: Path, m4a: Path, refs: Path, tracks: list, items: dict) -> None:
    items.update(json.loads(refs.read_text()))
    for name, a0 in (("k8s1", 600), ("k8s2", 1500)):
        wav = out / "devoxx" / f"{name}.wav"
        wav.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-ss", str(a0), "-t", "180", "-i", str(m4a),
                        "-ar", str(SR), "-ac", "1", "-c:a", "pcm_s16le", str(wav)], check=True)
        tracks.append({"id": name, "set": "devoxx", "item": name, "lang": "fr", "wav": f"devoxx/{name}.wav"})


def piper(out: Path, tracks: list, items: dict) -> None:
    """The lab's lines through spikes/s10-stt-models/bench.py, each with a second of silence after it."""
    cache = out / "piper-src"
    cache.mkdir(parents=True, exist_ok=True)
    if not (cache / "audio.npz").exists():
        subprocess.run([sys.executable, str(HERE.parent / "s10-stt-models" / "bench.py"), "audio", str(cache)], check=True)
    z = np.load(cache / "audio.npz")
    script = json.loads((HERE.parent / "s10-stt-models" / "script.json").read_text())
    for line in script["lines"]:
        a = np.concatenate([z[f"{line['id']}_16k"], np.zeros(SR, np.float32)])
        write(out / "piper" / f"{line['id']}.wav", a)
        tracks.append({"id": line["id"], "set": "piper", "item": line["id"], "lang": line["lang"], "wav": f"piper/{line['id']}.wav"})
        items[line["id"]] = line["text"]


def main() -> None:
    out, m4a, refs = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
    tracks: list = []
    items: dict = {}
    own: dict = {}
    piper(out, tracks, items)
    devoxx(out, m4a, refs, tracks, items)
    summre(out, tracks, items, own)
    (out / "manifest.json").write_text(json.dumps({"tracks": tracks, "items": items, "own": own}, ensure_ascii=False, indent=1) + "\n")
    print({s: sum(t["set"] == s for t in tracks) for s in ("summre", "devoxx", "piper")})


if __name__ == "__main__":
    main()
