import os
import re
from typing import Any

import pytest

from grill_stt.transcribe import Transcriber
from grill_stt.vad import SR, Audio, Segmenter, Silero


def words(s: str) -> set[str]:
    return set(re.findall(r"[\w']+", s.lower().replace("’", "'")))


@pytest.fixture(scope="module")
def model() -> str:
    from faster_whisper.utils import download_model

    try:
        return str(download_model(os.getenv("APP_MODEL", "small"), local_files_only=True))
    except Exception as e:  # missing from the cache: huggingface_hub raises several types
        if os.getenv("CI"):
            pytest.fail(f"whisper model missing: {e}")
        pytest.skip(f"whisper model not downloaded: {e}")


@pytest.mark.e2e
def test_each_utterance_in_its_own_language(model: str, speech: tuple[Audio, list[dict[str, Any]]]) -> None:
    """001-AC8: alternating FR and EN sentences, cut by the VAD, come out in the language spoken."""
    audio, utts = speech
    seg = Segmenter(Silero())
    cut: list[Audio] = []
    for i in range(0, len(audio), 160):
        cut += seg.feed(audio[i : i + 160])
    transcribe = Transcriber(model, cpu_threads=4)
    lines = [transcribe(u) for u in cut]
    assert [line.language for line in lines] == [u["lang"] for u in utts]
    for line, u in zip(lines, utts, strict=True):
        # Not translated: most of the words are the reference's own.
        got = words(line.text)
        assert len(got & words(u["text"])) / len(got) >= 0.5, (line.text, u["text"])
    assert all(len(u) / SR < 15 for u in cut)
