from pathlib import Path

import pytest

from grill_scenarios import cli, schema, voices

TABLE = """# Voices

| Voice | License | Dataset |
|---|---|---|
| `cc0-voice` | CC0 | x |
| `by-voice` | CC-BY-4.0 | x |
| `by3-voice` | CC-BY-3.0 | x |
| `pd-voice` | public domain | x |
| `pd-caps` | Public Domain | x |
| `sa-voice` | CC-BY-SA-4.0 | x |
| `nc-voice` | CC-BY-NC-4.0 | x |
| `nd-voice` | CC-BY-ND-4.0 | x |
| `mit-voice` | MIT | x |
| `custom` | https://example.org/license.html | x |

A voice named in prose, `prose-voice`, is not in the table.
"""


# 004-AC18: only CC0, CC-BY and public-domain voices listed in VOICES.md run.
@pytest.mark.parametrize("voice", ["cc0-voice", "by-voice", "by3-voice", "pd-voice", "pd-caps"])
def test_allowed(voice: str) -> None:
    assert voices.refused({voice}, voices.listed(TABLE)) == []


@pytest.mark.parametrize(
    ("voice", "why"),
    [
        ("sa-voice", "not CC0, CC-BY or public domain"),
        ("nc-voice", "not CC0, CC-BY or public domain"),
        ("nd-voice", "not CC0, CC-BY or public domain"),
        ("mit-voice", "not CC0, CC-BY or public domain"),
        ("custom", "not CC0, CC-BY or public domain"),
        ("prose-voice", "not listed"),
        ("unknown", "not listed"),
    ],
)
def test_refused(voice: str, why: str) -> None:
    (msg,) = voices.refused({voice}, voices.listed(TABLE))
    assert msg.startswith(voice) and why in msg


# 004-AC18: the repo's list allows every voice its scenarios use.
def test_repo_list_covers_scenarios() -> None:
    table = voices.listed(voices.VOICES_MD.read_text())
    used = set().union(*(schema.load(p).voices() for p in voices.VOICES_MD.parent.glob("*.yaml")))
    assert used and voices.refused(used, table) == []
    assert all(voices.ALLOWED.match(lic) for lic in table.values())


# 004-AC18: the CLI refuses to run a scenario with a voice off the list, before any work.
def test_cli_refuses(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    src = (voices.VOICES_MD.parent / "host-handover.yaml").read_text()
    f = tmp_path / "s.yaml"
    f.write_text(src.replace("en_US-joe-medium", "en_US-lessac-medium"))
    assert cli.main([str(f), "--url", "https://x.invalid", "--yes", "--out", str(tmp_path / "out")]) == 2
    assert "en_US-lessac-medium: not listed in VOICES.md" in capsys.readouterr().err
    assert not (tmp_path / "out").exists()
