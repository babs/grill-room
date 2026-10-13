"""The bounded queue between the audio tracks and the model, and the room API it posts lines to."""

import asyncio
import time
import unicodedata
from collections.abc import Callable
from dataclasses import dataclass

import httpx
import structlog
from prometheus_client import Counter, Gauge, Histogram

from .transcribe import Line
from .vad import SR, Audio

log = structlog.get_logger()

QUEUE_DEPTH = Gauge("grill_stt_queue_depth", "Utterances waiting for the model.")
DROPPED = Counter("grill_stt_utterances_dropped_total", "Utterances dropped because the queue was full.")
LINES = Counter("grill_stt_lines_total", "Transcribed utterances by outcome.", ["result"])
IMPLAUSIBLE = Counter(
    "grill_stt_lines_implausible_total",
    "Transcripts dropped as implausible, by first failed check.",
    ["reason"],
)
TRANSCRIBE = Histogram(
    "grill_stt_transcribe_seconds", "Model time per utterance.", buckets=(0.5, 1, 2, 3, 5, 8, 13, 20)
)
LATENCY = Histogram(
    "grill_stt_line_latency_seconds",
    "From the end of an utterance to its line stored in the room.",
    buckets=(1, 2, 3, 5, 7, 10, 15, 30),
)


# The script each language is written in, by the first word of its letters' Unicode names; any
# language not listed is written in Latin letters.
SCRIPTS = {
    "zh": {"CJK"},
    "yue": {"CJK"},
    "ja": {"CJK", "HIRAGANA", "KATAKANA"},
    "ko": {"HANGUL"},
    "ru": {"CYRILLIC"},
    "uk": {"CYRILLIC"},
    "be": {"CYRILLIC"},
    "bg": {"CYRILLIC"},
    "mk": {"CYRILLIC"},
    "sr": {"CYRILLIC", "LATIN"},
    "kk": {"CYRILLIC"},
    "mn": {"CYRILLIC"},
    "tt": {"CYRILLIC"},
    "ba": {"CYRILLIC"},
    "tg": {"CYRILLIC"},
    "el": {"GREEK"},
    "he": {"HEBREW"},
    "yi": {"HEBREW"},
    "ar": {"ARABIC"},
    "fa": {"ARABIC"},
    "ur": {"ARABIC"},
    "ps": {"ARABIC"},
    "sd": {"ARABIC"},
    "hi": {"DEVANAGARI"},
    "mr": {"DEVANAGARI"},
    "ne": {"DEVANAGARI"},
    "sa": {"DEVANAGARI"},
    "bn": {"BENGALI"},
    "as": {"BENGALI"},
    "pa": {"GURMUKHI"},
    "gu": {"GUJARATI"},
    "ta": {"TAMIL"},
    "te": {"TELUGU"},
    "kn": {"KANNADA"},
    "ml": {"MALAYALAM"},
    "si": {"SINHALA"},
    "th": {"THAI"},
    "lo": {"LAO"},
    "my": {"MYANMAR"},
    "km": {"KHMER"},
    "bo": {"TIBETAN"},
    "ka": {"GEORGIAN"},
    "hy": {"ARMENIAN"},
    "am": {"ETHIOPIC"},
}


@dataclass(frozen=True)
class Plausibility:
    """Thresholds past which a transcript is dropped rather than posted (spike S5, problem 6)."""

    # Whisper's own failure thresholds: mean token log-probability, and gzip ratio (repetition).
    min_avg_logprob: float = -1.0
    max_compression_ratio: float = 2.4
    # Whisper keeps a likely-silent segment it decoded confidently: only an unsure language too
    # makes it noise.
    max_no_speech_prob: float = 0.6
    # An unsure language on a long utterance is babble: real speech that long is detected at 0.8 and
    # up (S5). A short "oui" often comes out as English at 0.3, so short utterances are spared.
    min_language_probability: float = 0.5
    short_utterance_s: float = 2.0
    # Share of a line's letters that must be in the detected language's script.
    min_script_share: float = 0.9


def script_share(text: str, language: str) -> float:
    letters = [c for c in text if c.isalpha()]
    if not letters:
        return 1.0
    want = SCRIPTS.get(language, {"LATIN"})
    return sum(unicodedata.name(c, "?").split(" ")[0] in want for c in letters) / len(letters)


def implausible(line: Line, seconds: float, p: Plausibility) -> str | None:
    """The first check a transcript of seconds of audio fails, or None when it is fit to post."""
    if line.avg_logprob < p.min_avg_logprob:
        return "avg_logprob"
    if line.compression_ratio > p.max_compression_ratio:
        return "repetition"
    unsure = line.language_probability < p.min_language_probability
    if line.no_speech_prob > p.max_no_speech_prob and unsure:
        return "no_speech"
    if unsure and seconds >= p.short_utterance_s:
        return "language_probability"
    if script_share(line.text, line.language) < p.min_script_share:
        return "script"
    return None


@dataclass
class Utterance:
    room: str
    participant_id: int
    audio: Audio
    # time.monotonic() when the speaker stopped; the VAD sees it min_silence_ms later.
    ended: float


class RoomAPI:
    """The room server's STT routes, with the worker's service token."""

    def __init__(self, client: httpx.AsyncClient) -> None:
        self._client = client

    async def post_line(self, room: str, participant_id: int, text: str) -> bool:
        """Stores one line; False when the room refused it or could not be reached."""
        path = f"/api/rooms/{room}/voice/lines"
        body = {"participant_id": participant_id, "body": text}
        for attempt in range(3):
            try:
                r = await self._client.post(path, json=body)
            except (httpx.ConnectError, httpx.ConnectTimeout) as e:
                # Not sent, so safe to retry; a timeout after sending may have stored it already.
                log.warning("line_post_retry", room=room, attempt=attempt, error=str(e))
                await asyncio.sleep(0.5 * 2**attempt)
                continue
            except httpx.HTTPError as e:
                log.warning("line_post_failed", room=room, error=str(e))
                return False
            if r.status_code == 201:
                return True
            # 404: the speaker's access was revoked meanwhile; the line goes with it.
            log.warning("line_refused", room=room, participant=participant_id, status=r.status_code)
            return False
        return False

    async def prompt(self, room: str) -> str | None:
        """The room's Whisper prompt (005-AC23); None when it could not be read."""
        try:
            r = await self._client.get(f"/api/stt/rooms/{room}/prompt")
            if r.status_code == 200:
                prompt = r.json()["prompt"]
                if isinstance(prompt, str):
                    return prompt
            log.warning("prompt_refused", room=room, status=r.status_code)
        except (httpx.HTTPError, ValueError, KeyError, TypeError) as e:
            log.warning("prompt_failed", room=room, error=str(e))
        return None

    async def heartbeat(self, worker: str, rooms: list[str], voices: dict[str, list[int]]) -> bool:
        try:
            body = {"worker": worker, "rooms": rooms, "voices": voices}
            r = await self._client.post("/api/stt/heartbeat", json=body)
        except httpx.HTTPError as e:
            log.warning("heartbeat_failed", error=str(e))
            return False
        if r.status_code != 204:
            log.warning("heartbeat_refused", status=r.status_code)
            return False
        return True


class Pipeline:
    """One consumer drains the queue: one utterance at a time gets every model thread."""

    def __init__(
        self,
        transcribe: Callable[[Audio, str], Line],
        api: RoomAPI,
        size: int,
        rules: Plausibility | None = None,
        use_prompt: bool = False,
    ) -> None:
        self._transcribe = transcribe
        self._api = api
        self._use_prompt = use_prompt
        # The last prompt read for each room: a room server that does not answer keeps it in use.
        self._prompts: dict[str, str] = {}
        self._rules = rules or Plausibility()
        self._queue: asyncio.Queue[Utterance] = asyncio.Queue(size)
        QUEUE_DEPTH.set_function(self._queue.qsize)

    def submit(self, u: Utterance) -> bool:
        try:
            self._queue.put_nowait(u)
        except asyncio.QueueFull:
            # ponytail: one queue per worker; one STT pod per room (D2) makes it the room's queue.
            DROPPED.inc()
            log.warning(
                "utterance_dropped", room=u.room, participant=u.participant_id, queue=self._queue.qsize()
            )
            return False
        return True

    async def run(self) -> None:
        while True:
            u = await self._queue.get()
            try:
                await self.process(u)
            except Exception:
                # One utterance the model chokes on must not stop transcription for the room.
                LINES.labels("error").inc()
                log.exception("utterance_failed", room=u.room, participant=u.participant_id)
            finally:
                self._queue.task_done()

    async def process(self, u: Utterance) -> None:
        # Read before each utterance, so a glossary change applies from the next one (005-AC23).
        # Off by default: the prompt hurts small on real speech (spike S10b).
        prompt = await self._api.prompt(u.room) if self._use_prompt else ""
        if prompt is None:
            prompt = self._prompts.get(u.room, "")
        else:
            self._prompts[u.room] = prompt
        t0 = time.monotonic()
        line = await asyncio.to_thread(self._transcribe, u.audio, prompt)
        TRANSCRIBE.observe(time.monotonic() - t0)
        seconds = round(len(u.audio) / SR, 2)
        u.audio = u.audio[:0]  # the samples are no longer needed: drop them now, not at the next utterance
        if not line.text:
            LINES.labels("empty").inc()
            log.info("utterance_empty", room=u.room, seconds=seconds, language=line.language)
            return
        reason = implausible(line, seconds, self._rules)
        if reason:
            LINES.labels("implausible").inc()
            IMPLAUSIBLE.labels(reason).inc()
            log.info(
                "line_implausible",
                room=u.room,
                participant=u.participant_id,
                reason=reason,
                seconds=seconds,
                language=line.language,
                language_probability=round(line.language_probability, 2),
                avg_logprob=round(line.avg_logprob, 2),
                no_speech_prob=round(line.no_speech_prob, 2),
                compression_ratio=round(line.compression_ratio, 2),
                chars=len(line.text),
            )
            return
        ok = await self._api.post_line(u.room, u.participant_id, line.text)
        LINES.labels("posted" if ok else "failed").inc()
        if ok:
            LATENCY.observe(time.monotonic() - u.ended)
        # The text is personal data: logs carry its size and language, never the words.
        log.info(
            "line_done",
            room=u.room,
            participant=u.participant_id,
            posted=ok,
            seconds=seconds,
            language=line.language,
            chars=len(line.text),
            latency_s=round(time.monotonic() - u.ended, 2),
        )
