from typing import Any

from grill_tts.lexcheck import RENDERS, heard, langs, score, track_id
from grill_tts.textlayer import load_seed


# 013-AC33: the accepted spellings are the term, its spaced form, and numbers as digits or words;
# never a longer word holding it.
def test_heard() -> None:
    assert heard("GitHub", "fr", "Je pense que git hub pose problème ici.")
    assert heard("GitHub", "fr", "je pense que GitHub pose problème")
    assert heard("AWS", "en", "I think a w s is the problem")
    assert heard("k8s", "fr", "je pense que k huit s pose problème")
    assert heard("k8s", "en", "I think K8S is the problem")
    assert heard("CI/CD", "fr", "je pense que CI CD pose problème")
    assert heard("end-to-end", "en", "I think end to end is the problem")
    assert not heard("pod", "fr", "je pense que podcast pose problème")
    assert not heard("GitHub", "fr", "je pense que guite heub pose problème")


# 013-AC33: an entry passes on 2 renders of 3; one failing entry fails the job and is named.
def test_score() -> None:
    seed = load_seed()
    rows: list[dict[str, Any]] = []
    for i, e in enumerate(seed):
        for lang in langs(e):
            for n in range(RENDERS):
                rows.append({"id": track_id(i, lang, n), "text": f"x {e.term} y", "arrival": 1.0})
    assert score(rows)["pass"]
    gh = next(i for i, e in enumerate(seed) if e.term == "GitHub")
    rows = [r for r in rows if r["id"] != track_id(gh, "fr", 0)]
    assert score(rows)["pass"]
    rows = [r for r in rows if r["id"] != track_id(gh, "fr", 1)]
    res = score(rows)
    assert not res["pass"] and res["failed"] == ["GitHub (fr)"]
