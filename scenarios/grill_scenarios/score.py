"""Scores a run: STT accuracy, attribution and latency, agent turns, and the scenario's assertions."""

import re
import statistics
from collections import Counter
from typing import Any

from .schema import Scenario

# Below this share of its words found in a script line, a transcript line counts as made up.
MIN_MATCH = 0.3


def words(text: str) -> list[str]:
    return re.findall(r"\w+", text.lower().replace("’", "'").replace("-", " "))


def edits(ref: list[str], hyp: list[str]) -> int:
    """Word-level Levenshtein distance."""
    row = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        prev, row[0] = row[0], i
        for j, h in enumerate(hyp, 1):
            prev, row[j] = row[j], min(row[j] + 1, row[j - 1] + 1, prev + (r != h))
    return row[-1]


def match(line: list[str], ref: list[str]) -> float:
    return len(set(line) & set(ref)) / max(len(set(line)), 1)


def pct(v: list[float], q: float) -> float:
    s = sorted(v)
    return s[min(len(s) - 1, round(q * (len(s) - 1)))]


def voice(rounds: list[dict[str, Any]], res: dict[str, Any]) -> dict[str, Any]:
    """Each transcript line against the script: WER per script line, attribution, latency from the
    end of speech to the line, with the instant each fake mic opened as the round's time zero."""
    refs = [
        {**ln, "round": r["round"], "words": words(ln["text"]), "got": []}
        for r in rounds
        for ln in r["lines"]
    ]
    lines = [m for m in res.get("messages", []) if m.get("kind") == "voice"]
    if not refs:
        return {}
    right = wrong = unmatched = 0
    for m in lines:
        w = words(m["body"] or "")
        best = max(refs, key=lambda r: match(w, r["words"]))
        if match(w, best["words"]) < MIN_MATCH:
            unmatched += 1
        elif best["by"] == m["sender"]:
            right += 1
            best["got"].append(m)
        else:
            wrong += 1
    mics = {r["round"]: r["micAt"] for r in res.get("rounds", [])}
    errs = n = 0
    lat: list[float] = []
    per_line = []
    for r in refs:
        hyp = [w for m in sorted(r["got"], key=lambda m: m["ts"]) for w in words(m["body"])]
        e = edits(r["words"], hyp)
        errs, n = errs + e, n + len(r["words"])
        latency = None
        mic = mics.get(r["round"], {}).get(r["by"])
        if r["got"] and mic:
            latency = round((max(m["ts"] for m in r["got"]) - mic) / 1000 - r["end"], 2)
            lat.append(latency)
        per_line.append(
            {
                "round": r["round"],
                "by": r["by"],
                "lang": r["lang"],
                "text": r["text"],
                "lines": len(r["got"]),
                "wer": round(e / len(r["words"]), 3),
                "latency_s": latency,
            }
        )
    matched = right + wrong
    return {
        "wer": round(errs / n, 3),
        "attribution": round(right / matched, 3) if matched else 0.0,
        "right": right,
        "wrong": wrong,
        "unmatched": unmatched,
        "delivered": round(sum(1 for r in refs if r["got"]) / len(refs), 3),
        "script_lines": len(refs),
        "voice_lines": len(lines),
        "latency_s": {
            "min": min(lat),
            "median": statistics.median(lat),
            "p95": pct(lat, 0.95),
            "max": max(lat),
        }
        if lat
        else None,
        "per_line": per_line,
    }


def syntheses(res: dict[str, Any]) -> dict[str, list[dict[str, Any]]]:
    """Each persona's syntheses in order, joined with the turn that wrote it and how long it took
    from the press that asked for it, if one did."""
    turns = sorted((t for t in res.get("turns", []) if t.get("status") == "ok"), key=lambda t: t["turn"])
    by_persona: dict[str, list[dict[str, Any]]] = {}
    for m in res.get("messages", []):
        if m.get("kind") == "agent":
            by_persona.setdefault(m["persona"], []).append({**m})
    for persona, ms in by_persona.items():
        ts = [t for t in turns if t["persona"] == persona]
        for m, t in zip(ms, ts, strict=False):
            m["turn"] = t
    # A press records how many syntheses its persona had: the next one answers it.
    for p in res.get("presses", []):
        ms = by_persona.get(p["persona"], [])
        seen = [s["at"] for s in res.get("agentSeen", []) if s["persona"] == p["persona"]]
        if p["index"] < min(len(ms), len(seen)):
            ms[p["index"]]["press_to_synthesis_s"] = round((seen[p["index"]] - p["at"]) / 1000, 1)
    return by_persona


def check(s: Scenario, rounds: list[dict[str, Any]], res: dict[str, Any]) -> dict[str, Any]:
    """The run's figures and every assertion of the scenario, each with its verdict."""
    a = s.assertions
    checks: list[dict[str, Any]] = []

    def ok(name: str, passed: bool, detail: str) -> None:
        checks.append({"check": name, "ok": bool(passed), "detail": detail})

    if res.get("error"):
        ok("run completed", False, res["error"])
    budget = s.budget.max_requests
    ok("requests within budget", res.get("requests", 0) <= budget, f"{res.get('requests')} of {budget}")

    v = voice(rounds, res)
    if a.voice and v:
        ok("voice WER", v["wer"] <= a.voice.max_wer, f"{v['wer']} (max {a.voice.max_wer})")
        ok(
            "voice attribution",
            v["attribution"] >= a.voice.min_attribution,
            f"{v['right']}/{v['right'] + v['wrong']} right, {v['unmatched']} unmatched",
        )
        ok("voice delivered", v["delivered"] >= a.voice.min_delivered, f"{v['delivered']} of script lines")
        worst = v["latency_s"]["max"] if v["latency_s"] else None
        ok(
            "voice latency",
            worst is not None and worst <= a.voice.max_latency_s,
            f"max {worst} s (bound {a.voice.max_latency_s})",
        )

    ran = Counter(t["persona"] for t in res.get("turns", []) if t.get("status") == "ok")
    for persona, n in a.turns.items():
        ok(f"{persona} ran {n} turns", ran[persona] == n, f"ran {ran[persona]}")
    failed = [t for t in res.get("turns", []) if t.get("status") != "ok"]
    ok(
        "no failed turn",
        not failed,
        ", ".join(f"turn {t['turn']}: {t.get('error')}" for t in failed) or "none",
    )

    syn = syntheses(res)
    for c in a.syntheses:
        label = f"{c.persona} synthesis {c.nth}"
        got = syn.get(c.persona, [])
        if len(got) < c.nth:
            ok(label, False, f"only {len(got)} syntheses")
            continue
        m = got[c.nth - 1]
        body, turn = m["body"] or "", m.get("turn") or {}
        for p in c.contains:
            ok(f"{label} contains /{p}/", re.search(p, body, re.IGNORECASE) is not None, body[:120])
        for p in c.not_contains:
            hit = re.search(p, body, re.IGNORECASE)
            ok(f"{label} lacks /{p}/", hit is None, hit.group(0) if hit else "absent")
        if c.resumed is not None:
            ok(
                f"{label} resumed={c.resumed}",
                turn.get("resume") == c.resumed,
                f"resume {turn.get('resume')}",
            )
        if c.ran_on:
            ok(
                f"{label} ran on {c.ran_on}'s daemon",
                turn.get("daemon") == c.ran_on,
                f"ran on {turn.get('daemon')}",
            )
        if c.cause:
            ok(
                f"{label} cause '{c.cause}'",
                c.cause in (turn.get("cause") or ""),
                f"cause {turn.get('cause')!r}",
            )

    chain = res.get("chainSeen", [])
    if a.chain:
        top = max(chain, default=0)
        ok("chain count", a.chain.min <= top <= a.chain.max, f"highest {top}, seen {chain}")
    if a.max_turn_s is not None:
        times = [m["press_to_synthesis_s"] for ms in syn.values() for m in ms if "press_to_synthesis_s" in m]
        ok(
            "press to synthesis",
            bool(times) and max(times) <= a.max_turn_s,
            f"{times} s (bound {a.max_turn_s})",
        )
    done = sorted((t for t in res.get("turns", []) if t.get("status") == "ok"), key=lambda t: t["turn"])
    if a.min_context_pct is not None:
        last = done[-1] if done else {}
        window = last.get("context_window") or 0
        used = round(100 * last.get("context_tokens", 0) / window, 1) if window else 0.0
        ok("context use", used >= a.min_context_pct, f"{used}% of {window} tokens (min {a.min_context_pct}%)")
    if a.max_fetched_files_after_first is not None:
        later = [t.get("fetched_files", 0) for t in done[1:]]
        ok(
            "workspace fetched after the first turn",
            bool(later) and max(later) <= a.max_fetched_files_after_first,
            f"files per turn {[t.get('fetched_files') for t in done]} "
            f"(max {a.max_fetched_files_after_first} after the first)",
        )

    return {
        "ok": all(c["ok"] for c in checks),
        "checks": checks,
        "voice": v,
        "syntheses": syn,
        "chain_seen": chain,
        "requests": res.get("requests"),
        "model": res.get("model"),
    }
