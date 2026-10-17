#!/usr/bin/env python3
"""Build step: fetches every file the pod loads, each by repository, revision and sha256 (013-AC10).

The list comes from Pocket's own configs and the voice catalogue, so it names exactly what Pocket
asks Hugging Face for at run time; models.lock.json pins each file's sha256, and a file missing
from it or with another hash fails the build. The read token, needed for the gated weights,
comes from a file (a BuildKit secret) and is passed to each download, never written anywhere.

    fetch.py <token file>          fetch and check, into $HF_HOME
    fetch.py <token file> --pin    fetch, then rewrite models.lock.json (when a pin moves)
"""

import hashlib
import json
import sys
from pathlib import Path

from huggingface_hub import hf_hub_download

from .engine import POCKET_LANG, catalogue

LOCK = Path(__file__).with_name("models.lock.json")


def split(uri: str) -> tuple[str, str, str]:
    """hf://<owner>/<repo>/<path>@<revision> as (repo, path, revision); a pin always has a revision."""
    body, _, rev = uri.removeprefix("hf://").rpartition("@")
    if not rev or not body:
        raise ValueError(f"{uri}: no revision")
    owner, repo, path = body.split("/", 2)
    return f"{owner}/{repo}", path, rev


def wanted() -> list[tuple[str, str, str]]:
    """Every file the pod loads: weights and tokenizer per language, voice states and sources."""
    import pocket_tts
    from pocket_tts.utils.config import load_config
    from pocket_tts.utils.utils import get_predefined_voice

    if pocket_tts.__file__ is None:
        raise RuntimeError("pocket_tts is a namespace package here: its configs cannot be found")
    configs = Path(pocket_tts.__file__).parent / "config"
    uris: list[str] = []
    for name in POCKET_LANG.values():
        c = load_config(configs / f"{name}.yaml")
        uris += [c.weights_path, c.flow_lm.lookup_table.tokenizer_path]
        for v in catalogue():
            if v.get("pocket"):
                uris.append(get_predefined_voice(language=name, name=v["pocket"]))
    for v in catalogue():
        if not v.get("pocket"):
            uris.append("hf://" + v["source"])
    out = []
    for u in dict.fromkeys(uris):
        out.append(split(u))
    return out


def key(repo: str, path: str, rev: str) -> str:
    return f"{repo}/{path}@{rev}"


def sha256(p: Path) -> str:
    h = hashlib.sha256()
    with p.open("rb") as f:
        while chunk := f.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def main() -> None:
    if len(sys.argv) not in (2, 3) or sys.argv[2:] not in ([], ["--pin"]):
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    token = Path(sys.argv[1]).read_text().strip() or None
    pin = sys.argv[2:] == ["--pin"]
    lock: dict[str, str] = {} if pin else json.loads(LOCK.read_text())
    bad = []
    for repo, path, rev in wanted():
        k = key(repo, path, rev)
        if not pin and k not in lock:
            bad.append(f"{k}: not pinned in {LOCK.name}")
            continue
        got = sha256(Path(hf_hub_download(repo_id=repo, filename=path, revision=rev, token=token)))
        if pin:
            lock[k] = got
        elif got != lock[k]:
            bad.append(f"{k}: sha256 {got}, pinned {lock[k]}")
        print(f"fetched {k}", flush=True)
    if bad:
        print("\n".join(bad), file=sys.stderr)
        sys.exit(1)
    if pin:
        LOCK.write_text(json.dumps(dict(sorted(lock.items())), indent=2) + "\n")


if __name__ == "__main__":
    main()
