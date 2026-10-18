"""The audio chain between the engine and the track: level, silence cap and tempo, all streaming."""

from collections import deque

import numpy as np
from numpy.typing import NDArray
from scipy.signal import firwin, lfilter

Samples = NDArray[np.float32]

# A listener's echo canceller leaks 40-80 ms bursts of a persona over its first seconds of far-end
# audio, then converges (S16): during that warm-up an onset must last longer than a leak.
WARM_MS = 4000
ECHO_WINDOW, ECHO_START = 16, 9
# About -22 dBFS RMS, what S14 levelled each sentence to (013-AC15).
TARGET_RMS = 10 ** (-22 / 20)
PEAK = 0.95


def level(x: Samples, gain: float) -> Samples:
    """Applies a voice's gain and caps the peaks: voices differ in loudness up to 10 dB (S14)."""
    out: Samples = np.clip(x * gain, -PEAK, PEAK).astype(np.float32)
    return out


def gain_for(calibration: Samples) -> float:
    """The gain that brings a voice's calibration render to TARGET_RMS."""
    rms = float(np.sqrt(np.mean(np.square(calibration, dtype=np.float64)))) if len(calibration) else 0.0
    return TARGET_RMS / rms if rms > 1e-5 else 1.0


class SilenceCap:
    """Shortens every silence to at most max_gap_ms, across chunk and sentence boundaries (013-AC38).

    Silence is judged on 10 ms windows under -50 dBFS RMS, after the gain: quiet consonants sit well
    above it, so speech is never cut, only the tail of a long pause.
    """

    def __init__(self, sr: int, max_gap_ms: int = 300, floor_dbfs: float = -50.0) -> None:
        self._win = sr // 100
        self._max_silent = max_gap_ms * sr // 1000
        self._floor = 10 ** (floor_dbfs / 20)
        self._silent = 0
        self._rest = np.zeros(0, np.float32)

    def feed(self, x: Samples) -> Samples:
        x = np.concatenate([self._rest, x])
        n = len(x) // self._win * self._win
        self._rest = x[n:]
        out = []
        for w in x[:n].reshape(-1, self._win):
            if float(np.sqrt(np.mean(np.square(w)))) >= self._floor:
                self._silent = 0
                out.append(w)
            elif self._silent + self._win <= self._max_silent:
                self._silent += self._win
                out.append(w)
        return np.concatenate(out) if out else np.zeros(0, np.float32)

    def flush(self) -> Samples:
        rest, self._rest = self._rest, np.zeros(0, np.float32)
        return rest if self._silent < self._max_silent else rest[:0]


class Tempo:
    """WSOLA time stretch: rate 1.2 says the same in 1/1.2 of the time, at the same pitch (013-AC37).

    Hann frames of 40 ms at 50 % overlap; each next frame is taken within 10 ms of its nominal place
    where it best continues the previous one, so the waveform joins without a phase jump.
    """

    def __init__(self, sr: int, rate: float, frame_ms: int = 40, tolerance_ms: int = 10) -> None:
        self.rate = rate
        n = sr * frame_ms // 1000 // 2 * 2
        self._n, self._hs = n, n // 2
        self._ha = round(self._hs * rate)
        self._tol = sr * tolerance_ms // 1000
        # A periodic Hann sums to one at 50 % overlap.
        self._win = (0.5 - 0.5 * np.cos(2 * np.pi * np.arange(n) / n)).astype(np.float32)
        self._buf = np.zeros(0, np.float32)
        self._base = 0  # absolute index of self._buf[0]
        self._k = 0  # frames placed
        self._prev = -1  # absolute start of the previous frame
        self._tail = np.zeros(self._hs, np.float32)
        # Samples in and out: the flush stops where 1/rate of the input is reached.
        self._in = self._out = 0

    def _need(self) -> int:
        nominal = self._k * self._ha + self._tol + self._n
        return max(nominal, self._prev + self._hs + self._n) if self._prev >= 0 else self._n + self._tol

    def _frame(self) -> Samples:
        nominal = self._k * self._ha
        if self._prev < 0:
            pos = nominal
        else:
            ref = self._buf[self._prev + self._hs - self._base : self._prev + self._hs + self._n - self._base]
            lo = max(nominal - self._tol, self._base)
            span = self._buf[lo - self._base : nominal + self._tol + self._n - self._base]
            # Near the end of the input there is nothing left to search: keep the nominal place.
            if len(ref) == self._n and len(span) >= self._n:
                pos = lo + int(np.argmax(np.correlate(span, ref, mode="valid")))
            else:
                pos = nominal
        seg = self._buf[pos - self._base : pos - self._base + self._n]
        seg = np.pad(seg, (0, self._n - len(seg))) * self._win
        out: Samples = (self._tail + seg[: self._hs]).astype(np.float32)
        self._tail = seg[self._hs :].astype(np.float32)
        self._prev, self._k = pos, self._k + 1
        keep = min(self._k * self._ha - self._tol, self._prev + self._hs)
        if keep > self._base:
            self._buf, self._base = self._buf[keep - self._base :], keep
        return out

    def feed(self, x: Samples) -> Samples:
        if self.rate == 1.0:
            return x
        self._buf = np.concatenate([self._buf, x])
        self._in += len(x)
        out = []
        while self._base + len(self._buf) >= self._need():
            out.append(self._frame())
        y: Samples = np.concatenate(out).astype(np.float32) if out else np.zeros(0, np.float32)
        self._out += len(y)
        return y

    def flush(self) -> Samples:
        if self.rate == 1.0:
            return np.zeros(0, np.float32)
        out = []
        end = self._base + len(self._buf)
        while self._k * self._ha < end:
            out.append(self._frame())
        out.append(self._tail)
        y: Samples = np.concatenate(out).astype(np.float32)[: max(0, round(self._in / self.rate) - self._out)]
        self._buf, self._tail = np.zeros(0, np.float32), np.zeros(self._hs, np.float32)
        return y


class Double:
    """Streaming 2x upsampler, the engine's 24 kHz to the track's 48 kHz: zero-stuffing, then a
    low-pass at the old Nyquist. Plain numpy, so the synthesis thread never waits on LiveKit's FFI."""

    _taps = 2 * np.asarray(firwin(63, 0.5), np.float64)

    def __init__(self) -> None:
        self._zi = np.zeros(len(self._taps) - 1)

    def feed(self, x: Samples) -> Samples:
        if not len(x):
            return np.zeros(0, np.float32)
        up = np.zeros(2 * len(x))
        up[::2] = x
        y, self._zi = lfilter(self._taps, 1.0, up, zi=self._zi)
        out: Samples = y.astype(np.float32)
        return out

    def flush(self) -> Samples:
        return self.feed(np.zeros(len(self._taps) // 4, np.float32))


class Onset:
    """Energy voice activity on a human's track, faster than LiveKit's speaker updates (013-AC22).

    Speech starts once `start` of the last `window` 10 ms frames were above `floor_dbfs`: a word is
    caught within 60 ms even when its syllables dip under the floor, while a key click or a cough
    shorter than 40 ms is not speech. It ends after `hang_ms` below the floor, so the gaps between
    words do not end it.
    While a persona plays, a track's first WARM_MS of it need ECHO_START of the last ECHO_WINDOW
    frames instead.
    """

    def __init__(
        self, floor_dbfs: float = -42.0, window: int = 6, start: int = 4, hang_ms: int = 400
    ) -> None:
        self._floor = 10 ** (floor_dbfs / 20)
        self._recent: deque[bool] = deque(maxlen=max(window, ECHO_WINDOW))
        self._window, self._start, self._hang_ms = window, start, hang_ms
        self._quiet = 0.0
        self._heard_ms = 0.0
        self.speaking = False

    def feed(self, pcm: NDArray[np.int16], ms: float, playing: bool = False) -> bool | None:
        """Takes one frame of ms; returns the new state when it changed, None otherwise. playing:
        a persona's echo may be in this frame."""
        rms = float(np.sqrt(np.mean(np.square(pcm.astype(np.float32) / 32768)))) if len(pcm) else 0.0
        loud = rms >= self._floor
        self._recent.append(loud)
        self._quiet = 0.0 if loud else self._quiet + ms
        window, start = self._window, self._start
        if playing and self._heard_ms < WARM_MS:
            self._heard_ms += ms
            window, start = ECHO_WINDOW, ECHO_START
        if not self.speaking and sum(list(self._recent)[-window:]) >= start:
            self.speaking = True
            return True
        if self.speaking and self._quiet >= self._hang_ms:
            self.speaking = False
            self._recent.clear()
            return False
        return None
