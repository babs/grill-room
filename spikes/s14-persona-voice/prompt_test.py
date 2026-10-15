#!/usr/bin/env python3
"""Spike S14: asks haiku for spoken.md on fixed turns and checks the result against the spoken rules.

One `claude -p` request per turn. The persona text goes in as the appended system prompt, as the
daemon does; no tool, no MCP, no session kept, only project settings (none in the cwd).
"""

import argparse
import json
import re
import subprocess
from pathlib import Path

HERE = Path(__file__).resolve().parent / "prompt"

SLOP = [
    r"great question",
    r"happy to",
    r"\bdelve",
    r"in summary",
    r"to sum up",
    r"en résumé",
    r"pour résumer",
    r"n'hésitez pas",
    r"feel free",
    r"let me know",
    r"i hope",
    r"j'espère que",
    r"bonjour",
    r"hello",
    r"\bmerci\b",
    r"thank",
    r"it's worth noting",
    r"il est important de",
    r"crucial",
    r"essentiel",
]
# Markdown marks, links, paths, ids and emoji that a synthesizer would read out or choke on.
UNSPEAKABLE = {
    "markdown": r"[*_`#>|\[\]]|^\s*[-+] |^\s*\d+\. ",
    "url": r"https?://|www\.",
    "path": r"\b[\w-]+/[\w./-]+|\.md\b|\.json\b",
    "id": r"·|#\d+|\bnote \d+|\bnote n°",
    "emoji": "[\U0001f300-\U0001faff☀-➿]",
    "section_sign": "§",
    "colon_dash_bracket": r"\s:\s|:\s|\s[–—-]\s|[()]",
    "quotes": r"[\"«»“”]",
}


def render(turn: dict, addition: str) -> str:
    parts = [
        f'You are {turn["persona"]}, an agent of the grill session "{turn["room"]}".',
        "## Said since your last turn\n\n" + "\n".join(turn["said"]),
        "## Annotation threads with new messages\n\n" + "\n".join(turn["threads"]) if turn["threads"] else "",
        "## What you did this turn\n",
        "Document edits:\n" + ("\n".join(f"- {d}" for d in turn["doc_edits"]) or "- none"),
        "replies.json:\n" + json.dumps(turn["replies"], ensure_ascii=False),
        "annotations-new.json:\n" + json.dumps(turn["annotations_new"], ensure_ascii=False),
        "Your synthesis, as posted in the room:\n<<<\n" + turn["synthesis"] + "\n>>>",
        "## One more output\n\n" + addition.strip(),
        "Files cannot be written in this check: reply with the exact content of spoken.md and nothing "
        "else, or NONE if you would leave it out.",
    ]
    return "\n\n".join(p for p in parts if p)


def check(text: str) -> dict:
    words = len(re.findall(r"\w+(?:'\w+)?", text))
    found = {k: re.findall(p, text, re.M) for k, p in UNSPEAKABLE.items()}
    return {
        "words": words,
        "over_60": words > 60,
        "slop": [p for p in SLOP if re.search(p, text, re.I)],
        "unspeakable": {k: v for k, v in found.items() if v},
        "questions": text.count("?"),
        "longest_sentence": max((len(x.split()) for x in re.split(r"[.?!]+", text) if x.strip()), default=0),
        "first_person": bool(re.search(r"\b(I|I've|I'd|my|j'|je|mon|ma|mes)\b", text, re.I)),
    }


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("prompt", help="the spoken.md addition, under prompt/")
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--only", help="turn id")
    a = p.parse_args()
    addition = (HERE / a.prompt).read_text()
    turns = json.loads((HERE / "turns.json").read_text())
    scratch = a.out.parent
    scratch.mkdir(parents=True, exist_ok=True)
    with a.out.open("a") as out:
        for t in turns:
            if a.only and t["id"] != a.only:
                continue
            persona = scratch / f"{t['id']}.persona.md"
            persona.write_text(t["persona_text"] + "\n")
            cmd = [
                "claude",
                "-p",
                "--model",
                "haiku",
                "--output-format",
                "json",
                "--tools",
                "",
                "--strict-mcp-config",
                "--no-session-persistence",
                "--setting-sources",
                "project",
                "--append-system-prompt-file",
                str(persona),
                "--debug-file",
                str(scratch / f"{t['id']}.{a.prompt}.debug.log"),
            ]
            r = subprocess.run(cmd, input=render(t, addition), capture_output=True, text=True, cwd=scratch, timeout=300)
            res = json.loads(r.stdout)
            text = res.get("result", "").strip()
            row = {
                "prompt": a.prompt,
                "turn": t["id"],
                "model": list(res.get("modelUsage", {})),
                "text": text,
                "check": check(text),
            }
            out.write(json.dumps(row, ensure_ascii=False) + "\n")
            print(json.dumps(row, ensure_ascii=False, indent=1))


if __name__ == "__main__":
    main()
