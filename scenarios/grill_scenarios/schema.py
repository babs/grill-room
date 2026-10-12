"""The scenario file: a YAML script of a grill session, validated before anything runs."""

import re
from pathlib import Path
from typing import Annotated, Any, Literal

import yaml
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

# The room server's lists (internal/turn): a persona asking for anything else is refused.
MODELS = ("haiku", "sonnet", "opus", "fable")
EFFORTS = ("low", "medium", "high", "xhigh", "max")

Name = Annotated[str, Field(pattern=r"^[\w-]{1,30}$")]
Lang = Annotated[str, Field(pattern=r"^[a-z]{2}$")]


class Strict(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class Voice(Strict):
    model: str
    speaker: int | None = None


class Participant(Strict):
    name: Name
    voices: dict[Lang, Voice] = {}


class Persona(Strict):
    name: Name
    # A shipped preset key (default, secretary) or a library title; empty for a persona the room
    # already has, like Grill.
    template: str = ""
    model: Literal["haiku", "sonnet", "opus", "fable"]
    effort: Literal["", "low", "medium", "high", "xhigh", "max"] = ""
    every_n: int = Field(0, ge=0, le=100)
    text: str = ""
    # What one turn costs in Claude requests: 1 with no tool, one more per round of tool calls.
    requests_per_turn: int = Field(ge=1, le=20)


class Line(Strict):
    by: Name
    lang: Lang
    text: str = Field(min_length=1)
    # Silence before the line starts, after the previous line ended; negative overlaps it.
    gap: float | None = Field(None, ge=-10, le=60)


class VoiceRound(Strict):
    # Silence before the first line: the STT worker needs a few seconds to subscribe to new tracks.
    lead: float = Field(10.0, ge=0, le=60)
    lines: list[Line] = Field(min_length=1)


class Upload(Strict):
    by: Name
    file: str

    @field_validator("file")
    @classmethod
    def inside(cls, v: str) -> str:
        # Relative to the scenario file, inside its folder: a shared scenario must not upload
        # anything else from this machine.
        if not v or v.startswith("/") or ".." in v.split("/"):
            raise ValueError("must be a path inside the scenario's folder")
        return v


class Annotate(Strict):
    by: Name
    doc: str
    quote: str
    comment: str


class Say(Strict):
    by: Name
    text: str = Field(min_length=1)


class Press(Strict):
    by: Name
    persona: Name


class Wait(Strict):
    # Until the room holds this many agent syntheses, or a fixed pause.
    turns: int | None = Field(None, ge=1)
    seconds: float | None = Field(None, gt=0, le=600)
    timeout: float = Field(600, gt=0, le=3600)

    @model_validator(mode="after")
    def one(self) -> Wait:
        if (self.turns is None) == (self.seconds is None):
            raise ValueError("wait takes turns or seconds")
        return self


class Flood(Strict):
    by: list[Name] = Field(min_length=1)
    messages: int = Field(ge=1, le=5000)


class Handover(Strict):
    to: Name


class By(Strict):
    by: Name


class Step(Strict):
    voice: VoiceRound | None = None
    daemon: By | None = None
    stop_daemon: By | None = None
    upload: Upload | None = None
    annotate: Annotate | None = None
    say: Say | None = None
    press: Press | None = None
    wait: Wait | None = None
    flood: Flood | None = None
    handover: Handover | None = None

    @model_validator(mode="after")
    def one(self) -> Step:
        set_ = [k for k, v in self if v is not None]
        if len(set_) != 1:
            raise ValueError(f"a step is exactly one action, got {set_ or 'none'}")
        return self

    @property
    def kind(self) -> str:
        return next(k for k, v in self if v is not None)


class VoiceChecks(Strict):
    max_wer: float = Field(ge=0, le=1)
    # Share of matched transcript lines posted under the right speaker.
    min_attribution: float = Field(1.0, ge=0, le=1)
    # Share of script lines that got a transcript at all.
    min_delivered: float = Field(1.0, ge=0, le=1)
    max_latency_s: float = Field(gt=0)


class SynthesisCheck(Strict):
    persona: Name
    nth: int = Field(1, ge=1)
    # Case-insensitive regular expressions.
    contains: list[str] = []
    not_contains: list[str] = []
    # The turn behind it resumed an earlier session.
    resumed: bool | None = None
    # Whose daemon ran it.
    ran_on: Name | None = None
    # Why it ran, as the host's monitoring records it ("called by @Grill", "every round").
    cause: str | None = None

    @model_validator(mode="after")
    def patterns(self) -> SynthesisCheck:
        for p in self.contains + self.not_contains:
            try:
                re.compile(p)
            except re.error as e:
                raise ValueError(f"bad pattern {p!r}: {e}") from e
        return self


class Range(Strict):
    min: int = 0
    max: int


class Assertions(Strict):
    # Persona name to the exact number of turns it must complete. The budget is computed from it.
    turns: dict[Name, int]
    voice: VoiceChecks | None = None
    syntheses: list[SynthesisCheck] = []
    chain: Range | None = None
    # Press to synthesis, for every pressed turn.
    max_turn_s: float | None = Field(None, gt=0)
    # Context use of the last turn, in percent of the window.
    min_context_pct: float | None = Field(None, ge=0, le=100)
    # Workspace files each turn after the first fetched: transcript chunks are immutable once full.
    max_fetched_files_after_first: int | None = Field(None, ge=0)


class Budget(Strict):
    max_requests: int = Field(ge=1, le=100)


class Scenario(Strict):
    name: str = Field(min_length=1, max_length=80)
    about: str = ""
    budget: Budget
    gap: float = Field(1.2, ge=0, le=60)
    participants: list[Participant] = Field(min_length=1)
    personas: list[Persona] = Field(min_length=1)
    steps: list[Step] = Field(min_length=1)
    assertions: Assertions

    @model_validator(mode="after")
    def references(self) -> Scenario:
        people = {p.name: p for p in self.participants}
        if len(people) != len(self.participants):
            raise ValueError("participant names must be unique")
        personas = {p.name for p in self.personas}
        if len(personas) != len(self.personas):
            raise ValueError("persona names must be unique")
        paired: set[str] = set()
        running: set[str] = set()
        for i, s in enumerate(self.steps, 1):
            for who in _people(s):
                if who not in people:
                    raise ValueError(f"step {i}: {who} is not a participant")
            if s.voice:
                for ln in s.voice.lines:
                    if ln.lang not in people[ln.by].voices:
                        raise ValueError(f"step {i}: {ln.by} has no {ln.lang} voice")
            if s.daemon:
                if s.daemon.by in running:
                    raise ValueError(f"step {i}: {s.daemon.by}'s daemon already runs")
                paired.add(s.daemon.by)
                running.add(s.daemon.by)
            if s.stop_daemon:
                if s.stop_daemon.by not in running:
                    raise ValueError(f"step {i}: {s.stop_daemon.by} has no running daemon")
                running.discard(s.stop_daemon.by)
            if s.handover and s.handover.to not in paired:
                raise ValueError(f"step {i}: {s.handover.to} has no daemon to hand over to")
            if s.press and s.press.persona not in personas:
                raise ValueError(f"step {i}: no persona {s.press.persona}")
        if not paired:
            raise ValueError("no daemon step: no persona can run")
        a = self.assertions
        for name in set(a.turns) - personas:
            raise ValueError(f"assertions.turns: no persona {name}")
        for c in a.syntheses:
            if c.persona not in personas:
                raise ValueError(f"assertions.syntheses: no persona {c.persona}")
            if c.nth > a.turns.get(c.persona, 0):
                raise ValueError(f"assertions.syntheses: {c.persona} runs {a.turns.get(c.persona, 0)} turns")
        est = estimate(self)
        if est > self.budget.max_requests:
            raise ValueError(
                f"the turns asserted cost about {est} requests, over the budget of {self.budget.max_requests}"
            )
        return self

    def voices(self) -> set[str]:
        return {v.model for p in self.participants for v in p.voices.values()}


def _people(s: Step) -> list[str]:
    match s.kind:
        case "voice":
            return [ln.by for ln in s.voice.lines] if s.voice else []
        case "flood":
            return s.flood.by if s.flood else []
        case "handover":
            return [s.handover.to] if s.handover else []
    action = getattr(s, s.kind)
    return [action.by] if hasattr(action, "by") else []


def estimate(s: Scenario) -> int:
    """Claude requests the run should spend: every asserted turn at its persona's cost."""
    cost = {p.name: p.requests_per_turn for p in s.personas}
    return sum(n * cost[name] for name, n in s.assertions.turns.items() if name in cost)


def with_model(s: Scenario, model: str) -> Scenario:
    """The scenario with every persona on model, for a run on another model than written."""
    if model not in MODELS:
        raise ValueError(f"model must be one of {MODELS}")
    return s.model_copy(update={"personas": [p.model_copy(update={"model": model}) for p in s.personas]})


def load(path: Path) -> Scenario:
    data: Any = yaml.safe_load(path.read_text(encoding="utf-8"))
    return Scenario.model_validate(data)
