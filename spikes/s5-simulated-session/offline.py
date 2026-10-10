#!/usr/bin/env python3
"""Replays the S5 audio through the STT worker's VAD and model, offline, on one CPU budget.

Per script line: transcript lines, WER. Per utterance: model time, language, confidence. A
single-consumer queue fed at the times the VAD cuts gives the latency AC7 bounds and the drops.

Usage, from stt/ so the worker's package and venv are used:
  uv run python ../spikes/s5-simulated-session/offline.py <audio dir> [options]
"""

import argparse
import json
import statistics
import sys
import time
from pathlib import Path
from typing import Any

from faster_whisper import WhisperModel
from faster_whisper.audio import decode_audio
from grill_stt.pipeline import Plausibility, implausible
from grill_stt.transcribe import Line, Transcriber
from grill_stt.vad import SR, Audio, Segmenter, Silero

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from analyze import edits, words  # noqa: E402

FRAME = 160  # 10 ms, as LiveKit delivers them


class TwoPass:
    """The worker before S5: transcribe(language=None), which encodes each utterance twice."""

    def __init__(self, model: str, threads: int) -> None:
        self._m = WhisperModel(model, device="cpu", compute_type="int8", cpu_threads=threads)

    def __call__(self, audio: Audio) -> Line:
        segments, info = self._m.transcribe(
            audio, beam_size=1, without_timestamps=True, condition_on_previous_text=False, vad_filter=False
        )
        kept = [s for s in segments if not (s.no_speech_prob > 0.6 and s.avg_logprob < -1.0)]
        text = " ".join(t for t in (s.text.strip() for s in kept) if t)
        return Line(
            text,
            info.language,
            info.language_probability,
            min((s.avg_logprob for s in kept), default=0.0),
            max((s.no_speech_prob for s in kept), default=0.0),
            max((s.compression_ratio for s in kept), default=1.0),
        )


def cut(audio: Audio, min_silence_ms: int, max_speech_s: float) -> list[dict[str, Any]]:
    """Utterances as the worker cuts them: audio, the instant the VAD emits it, the end of speech."""
    seg = Segmenter(Silero(), min_silence_ms=min_silence_ms, max_speech_s=max_speech_s)
    out: list[dict[str, Any]] = []
    for i in range(0, len(audio), FRAME):
        at = (i + FRAME) / SR
        for u in seg.feed(audio[i : i + FRAME]):
            out.append({"audio": u, "at": at, "ended": at - min_silence_ms / 1000, "start": at - len(u) / SR})
    for u in seg.flush():
        out.append({"audio": u, "at": len(audio) / SR, "ended": len(audio) / SR, "start": 0.0})
    return out


def pct(v: list[float], q: float) -> float:
    v = sorted(v)
    return v[min(len(v) - 1, round(q * (len(v) - 1)))]


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("audio", type=Path, help="dir with <speaker>-r<round>.wav from make_audio.py")
    ap.add_argument("--model", default="small")
    ap.add_argument("--threads", type=int, default=4)
    ap.add_argument("--min-silence-ms", type=int, default=500)
    ap.add_argument("--max-speech-s", type=float, default=15.0)
    ap.add_argument("--queue", type=int, default=8)
    ap.add_argument("--two-pass", action="store_true", help="the pre-S5 transcriber")
    ap.add_argument("--fallback", action="store_true", help="temperature fallback 0.0-1.0 (pre-S5 default)")
    ap.add_argument("--json", type=Path, help="write every utterance here")
    a = ap.parse_args()

    session = json.loads((HERE / "session.json").read_text())
    if a.two_pass:
        transcribe: Any = TwoPass(a.model, a.threads)
    else:
        temps = [0.0, 0.2, 0.4, 0.6, 0.8, 1.0] if a.fallback else 0.0
        transcribe = Transcriber(a.model, a.threads, temperature=temps)
    transcribe(decode_audio(str(next(a.audio.glob("*-r1.wav"))), SR)[: SR * 3])  # warm-up
    rules = Plausibility()

    refs: list[dict[str, Any]] = []
    utts: list[dict[str, Any]] = []
    for n, rnd in enumerate(session["rounds"], 1):
        lines = [{**ln, "round": n, "hyp": []} for ln in rnd["lines"]]
        refs += lines
        for name in session["speakers"]:
            audio = decode_audio(str(a.audio / f"{name}-r{n}.wav"), SR)
            for u in cut(audio, a.min_silence_ms, a.max_speech_s):
                mine = [ln for ln in lines if ln["speaker"] == name]
                best = max(mine, key=lambda ln: min(ln["end"], u["ended"]) - max(ln["start"], u["start"]))
                overlap = min(best["end"], u["ended"]) - max(best["start"], u["start"])
                utts.append({**u, "round": n, "speaker": name, "ref": best if overlap > 0 else None})

    # Rounds play one after the other: offset round 2 so the queue sees one timeline.
    offset = {1: 0.0, 2: max(float(u["at"]) for u in utts if u["round"] == 1) + 60}
    utts.sort(key=lambda u: offset[u["round"]] + u["at"])
    for u in utts:
        audio = u.pop("audio")
        t0 = time.monotonic()
        line: Line = transcribe(audio)
        u["cost"] = time.monotonic() - t0
        u["line"] = line
        u["drop"] = implausible(line, len(audio) / SR, rules)
        if u["ref"] is not None and line.text and not u["drop"]:
            u["ref"]["hyp"].append(line.text)

    # One consumer, a bounded queue: what the worker does with these costs.
    free, dropped = 0.0, 0
    waiting: list[float] = []
    latency: list[float] = []
    for u in utts:
        arrive = offset[u["round"]] + u["at"]
        waiting = [d for d in waiting if d > arrive]  # start times still in the future
        if len(waiting) >= a.queue:
            dropped += 1
            continue
        start = max(arrive, free)
        free = start + u["cost"]
        waiting.append(start)
        latency.append(free - (offset[u["round"]] + u["ended"]))

    print(
        "| r | who | lang | prob | logprob | nospeech | cost s | drop | text |\n"
        "|---|---|---|---|---|---|---|---|---|"
    )
    for u in utts:
        ln = u["line"]
        print(
            f"| {u['round']} | {u['speaker']} | {ln.language} | {ln.language_probability:.2f}"
            f" | {ln.avg_logprob:.2f}"
            f" | {ln.no_speech_prob:.2f} | {u['cost']:.2f} | {u['drop'] or ''} | {ln.text[:60]} |"
        )
    agg: dict[str, list[int]] = {}
    for r in refs:
        ref, hyp = words(r["text"]), words(" ".join(r["hyp"]))
        for key in (r["speaker"], r["lang"], "all"):
            e, n = agg.setdefault(key, [0, 0])
            agg[key] = [e + edits(ref, hyp), n + len(ref)]
    per_line = [sum(1 for u in utts if u["ref"] is r) for r in refs]
    costs = [u["cost"] for u in utts]
    print(
        f"\nconfig: model={a.model} threads={a.threads} two_pass={a.two_pass} fallback={a.fallback} "
        f"min_silence_ms={a.min_silence_ms} max_speech_s={a.max_speech_s}"
    )
    print(
        f"utterances {len(utts)} for {len(refs)} script lines ({len(utts) / len(refs):.2f} per line,"
        f" max {max(per_line)}), unmatched {sum(1 for u in utts if u['ref'] is None)},"
        f" implausible {sum(1 for u in utts if u['drop'])}"
    )
    print(
        f"model s per utterance: median {statistics.median(costs):.2f}, max {max(costs):.2f},"
        f" total {sum(costs):.1f}"
    )
    print(
        f"latency s: median {statistics.median(latency):.1f}, p95 {pct(latency, 0.95):.1f},"
        f" max {max(latency):.1f};"
        f" over 10 s {sum(1 for x in latency if x > 10)}, queue drops {dropped}"
    )
    print("WER " + ", ".join(f"{k} {e / n:.3f} ({n} words)" for k, (e, n) in sorted(agg.items())))
    if a.json:
        a.json.write_text(
            "".join(
                json.dumps(
                    {
                        "round": u["round"],
                        "speaker": u["speaker"],
                        "cost": u["cost"],
                        "drop": u["drop"],
                        "ref": u["ref"]["text"] if u["ref"] else None,
                        **vars(u["line"]),
                    },
                    ensure_ascii=False,
                )
                + "\n"
                for u in utts
            )
        )


if __name__ == "__main__":
    main()
