#!/usr/bin/env python3
"""Scores S17 browser runs (S11's analyze.py): WER / English leak per set, end-of-speech-to-line latency, RTFx, RAM, load.

usage: analyze-browser.py <runs-dir> <manifest.json> <tag>...   (one JSON line per run)
"""

import importlib.util
import json
import statistics
import sys
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "s10score", Path(__file__).resolve().parents[1] / "s10-validation" / "score.py"
)
s10 = importlib.util.module_from_spec(spec)
assert spec.loader
spec.loader.exec_module(s10)
edits, en_share, pct, words = s10.edits, s10.en_share, s10.pct, s10.words

SR = 16000
RUNS = Path(sys.argv[1])
MAN = json.loads(Path(sys.argv[2]).read_text())


def assign(lines: list[dict], offset: float) -> dict[str, list[dict]]:
    """Utterance midpoint -> the item whose span (plus its trailing pause) holds it."""
    by: dict[str, list[dict]] = {s["item"]: [] for s in MAN["spans"]}
    for ln in lines:
        mid = (ln["start"] + ln["end"]) / 2 / SR + offset
        for s, (p0, p1) in zip(MAN["spans"], MAN["pauses"], strict=True):
            if s["start"] - 1.0 <= mid < p1:
                by[s["item"]].append(ln)
                break
    return by


def best_offset(lines: list[dict]) -> float:
    """Live capture starts a little before or after the corpus clock: align utterances onto the spans."""

    def inside(off: float) -> int:
        return sum(
            any(
                s["start"] - 0.3 <= ln["start"] / SR + off and ln["end"] / SR + off <= s["end"] + 0.5
                for s in MAN["spans"]
            )
            for ln in lines
        )

    grid = [x / 20 for x in range(-60, 61)]
    return max(grid, key=inside)


def wer(by: dict[str, list[dict]], pred, horizon: float = 1e9) -> dict:
    err = n = hw = 0
    leak_w = 0.0
    for s in MAN["spans"]:
        if not pred(s) or s["end"] > horizon:
            continue
        hyp_text = " ".join(ln["text"] for ln in sorted(by[s["item"]], key=lambda x: x["start"]))
        ref, hyp = words(s["ref"]), words(hyp_text)
        err += edits(ref, hyp)
        n += len(ref)
        hw += len(hyp)
        if s["lang"] == "fr":
            leak_w += en_share(hyp_text) * len(hyp)
    return {
        "wer": round(err / n, 3) if n else None,
        "ref_words": n,
        "hyp_ref": round(hw / n, 3) if n else None,
        "en_leak": round(leak_w / hw, 3) if hw else 0.0,
    }


def q(xs: list[float]) -> dict:
    return (
        {
            "p50": round(pct(xs, 0.5), 2),
            "p95": round(pct(xs, 0.95), 2),
            "max": round(max(xs), 2),
            "n": len(xs),
        }
        if xs
        else {}
    )


def run(tag: str) -> dict:
    d = RUNS / tag
    ev = [json.loads(x) for x in (d / "events.jsonl").read_text().splitlines()]
    res = json.loads((d / "result.json").read_text())
    lines = [e for e in ev if e["ev"] == "line"]
    live = res["mode"] == "live"
    off = best_offset(lines) if live else 0.0
    by = assign(lines, off)
    # A partial run only scores the items it heard to the end.
    hz = (
        max(ln["end"] for ln in lines) / SR + off + 1
        if res.get("error")
        else res.get("liveEnd", 1e9)
        if live
        else 1e9
    )
    out = {
        "tag": tag,
        "mode": res["mode"],
        "args": " ".join(res["argv"]),
        "error": res.get("error"),
        "offset": off,
        "utts": len(lines),
    }
    out["env"] = next((e for e in ev if e["ev"] == "env"), {})
    out["load"] = [
        {k: e[k] for k in ("fetchMs", "sessionMs", "warmupMs", "files")} for e in ev if e["ev"] == "stt-ready"
    ]
    out["loaded_ms"] = [round(e["ms"]) for e in ev if e["ev"] == "loaded"]
    out["talk"] = wer(by, lambda s: s["set"] == "talk", hz)
    out["summre"] = wer(by, lambda s: s["set"] == "summre", hz)
    out["piper"] = wer(by, lambda s: s["set"] == "piper", hz)
    out["piper_fr"] = wer(by, lambda s: s["set"] == "piper" and s["lang"] == "fr", hz)
    out["piper_en"] = wer(by, lambda s: s["set"] == "piper" and s["lang"] == "en", hz)
    dec = [(ln["decodeEnd"] - ln["decodeStart"]) / 1000 for ln in lines]
    out["rtfx"] = round(sum(ln["dur"] for ln in lines) / sum(dec), 2) if dec else None
    out["decode_s"] = q(dec)
    out["speech_min"] = round(sum(ln["dur"] for ln in lines) / 60, 1)
    if live:
        # End of speech = receive time of the last voiced VAD window; line = decode finished in the STT worker.
        paused = [ln for ln in lines if ln["cutAt"] - ln["lastVoiced"] >= 0.7 * SR]
        out["lat_all"] = q([(ln["decodeEnd"] - ln["lastWall"]) / 1000 for ln in lines])
        out["lat_pause_cut"] = q([(ln["decodeEnd"] - ln["lastWall"]) / 1000 for ln in paused])
        out["lat_maxlen_cut"] = q(
            [(ln["decodeEnd"] - ln["cutWall"]) / 1000 for ln in lines if ln not in paused]
        )
        out["queue_s"] = q([(ln["decodeStart"] - ln["cutWall"]) / 1000 for ln in lines])
        out["vad_wait_s"] = q([(ln["cutWall"] - ln["lastWall"]) / 1000 for ln in paused])
        # Lines whose midpoint fell in a pause: invented text.
        out["in_pause"] = [
            ln["text"]
            for ln in lines
            if not any(
                s["start"] - 1.0 <= (ln["start"] + ln["end"]) / 2 / SR + off <= s["end"] + 0.5
                for s in MAN["spans"]
            )
        ]
    procs = [json.loads(x) for x in (d / "proc.jsonl").read_text().splitlines()]
    tot = [sum(v["rss"] for k, v in p.items() if isinstance(v, dict)) for p in procs]
    cpu = [sum(v["cpu"] for k, v in p.items() if isinstance(v, dict)) for p in procs]
    out["rss_mb"] = {
        "peak": round(max(tot)),
        "median": round(statistics.median(tot)),
        "renderer_peak": round(max(p.get("renderer", {}).get("max", 0) for p in procs)),
    }
    out["cpu_cores_mean"] = round(statistics.mean(cpu[1:]), 2) if len(cpu) > 1 else None
    vs = next((e for e in ev if e["ev"] in ("vad-done", "vad-stats")), None)
    if vs:
        out["vad_ms_per_window"] = round(vs["vadMs"] / max(vs["windows"], 1), 3)
    return out


if __name__ == "__main__":
    for t in sys.argv[3:]:
        print(json.dumps(run(t), ensure_ascii=False))
