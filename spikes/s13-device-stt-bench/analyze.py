#!/usr/bin/env python3
# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Scores an S13 run directory (run.sh's out-dir) and prints one JSON summary.

usage: analyze.py ac19|ac18 <out-dir> <data-dir>
  data-dir: the manifest.json prep.py wrote. WER and English leak use ../s10-validation/score.py
  (same normalisation and heuristic as S10b and S11).
"""

import json
import sqlite3
import statistics
import sys
from datetime import datetime
from itertools import pairwise
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "s10-validation"))
from score import edits, en_share, pct, words

# A 15 s cut keeps at least 451 of its 469 windows of 512 samples; a pause cut at most 450.
MAXLEN_SAMPLES = 451 * 512
P95_LIMIT_S = 4.0


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


def ts_ms(s: str) -> float:
    # Go writes RFC 3339 with nanoseconds; Python parses microseconds at most.
    head, frac = (s[:-1].split(".") + ["0"])[:2]
    return (
        datetime.fromisoformat(head + "+00:00").timestamp() * 1000
        + float("0." + frac) * 1000
    )


def voice_lines(db: Path) -> list[dict]:
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    rows = con.execute(
        "SELECT ts, sender, source, body FROM messages WHERE kind = 'voice' ORDER BY id"
    ).fetchall()
    return [
        {"t": ts_ms(t), "sender": s, "source": src, "body": b} for t, s, src, b in rows
    ]


def assign(lines: list[dict], t0: float, man: dict) -> tuple[dict, float]:
    """Line time on the corpus clock, minus the lag that puts the most lines inside a span -> item."""
    spans, pauses = man["spans"], man["pauses"]
    at = [(ln["t"] - t0) / 1000 for ln in lines]

    def inside(lag: float) -> int:
        return sum(
            any(s["start"] - 0.3 <= c - lag <= s["end"] + 0.5 for s in spans)
            for c in at
        )

    lag = max((x / 20 for x in range(161)), key=inside)
    by: dict[str, list[str]] = {s["item"]: [] for s in spans}
    for c, ln in zip(at, lines, strict=True):
        for s, (_, p1) in zip(spans, pauses, strict=True):
            if s["start"] - 1.0 <= c - lag < p1:
                by[s["item"]].append(ln["body"])
                break
    return by, lag


def wer(by: dict, man: dict, pred, horizon: float) -> dict:
    err = n = hw = 0
    leak_w = 0.0
    for s in man["spans"]:
        if not pred(s) or s["end"] > horizon:
            continue
        hyp = " ".join(by[s["item"]])
        r, h = words(s["ref"]), words(hyp)
        err += edits(r, h)
        n += len(r)
        hw += len(h)
        if s["lang"] == "fr":
            leak_w += en_share(hyp) * len(h)
    return {
        "wer": round(err / n, 3) if n else None,
        "ref_words": n,
        "en_leak": round(leak_w / hw, 3) if hw else 0.0,
    }


def scores(by: dict, man: dict, horizon: float) -> dict:
    return {
        "talk": wer(by, man, lambda s: s["set"] == "talk", horizon),
        "summre": wer(by, man, lambda s: s["set"] == "summre", horizon),
        "piper_fr": wer(
            by, man, lambda s: s["set"] == "piper" and s["lang"] == "fr", horizon
        ),
        "piper_en": wer(
            by, man, lambda s: s["set"] == "piper" and s["lang"] == "en", horizon
        ),
        "fr_all": wer(by, man, lambda s: s["lang"] == "fr", horizon),
    }


def ac19(out: Path, man: dict) -> dict:
    a = json.loads((out / "alice.json").read_text())
    b = json.loads((out / "bob.json").read_text())
    lines = voice_lines(out / "server.db")
    res: dict = {
        "errors": [a.get("error"), b.get("error")],
        "live_end_s": round(a["liveEnd"], 1),
    }
    lat = [x for x in a["rec"]["lat"] if x[1] is not None]
    probes = a["rec"]["probes"]
    res["counts"] = {
        "latencies": len(lat),
        "probes": len(probes),
        **{
            k: a["pipeline"].get(k)
            for k in ("utterances", "decoded", "posted", "dropped", "lost")
        },
    }
    # FIFO: the n-th utterance sent to the engine is the n-th latency reported, and is sent before it.
    paired = list(zip(lat, probes, strict=True))
    late = [p["id"] for x, p in paired if p["t"] > x[0]]
    if late:
        raise ValueError(f"probes after their latency, the pairing is off: {late}")
    pause = [x[1] / 1000 for x, p in paired if p["n"] < MAXLEN_SAMPLES]
    res["latency_s"] = {
        "all": q([x[1] / 1000 for x in lat]),
        "pause_cut": q(pause),
        "maxlen_cut": q([x[1] / 1000 for x, p in paired if p["n"] >= MAXLEN_SAMPLES]),
    }
    slices: dict[int, list[float]] = {}
    for t, ms, *_ in lat:
        slices.setdefault(int((t - a["t0"]) / 300_000), []).append(ms / 1000)
    res["latency_s"]["per_5_min"] = {
        f"{k * 5}-{k * 5 + 5} min": q(v) for k, v in sorted(slices.items())
    }
    res["modes"] = a["rec"]["modes"]
    res["mode_end"] = a["pipeline"].get("mode")
    for who, d, src in (("device", a, "device"), ("server", b, "server")):
        mine = [
            ln
            for ln in lines
            if ln["sender"] == ("Alice" if who == "device" else "Bob")
        ]
        by, lag = assign(mine, d["t0"], man)
        res[who] = {
            "lines": len(mine),
            "other_source": sum(ln["source"] != src for ln in mine),
            "lag_s": lag,
            **scores(by, man, a["liveEnd"]),
        }
    mem = a["mem"]
    # Peak of the largest renderer (the tab, its workers included) per phase, between the driver's marks.
    marks = a["marks"] + [["live", a["t0"]], ["after", a["t0"] + a["liveEnd"] * 1000]]
    marks.sort(key=lambda m: m[1])
    res["memory_mb"] = {"chrome_total_peak": round(max(r["total"] for r in mem))}
    for (name, t1), (_, t2) in pairwise(marks):
        rows = [r["renderer_max"] for r in mem if t1 <= r["t"] < t2]
        if rows:
            res["memory_mb"][f"tab_peak_{name}"] = round(max(rows))
    live = [
        r["renderer_max"]
        for r in mem
        if a["t0"] <= r["t"] <= a["t0"] + a["liveEnd"] * 1000
    ]
    res["memory_mb"]["tab_median_live"] = (
        round(statistics.median(live)) if live else None
    )
    play = [r for r in mem if a["t0"] <= r["t"] <= a["t0"] + a["liveEnd"] * 1000]
    res["cpu_cores_mean_playing"] = (
        round(statistics.mean(r["cpu"] for r in play[1:]), 2) if len(play) > 1 else None
    )
    res["cold"] = {
        "sec": a["cold"]["sec"],
        "self_test": {k: a["cold"]["selfTest"][k] for k in ("backend", "rtfx", "wer")},
        "load_ms": a["cold"]["load"],
    }
    res["warm_s"] = [round(w["sec"], 2) for w in a["warm"]]
    res["warm_load_ms"] = [w["load"] for w in a["warm"]]
    return res


def ac18(out: Path) -> dict:
    s = json.loads((out / "ac18-setup.json").read_text())
    c = json.loads((out / "ac18-collect.json").read_text())
    times = json.loads((out / "ac18-times.json").read_text())
    rec = c["rec"]
    res: dict = {
        "errors": [s.get("error"), c.get("error")],
        "status": s["status"],
        "deafened": s["deaf"],
        "mic_muted": s["micMuted"],
        "self_test": {k: s["selfTest"][k] for k in ("backend", "rtfx", "wer")},
        "load_ms": s["load"],
    }
    hidden = [t for t, v in rec["vis"] if v == "hidden"]
    shown = [t for t, v in rec["vis"] if v == "visible" and hidden and t > hidden[0]]
    h0, h1 = hidden[0] if hidden else None, shown[0] if shown else None
    res["visibility"] = [
        [round((t - times["hide"]) / 1000, 2), v] for t, v in rec["vis"]
    ]
    res["hidden_s"] = round((h1 - h0) / 1000, 1) if h0 and h1 else None
    if h0 and h1:
        ticks = [t for t in rec["ticks"] if h0 <= t <= h1]
        gaps = [(b - a) / 1000 for a, b in pairwise(ticks)]
        res["main_thread_1s_timer_while_hidden"] = {
            "ticks": len(ticks),
            "max_gap_s": round(max(gaps), 1) if gaps else None,
            "ticks_last_5_min": sum(t > h1 - 300_000 for t in ticks),
        }
    res["pipeline_end"] = {
        k: c["pipeline"].get(k)
        for k in (
            "utterances",
            "decoded",
            "posted",
            "dropped",
            "lost",
            "beats",
            "ticks",
            "mode",
        )
    }
    res["modes"] = [
        [round((t - times["hide"]) / 1000, 1), m, e] for t, m, e in rec["modes"]
    ]
    lat = [(t, ms) for t, ms, *_ in rec["lat"] if ms is not None]
    res["device_latency_s"] = {
        "hidden": q([ms / 1000 for t, ms in lat if h0 and h1 and h0 <= t <= h1]),
        "all": q([ms / 1000 for _, ms in lat]),
    }
    lines = [ln for ln in voice_lines(out / "server.db") if ln["sender"] == "Alice"]
    sched = s["schedule"]
    per = []
    for i, x in enumerate(sched):
        # Sentences are EVERY_S apart, far above any latency here: a line belongs to the sentence it follows.
        nxt = sched[i + 1]["start"] if i + 1 < len(sched) else x["end"] + 20_000
        got = [ln for ln in lines if x["start"] <= ln["t"] < nxt]
        when = "hidden" if h0 and h1 and h0 <= x["end"] <= h1 else "visible"
        per.append(
            {
                "item": x["item"],
                "when": when,
                "sources": [ln["source"] for ln in got],
                "line_after_end_s": round((got[-1]["t"] - x["end"]) / 1000, 2)
                if got
                else None,
                "wer": round(
                    edits(words(x["ref"]), words(" ".join(ln["body"] for ln in got)))
                    / len(words(x["ref"])),
                    2,
                ),
            }
        )
    res["sentences"] = {
        "total": len(per),
        "hidden": sum(p["when"] == "hidden" for p in per),
        "missed": [p["item"] for p in per if not p["sources"]],
        "server_lines": sum("server" in p["sources"] for p in per),
    }
    res["end_to_line_s"] = {
        w: q(
            [
                p["line_after_end_s"]
                for p in per
                if p["when"] == w and p["line_after_end_s"] is not None
            ]
        )
        for w in ("hidden", "visible")
    }
    fell = [m for m in res["modes"] if m[1] != "device"]
    p95 = res["end_to_line_s"]["hidden"].get("p95")
    if fell:
        res["verdict"] = "fallback: " + (
            "every sentence after it has a line"
            if not res["sentences"]["missed"]
            else "sentences missed"
        )
    else:
        res["verdict"] = (
            f"no fallback, hidden p95 {'<=' if p95 is not None and p95 <= P95_LIMIT_S else '>'} {P95_LIMIT_S} s"
        )
    res["per_sentence"] = per
    return res


def main() -> None:
    kind, out, data = sys.argv[1], Path(sys.argv[2]), Path(sys.argv[3])
    man = json.loads((data / "manifest.json").read_text())
    print(
        json.dumps(
            ac19(out, man) if kind == "ac19" else ac18(out),
            ensure_ascii=False,
            indent=1,
        )
    )


if __name__ == "__main__":
    main()
