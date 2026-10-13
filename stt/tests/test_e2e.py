import re
from collections.abc import Callable
from typing import Any

import pytest

from grill_stt.pipeline import guess_language
from grill_stt.transcribe import Line
from grill_stt.vad import SR, Audio, Segmenter, Silero


def words(s: str) -> set[str]:
    return set(re.findall(r"[\w']+", s.lower().replace("’", "'")))


@pytest.mark.e2e
def test_each_utterance_in_its_own_language(
    transcriber: Callable[[Audio], Line], speech: tuple[Audio, list[dict[str, Any]]]
) -> None:
    """001-AC8, 007-AC2: alternating FR and EN sentences, cut by the VAD, come out in the language
    spoken. Whisper labels it; for parakeet the function-word guess reads it from the text."""
    audio, utts = speech
    seg = Segmenter(Silero())
    cut: list[Audio] = []
    for i in range(0, len(audio), 160):
        cut += seg.feed(audio[i : i + 160])
    lines = [transcriber(u) for u in cut]
    assert [line.language or guess_language(line.text) for line in lines] == [u["lang"] for u in utts]
    for line, u in zip(lines, utts, strict=True):
        # Not translated: most of the words are the reference's own.
        got = words(line.text)
        assert len(got & words(u["text"])) / len(got) >= 0.5, (line.text, u["text"])
    assert all(len(u) / SR < 15 for u in cut)
