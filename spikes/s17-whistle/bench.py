#!/usr/bin/env python3
"""One engine over one corpus: one JSON line per utterance in $D/out/<tag>.<set>.jsonl, rows as S10's score.py reads them.

usage: bench.py <engine> <set> [--keywords none|piper|own] [--cpus 8,10] [--threads N] [--track K] [--paced] [--depth N] [--language xx] [--tag T]
engine: parakeet  the shipped worker binding (grill_stt.parakeet, transcribe.cpp Q8_0, greedy)
        orukeet   the same binding on $ORUKEET_MODEL (oruk/orukeet transcribe.cpp Q8_0)
        kroko     Kroko FR community through sherpa-onnx ($KROKO_DIR)
        whistle   the official Cactus engine (`needle` CLI), language detected unless --language
--keywords: Whistle keyword biasing with the Piper room glossary, or the set's own glossary (parakeet has none)
--cpus: pin this process (and the engine threads it starts) to these cores
--track K: only the K-th longest speaker track (concurrency runs give each process one track)
--paced: submit each utterance at its arrival time on the track, so latency includes queueing (concurrency runs)
"""

import argparse
import json
import os
import resource
import subprocess
import time
import wave
from pathlib import Path

import numpy as np

D = Path(os.environ.get("D", "/data"))
VAD_CUT_S = 0.8


def rss_mb() -> float:
    for line in Path("/proc/self/status").read_text().splitlines():
        if line.startswith("VmRSS:"):
            return int(line.split()[1]) / 1024
    return 0.0


class Whistle:
    """The official `needle` CLI, one process per utterance with an explicit --threads.

    The C API (needle.h, libneedle3.so) has no thread setting, and the engine's default picks 12 threads in a
    4-core cpuset and runs ~100x slower; the CLI's --threads is the only way to bound it. Model load is 17 MB,
    so the per-call process start stays in the measured time (conservative).
    """

    def __init__(
        self, cli: str, cact: str, threads: int, keywords: list[str] | None, language: str | None, depth: int
    ) -> None:
        self.tmp = Path(f"/dev/shm/s17-{os.getpid()}")
        self.tmp.mkdir(exist_ok=True)
        self.cmd = [cli, "--model", cact, "--threads", str(threads)]
        if keywords:
            (self.tmp / "kw.txt").write_text("".join(f"{k}\n" for k in keywords))
            self.cmd += ["--audio-keywords", str(self.tmp / "kw.txt")]
        if language:
            self.cmd += ["--audio-language", language]
        if depth:
            self.cmd += ["--audio-depth", str(depth)]

    def __call__(self, audio: np.ndarray) -> dict:
        wav = self.tmp / "u.wav"
        with wave.open(str(wav), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(4)
            w.setframerate(16000)
            # float64: in float32, 1.0 * (2**31 - 1) rounds to 2**31 and wraps to -2**31.
            w.writeframes((np.clip(audio, -1, 1).astype(np.float64) * 2147483647).astype("<i4").tobytes())
        p = subprocess.run([*self.cmd, "--audio", str(wav)], capture_output=True, text=True, check=False)
        rows = [x for x in p.stdout.splitlines() if x.startswith("{")]
        if p.returncode or not rows:
            return {"hyp": "", "error": f"rc={p.returncode} {p.stderr[-300:]}"}
        r = json.loads(rows[-1])
        return {
            "hyp": r["text"],
            "wlang": r["language"],
            "ttft_ms": r["ttft_ms"],
            "decode_tps": r["decode_tps"],
        }


class Parakeet:
    def __init__(self, model: str, threads: int) -> None:
        from grill_stt.parakeet import ParakeetTranscriber

        self.t = ParakeetTranscriber(model, threads)

    def __call__(self, audio: np.ndarray) -> dict:
        return {"hyp": self.t(audio).text}


class Kroko:
    """Kroko FR community (streaming zipformer, sherpa-onnx), fed one whole utterance, greedy."""

    def __init__(self, d: str, threads: int) -> None:
        import sherpa_onnx

        self.r = sherpa_onnx.OnlineRecognizer.from_transducer(
            tokens=f"{d}/tokens.txt",
            encoder=f"{d}/encoder.onnx",
            decoder=f"{d}/decoder.onnx",
            joiner=f"{d}/joiner.onnx",
            num_threads=threads,
            sample_rate=16000,
            feature_dim=80,
            decoding_method="greedy_search",
        )

    def __call__(self, audio: np.ndarray) -> dict:
        s = self.r.create_stream()
        s.accept_waveform(16000, audio)
        s.accept_waveform(16000, np.zeros(8000, np.float32))  # flush the encoder's right context
        s.input_finished()
        while self.r.is_ready(s):
            self.r.decode_stream(s)
        return {"hyp": self.r.get_result(s)}


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("engine", choices=["parakeet", "orukeet", "whistle", "kroko"])
    ap.add_argument("set")
    ap.add_argument("--keywords", choices=["none", "piper", "own"], default="none")
    ap.add_argument("--language", default=None)
    ap.add_argument("--depth", type=int, default=0)
    ap.add_argument("--cpus", default="")
    ap.add_argument("--threads", type=int, default=4)
    ap.add_argument("--track", type=int, default=-1)
    ap.add_argument("--paced", action="store_true")
    ap.add_argument("--tag", default="")
    a = ap.parse_args()
    if a.cpus:
        os.sched_setaffinity(0, {int(c) for c in a.cpus.split(",")})
    meta = json.loads((D / "corpus" / f"{a.set}.json").read_text())
    z = np.load(D / "corpus" / f"{a.set}.npz")
    utts = meta["utts"]
    if a.track >= 0:
        speech: dict[str, float] = {}
        for u in utts:
            speech[u["item"]] = speech.get(u["item"], 0.0) + u["dur"]
        item = sorted(speech, key=lambda i: -speech[i])[a.track]
        utts = [u for u in utts if u["item"] == item]
    gloss = json.loads((D / "corpus" / "glossary.json").read_text())
    terms = {"none": None, "piper": gloss["piper"], "own": gloss[a.set]}[a.keywords]
    tag = a.tag or f"{a.engine}.kw-{a.keywords}"
    if a.track >= 0:
        tag += f".k{a.track}"
    od = D / "out"
    od.mkdir(exist_ok=True)
    base = rss_mb()
    t0 = time.perf_counter()
    if a.engine == "whistle":
        eng = Whistle(
            os.environ["NEEDLE_CLI"], os.environ["WHISTLE_CACT"], a.threads, terms, a.language, a.depth
        )
    elif a.engine == "kroko":
        eng = Kroko(os.environ.get("KROKO_DIR", "/eng/kroko"), a.threads)
    else:
        model = {
            "parakeet": "/models/parakeet/parakeet-ultra-Q8_0.gguf",
            "orukeet": os.environ.get("ORUKEET_MODEL", ""),
        }
        eng = Parakeet(model[a.engine], a.threads)
    load = time.perf_counter() - t0
    eng(z[utts[0]["id"]])  # warm-up, as the worker does at start
    with open(od / f"{tag}.{a.set}.jsonl", "w") as out:
        start = time.perf_counter()
        for u in utts:
            if a.paced:
                # The Segmenter hands the utterance over at `arrival`; a busy engine makes it wait.
                time.sleep(max(0.0, start + u["arrival"] - time.perf_counter()))
            s = time.perf_counter()
            r = eng(z[u["id"]])
            e = time.perf_counter()
            row = u | r | {"infer": e - s}
            if a.paced:
                row["wait"] = max(0.0, s - (start + u["arrival"]))
            out.write(json.dumps(row, ensure_ascii=False) + "\n")
            out.flush()
    who = resource.RUSAGE_CHILDREN if a.engine == "whistle" else resource.RUSAGE_SELF
    peak = resource.getrusage(who).ru_maxrss / 1024
    m = {
        "rss_mb": peak,
        "rss_base_mb": base,
        "load_s": load,
        "threads_seen": len(os.listdir("/proc/self/task")),
        "wall": time.perf_counter() - t0,
    }
    (od / f"{tag}.{a.set}.meta").write_text(json.dumps(m))
    print(f"done {tag} {a.set} {json.dumps(m)}", flush=True)


if __name__ == "__main__":
    main()
