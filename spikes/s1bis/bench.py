#!/usr/bin/env python3
"""S1-bis: faster-whisper int8 on CPU. Subcommands: fetch, rtf, stream, chunk (see S1bis-whisper-cpu.md)."""

import argparse
import csv
import io
import json
import multiprocessing as mp
import os
import re
import resource
import statistics
import tarfile
import time
import urllib.request
import wave
from concurrent.futures import ProcessPoolExecutor
from pathlib import Path
from typing import Any

SCRATCH = Path(__file__).resolve().parents[2] / ".scratchpad" / "s1bis"
AUDIO = SCRATCH / "audio"
# huggingface_hub reads HF_HOME at import time, so it must be set before faster_whisper is imported.
os.environ.setdefault("HF_HOME", str(SCRATCH / "hf"))

import jiwer  # noqa: E402
import numpy as np  # noqa: E402
from faster_whisper import WhisperModel, decode_audio  # noqa: E402
from faster_whisper.vad import VadOptions, get_speech_timestamps  # noqa: E402

SR = 16000
CLIPS = ("fr", "en", "mixed")
FLEURS = "https://huggingface.co/datasets/google/fleurs/resolve/main/data/{lang}/{path}"
PEAK = 0.5
GAP_S = 0.6  # pause between utterances, long enough for the VAD to close a segment
MIN_SIL_MS = 500  # VAD end-of-utterance wait: a floor on latency that no CPU can remove


def fleurs(lang: str, n: int) -> list[tuple[np.ndarray, str]]:
    """First n distinct dev utterances, streamed so only a few MB of the 150 MB archive are read."""
    with urllib.request.urlopen(FLEURS.format(lang=lang, path="dev.tsv"), timeout=60) as r:
        rows = csv.reader(io.TextIOWrapper(r, "utf-8"), delimiter="\t", quoting=csv.QUOTE_NONE)
        refs = {row[1]: row[2] for row in rows}
    out: list[tuple[np.ndarray, str]] = []
    with (
        urllib.request.urlopen(FLEURS.format(lang=lang, path="audio/dev.tar.gz"), timeout=60) as r,
        tarfile.open(fileobj=r, mode="r|gz") as tar,
    ):
        for m in tar:
            text = refs.get(Path(m.name).name)
            # FLEURS has several speakers per sentence; keep each sentence once.
            if not m.isfile() or text is None or any(text == t for _, t in out):
                continue
            f = tar.extractfile(m)
            assert f is not None
            out.append((decode_audio(io.BytesIO(f.read()), sampling_rate=SR), text))
            if len(out) == n:
                break
    return out


def write_clip(name: str, utts: list[tuple[np.ndarray, str, str]]) -> None:
    gap = np.zeros(int(GAP_S * SR), dtype=np.float32)
    parts, segs, t = [], [], 0.0
    for audio, text, lang in utts:
        # Some FLEURS takes peak at -40 dBFS and the VAD drops them; browsers apply AGC, so normalize here.
        audio = audio * (PEAK / max(float(np.abs(audio).max()), 1e-6))
        parts += [audio, gap]
        segs.append({"start": t, "end": t + len(audio) / SR, "lang": lang, "text": text})
        t += len(audio) / SR + GAP_S
    pcm = (np.clip(np.concatenate(parts), -1, 1) * 32767).astype("<i2")
    with wave.open(str(AUDIO / f"{name}.wav"), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())
    (AUDIO / f"{name}.json").write_text(json.dumps(segs, ensure_ascii=False, indent=1) + "\n")
    print(f"{name}: {t:.1f}s, {len(utts)} utterances")


def cmd_fetch(_: argparse.Namespace) -> None:
    AUDIO.mkdir(parents=True, exist_ok=True)
    fr, en = fleurs("fr_fr", 7), fleurs("en_us", 7)
    write_clip("fr", [(a, t, "fr") for a, t in fr[:4]])
    write_clip("en", [(a, t, "en") for a, t in en[:4]])
    write_clip("mixed", [(a, t, lg) for (a, t), lg in zip([fr[4], en[4], fr[5], en[5], fr[6]], "fr en fr en fr".split())])


def load_clip(name: str) -> tuple[np.ndarray, list[dict[str, Any]]]:
    return decode_audio(str(AUDIO / f"{name}.wav"), sampling_rate=SR), json.loads((AUDIO / f"{name}.json").read_text())


def norm(s: str) -> str:
    return " ".join(re.sub(r"[^\w']+", " ", s.lower().replace("’", "'")).split())


def wer(ref: str, hyp: str) -> float:
    return jiwer.wer(norm(ref), norm(hyp)) if norm(hyp) else 1.0


def model_for(name: str, threads: int) -> WhisperModel:
    m = WhisperModel(name, device="cpu", compute_type="int8", cpu_threads=threads)
    list(m.transcribe(np.zeros(SR, dtype=np.float32), language="en")[0])  # warm-up, excluded from timings
    return m


def rss_mb() -> float:
    return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024


def rss_now_mb() -> float:
    # Peak RSS includes the float16 -> int8 conversion at load; steady state is what a pod keeps.
    return int(Path("/proc/self/statm").read_text().split()[1]) * os.sysconf("SC_PAGE_SIZE") / 2**20


def file_rtf(name: str, threads: int) -> dict[str, Any]:
    m = model_for(name, threads)
    out: dict[str, Any] = {"model": name, "threads": threads}
    for clip in CLIPS:
        audio, segs = load_clip(clip)
        t0 = time.perf_counter()
        hyp_segs, info = m.transcribe(audio, beam_size=1)
        hyp = " ".join(s.text for s in hyp_segs)
        dt = time.perf_counter() - t0
        out[clip] = {"rtf": dt / (len(audio) / SR), "wer": wer(" ".join(s["text"] for s in segs), hyp), "lang": info.language}
        if clip == "mixed":
            out[clip]["hyp"] = hyp
    out["rss_mb"], out["rss_now_mb"] = rss_mb(), rss_now_mb()
    return out


def chunks(audio: np.ndarray, max_s: float) -> list[tuple[int, int]]:
    opts = VadOptions(min_silence_duration_ms=MIN_SIL_MS, max_speech_duration_s=max_s)
    return [(c["start"], c["end"]) for c in get_speech_timestamps(audio, opts)]


def stream_pass(m: WhisperModel, max_s: float, lang: str | None = None) -> list[dict[str, Any]]:
    """Transcribe each VAD chunk on its own, as a live pipeline would once the speaker pauses."""
    recs = []
    for clip in CLIPS:
        audio, segs = load_clip(clip)
        for s, e in chunks(audio, max_s):
            mid = (s + e) / 2 / SR
            truth = next((g for g in segs if g["start"] <= mid <= g["end"]), segs[-1])
            t0 = time.perf_counter()
            hyp_segs, info = m.transcribe(
                audio[s:e], language=lang, beam_size=1, without_timestamps=True, condition_on_previous_text=False
            )
            text = " ".join(x.text for x in hyp_segs)
            recs.append({"clip": clip, "dur": (e - s) / SR, "dt": time.perf_counter() - t0,
                         "lang": info.language, "truth": truth["lang"], "utt": truth["text"], "text": text})
    return recs


def stream_one(name: str, threads: int, max_s: float, lang: str | None) -> dict[str, Any]:
    m = model_for(name, threads)
    recs = stream_pass(m, max_s, lang)
    return {"model": name, "threads": threads, "max_s": max_s, "lang": lang, "recs": recs, "rss_mb": rss_mb()}


def summarize(recs: list[dict[str, Any]]) -> dict[str, float]:
    dts = sorted(r["dt"] for r in recs)
    return {"rtf": sum(dts) / sum(r["dur"] for r in recs), "p50": statistics.median(dts),
            "p95": dts[int(0.95 * (len(dts) - 1))], "max": dts[-1], "n": len(dts)}


def isolated(fn: Any, *args: Any) -> Any:
    """Fresh process per config so ru_maxrss is that config's peak, not the bench's."""
    with ProcessPoolExecutor(1, mp_context=mp.get_context("spawn")) as ex:
        return ex.submit(fn, *args).result()


def save(kind: str, data: Any) -> None:
    path = SCRATCH / f"results-{kind}.jsonl"
    with path.open("a") as f:
        f.write(json.dumps(data, ensure_ascii=False) + "\n")


def cmd_rtf(a: argparse.Namespace) -> None:
    for name in a.models:
        for t in a.threads:
            r = isolated(file_rtf, name, t)
            save("rtf", r)
            print(f"{name:15} t={t} rss peak={r['rss_mb']:.0f}MB now={r['rss_now_mb']:.0f}MB " + " ".join(
                f"{c}: rtf={r[c]['rtf']:.2f} wer={r[c]['wer']:.2f} lang={r[c]['lang']}" for c in CLIPS), flush=True)


def cmd_stream(a: argparse.Namespace) -> None:
    for name in a.models:
        for t in a.threads:
            r = isolated(stream_one, name, t, a.max_chunk, a.lang)
            save("stream", r)
            recs = r["recs"]
            mixed = [x for x in recs if x["clip"] == "mixed"]
            ok = sum(x["lang"] == x["truth"] for x in mixed)
            s = summarize(recs)
            clip_wer = {c: wer(" ".join(g["text"] for g in load_clip(c)[1]),
                               " ".join(x["text"] for x in recs if x["clip"] == c)) for c in CLIPS}
            print(f"{name:15} t={t} max={a.max_chunk}s wer={clip_wer} chunks={s['n']} rtf={s['rtf']:.2f} infer p50={s['p50']:.2f}s "
                  f"p95={s['p95']:.2f}s max={s['max']:.2f}s mixed-lang={ok}/{len(mixed)}", flush=True)


def cmd_chunk(a: argparse.Namespace) -> None:
    """One timed 2-5 s chunk per config: cheap enough to sweep core types under taskset."""
    m = model_for(a.model, a.threads)
    audio, _ = load_clip("mixed")
    s, e = chunks(audio, 5.0)[1]  # English chunk, so auto-detect has to switch away from French
    t0 = time.perf_counter()
    segs, info = m.transcribe(audio[s:e], beam_size=1, without_timestamps=True, condition_on_previous_text=False)
    text = " ".join(x.text for x in segs)
    dt = time.perf_counter() - t0
    row = {"model": a.model, "threads": a.threads, "cpus": sorted(os.sched_getaffinity(0)), "dur": (e - s) / SR,
           "dt": dt, "lang": info.language, "text": text, "rss_mb": rss_mb(), "rss_now_mb": rss_now_mb()}
    save("chunk", row)
    print(f"{a.model:15} t={a.threads} cpus={row['cpus']} chunk={row['dur']:.1f}s infer={dt:.2f}s "
          f"lang={info.language} rss={row['rss_now_mb']:.0f}MB", flush=True)


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(required=True)
    sub.add_parser("fetch").set_defaults(fn=cmd_fetch)
    for kind, fn in (("rtf", cmd_rtf), ("stream", cmd_stream)):
        s = sub.add_parser(kind)
        s.add_argument("--models", nargs="+", default=["small", "medium", "large-v3-turbo"])
        s.add_argument("--threads", nargs="+", type=int, default=[1, 2, 4])
        s.add_argument("--max-chunk", type=float, default=5.0, help="VAD max speech chunk, seconds")
        if kind == "stream":
            # faster-whisper 1.2.1 discards the detection encoder pass, so auto-detect costs a second encode.
            s.add_argument("--lang", help="force a language, to measure what auto-detection costs")
        s.set_defaults(fn=fn)
    s = sub.add_parser("chunk")
    s.add_argument("--model", required=True)
    s.add_argument("--threads", type=int, required=True)
    s.set_defaults(fn=cmd_chunk)
    a = p.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
