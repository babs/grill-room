"""Synthesis engines: Pocket TTS 3.3.0, and a fake one for tests and the voice e2e."""

import json
import threading
from collections import OrderedDict
from collections.abc import Iterator
from pathlib import Path
from typing import Any, Protocol

import numpy as np

from .audio import Samples
from .textlayer import Lang

# The Pocket configs the image pins: one model per language, every voice a state on it.
POCKET_LANG: dict[Lang, str] = {"fr": "french", "en": "english_2026-09"}
CATALOGUE = Path(__file__).with_name("voices.json")


def catalogue() -> list[dict[str, str]]:
    """The pinned voice catalogue, the same file as the room server's (013-AC11)."""
    data: list[dict[str, str]] = json.loads(CATALOGUE.read_text())
    return data


class Engine(Protocol):
    sample_rate: int

    def gain(self, voice: str, lang: Lang) -> float:
        """The gain that levels this voice (audio.gain_for at build)."""
        ...

    def stream(self, voice: str, lang: Lang, text: str, stop: threading.Event) -> Iterator[Samples]:
        """Float audio at sample_rate, chunk by chunk as it is made; stops early once stop is set."""
        ...


class Pocket:
    """Both language models in one process, each voice a conditioning state loaded on first use."""

    sample_rate = 24000

    def __init__(self, models_dir: Path, threads: int, states: int = 12) -> None:
        import torch
        from pocket_tts import TTSModel

        torch.set_num_threads(threads)
        self._models: dict[Lang, Any] = {
            lg: TTSModel.load_model(language=name) for lg, name in POCKET_LANG.items()
        }
        self._voices = {v["id"]: v for v in catalogue()}
        self._dir = models_dir
        gains = models_dir / "voices" / "gains.json"
        self._gains: dict[str, float] = json.loads(gains.read_text()) if gains.exists() else {}
        # A state holds the voice prompt's attention cache: an LRU keeps memory bounded.
        self._states: OrderedDict[tuple[str, Lang], Any] = OrderedDict()
        self._max_states = states
        self._lock = threading.Lock()

    def state(self, voice: str, lang: Lang) -> Any:
        with self._lock:
            key = (voice, lang)
            if key in self._states:
                self._states.move_to_end(key)
                return self._states[key]
            v = self._voices[voice]
            # Catalogue voices ship as Pocket's own states; the others were encoded at build.
            src: Any = v.get("pocket") or (self._dir / "voices" / lang / f"{voice}.safetensors")
            st = self._models[lang].get_state_for_audio_prompt(src)
            self._states[key] = st
            while len(self._states) > self._max_states:
                self._states.popitem(last=False)
            return st

    def gain(self, voice: str, lang: Lang) -> float:
        return self._gains.get(f"{voice}/{lang}", 1.0)

    def stream(self, voice: str, lang: Lang, text: str, stop: threading.Event) -> Iterator[Samples]:
        model = self._models[lang]
        for chunk in model.generate_audio_stream(self.state(voice, lang), text, stop=stop):
            yield chunk.numpy().astype(np.float32)


class Fake:
    """A tone per word, 250 ms at a pitch per voice, with 100 ms between words and a 1 s pause after
    each sentence mark: what the silence cap shortens. 20 ms chunks, made faster than real time."""

    sample_rate = 24000
    # Its audio is made in microseconds: the pod runs it in the event loop, so tests need no thread.
    inline = True

    def gain(self, voice: str, lang: Lang) -> float:
        return 1.0

    def stream(self, voice: str, lang: Lang, text: str, stop: threading.Event) -> Iterator[Samples]:
        hz = 180.0 + 40.0 * (sum(map(ord, voice)) % 8) + (20.0 if lang == "en" else 0.0)
        t = np.arange(self.sample_rate // 4) / self.sample_rate
        word = (0.1 * np.sin(2 * np.pi * hz * t)).astype(np.float32)
        gap = np.zeros(self.sample_rate // 10, np.float32)
        pause = np.zeros(self.sample_rate, np.float32)
        audio = np.concatenate(
            [np.concatenate([word, pause if w[-1] in ".!?" else gap]) for w in text.split()]
        )
        n = self.sample_rate // 50
        for i in range(0, len(audio), n):
            if stop.is_set():
                return
            yield audio[i : i + n]
