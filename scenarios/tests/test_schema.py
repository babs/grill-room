from pathlib import Path
from typing import Any

import pytest
import yaml
from pydantic import ValidationError

from grill_scenarios import schema

HERE = Path(__file__).resolve().parent.parent


def base() -> dict[str, Any]:
    return {
        "name": "t",
        "budget": {"max_requests": 6},
        "participants": [{"name": "Ann", "voices": {"en": {"model": "en_US-joe-medium"}}}, {"name": "Bob"}],
        "personas": [
            {"name": "Grill", "model": "haiku", "requests_per_turn": 1},
            {
                "name": "Secretary",
                "template": "secretary",
                "model": "haiku",
                "every_n": 1,
                "requests_per_turn": 2,
            },
        ],
        "steps": [
            {"daemon": {"by": "Ann"}},
            {"voice": {"lines": [{"by": "Ann", "lang": "en", "text": "Hello."}]}},
            {"press": {"by": "Ann", "persona": "Grill"}},
            {"wait": {"turns": 2}},
        ],
        "assertions": {"turns": {"Grill": 2, "Secretary": 2}},
    }


# 004-AC16: every scenario the repo ships validates, with a budget its turns fit in.
@pytest.mark.parametrize("path", sorted(HERE.glob("*.yaml")), ids=lambda p: p.name)
def test_shipped_scenarios_validate(path: Path) -> None:
    s = schema.load(path)
    assert schema.estimate(s) <= s.budget.max_requests


# 004-AC17: the four scenarios of the library are there.
def test_library() -> None:
    names = {schema.load(p).name for p in HERE.glob("*.yaml")}
    assert names == {"Dark mode grill", "Two-persona chain", "Long session", "Host hand-over"}


def test_base_is_valid() -> None:
    schema.Scenario.model_validate(base())


# A daemon can stop and pair again; an upload may sit in a subfolder or have dots in its name.
def test_valid_shapes() -> None:
    d = base()
    d["steps"] += [{"stop_daemon": {"by": "Ann"}}, {"daemon": {"by": "Ann"}}]
    d["steps"] += [{"upload": {"by": "Ann", "file": f}} for f in ("plan.md", "docs/v1.2/plan..md", "./a.md")]
    schema.Scenario.model_validate(d)


def bad(mutate: Any, match: str) -> None:
    d = base()
    mutate(d)
    with pytest.raises(ValidationError, match=match):
        schema.Scenario.model_validate(d)


# 004-AC16: the validator names what is wrong, before any audio or request.
@pytest.mark.parametrize(
    ("mutate", "match"),
    [
        (lambda d: d["steps"].append({"say": {"by": "Zed", "text": "hi"}}), "Zed is not a participant"),
        (
            lambda d: d["steps"].append({"voice": {"lines": [{"by": "Bob", "lang": "en", "text": "x"}]}}),
            "Bob has no en voice",
        ),
        (
            lambda d: d["steps"][1]["voice"]["lines"].append({"by": "Ann", "lang": "fr", "text": "x"}),
            "Ann has no fr voice",
        ),
        (lambda d: d["steps"].append({"press": {"by": "Ann", "persona": "Nobody"}}), "no persona Nobody"),
        (
            lambda d: d["steps"].append({"say": {"by": "Ann", "text": "x"}, "wait": {"seconds": 1}}),
            "exactly one action",
        ),
        (lambda d: d["steps"].append({}), "exactly one action"),
        (lambda d: d["steps"].append({"wait": {"turns": 1, "seconds": 2}}), "turns or seconds"),
        (lambda d: d["steps"].append({"wait": {}}), "turns or seconds"),
        (lambda d: d["steps"].append({"handover": {"to": "Bob"}}), "Bob has no daemon"),
        (lambda d: d["steps"].pop(0), "no daemon step"),
        (lambda d: d["steps"].append({"stop_daemon": {"by": "Bob"}}), "Bob has no running daemon"),
        (lambda d: d["steps"].extend([{"stop_daemon": {"by": "Ann"}}] * 2), "Ann has no running daemon"),
        (lambda d: d["steps"].append({"daemon": {"by": "Ann"}}), "Ann's daemon already runs"),
        (lambda d: d["steps"].append({"upload": {"by": "Ann", "file": "../../.env"}}), "steps.4.upload.file"),
        (
            lambda d: d["steps"].append({"upload": {"by": "Ann", "file": "/etc/passwd"}}),
            "steps.4.upload.file",
        ),
        (
            lambda d: d["steps"].append({"upload": {"by": "Ann", "file": "docs/../../x.md"}}),
            "steps.4.upload.file",
        ),
        (lambda d: d["steps"].append({"upload": {"by": "Ann", "file": ".."}}), "steps.4.upload.file"),
        (lambda d: d["personas"][0].update(model="gpt"), "personas.0.model"),
        (lambda d: d["personas"][0].update(effort="huge"), "personas.0.effort"),
        (lambda d: d["personas"][0].pop("requests_per_turn"), "requests_per_turn"),
        (lambda d: d["personas"].append(dict(d["personas"][0])), "persona names must be unique"),
        (lambda d: d["participants"].append({"name": "Ann"}), "participant names must be unique"),
        (lambda d: d["participants"][0].update(name="Ann Lee"), "participants.0.name"),
        (lambda d: d.update(colour="red"), "colour"),
        (lambda d: d["steps"][2]["press"].update(when="now"), "when"),
        (lambda d: d["assertions"]["turns"].update(Ghost=1), "no persona Ghost"),
        (lambda d: d["assertions"].update(syntheses=[{"persona": "Grill", "nth": 3}]), "Grill runs 2 turns"),
        (
            lambda d: d["assertions"].update(syntheses=[{"persona": "Grill", "contains": ["("]}]),
            "missing \\)",
        ),
        (lambda d: d["assertions"].update(voice={"max_wer": 1.5, "max_latency_s": 10}), "max_wer"),
        (lambda d: d["assertions"].pop("turns"), "turns"),
        (lambda d: d.pop("budget"), "budget"),
    ],
)
def test_refused(mutate: Any, match: str) -> None:
    bad(mutate, match)


# 004-AC16, 004-AC17: the budget is every asserted turn at its persona's cost, and a scenario whose
# turns cost more than its budget line does not load.
def test_budget() -> None:
    s = schema.Scenario.model_validate(base())
    assert schema.estimate(s) == 2 * 1 + 2 * 2
    d = base()
    d["budget"]["max_requests"] = 5
    with pytest.raises(ValidationError, match="cost about 6 requests, over the budget of 5"):
        schema.Scenario.model_validate(d)
    d = base()
    d["personas"][1]["requests_per_turn"] = 3
    with pytest.raises(ValidationError, match="cost about 8 requests"):
        schema.Scenario.model_validate(d)
    d = base()
    d["assertions"]["turns"] = {"Grill": 1}
    assert schema.estimate(schema.Scenario.model_validate(d)) == 1


def test_with_model() -> None:
    s = schema.with_model(schema.Scenario.model_validate(base()), "opus")
    assert {p.model for p in s.personas} == {"opus"}
    assert schema.estimate(s) == 6
    with pytest.raises(ValueError, match="model must be one of"):
        schema.with_model(s, "gpt")


def test_load_yaml(tmp_path: Path) -> None:
    f = tmp_path / "s.yaml"
    f.write_text(yaml.safe_dump(base()))
    assert schema.load(f).name == "t"
    assert schema.load(f).voices() == {"en_US-joe-medium"}
