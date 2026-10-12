"""The voice license gate (004-AC18): a Piper voice runs only if VOICES.md lists it under an open license."""

import re
from pathlib import Path

VOICES_MD = Path(__file__).resolve().parent.parent / "VOICES.md"

# CC0, CC-BY (any version) and public domain. CC-BY-SA, -NC and -ND are not CC-BY: they add terms.
ALLOWED = re.compile(r"^(cc0(-1\.0)?|cc-by(-\d\.\d)?|public domain)$", re.IGNORECASE)
# A table row of VOICES.md: | `voice` | license | ...
ROW = re.compile(r"^\|\s*`([^`]+)`\s*\|\s*([^|]+?)\s*\|")


def listed(text: str) -> dict[str, str]:
    """Voice name to license, from the table rows of a VOICES.md text."""
    return {m[1]: m[2] for line in text.splitlines() if (m := ROW.match(line))}


def refused(voices: set[str], table: dict[str, str]) -> list[str]:
    """Why each voice that may not run is refused; empty when all may."""
    out = []
    for v in sorted(voices):
        lic = table.get(v)
        if lic is None:
            out.append(f"{v}: not listed in VOICES.md")
        elif not ALLOWED.match(lic):
            out.append(f"{v}: license {lic!r} is not CC0, CC-BY or public domain")
    return out
