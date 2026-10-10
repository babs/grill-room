"""Streaming voice activity detection: cuts a live 16 kHz track into utterances on pauses."""

from collections import deque
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np
import numpy.typing as npt

SR = 16000
WINDOW = 512  # samples per Silero decision, 32 ms
CONTEXT = 64  # samples of the previous window Silero v6 expects in front of each window

Audio = npt.NDArray[np.float32]
Probs = Callable[[Audio], npt.NDArray[np.float32]]


class Silero:
    """faster-whisper's Silero v6 ONNX model, fed window by window with its state carried over.

    faster-whisper only runs it on a whole file and resets the state on every call, which would
    restart the model on each 10 ms LiveKit frame.
    """

    def __init__(self) -> None:
        import onnxruntime
        from faster_whisper.utils import get_assets_path

        opts = onnxruntime.SessionOptions()
        opts.inter_op_num_threads = 1
        opts.intra_op_num_threads = 1
        opts.log_severity_level = 4
        self._session = onnxruntime.InferenceSession(
            str(Path(get_assets_path()) / "silero_vad_v6.onnx"),
            providers=["CPUExecutionProvider"],
            sess_options=opts,
        )
        self._h = np.zeros((1, 1, 128), np.float32)
        self._c = np.zeros((1, 1, 128), np.float32)
        self._context = np.zeros(CONTEXT, np.float32)

    def __call__(self, windows: Audio) -> npt.NDArray[np.float32]:
        """Speech probability of each row of windows, shape (n, WINDOW)."""
        prev = np.vstack([self._context, windows[:-1, -CONTEXT:]])
        batch = np.hstack([prev, windows]).astype(np.float32)
        out, self._h, self._c = self._session.run(None, {"input": batch, "h": self._h, "c": self._c})
        self._context = windows[-1, -CONTEXT:].copy()
        return np.asarray(out, np.float32).reshape(-1)


@dataclass
class Segmenter:
    """Accumulates speech and emits it as one utterance once the speaker pauses.

    Thresholds follow faster-whisper's VadOptions. min_silence_ms was 500 (S1-bis) until spike S5:
    that cut 18 script lines into 32 utterances, each paying a full encoder pass; 700 and up keep a
    line whole there, and 800 leaves a margin for slower talkers.
    """

    probs: Probs
    threshold: float = 0.5
    neg_threshold: float = 0.35
    min_silence_ms: int = 800
    min_speech_ms: int = 400
    max_speech_s: float = 15.0
    pad_ms: int = 200
    _pending: Audio = field(default_factory=lambda: np.zeros(0, np.float32))
    _preroll: deque[Audio] = field(default_factory=deque)
    _speech: list[Audio] = field(default_factory=list)
    _voiced: int = 0
    _silent: int = 0

    def _windows(self, ms: float) -> int:
        return max(1, round(ms * SR / 1000 / WINDOW))

    def feed(self, samples: Audio) -> list[Audio]:
        """Adds samples; returns the utterances that ended in them."""
        buf = np.concatenate([self._pending, samples])
        n = len(buf) // WINDOW
        self._pending = buf[n * WINDOW :]
        if n == 0:
            return []
        windows = buf[: n * WINDOW].reshape(n, WINDOW)
        out: list[Audio] = []
        pad = self._windows(self.pad_ms)
        for w, p in zip(windows, self.probs(windows), strict=True):
            if not self._speech:
                self._preroll.append(w)
                if len(self._preroll) > pad:
                    self._preroll.popleft()
                if p >= self.threshold:
                    self._speech, self._voiced, self._silent = list(self._preroll), 1, 0
                    self._preroll.clear()
                continue
            self._speech.append(w)
            if p >= self.threshold:
                self._voiced += 1
                self._silent = 0
            elif p < self.neg_threshold:
                self._silent += 1
            if self._silent >= self._windows(self.min_silence_ms):
                out += self._emit()
            elif len(self._speech) >= self._windows(self.max_speech_s * 1000):
                out += self._emit()
        return out

    def flush(self) -> list[Audio]:
        """Emits the speech in progress, when the track ends."""
        return self._emit() if self._speech else []

    def _emit(self) -> list[Audio]:
        # Keep pad_ms of the trailing silence: Whisper clips the last word without it.
        keep = len(self._speech) - max(0, self._silent - self._windows(self.pad_ms))
        speech, voiced = self._speech[:keep], self._voiced
        self._speech, self._voiced, self._silent = [], 0, 0
        if voiced < self._windows(self.min_speech_ms):
            return []  # a click or a cough: Whisper turns these into invented sentences (S1-bis)
        return [np.concatenate(speech)]
