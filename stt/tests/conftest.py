import json
import os
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


@pytest.fixture(scope="module")
def model() -> str:
    """The Whisper model: APP_MODEL as a directory, or a size from the Hugging Face cache."""
    from faster_whisper.utils import download_model

    name = os.getenv("APP_MODEL", "small")
    if Path(name).is_dir():
        return name
    try:
        return str(download_model(name, local_files_only=True))
    except Exception as e:  # missing from the cache: huggingface_hub raises several types
        if os.getenv("CI"):
            pytest.fail(f"whisper model missing: {e}")
        pytest.skip(f"whisper model not downloaded: {e}")
