"""The run's report in Markdown, from check()'s result."""

from typing import Any


def markdown(name: str, url: str, estimate: int, budget: int, r: dict[str, Any]) -> str:
    out = [
        f"# Scenario: {name}",
        "",
        f"- Verdict: **{'pass' if r['ok'] else 'FAIL'}**",
        f"- Instance: {url}",
        f"- Model: {r['model']}",
        f"- Claude requests: {r['requests']} (estimate {estimate}, budget {budget})",
        "",
        "## Checks",
        "",
        "| check | result | detail |",
        "|---|---|---|",
    ]
    out += [f"| {c['check']} | {'ok' if c['ok'] else 'FAIL'} | {_cell(c['detail'])} |" for c in r["checks"]]
    v = r["voice"]
    if v:
        lat = v["latency_s"] or {}
        out += [
            "",
            "## Voice",
            "",
            f"WER {v['wer']}, attribution {v['right']}/{v['right'] + v['wrong']} "
            f"({v['unmatched']} unmatched), delivered {v['delivered']}, "
            f"{v['voice_lines']} voice lines for {v['script_lines']} script lines.",
            "Latency min / median / p95 / max: "
            + " / ".join(f"{lat.get(k, '-')}" for k in ("min", "median", "p95", "max"))
            + " s.",
            "",
            "| r | speaker | lang | line | lines | WER | latency s |",
            "|---|---|---|---|---|---|---|",
        ]
        out += [
            f"| {x['round']} | {x['by']} | {x['lang']} | {_cell(x['text'][:50])} | {x['lines']} | {x['wer']} "
            f"| {x['latency_s'] if x['latency_s'] is not None else '-'} |"
            for x in v["per_line"]
        ]
    out += [
        "",
        "## Agent turns",
        "",
        "| persona | n | daemon | model | effort | cause | resume | turn s | press to synthesis s "
        "| context % | fetched |",
        "|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for persona, ms in r["syntheses"].items():
        for i, m in enumerate(ms, 1):
            t = m.get("turn") or {}
            window = t.get("context_window") or 0
            ctx = round(100 * t.get("context_tokens", 0) / window, 1) if window else "-"
            out.append(
                f"| {persona} | {i} | {t.get('daemon', '-')} | {t.get('model', m.get('model'))} | "
                f"{t.get('effort') or '-'} | {_cell(t.get('cause') or '-')} | {t.get('resume', '-')} | "
                f"{round(t['duration_ms'] / 1000, 1) if 'duration_ms' in t else '-'} | "
                f"{m.get('press_to_synthesis_s', '-')} | {ctx} | "
                f"{t.get('fetched_files', '-')} files, {t.get('fetched_bytes', '-')} B |"
            )
    out += ["", f"Chain counts seen: {r['chain_seen']}", "", "## Syntheses", ""]
    for persona, ms in r["syntheses"].items():
        for i, m in enumerate(ms, 1):
            out += [f"### {persona} {i}", "", "> " + (m["body"] or "").strip().replace("\n", "\n> "), ""]
    return "\n".join(out).rstrip() + "\n"


def _cell(v: Any) -> str:
    return str(v).replace("|", "\\|").replace("\n", " ")
