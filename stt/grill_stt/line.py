"""One transcript and the confidence the pipeline judges it on."""

from dataclasses import dataclass


@dataclass(frozen=True)
class Line:
    text: str
    language: str
    # Whisper's confidence: in the language, and over the kept segments the lowest mean token
    # log-probability, the highest chance of no speech and the highest gzip ratio (repetition). The
    # pipeline judges plausibility on them.
    language_probability: float = 1.0
    avg_logprob: float = 0.0
    no_speech_prob: float = 0.0
    compression_ratio: float = 1.0
