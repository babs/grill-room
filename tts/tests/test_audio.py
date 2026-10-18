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


def pcm(seconds: float, amp: float) -> np.ndarray:
    return (tone(seconds, amp=amp) * 32767).astype(np.int16)


# 013-AC22: an onset is caught within two 10 ms frames at speech level; the gaps between words do
# not end it; quiet past the hang ends it; a whisper of echo under the floor never starts it.
@pytest.mark.parametrize("amp", [0.05, 0.3])
def test_onset(amp: float) -> None:
    from grill_tts.audio import Onset

    o = Onset()
    frames = [(pcm(0.01, 0.002), "echo")] * 30 + [(pcm(0.01, amp), "word")] * 8
    frames += (
        [(pcm(0.01, 0.0), "gap")] * 20 + [(pcm(0.01, amp), "word")] * 8 + [(pcm(0.01, 0.0), "quiet")] * 50
    )
    changes = [(i, kind, ch) for i, (f, kind) in enumerate(frames) if (ch := o.feed(f, 10.0)) is not None]
    assert changes[0] == (33, "word", True), "not by the 4th loud frame of the first word"
    assert [c[2] for c in changes] == [True, False], "a 200 ms gap between words ended the speech"
    assert changes[1] == (30 + 8 + 20 + 8 + 39, "quiet", False), "not 400 ms after the last word"


# 013-AC22: clicks of 10 to 30 ms, shorter than the 40 ms the onset needs, never start speech.
@pytest.mark.parametrize("burst", [1, 2, 3])
def test_onset_ignores_clicks(burst: int) -> None:
    from grill_tts.audio import Onset

    o = Onset()
    for _ in range(20):
        for f in [pcm(0.01, 0.5)] * burst + [pcm(0.01, 0.0)] * 15:
            assert o.feed(f, 10.0) is None, f"a {burst * 10} ms click started speech"


# 013-AC22: syllables that dip under the floor for a frame still start speech within 60 ms.
def test_onset_through_dips() -> None:
    from grill_tts.audio import Onset

    o = Onset()
    choppy = [pcm(0.01, 0.2), pcm(0.01, 0.2), pcm(0.01, 0.0)] * 4
    got = [o.feed(f, 10.0) for f in choppy]
    assert True in got[:6], "two loud frames, one dip, repeated, did not start speech within 60 ms"


# 013-AC35: during a listener's first 4 s of hearing a persona, a 60 ms burst at -20 dBFS, the
# echo canceller's warm-up leak (S16), is not an onset; past those 4 s, or with no persona playing,
# it is.
def test_onset_warm_up_ignores_echo_burst() -> None:
    from grill_tts.audio import Onset

    burst = [pcm(0.01, 0.1 * np.sqrt(2))] * 6 + [pcm(0.01, 0.0)] * 50
    o = Onset()
    warm = [o.feed(f, 10.0, playing=True) for f in [pcm(0.01, 0.0)] * 100 + burst]
    assert True not in warm, "a 60 ms burst 1 s into the persona's audio started speech"
    late = [o.feed(f, 10.0, playing=True) for f in [pcm(0.01, 0.0)] * 300 + burst]
    assert True in late, "the same burst past 4 s of persona audio did not start speech"
    o = Onset()
    assert True in [o.feed(f, 10.0) for f in burst], "the burst with no persona playing was ignored"


# 013-AC22: a human who speaks during the warm-up still cuts in, by the 9th loud frame.
def test_onset_warm_up_catches_speech() -> None:
    from grill_tts.audio import Onset

    o = Onset()
    got = [o.feed(f, 10.0, playing=True) for f in [pcm(0.01, 0.0)] * 100 + [pcm(0.01, 0.1)] * 30]
    assert got.index(True) == 100 + 8, "sustained speech in the warm-up did not start by 90 ms"
