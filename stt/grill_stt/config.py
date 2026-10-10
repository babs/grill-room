import socket

from pydantic import SecretStr
from pydantic_settings import BaseSettings, SettingsConfigDict


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
    cpu_threads: int = 4
    # Utterances waiting for the model; past it new ones are dropped and counted.
    queue_size: int = 8
    poll_seconds: float = 2.0
    heartbeat_seconds: float = 5.0
    port: int = 9100
    log_level: str = "info"
    worker_name: str = socket.gethostname()
