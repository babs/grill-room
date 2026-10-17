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
    assert heard("kubectl", "en", "I think kube control is the problem here")
    assert heard("OK", "en", "I think okay is the problem here")
    assert not heard("pod", "fr", "je pense que podcast pose problème")
    assert not heard("GitHub", "fr", "je pense que guite heub pose problème")


def all_heard() -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for i, e in enumerate(load_seed()):
        for lang in langs(e):
            for n in range(RENDERS):
                rows.append({"id": track_id(i, lang, n), "text": f"x {e.term} y", "arrival": 1.0})
    return rows


def github_fr(n: int) -> str:
    return track_id(next(i for i, e in enumerate(load_seed()) if e.term == "GitHub"), "fr", n)


# 013-AC33: an entry passes on 6 renders of 10; one failing entry fails the job and is named.
def test_score_missing_tracks() -> None:
    rows = all_heard()
    assert score(rows)["pass"]
    lost = {github_fr(n) for n in range(4)}
    assert score([r for r in rows if r["id"] not in lost])["pass"]
    lost.add(github_fr(4))
    res = score([r for r in rows if r["id"] not in lost])
    assert not res["pass"] and res["failed"] == ["GitHub (fr)"]


# 013-AC33: misses heard as other words, scattered, count the same; a track heard in two rows joins.
def test_score_misheard_tracks() -> None:
    rows = [r for r in all_heard() if r["id"] != github_fr(1)]
    rows += [
        {"id": github_fr(1), "text": "hub y", "arrival": 2.0},
        {"id": github_fr(1), "text": "x git", "arrival": 1.0},
    ]
    wrong = {github_fr(n) for n in (0, 3, 5, 7)}
    for r in rows:
        if r["id"] in wrong:
            r["text"] = "je pense que guite heub pose problème"
    assert score(rows)["pass"]
    for r in rows:
        if r["id"] == github_fr(9):
            r["text"] = "je pense que guite heub pose problème"
    res = score(rows)
    assert not res["pass"] and res["failed"] == ["GitHub (fr)"]
