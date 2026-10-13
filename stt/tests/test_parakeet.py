import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

import httpx
import numpy as np
import pytest
from prometheus_client import REGISTRY

from grill_stt.__main__ import engine, engine_info
from grill_stt.config import Settings
from grill_stt.parakeet import compression_ratio
from grill_stt.pipeline import (
    Pipeline,
    Plausibility,
    RoomAPI,
    Utterance,
    guess_language,
    implausible_parakeet,
)
from grill_stt.transcribe import Line

RULES = Plausibility()
REQUIRED = {
    "APP_ROOM_URL": "http://room",
    "APP_ROOM_TOKEN": "t",
    "APP_LIVEKIT_URL": "ws://livekit:7880",
    "APP_LIVEKIT_API_KEY": "k",
    "APP_LIVEKIT_API_SECRET": "s" * 32,
}


def settings(**kw: Any) -> Settings:
    base: dict[str, Any] = {k[4:].lower(): v for k, v in REQUIRED.items()}
    return Settings(**(base | kw))


def parakeet_line(text: str) -> Line:
    """A transcript as ParakeetTranscriber returns it: no language, no confidence."""
    return Line(text, "", compression_ratio=compression_ratio(text))


@pytest.mark.parametrize(
    ("text", "reason"),
    [
        # 007-AC3: a looped phrase, and one token repeated.
        ("on parle de la carte, " * 12, "repetition"),
        ("oui " * 40, "repetition"),
        # 007-AC3: a transcript in another script means the engine went astray.
        ("Привет всем, как дела сегодня", "script"),
        ("我们今天讨论一下部署的问题", "script"),
        ("Bonjour, привет всем как дела", "script"),
    ],
)
def test_parakeet_drops_repetition_and_other_scripts(text: str, reason: str) -> None:
    assert implausible_parakeet(parakeet_line(text), 3.0, RULES) == reason


@pytest.mark.parametrize(
    "text",
    [
        "Oui.",
        "OK",
        "Ça marche, d'accord : c'est noté à 100 %.",
        "On déploie le chart Helm avec Argo CD, puis on regarde les pods dans Kubernetes.",
        "Okay, let me switch to English.",
        "1, 2, 3",
        # Whisper's confidence checks do not apply: a Line with none of them still passes.
        "Les métriques de latence sont dans Grafana.",
    ],
)
def test_parakeet_posts_french_and_english(text: str) -> None:
    """007-AC3: no language probability, no language script: only repetition and Latin letters."""
    assert implausible_parakeet(parakeet_line(text), 0.8, RULES) is None
    assert implausible_parakeet(Line(text, "", 0.1, -3.0, 0.99, 1.0), 6.0, RULES) is None


def test_compression_ratio_matches_whisper() -> None:
    assert compression_ratio("") == 0.0
    assert compression_ratio("Bonjour à tous.") < 1.5
    assert compression_ratio("la la " * 50) > 2.4


@pytest.mark.parametrize(
    ("text", "guess"),
    [
        ("Oui.", "fr"),
        ("D'accord, c'est noté.", "fr"),
        # French dense in English terms is still French.
        ("On déploie le chart Helm avec Argo CD et on check les pods dans le cluster.", "fr"),
        ("Le build est cassé, le pipeline CI ne passe pas sur la merge request.", "fr"),
        ("Yes.", "en"),
        ("Okay, let me switch to English for the rest of the call.", "en"),
        ("We deploy the chart with Argo CD and check the pods.", "en"),
        ("OK", "unknown"),
        ("Kubernetes, Helm, Argo CD", "unknown"),
        ("", "unknown"),
        ("1, 2, 3", "unknown"),
    ],
)
def test_language_guess(text: str, guess: str) -> None:
    """007-AC4: fr, en or unknown from function words, English terms in French included."""
    assert guess_language(text) == guess


def metric(name: str, **labels: str) -> float:
    return REGISTRY.get_sample_value(name, labels) or 0.0


async def test_parakeet_pipeline_counts_the_guess_and_never_reads_the_prompt() -> None:
    """007-AC3, AC4, AC14: whitespace is empty, a Cyrillic line is dropped, a posted line counts its
    guessed language, and the room prompt is never asked for."""
    stored: list[str] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.method == "GET":
            raise AssertionError("read the room prompt")
        stored.append(json.loads(r.content)["body"])
        return httpx.Response(201)

    texts = iter(
        ["   ", "Привет всем, как дела", "On check les pods dans le cluster.", "Let me check the pods."]
    )
    client = httpx.AsyncClient(base_url="http://room", transport=httpx.MockTransport(handle))
    p = Pipeline(
        [lambda _a, _p: parakeet_line(next(texts))], RoomAPI(client), 4, use_prompt=False, engine="parakeet"
    )
    before = {
        "empty": metric("grill_stt_lines_total", result="empty"),
        "script": metric("grill_stt_lines_implausible_total", reason="script"),
        "fr": metric("grill_stt_lines_language_total", engine="parakeet", guess="fr"),
        "en": metric("grill_stt_lines_language_total", engine="parakeet", guess="en"),
    }
    for _ in range(4):
        await p.process(Utterance("r1", 7, np.ones(16000, np.float32), time.monotonic()))
    assert stored == ["On check les pods dans le cluster.", "Let me check the pods."]
    assert metric("grill_stt_lines_total", result="empty") == before["empty"] + 1
    assert metric("grill_stt_lines_implausible_total", reason="script") == before["script"] + 1
    assert metric("grill_stt_lines_language_total", engine="parakeet", guess="fr") == before["fr"] + 1
    assert metric("grill_stt_lines_language_total", engine="parakeet", guess="en") == before["en"] + 1


def test_parakeet_ignores_the_glossary_prompt_setting() -> None:
    """007-AC14: APP_GLOSSARY_PROMPT only reaches Whisper."""
    s = settings(glossary_prompt=True)
    assert s.engine == "parakeet" and not s.uses_glossary
    assert settings(engine="whisper", glossary_prompt=True).uses_glossary


def test_engine_info_names_engine_model_and_revision() -> None:
    """007-AC1: grill_stt_engine_info{engine,model,revision}."""
    s = settings(parakeet_model="/models/parakeet/parakeet-ultra-Q8_0.gguf", parakeet_revision="39eeb55")
    assert engine(s) == ("/models/parakeet/parakeet-ultra-Q8_0.gguf", "39eeb55")
    engine_info(s)
    labels = {"engine": "parakeet", "model": "parakeet-ultra-Q8_0.gguf", "revision": "39eeb55"}
    assert metric("grill_stt_engine_info", **labels) == 1
    w = settings(engine="whisper", model="/models/whisper-small", whisper_revision="536b066")
    assert engine(w) == ("/models/whisper-small", "536b066")


@pytest.mark.parametrize("value", ["sherpa", "Parakeet", ""])
def test_unknown_engine_stops_the_worker_naming_the_allowed_values(value: str) -> None:
    """007-AC1: any other APP_ENGINE stops the worker at start, with one JSON line."""
    env = {"PATH": os.environ["PATH"], "HOME": os.environ.get("HOME", "/"), **REQUIRED, "APP_ENGINE": value}
    r = subprocess.run(
        [sys.executable, "-m", "grill_stt"],
        env=env,
        capture_output=True,
        text=True,
        timeout=60,
        cwd=Path(__file__).parents[1],
    )
    assert r.returncode == 2, r.stderr
    (out,) = r.stdout.splitlines()
    event = json.loads(out)
    assert event["event"] == "config_invalid"
    assert any("APP_ENGINE" in e and "'parakeet'" in e and "'whisper'" in e for e in event["errors"]), event
