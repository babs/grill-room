import json
import wave
from pathlib import Path
from typing import Any

import numpy as np
import pytest

from grill_stt.vad import Audio

FIXTURES = Path(__file__).resolve().parents[2] / "e2e" / "voice" / "fixtures"


@pytest.fixture
def speech() -> tuple[Audio, list[dict[str, Any]]]:
    """The FR/EN FLEURS fixture the browser tests play, as float samples, with its utterance timings."""
    with wave.open(str(FIXTURES / "fr-en.wav")) as w:
        pcm = np.frombuffer(w.readframes(w.getnframes()), "<i2")
    audio: Audio = pcm.astype(np.float32) / np.float32(32768)
    return audio, json.loads((FIXTURES / "fr-en.json").read_text())["utterances"]
