"""The pod's metrics (013-AC20); docs/architecture.md lists them with the room server's."""

from prometheus_client import Counter, Gauge, Histogram

RECEIVED = Counter("grill_tts_lines_received_total", "Lines the pod took from the room server.")
SPOKEN = Counter("grill_tts_lines_spoken_total", "Lines said to their end.")
INTERRUPTED = Counter("grill_tts_lines_interrupted_total", "Times a human cut a persona off.")
RESUMED = Counter(
    "grill_tts_lines_resumed_total", "Interrupted lines picked up again from the sentence's start."
)
DROPPED = Counter("grill_tts_lines_dropped_total", "Lines dropped from speech, by reason.", ["reason"])
QUEUE_WAIT = Histogram(
    "grill_tts_queue_wait_seconds",
    "From a line's arrival to its start.",
    buckets=(0.1, 0.5, 1, 2, 5, 10, 20, 40, 80),
)
FIRST_AUDIO = Histogram(
    "grill_tts_first_audio_seconds",
    "From a line's arrival to its first frame on the track (013-AC17).",
    buckets=(0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5),
)
STOP = Histogram(
    "grill_tts_stop_seconds",
    "From LiveKit naming a human speaker to the persona's audio cleared (013-AC22).",
    buckets=(0.01, 0.02, 0.05, 0.1, 0.2, 0.3, 0.5),
)
RTF = Histogram(
    "grill_tts_synthesis_rtf",
    "Synthesis time over audio time, per span.",
    buckets=(0.1, 0.2, 0.3, 0.4, 0.5, 0.7, 1, 1.5),
)
UNDERRUNS = Counter(
    "grill_tts_underruns_total", "Gaps over 100 ms inside a sentence: synthesis fell behind (013-AC17)."
)
BOTS = Gauge("grill_tts_bots", "Persona bots in a call now.")
HUMAN_TRACKS = Gauge(
    "grill_tts_human_tracks",
    "Humans' audio tracks the bots hear: LiveKit names a speaker to a bot only then (S14).",
)
BOTS_JOINED = Counter("grill_tts_bots_joined_total", "Persona bots that joined a call.")

for reason in ("interrupted", "stale", "no_listener", "no_service", "error"):
    DROPPED.labels(reason)
