#!/usr/bin/env python3
"""Score an S5 run against session.json: STT WER, attribution and latency, agent turns, doc diffs.

Usage: uv run ./analyze.py <result.json written by s5.spec.js>
"""

import difflib
import json
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

SESSION = Path(__file__).with_name("session.json")
# Below this share of its words found in a script line, a transcript line counts as made up.
MIN_MATCH = 0.3


def words(text: str) -> list[str]:
    return re.findall(r"[\w]+", text.lower().replace("’", "'").replace("-", " "))


def edits(ref: list[str], hyp: list[str]) -> int:
    row = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        prev, row[0] = row[0], i
        for j, h in enumerate(hyp, 1):
            prev, row[j] = row[j], min(row[j] + 1, row[j - 1] + 1, prev + (r != h))
    return row[-1]


def match(line: list[str], ref: list[str]) -> float:
    return len(set(line) & set(ref)) / max(len(set(line)), 1)


def word_diff(a: str, b: str) -> tuple[int, int]:
    """Words added and removed going from a to b."""
    sm = difflib.SequenceMatcher(a=words(a), b=words(b), autojunk=False)
    ops = [(i2 - i1, j2 - j1) for tag, i1, i2, j1, j2 in sm.get_opcodes() if tag != "equal"]
    return sum(o[1] for o in ops), sum(o[0] for o in ops)


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    session = json.loads(SESSION.read_text())
    res: dict[str, Any] = json.loads(Path(sys.argv[1]).read_text())
    refs = [
        {**line, "round": n, "words": words(line["text"]), "lines": []}
        for n, rnd in enumerate(session["rounds"], 1)
        for line in rnd["lines"]
    ]
    voice = [m for m in res["messages"] if m.get("kind") == "voice"]

    print("## STT lines\n")
    print("| speaker | line | best script line | match | attribution |\n|---|---|---|---|---|")
    attributed = unmatched = 0
    for m in voice:
        w = words(m["body"])
        best = max(refs, key=lambda r: match(w, r["words"]))
        score = match(w, best["words"])
        # A line counts toward a script line's WER and latency only under the right speaker.
        if score >= MIN_MATCH and best["speaker"] == m["sender"]:
            best["lines"].append(m)
        if score < MIN_MATCH:
            unmatched += 1
            verdict = "no match"
        else:
            ok = best["speaker"] == m["sender"]
            attributed += ok
            verdict = "right" if ok else f"WRONG ({best['speaker']})"
        print(f"| {m['sender']} | {m['body'][:70]} | {best['text'][:40]} | {score:.2f} | {verdict} |")
    matched = len(voice) - unmatched
    print(f"\nAttribution: {attributed}/{matched} matched lines right, {unmatched} unmatched.\n")

    print("## WER and latency per script line\n")
    print("| r | speaker | lang | ref | lines | WER | latency s |\n|---|---|---|---|---|---|---|")
    agg: dict[str, list[int]] = defaultdict(lambda: [0, 0])
    lat: dict[str, list[float]] = defaultdict(list)
    for r in refs:
        hyp = [w for m in sorted(r["lines"], key=lambda m: m["ts"]) for w in words(m["body"])]
        e = edits(r["words"], hyp)
        for key in (r["speaker"], r["lang"], "all"):
            agg[key][0] += e
            agg[key][1] += len(r["words"])
            if r["lines"]:
                agg[key + " (delivered)"][0] += e
                agg[key + " (delivered)"][1] += len(r["words"])
        latency = ""
        if r["lines"]:
            t0 = res["rounds"][r["round"] - 1]["micAt"][r["speaker"]]
            s = (max(m["ts"] for m in r["lines"]) - t0) / 1000 - r["end"]
            lat[r["speaker"]].append(s)
            lat["all"].append(s)
            latency = f"{s:.1f}"
        flag = " (overlap)" if r.get("overlap") else ""
        print(
            f"| {r['round']} | {r['speaker']} | {r['lang']} | {r['text'][:45]}{flag} | {len(r['lines'])} "
            f"| {e / len(r['words']):.2f} | {latency} |"
        )
    delivered = sum(1 for r in refs if r["lines"])
    print(f"\nScript lines with a transcript: {delivered}/{len(refs)}. A missing line counts as WER 1.")
    print(f"Voice lines per script line: {len(voice) / len(refs):.2f} ({len(voice)} for {len(refs)}).")
    print("\n| group | WER | words |\n|---|---|---|")
    for key, (e, n) in sorted(agg.items()):
        print(f"| {key} | {e / n:.2f} | {n} |")
    print("\n| speaker | latency min / median / p95 / max s |\n|---|---|")
    for key, v in lat.items():
        v = sorted(v)
        p95 = v[min(len(v) - 1, round(0.95 * (len(v) - 1)))]
        print(f"| {key} | {v[0]:.1f} / {v[len(v) // 2]:.1f} / {p95:.1f} / {v[-1]:.1f} |")

    print("\n## Agent turns\n")
    for i, t in enumerate(res.get("turns", []), 1):
        turn = t["turn"]
        print(
            f"Turn {i}: press to synthesis {t['synthesisMs'] / 1000:.0f} s, status {turn['status']}, "
            f"resume {t['resume']}, report {json.dumps(turn['report'])}, requests so far {t['requestsSoFar']}"
        )
        print(f"> {t['synthesis'].strip()}\n")
    models = [m.get("model") for m in res["messages"] if m.get("kind") == "agent"]
    print(
        f"Real Claude requests: {res.get('requests')} ({res.get('model')}); "
        f"synthesis models shown: {models}\n"
    )
    print("## Threads after the agent turns\n")
    for a in res.get("threadsAfterTurns", []):
        print(f"- on {a['doc']}, opened by {a['opener']}, {a['status']}: {' > '.join(a['messages'])}")
    print()

    print("## Documents\n")
    docs = res.get("documents", [])
    plan = next((d for d in docs if d["name"] == "plan.md"), None)
    for d in docs:
        prev = plan["texts"][0]["text"] if plan and d is not plan else ""
        for rev in d["texts"]:
            added, removed = word_diff(prev, rev["text"])
            base = f"rev {rev['n'] - 1}" if rev["n"] > 1 else "plan.md" if prev else "nothing"
            print(f"- {d['name']} rev {rev['n']} by {rev['author']}: +{added} / -{removed} words vs {base}")
            prev = rev["text"]
        for a in d.get("annotations") or []:
            for m in a["messages"]:
                print(f"  - annotation {a['id']} ({a['status']}) {m['author']}: {m['body'].strip()}")


if __name__ == "__main__":
    main()
