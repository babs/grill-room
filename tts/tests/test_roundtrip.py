from typing import Any

from grill_tts.roundtrip import BAR, FLAGGED, SET, norm, score, voices, wer


def rows(said: dict[str, str]) -> list[dict[str, Any]]:
    return [{"id": k, "text": v, "arrival": 1.0} for k, v in said.items()]


def perfect() -> dict[str, str]:
    return {
        f"{v}/{s['id']}": s["text"]
        for lang in ("fr", "en")
        for v in voices(lang)
        for s in SET
        if s["lang"] == lang
    }


# 013-AC18: a perfect round trip passes; numbers compare as words on both sides.
def test_perfect_round_trip_passes() -> None:
    res = score(rows(perfect()))
    assert res["wer_fr"] == 0 and res["wer_en"] == 0 and res["pass"]
    assert res["flagged"] == dict.fromkeys(FLAGGED, 5)
    assert norm("Le 502 à 10 %", "fr") == ["le", "cinq", "cent", "deux", "à", "dix", "pour", "cent"]
    assert wer(["a", "b", "c"], ["a", "x", "c", "d"]) == 2 / 3


# 013-AC18: a flagged term lost in two French voices fails, even with a low WER; so does a WER
# over the bar in one language only.
def test_round_trip_bars() -> None:
    said = perfect()
    for v in voices("fr")[:2]:
        said[f"{v}/fr08"] = said[f"{v}/fr08"].replace("GitHub", "jithub")
    res = score(rows(said))
    assert res["flagged"]["GitHub"] == 3 and res["wer_fr"] < BAR and not res["pass"]
    said = perfect()
    for k in [k for k in said if k.endswith(("en01", "en02", "en03", "en04"))]:
        said[k] = "nothing like it"
    res = score(rows(said))
    assert res["wer_en"] > BAR and res["wer_fr"] == 0 and not res["pass"]
    assert not score([])["pass"]


def test_set() -> None:
    assert sum(s["lang"] == "fr" for s in SET) == 17 and sum(s["lang"] == "en" for s in SET) == 8
    assert (
        voices("fr") == ["estelle", "cml-2114", "cml-3267", "cml-4193", "cml-12080"]
        and len(voices("en")) == 5
    )
