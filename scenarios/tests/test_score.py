from typing import Any

from grill_scenarios import schema, score
from tests.test_schema import base

ROUNDS = [
    {
        "round": 1,
        "lines": [
            {"by": "Ann", "lang": "en", "text": "The cache needs a test.", "start": 4.0, "end": 6.0},
            {"by": "Bob", "lang": "en", "text": "I will write it tomorrow.", "start": 7.0, "end": 9.0},
        ],
    }
]


def result(**over: Any) -> dict[str, Any]:
    t0 = 1_000_000
    r: dict[str, Any] = {
        "model": "haiku",
        "requests": 4,
        "rounds": [{"round": 1, "micAt": {"Ann": t0, "Bob": t0}}],
        "messages": [
            {"kind": "voice", "sender": "Ann", "body": "The cache needs a test.", "ts": t0 + 10_000},
            {"kind": "voice", "sender": "Bob", "body": "I will write it tomorrow", "ts": t0 + 14_000},
            {"kind": "agent", "persona": "Grill", "body": "Risk: no test. @Secretary", "model": "haiku"},
            {"kind": "agent", "persona": "Secretary", "body": "Minutes.", "model": "haiku"},
            {"kind": "agent", "persona": "Grill", "body": "Clear.", "model": "haiku"},
            {"kind": "agent", "persona": "Secretary", "body": "Minutes 2.", "model": "haiku"},
        ],
        "presses": [
            {"persona": "Grill", "at": 0, "index": 0},
            {"persona": "Grill", "at": 50_000, "index": 1},
        ],
        "agentSeen": [
            {"persona": "Grill", "at": 20_000},
            {"persona": "Secretary", "at": 30_000},
            {"persona": "Grill", "at": 60_000},
            {"persona": "Secretary", "at": 70_000},
        ],
        "chainSeen": [0, 1, 0, 1],
        "turns": [
            {
                "turn": 1,
                "persona": "Grill",
                "status": "ok",
                "daemon": "Ann",
                "resume": False,
                "cause": "button",
                "context_tokens": 1000,
                "context_window": 200000,
                "fetched_files": 9,
            },
            {
                "turn": 2,
                "persona": "Secretary",
                "status": "ok",
                "daemon": "Ann",
                "resume": False,
                "cause": "called by @Grill",
                "fetched_files": 1,
            },
            {
                "turn": 3,
                "persona": "Grill",
                "status": "ok",
                "daemon": "Ann",
                "resume": True,
                "cause": "button",
                "fetched_files": 1,
            },
            {
                "turn": 4,
                "persona": "Secretary",
                "status": "ok",
                "daemon": "Ann",
                "resume": True,
                "cause": "every round",
                "context_tokens": 4000,
                "context_window": 200000,
                "fetched_files": 2,
            },
        ],
    }
    r.update(over)
    return r


def scenario(**assertions: Any) -> schema.Scenario:
    d = base()
    d["assertions"].update(assertions)
    return schema.Scenario.model_validate(d)


def failed(r: dict[str, Any]) -> list[str]:
    return [c["check"] for c in r["checks"] if not c["ok"]]


def test_wer() -> None:
    assert score.edits("a b c".split(), "a x c d".split()) == 2
    assert score.words("Aujourd’hui, l'en-tête!") == ["aujourd", "hui", "l", "en", "tête"]


# 004-AC16: voice lines scored against the script, by speaker, with latency from the end of speech.
def test_voice() -> None:
    v = score.voice(ROUNDS, result())
    assert v["wer"] == 0.0 and v["attribution"] == 1.0 and v["delivered"] == 1.0
    assert v["latency_s"] == {"min": 4.0, "median": 4.5, "p95": 5.0, "max": 5.0}
    swapped = result(
        messages=[{"kind": "voice", "sender": "Bob", "body": "The cache needs a test.", "ts": 1_010_000}]
    )
    v = score.voice(ROUNDS, swapped)
    assert v["wrong"] == 1 and v["attribution"] == 0.0 and v["delivered"] == 0.0 and v["wer"] == 1.0


# 004-AC16: every assertion kind passes on a run that meets it.
def test_all_pass() -> None:
    s = scenario(
        voice={"max_wer": 0.1, "max_latency_s": 6},
        syntheses=[
            {"persona": "Grill", "nth": 1, "contains": ["@secretary\\b"], "resumed": False, "ran_on": "Ann"},
            {"persona": "Grill", "nth": 2, "not_contains": ["@secretary"], "resumed": True},
            {"persona": "Secretary", "nth": 1, "cause": "called by @Grill"},
            {"persona": "Secretary", "nth": 2, "cause": "every round"},
        ],
        chain={"min": 1, "max": 1},
        max_turn_s=20,
        min_context_pct=1,
        max_fetched_files_after_first=2,
    )
    r = score.check(s, ROUNDS, result())
    assert failed(r) == []
    assert r["ok"]
    assert [m["press_to_synthesis_s"] for m in r["syntheses"]["Grill"]] == [20.0, 10.0]


# 004-AC16: each assertion goes red on the run that breaks it.
def test_each_fails() -> None:
    s = scenario(
        voice={"max_wer": 0.1, "max_latency_s": 4.5},
        syntheses=[
            {
                "persona": "Grill",
                "nth": 1,
                "contains": ["nope"],
                "not_contains": ["risk"],
                "resumed": True,
                "ran_on": "Bob",
            },
            {"persona": "Secretary", "nth": 2, "cause": "called by"},
        ],
        chain={"min": 2, "max": 3},
        max_turn_s=15,
        min_context_pct=5,
        max_fetched_files_after_first=1,
    )
    r = score.check(s, ROUNDS, result(requests=7, error="boom"))
    assert set(failed(r)) == {
        "run completed",
        "requests within budget",
        "voice latency",
        "Grill synthesis 1 contains /nope/",
        "Grill synthesis 1 lacks /risk/",
        "Grill synthesis 1 resumed=True",
        "Grill synthesis 1 ran on Bob's daemon",
        "Secretary synthesis 2 cause 'called by'",
        "chain count",
        "press to synthesis",
        "context use",
        "workspace fetched after the first turn",
    }
    assert not r["ok"]


def test_turn_counts_and_failures() -> None:
    res = result()
    res["turns"][3]["status"] = "failed"
    res["turns"][3]["error"] = "sandbox died"
    r = score.check(scenario(), ROUNDS, res)
    assert set(failed(r)) == {"Secretary ran 2 turns", "no failed turn"}
    missing = result(messages=[m for m in result()["messages"] if m["kind"] != "agent"])
    r = score.check(scenario(syntheses=[{"persona": "Grill", "nth": 2}]), ROUNDS, missing)
    assert "Grill synthesis 2" in failed(r)
