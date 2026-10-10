"""faster-whisper on CPU: small int8, language detected per utterance (spike S1-bis)."""

from dataclasses import dataclass

from faster_whisper import WhisperModel

from .vad import Audio


@dataclass(frozen=True)
class Line:
    text: str
    language: str


class Transcriber:
    def __init__(self, model: str, cpu_threads: int) -> None:
        self._model = WhisperModel(model, device="cpu", compute_type="int8", cpu_threads=cpu_threads)

    def __call__(self, audio: Audio) -> Line:
        # language=None: each utterance is detected on its own, so FR and EN mix freely (D16).
        segments, info = self._model.transcribe(
            audio, beam_size=1, without_timestamps=True, condition_on_previous_text=False, vad_filter=False
        )
        # Whisper's own silence rule: a segment that is likely no speech and decoded with low
        # confidence is a hallucination ("merci d'avoir regardé cette vidéo").
        kept = [s.text.strip() for s in segments if not (s.no_speech_prob > 0.6 and s.avg_logprob < -1.0)]
        return Line(" ".join(t for t in kept if t), info.language)
