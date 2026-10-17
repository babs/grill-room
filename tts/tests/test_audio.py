import numpy as np
import pytest

from grill_tts.audio import PEAK, TARGET_RMS, SilenceCap, Tempo, gain_for, level

SR = 24000


def tone(seconds: float, hz: float = 220.0, amp: float = 0.1) -> np.ndarray:
    t = np.arange(int(SR * seconds)) / SR
    return (amp * np.sin(2 * np.pi * hz * t)).astype(np.float32)


def silence(seconds: float) -> np.ndarray:
    return np.zeros(int(SR * seconds), np.float32)


def chunked(f: object, x: np.ndarray, size: int) -> np.ndarray:
    """Feeds x in chunks of size, as the engine streams it, then flushes."""
    out = [f.feed(x[i : i + size]) for i in range(0, len(x), size)]  # type: ignore[attr-defined]
    out.append(f.flush())  # type: ignore[attr-defined]
    return np.concatenate(out)


def gaps(x: np.ndarray, floor: float = 10 ** (-50 / 20)) -> list[float]:
    """The silent runs of x, in seconds, by 10 ms windows."""
    w = SR // 100
    runs, run = [], 0
    for i in range(0, len(x) - w + 1, w):
        if np.sqrt(np.mean(np.square(x[i : i + w]))) < floor:
            run += w
        elif run:
            runs.append(run / SR)
            run = 0
    return runs + ([run / SR] if run else [])


# 013-AC38: a 1 s and a 2.5 s pause come out at 300 ms at most, whatever the chunk size; speech and
# short pauses are untouched.
@pytest.mark.parametrize("chunk", [1920, 480, 7777])
def test_long_pauses_are_capped(chunk: int) -> None:
    x = np.concatenate([tone(0.5), silence(1.0), tone(0.4), silence(0.2), tone(0.3), silence(2.5), tone(0.2)])
    out = chunked(SilenceCap(SR), x, chunk)
    assert max(gaps(out)) <= 0.3 + 1e-9
    assert gaps(out)[1] == pytest.approx(0.2, abs=0.011), "a short pause keeps its length"
    assert np.sum(np.abs(out) > 0.05) == np.sum(np.abs(x) > 0.05), "speech is never cut"


# 013-AC38: the cap holds across two sentences fed to the same cap, the line's trailing and leading
# silences added up.
def test_the_cap_spans_sentence_joins() -> None:
    cap = SilenceCap(SR)
    a = chunked(cap, np.concatenate([tone(0.3), silence(0.25)]), 960)
    b = chunked(cap, np.concatenate([silence(0.25), tone(0.3)]), 960)
    assert max(gaps(np.concatenate([a, b]))) <= 0.3 + 1e-9


# 013-AC15: a voice's gain brings it to about -22 dBFS, and peaks stay under the cap.
@pytest.mark.parametrize("amp", [0.02, 0.6])
def test_level(amp: float) -> None:
    x = tone(1.0, amp=amp)
    y = level(x, gain_for(x))
    assert 20 * np.log10(np.sqrt(np.mean(np.square(y)))) == pytest.approx(-22, abs=0.5)
    assert np.abs(level(tone(0.1, amp=0.9), 5.0)).max() <= PEAK
    assert gain_for(silence(1.0)) == 1.0 and TARGET_RMS < PEAK


def dominant_hz(x: np.ndarray) -> float:
    spec = np.abs(np.fft.rfft(x * np.hanning(len(x))))
    return float(np.fft.rfftfreq(len(x), 1 / SR)[np.argmax(spec)])


# 013-AC37: at rate r a line lasts 1/r of its length within 5 %, at the same pitch, for tones,
# chunk sizes and rates it was not tuned on.
@pytest.mark.parametrize(
    ("rate", "hz", "chunk"), [(1.2, 220.0, 1920), (0.9, 330.0, 500), (1.3, 150.0, 4096), (1.05, 440.0, 960)]
)
def test_tempo_keeps_pitch(rate: float, hz: float, chunk: int) -> None:
    x = tone(3.0, hz=hz)
    y = chunked(Tempo(SR, rate), x, chunk)
    assert len(y) / SR == pytest.approx(3.0 / rate, rel=0.05)
    mid = y[len(y) // 4 : 3 * len(y) // 4]
    assert dominant_hz(mid) == pytest.approx(hz, rel=0.02)
    # No phase jump: the stretched tone stays close to a pure tone's level throughout.
    assert np.sqrt(np.mean(np.square(mid))) == pytest.approx(0.1 / np.sqrt(2), rel=0.1)


def test_rate_one_passes_through() -> None:
    x = tone(0.5)
    t = Tempo(SR, 1.0)
    assert np.array_equal(t.feed(x), x) and len(t.flush()) == 0


# The 2x upsampler keeps a tone's pitch and level, chunk by chunk, with no image above 12 kHz.
@pytest.mark.parametrize(("hz", "chunk"), [(440.0, 1920), (3000.0, 333)])
def test_double(hz: float, chunk: int) -> None:
    from grill_tts.audio import Double

    x = tone(1.0, hz=hz)
    y = chunked(Double(), x, chunk)
    assert len(y) >= 2 * len(x)
    mid = y[len(y) // 4 : 3 * len(y) // 4]
    spec = np.abs(np.fft.rfft(mid * np.hanning(len(mid))))
    freqs = np.fft.rfftfreq(len(mid), 1 / 48000)
    assert freqs[np.argmax(spec)] == pytest.approx(hz, rel=0.01)
    assert spec[freqs > 13000].max() < spec.max() / 100, "an alias of the tone above 12 kHz"
    assert np.sqrt(np.mean(np.square(mid))) == pytest.approx(0.1 / np.sqrt(2), rel=0.05)
