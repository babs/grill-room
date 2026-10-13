import json
import os
import wave
from collections.abc import Callable
from pathlib import Path
from typing import Any

import numpy as np
import pytest

from grill_stt.transcribe import Line
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


@pytest.fixture(scope="module")
def parakeet() -> str:
    """The parakeet GGUF the image bakes in (APP_PARAKEET_MODEL), loaded through TRANSCRIBE_LIBRARY."""
    path = os.getenv("APP_PARAKEET_MODEL", "/models/parakeet/parakeet-ultra-Q8_0.gguf")
    if Path(path).is_file() and os.getenv("TRANSCRIBE_LIBRARY"):
        return path
    if os.getenv("GRILL_STT_IMAGE_TEST"):
        pytest.fail(f"parakeet model or TRANSCRIBE_LIBRARY missing in the image: {path}")
    pytest.skip("parakeet runs in the image only: make stt-image-test")


@pytest.fixture(scope="module", params=["whisper", "parakeet"])
def transcriber(request: pytest.FixtureRequest) -> Callable[[Audio], Line]:
    """Each engine as the worker loads it (spec 007, AC2 and AC15)."""
    if request.param == "parakeet":
        from grill_stt.parakeet import ParakeetTranscriber

        return ParakeetTranscriber(request.getfixturevalue("parakeet"), cpu_threads=4)
    from grill_stt.transcribe import Transcriber

    return Transcriber(request.getfixturevalue("model"), cpu_threads=4)
