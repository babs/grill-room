import wave
from pathlib import Path

import numpy as np

from grill_scenarios import audio, schema
from tests.test_schema import base


def test_timeline_gaps_and_overlap() -> None:
    rnd = schema.VoiceRound.model_validate(
        {
            "lead": 4,
            "lines": [
                {"by": "A", "lang": "en", "text": "a"},
                {"by": "B", "lang": "en", "text": "b"},
                {"by": "A", "lang": "en", "text": "c", "gap": -1.5},
                {"by": "B", "lang": "en", "text": "d"},
            ],
        }
    )
    # The overlapping line starts 1.5 s before b ends and does not push d back past b's end.
    assert audio.timeline(rnd, 1.2, [2, 3, 1, 1]) == [(4.0, 6.0), (7.2, 10.2), (8.7, 9.7), (11.4, 12.4)]


def test_resample_keeps_tone_and_length() -> None:
    sr = 22050
    t = np.arange(sr) / sr
    out = audio.resample(np.sin(2 * np.pi * 440 * t).astype(np.float32), sr)
    assert len(out) == audio.SR
    peak = np.argmax(np.abs(np.fft.rfft(out)))
    assert abs(peak - 440) <= 1
    assert abs(float(np.abs(out).max()) - 1) < 0.05


# 004-AC16: per-speaker tracks at 16 kHz on the round's timeline, silence while others talk.
def test_render(tmp_path: Path) -> None:
    d = base()
    d["participants"][1]["voices"] = {"en": {"model": "en_GB-cori-medium"}}
    d["steps"][1]["voice"]["lines"].append({"by": "Bob", "lang": "en", "text": "Hi."})
    s = schema.Scenario.model_validate(d)
    calls: list[tuple[str, int | None, str]] = []

    def synth(model: str, speaker: int | None, text: str) -> audio.Audio:
        calls.append((model, speaker, text))
        return np.full(audio.SR, 0.5, np.float32)

    (rnd,) = audio.render(s, tmp_path, synth)
    assert calls == [("en_US-joe-medium", None, "Hello."), ("en_GB-cori-medium", None, "Hi.")]
    assert [(ln["by"], ln["start"], ln["end"]) for ln in rnd["lines"]] == [
        ("Ann", 10.0, 11.0),
        ("Bob", 12.2, 13.2),
    ]
    with wave.open(rnd["tracks"]["Bob"]) as w:
        assert w.getframerate() == 16000 and w.getnchannels() == 1
        pcm = np.frombuffer(w.readframes(w.getnframes()), "<i2")
    assert len(pcm) == int((13.2 + audio.TAIL_S) * audio.SR)
    assert not pcm[: int(12.2 * audio.SR) - 1].any() and pcm[int(12.5 * audio.SR)] > 0
