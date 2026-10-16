#!/usr/bin/env python3
"""Rebuilds the S10 corpora under $D/corpus with the worker's own Segmenter (grill_stt.vad).

Same cuts, references and order as ../s10-validation/prep.py; only the inputs are paths, not a lab volume:
  $D/in/piper.npz         the Piper lines S13 rendered (../s10-stt-models/bench.py audio), key <id>_16k
  $D/in/script.json       ../s10-stt-models/script.json
  $D/in/k8s_ops.m4a       the talk (yt-dlp https://youtu.be/36iJtvJmqhc)
  $D/in/devoxx_items.json its fr-orig auto-captions per clip (silver reference)
  $HF_HOME                holds the linagora/SUMM-RE test shards 0-2 (gold reference)

corpus/<set>.npz   utterance audio (16 kHz float32), key = utterance id
corpus/<set>.json  {"utts": [{id, item, lang, dur, arrival}], "items": {item: reference}}

Runs in the STT worker image: PYTHONPATH=/app:<dir with pyarrow, soundfile, soxr>.
usage: prep.py [piper] [devoxx] [summre]
"""

import io
import json
import os
import sys
from pathlib import Path

import numpy as np
import soundfile as sf
from grill_stt.vad import SR, Segmenter, Silero

D = Path(os.environ.get("D", "/data"))
IN = D / "in"
C = D / "corpus"
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
    tot = sum(u["dur"] for u in utts)
    print(f"{name}: {len(utts)} utterances, {tot / 60:.1f} min speech, {len(items)} items", flush=True)


def piper() -> None:
    z = np.load(IN / "piper.npz")
    s = json.loads((IN / "script.json").read_text())
    utts, arrays, items = [], {}, {}
    for line in s["lines"]:
        a = z[f"{line['id']}_16k"]
        arrays[line["id"]] = a
        items[line["id"]] = line["text"]
        utts.append(
            {"id": line["id"], "item": line["id"], "lang": line["lang"], "dur": len(a) / SR, "arrival": 0.0}
        )
    save("piper", utts, arrays, items)


def devoxx() -> None:
    from faster_whisper import decode_audio

    items = json.loads((IN / "devoxx_items.json").read_text())
    talk = decode_audio(str(IN / "k8s_ops.m4a"), sampling_rate=SR)
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
    import soxr
    from huggingface_hub import hf_hub_download

    utts, arrays, items = [], {}, {}
    speech = 0.0
    for shard in range(3):
        f = hf_hub_download(
            "linagora/SUMM-RE", f"data/test/test-{shard:05d}-of-00028.parquet", repo_type="dataset"
        )
        pf = pq.ParquetFile(f)
        for rg in range(pf.num_row_groups):
            t = pf.read_row_group(rg, columns=["meeting_id", "speaker_id", "audio", "segments"]).to_pylist()
            for row in t:
                segs = row["segments"]
                if not segs:
                    continue
                # A window in the middle of the meeting, snapped to this speaker's segments.
                mid = (segs[0]["start"] + segs[-1]["end"]) / 2
                inside = [
                    s for s in segs if mid - window_s / 2 <= s["start"] and s["end"] <= mid + window_s / 2
                ]
                if len(inside) < 5:
                    continue
                w0, w1 = inside[0]["start"] - 0.3, inside[-1]["end"] + 0.5
                audio, sr = sf.read(io.BytesIO(row["audio"]["bytes"]), dtype="float32")
                if audio.ndim > 1:
                    audio = audio.mean(axis=1)
                if sr != SR:
                    audio = soxr.resample(audio, sr, SR).astype(np.float32)
                audio = audio[int(max(w0, 0) * SR) : int(w1 * SR)]
                item = f"{row['meeting_id']}_{row['speaker_id']}"
                items[item] = " ".join(
                    s["transcript"] if "transcript" in s else s.get("text", "") for s in inside
                )
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
    C.mkdir(parents=True, exist_ok=True)
    for step in sys.argv[1:] or ["piper", "devoxx", "summre"]:
        globals()[step]()
