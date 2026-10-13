import socket
from dataclasses import fields

from pydantic import SecretStr
from pydantic_settings import BaseSettings, SettingsConfigDict

from .pipeline import Plausibility
from .vad import Segmenter


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="APP_", env_file=".env", extra="ignore")

    # The room server's API, as the worker reaches it.
    room_url: str
    room_token: SecretStr
    # LiveKit's signalling URL (ws:// or wss://); its HTTP API is the same host.
    livekit_url: str
    livekit_api_key: str
    livekit_api_secret: SecretStr
    # A model size faster-whisper downloads, or the directory of a downloaded one.
    model: str = "small"
    # One CPU of a 4-CPU limit stays free for the event loop, the VAD and LiveKit's threads.
    cpu_threads: int = 3
    # Utterances waiting for the model; past it new ones are dropped and counted.
    queue_size: int = 8
    # Silence that ends an utterance, and the longest one before a forced cut.
    vad_min_silence_ms: int = Segmenter.min_silence_ms
    vad_max_speech_s: float = Segmenter.max_speech_s
    # Plausibility checks a transcript passes before it is posted (pipeline.Plausibility).
    min_avg_logprob: float = Plausibility.min_avg_logprob
    max_compression_ratio: float = Plausibility.max_compression_ratio
    max_no_speech_prob: float = Plausibility.max_no_speech_prob
    min_language_probability: float = Plausibility.min_language_probability
    short_utterance_s: float = Plausibility.short_utterance_s
    min_script_share: float = Plausibility.min_script_share
    # The room's glossary prompt as Whisper's initial_prompt. Off: on real speech it raised small's
    # WER (meetings 0.327 to 0.398, a talk 0.252 to 0.552) by looping on the prompt (spike S10b).
    glossary_prompt: bool = False
    poll_seconds: float = 2.0
    heartbeat_seconds: float = 5.0
    port: int = 9100
    log_level: str = "info"
    worker_name: str = socket.gethostname()

    def plausibility(self) -> Plausibility:
        return Plausibility(**{f.name: getattr(self, f.name) for f in fields(Plausibility)})
