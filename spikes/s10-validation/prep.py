#!/usr/bin/env python3
"""Builds $D/corpus: utterances cut by the worker's own Segmenter (stt/grill_stt/vad.py), plus references.

corpus/<set>.npz   utterance audio (16 kHz float32), key = utterance id
corpus/<set>.json  [{id, item, lang, dur, arrival}] in arrival order; items: {item: ref text}
wav/<id>.wav       the same audio for the transcribe.cpp CLI

The talk set needs $D/real/k8s_ops.m4a (the talk's audio) and $D/real/devoxx_items.json (its YouTube
fr-orig auto-captions per clip, the silver reference), both copied in before the run.
"""
import io
import json
import os
import subprocess
import sys
from pathlib import Path

import numpy as np
import soundfile as sf

sys.path.insert(0, "/bench")
from vad import SR, Segmenter, Silero

D = Path(os.environ.get("D", "/data"))
C = D / "corpus"
C.mkdir(parents=True, exist_ok=True)
STEP = 160  # LiveKit hands the worker 10 ms frames


def segment(audio: np.ndarray) -> list[tuple[np.ndarray, float]]:
    """(utterance, arrival_s): arrival is the track position when the Segmenter emits it."""
    seg = Segmenter(Silero())
    out = []
    for i in range(0, len(audio), STEP * 32):
        for u in seg.feed(audio[i : i + STEP * 32]):
            out.append((u, min(i + STEP * 32, len(audio)) / SR))
    for u in seg.flush():
        out.append((u, len(audio) / SR))
    return out


def save(name: str, utts: list[dict], arrays: dict, items: dict) -> None:
    np.savez(C / f"{name}.npz", **arrays)
    (C / f"{name}.json").write_text(json.dumps({"utts": utts, "items": items}, ensure_ascii=False))
    (D / "wav").mkdir(exist_ok=True)
    for uid, a in arrays.items():
        sf.write(D / "wav" / f"{uid}.wav", a, SR)
    tot = sum(u["dur"] for u in utts)
    print(f"{name}: {len(utts)} utterances, {tot / 60:.1f} min speech, {len(items)} items", flush=True)


def piper() -> None:
    p = D / "piper"
    if not (p / "audio.npz").exists():
        p.mkdir(exist_ok=True)
        subprocess.run([sys.executable, "/bench/labbench.py", "audio", str(p)], check=True)
    z = np.load(p / "audio.npz")
    s = json.loads(Path("/bench/script.json").read_text())
    utts, arrays, items = [], {}, {}
    for line in s["lines"]:
        a = z[f"{line['id']}_16k"]
        arrays[line["id"]] = a
        items[line["id"]] = line["text"]
        utts.append({"id": line["id"], "item": line["id"], "lang": line["lang"], "dur": len(a) / SR, "arrival": 0.0})
    save("piper", utts, arrays, items)


def devoxx() -> None:
    from faster_whisper import decode_audio

    r = D / "real"
    items = json.loads((r / "devoxx_items.json").read_text())
    talk = decode_audio(str(r / "k8s_ops.m4a"), sampling_rate=SR)
    utts, arrays = [], {}
    for name, a0 in (("k8s1", 600), ("k8s2", 1500)):
        audio = talk[a0 * SR : (a0 + 180) * SR]
        for k, (u, t) in enumerate(segment(audio)):
            uid = f"{name}_{k:03d}"
            arrays[uid] = u
            utts.append({"id": uid, "item": name, "lang": "fr", "dur": len(u) / SR, "arrival": t})
    save("devoxx", utts, arrays, items)


def summre(target_min: float = 15.0, window_s: float = 300.0) -> None:
    """Gold-reference meetings: one track per speaker, a window per meeting, until target_min of speech."""
    import pyarrow.parquet as pq
    from huggingface_hub import hf_hub_download

    utts, arrays, items = [], {}, {}
    speech = 0.0
    for shard in range(3):
        f = hf_hub_download("linagora/SUMM-RE", f"data/test/test-{shard:05d}-of-00028.parquet", repo_type="dataset")
        pf = pq.ParquetFile(f)
        for rg in range(pf.num_row_groups):
            t = pf.read_row_group(rg, columns=["meeting_id", "speaker_id", "audio", "segments"]).to_pylist()
            for row in t:
                segs = row["segments"]
                if not segs:
                    continue
                # A window in the middle of the meeting, snapped to this speaker's segments.
                mid = (segs[0]["start"] + segs[-1]["end"]) / 2
                inside = [s for s in segs if mid - window_s / 2 <= s["start"] and s["end"] <= mid + window_s / 2]
                if len(inside) < 5:
                    continue
                w0, w1 = inside[0]["start"] - 0.3, inside[-1]["end"] + 0.5
                audio, sr = sf.read(io.BytesIO(row["audio"]["bytes"]), dtype="float32")
                if audio.ndim > 1:
                    audio = audio.mean(axis=1)
                if sr != SR:
                    import soxr
                    audio = soxr.resample(audio, sr, SR).astype(np.float32)
                audio = audio[int(max(w0, 0) * SR) : int(w1 * SR)]
                item = f"{row['meeting_id']}_{row['speaker_id']}"
                items[item] = " ".join(s["transcript"] if "transcript" in s else s.get("text", "") for s in inside)
                own = [(s["start"] - max(w0, 0), s["end"] - max(w0, 0)) for s in inside]
                for k, (u, at) in enumerate(segment(audio)):
                    # Keep utterances that are mostly this speaker: the tracks pick up the other people in the room.
                    e = at - 0.6
                    b = e - len(u) / SR
                    ov = sum(max(0.0, min(e, y) - max(b, x)) for x, y in own)
                    if ov < 0.5 * (e - b):
                        continue
                    uid = f"{item}_{k:03d}"
                    arrays[uid] = u
                    utts.append({"id": uid, "item": item, "lang": "fr", "dur": len(u) / SR, "arrival": at})
                speech += sum(s["end"] - s["start"] for s in inside)
                print(f"  {item}: {len(inside)} segs, speech so far {speech / 60:.1f} min", flush=True)
                if speech >= target_min * 60:
                    save("summre", utts, arrays, items)
                    return
    save("summre", utts, arrays, items)


if __name__ == "__main__":
    for step in sys.argv[1:] or ["piper", "devoxx", "summre"]:
        globals()[step]()
