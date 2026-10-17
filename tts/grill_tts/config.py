import socket
from pathlib import Path
from typing import Literal

from pydantic import Field, SecretStr
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="APP_", env_file=".env", extra="ignore")

    # The room server's API, as the pod reaches it, and the pod's own token (GRILL_ROOM_TTS_TOKEN).
    room_url: str
    room_token: SecretStr
    # LiveKit's signalling URL (ws:// or wss://), and the key pair the bots' tokens are signed with.
    livekit_url: str
    livekit_api_key: str
    livekit_api_secret: SecretStr
    # pocket: Pocket TTS 3.3.0 with the weights baked in the image. fake: tones, for tests and e2e.
    engine: Literal["pocket", "fake"] = "pocket"
    models_dir: Path = Path("/models")
    # Torch threads per synthesis: one keeps up (RTF 0.47, S14), so two rooms fit 2 CPUs. Two threads
    # under a 2-CPU limit starved LiveKit's media threads: gaps in the voice e2e, none with one.
    cpu_threads: int = Field(default=1, ge=1)
    heartbeat_seconds: float = Field(default=2.0, gt=0)
    # A bot stays this long after its persona stopped speaking or the last listener left (013-AC15).
    leave_after_s: float = Field(default=60.0, ge=0)
    port: int = 9100
    log_level: str = "info"
    pod_name: str = socket.gethostname()
