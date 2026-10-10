from typing import Any

import numpy as np
import numpy.typing as npt
import pytest

from grill_stt.vad import SR, Audio, Segmenter, Silero


def loudness(windows: Audio) -> npt.NDArray[np.float32]:
    """A stand-in model: loud windows are speech, quiet ones are between thresholds, zeros silence."""
    level = np.abs(windows).mean(axis=1)
    return np.where(level > 0.3, 0.9, np.where(level > 0.05, 0.4, 0.0)).astype(np.float32)


def tone(seconds: float, level: float = 0.5) -> Audio:
    return np.full(int(seconds * SR), level, np.float32)


def silence(seconds: float) -> Audio:
    return np.zeros(int(seconds * SR), np.float32)


def feed(seg: Segmenter, audio: Audio, frame: int = 160) -> list[Audio]:
    """Feeds audio in LiveKit-sized frames, like a track does."""
    out: list[Audio] = []
    for i in range(0, len(audio), frame):
        out += seg.feed(audio[i : i + frame])
    return out


def test_cuts_on_a_pause() -> None:
    seg = Segmenter(loudness)
    out = feed(seg, np.concatenate([silence(1), tone(2), silence(1), tone(1), silence(0.9)]))
    assert len(out) == 2
    # Each utterance keeps pad_ms of lead-in and of trailing silence around its speech.
    assert abs(len(out[0]) / SR - 2.4) < 0.07
    assert abs(len(out[1]) / SR - 1.4) < 0.07


def test_frame_size_does_not_change_the_cuts() -> None:
    audio = np.concatenate([silence(0.5), tone(1.3), silence(0.9), tone(0.9), silence(0.9)])
    frames = (100, 160, 480, 512, 4000)
    shapes = {frame: [len(u) for u in feed(Segmenter(loudness), audio, frame)] for frame in frames}
    assert len(set(map(tuple, shapes.values()))) == 1, shapes


@pytest.mark.parametrize("pause", [0.3, 0.6])
def test_a_short_pause_does_not_cut(pause: float) -> None:
    """001-AC7: a pause between two sentences of one turn keeps them in one utterance. At 500 ms
    the S5 script's 18 lines became 32 utterances, each paying a full model pass."""
    out = feed(Segmenter(loudness), np.concatenate([tone(1), silence(pause), tone(1), silence(0.9)]))
    assert len(out) == 1


def test_between_thresholds_is_not_silence() -> None:
    # 0.4 is under threshold but over neg_threshold: a soft syllable must not end the utterance.
    out = feed(Segmenter(loudness), np.concatenate([tone(1), tone(1, 0.1), tone(1), silence(0.9)]))
    assert len(out) == 1
    assert len(out[0]) / SR > 3


def test_drops_blips_shorter_than_min_speech() -> None:
    assert feed(Segmenter(loudness), np.concatenate([tone(0.2), silence(1)])) == []
    assert len(feed(Segmenter(loudness), np.concatenate([tone(0.6), silence(1)]))) == 1


def test_cuts_long_speech_at_max() -> None:
    out = feed(Segmenter(loudness, max_speech_s=2), np.concatenate([tone(5), silence(0.9)]))
    assert [round(len(u) / SR) for u in out] == [2, 2, 1]


def test_flush_emits_speech_in_progress() -> None:
    seg = Segmenter(loudness)
    assert feed(seg, np.concatenate([silence(0.5), tone(1)])) == []
    out = seg.flush()
    assert len(out) == 1
    assert seg.flush() == []


def test_silero_cuts_the_fixture_after_each_sentence(speech: tuple[Audio, list[dict[str, Any]]]) -> None:
    """001-AC7: the real model, state carried across 10 ms frames, ends each utterance on its pause."""
    audio, utts = speech
    seg = Segmenter(Silero())
    cuts: list[float] = []
    for i in range(0, len(audio), 160):
        cuts += [(i + 160) / SR for _ in seg.feed(audio[i : i + 160])]
    assert len(cuts) == len(utts)
    for cut, u in zip(cuts, utts, strict=True):
        # Cut within min_silence_ms of the end of speech, plus slack for a soft last syllable.
        assert u["end"] - 1.0 < cut < u["end"] + 0.9, (cut, u["end"])
