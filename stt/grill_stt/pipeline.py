"""The bounded queue between the audio tracks and the model, and the room API it posts lines to."""

import asyncio
import re
import time
import unicodedata
from collections import deque
from collections.abc import Callable, Coroutine, Sequence
from dataclasses import dataclass, field
from typing import Any

import httpx
import structlog
from prometheus_client import Counter, Gauge, Histogram

from .engines import EngineDied, ProcessEngine, Transcribe
from .line import Line
from .vad import SR, Audio

log = structlog.get_logger()

# (room, participant id) pairs the room server says a device transcribes (spec 010, AC14).
Flags = frozenset[tuple[str, int]]
# Below the room's 3 s flag lapse: a hung beat must not keep a lapsed flag past it (010-AC15).
HEARTBEAT_TIMEOUT_S = 2.0

QUEUE_DEPTH = Gauge("grill_stt_queue_depth", "Utterances waiting for an engine.")
QUEUE_WAIT = Histogram(
    "grill_stt_queue_wait_seconds",
    "From the VAD cut to an engine taking the utterance; the alert fires past 10 s (spec 007, AC19).",
    buckets=(0.1, 0.5, 1, 2, 5, 10, 20, 30, 60),
)
ENGINES_BUSY = Gauge("grill_stt_engines_busy", "Engine processes transcribing an utterance.")
DROPPED = Counter("grill_stt_utterances_dropped_total", "Utterances dropped because the queue was full.")
LINES = Counter("grill_stt_lines_total", "Transcribed utterances by outcome.", ["result"])
IMPLAUSIBLE = Counter(
    "grill_stt_lines_implausible_total",
    "Transcripts dropped as implausible, by first failed check.",
    ["reason"],
)
TRANSCRIBE = Histogram(
    "grill_stt_transcribe_seconds",
    "Model time per utterance.",
    ["engine"],
    buckets=(0.5, 1, 2, 3, 5, 8, 13, 20),
)
LANGUAGE = Counter(
    "grill_stt_lines_language_total",
    "Lines stored in the room, by guessed language: English lines in a French room show an English leak.",
    ["engine", "guess"],
)
LATENCY = Histogram(
    "grill_stt_line_latency_seconds",
    "From the end of an utterance to its line stored in the room.",
    ["engine"],
    buckets=(1, 2, 3, 5, 7, 10, 15, 30),
)
# Queue wait that logs a warning; the alert rule uses the same bound.
SLOW_WAIT_S = 10.0
# At most one "line lost" notice per speaker in this window, so a burst cannot flood the chat.
DROP_NOTICE_S = 30.0


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


def implausible_parakeet(line: Line, _seconds: float, p: Plausibility) -> str | None:
    """As implausible, without the checks on Whisper's confidence and language (spec 007, AC3)."""
    if line.compression_ratio > p.max_compression_ratio:
        return "repetition"
    # French and English both use Latin letters: anything else means the engine went astray.
    if script_share(line.text, "") < p.min_script_share:
        return "script"
    return None


# Words frequent in one language and absent from the other's: "a", "on", "en" or "pour" would not do.
FRENCH = frozenset(
    "le la les un une des du et est sont que qui pas ne je tu il elle nous vous ils elles ce cette ces dans "
    "avec sur au aux mais ou donc oui non très aussi bien fait faire être avoir mon ma mes ton ta son sa "
    "leur notre votre quoi comme alors parce puis voilà accord".split()
)
ENGLISH = frozenset(
    "the and is are was were of to that it this for with not you we they be have has do does but or so "
    "yes no very what which there their our your my his her will would can could should just about "
    "because then okay".split()
)


def guess_language(text: str) -> str:
    """fr, en or unknown, by function words; it feeds metrics and logs only (spec 007, AC4)."""
    words = re.findall(r"[^\W\d_]+", text.lower())
    fr = sum(w in FRENCH for w in words)
    en = sum(w in ENGLISH for w in words)
    if fr == en:
        return "unknown"
    return "fr" if fr > en else "en"


@dataclass
class Utterance:
    room: str
    participant_id: int
    audio: Audio
    # time.monotonic() when the speaker stopped; the VAD sees it min_silence_ms later.
    ended: float
    # When the VAD cut it, on the pipeline's clock: the start of its queue wait.
    queued: float = field(default=0.0, compare=False)

    @property
    def speaker(self) -> tuple[str, int]:
        return self.room, self.participant_id


class RoomAPI:
    """The room server's STT routes, with the worker's service token."""

    def __init__(self, client: httpx.AsyncClient) -> None:
        self._client = client
        self._bad_device_logged = False

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
            if r.status_code == 200:
                # 010-AC16: the room kept the device's copy of this line and dropped ours.
                log.info("line_deduped", room=room, participant=participant_id)
                return True
            # 404: the speaker's access was revoked meanwhile; the line goes with it.
            log.warning("line_refused", room=room, participant=participant_id, status=r.status_code)
            return False
        return False

    async def post_dropped(self, room: str, participant_id: int) -> bool:
        """Has the room say a line of this speaker was lost to a full queue (spec 007, AC8)."""
        try:
            r = await self._client.post(
                f"/api/rooms/{room}/voice/dropped", json={"participant_id": participant_id}
            )
        except httpx.HTTPError as e:
            log.warning("drop_notice_failed", room=room, error=str(e))
            return False
        if r.status_code != 201:
            log.warning("drop_notice_refused", room=room, participant=participant_id, status=r.status_code)
            return False
        return True

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

    async def heartbeat(
        self,
        worker: str,
        rooms: list[str],
        voices: dict[str, list[int]],
        engine: str,
        glossary: bool,
        timeout: float = HEARTBEAT_TIMEOUT_S,
    ) -> Flags:
        """The participants a device transcribes; a failed or late beat flags nobody."""
        try:
            # engine and glossary drive the audio menu's credit and the glossary tab (spec 007, AC13-14).
            body = {
                "worker": worker,
                "rooms": rooms,
                "voices": voices,
                "engine": engine,
                "glossary": glossary,
            }
            # httpx times each read alone: a body that trickles in would outlive the room's flag lapse.
            async with asyncio.timeout(timeout):
                r = await self._client.post("/api/stt/heartbeat", json=body)
        except (httpx.HTTPError, TimeoutError) as e:
            log.warning("heartbeat_failed", error=str(e))
            return frozenset()
        if r.status_code == 204:  # a server from before spec 010 flags nobody
            return frozenset()
        if r.status_code != 200:
            log.warning("heartbeat_refused", status=r.status_code)
            return frozenset()
        try:
            flags = frozenset((room, pid) for room, pids in r.json()["device"].items() for pid in pids)
            if not all(type(pid) is int for _, pid in flags):
                raise TypeError("participant id is not an int")
            self._bad_device_logged = False
            return flags
        except (ValueError, KeyError, TypeError, AttributeError) as e:
            if not self._bad_device_logged:
                self._bad_device_logged = True
                log.warning("heartbeat_device_unreadable", error=str(e))
            return frozenset()


class Pipeline:
    """One bounded queue drained by one task per engine process (spec 007, AC6-AC8).

    An utterance waits while its speaker's previous one is in an engine, so a speaker's lines post
    in speech order; other speakers' utterances pass it.
    """

    def __init__(
        self,
        engines: Sequence[Transcribe],
        api: RoomAPI,
        size: int,
        rules: Plausibility | None = None,
        use_prompt: bool = False,
        engine: str = "whisper",
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._engines = list(engines)
        self._api = api
        self._size = size
        self._use_prompt = use_prompt
        self._engine = engine
        self._judge = implausible_parakeet if engine == "parakeet" else implausible
        self._clock = clock
        # The last prompt read for each room: a room server that does not answer keeps it in use.
        self._prompts: dict[str, str] = {}
        self._rules = rules or Plausibility()
        self._pending: deque[Utterance] = deque()
        self._busy: set[tuple[str, int]] = set()
        # Set on every submit and every finished utterance: idle engine tasks look again.
        self._wake = asyncio.Event()
        self._notified: dict[tuple[str, int], float] = {}
        self._background: set[asyncio.Task[None]] = set()
        # Health: when an engine process was last seen up; None until the first one is.
        self._up_at: float | None = None
        QUEUE_DEPTH.set_function(lambda: len(self._pending))

    def submit(self, u: Utterance) -> bool:
        if len(self._pending) >= self._size:
            # ponytail: one queue for the one STT pod; "STT on demand" shards rooms across pods.
            DROPPED.inc()
            log.warning(
                "utterance_dropped", room=u.room, participant=u.participant_id, queue=len(self._pending)
            )
            self._notify_drop(u)
            return False
        u.queued = self._clock()
        self._pending.append(u)
        self._wake.set()
        return True

    def _notify_drop(self, u: Utterance) -> None:
        """Tells the room a line was lost (spec 007, AC8), once per speaker per DROP_NOTICE_S."""
        now = self._clock()
        last = self._notified.get(u.speaker)
        if last is not None and now - last < DROP_NOTICE_S:
            return
        # Only speakers inside the window matter: forget the others, so the map stays small.
        self._notified = {k: t for k, t in self._notified.items() if now - t < DROP_NOTICE_S}
        self._notified[u.speaker] = now
        self._spawn(self._api.post_dropped(u.room, u.participant_id))

    def _spawn(self, coro: Coroutine[Any, Any, Any]) -> None:
        # The event loop keeps only weak references to tasks: hold the fire-and-forget ones.
        task = asyncio.create_task(coro)
        self._background.add(task)
        task.add_done_callback(self._background.discard)

    def healthy(self) -> bool:
        """False once no engine process has been up for 60 s (spec 007, AC6)."""
        now = self._clock()
        if any(not isinstance(e, ProcessEngine) or e.alive() for e in self._engines):
            self._up_at = now
        return self._up_at is not None and now - self._up_at <= 60

    async def start(self) -> None:
        """Loads every engine process at once."""
        await asyncio.gather(
            *(asyncio.to_thread(e.start) for e in self._engines if isinstance(e, ProcessEngine))
        )
        self.healthy()

    def stop(self) -> None:
        for e in self._engines:
            if isinstance(e, ProcessEngine):
                e.stop()

    async def idle(self) -> None:
        """Returns once nothing is queued or in an engine."""
        while self._pending or self._busy:
            self._wake.clear()
            await self._wake.wait()

    async def _take(self) -> Utterance:
        while True:
            for i, u in enumerate(self._pending):
                if u.speaker not in self._busy:
                    del self._pending[i]
                    self._busy.add(u.speaker)
                    return u
            self._wake.clear()
            await self._wake.wait()

    async def run(self) -> None:
        async with asyncio.TaskGroup() as tg:
            for e in self._engines:
                tg.create_task(self._serve(e))

    async def _serve(self, engine: Transcribe) -> None:
        while True:
            u = await self._take()
            wait = self._clock() - u.queued
            QUEUE_WAIT.observe(wait)
            if wait > SLOW_WAIT_S:
                log.warning(
                    "queue_wait_slow", room=u.room, participant=u.participant_id, wait_s=round(wait, 1)
                )
            try:
                if isinstance(engine, ProcessEngine) and not engine.alive():
                    # Died while idle: restart it before it costs this utterance.
                    log.error("engine_died", pid=engine.pid, idle=True)
                    await self._restart(engine)
                await self.process(u, engine)
            except EngineDied as e:
                LINES.labels("error").inc()
                log.error("engine_died", room=u.room, participant=u.participant_id, error=str(e))
                await self._restart(engine)
            except Exception:
                # One utterance the model chokes on must not stop transcription for the room.
                LINES.labels("error").inc()
                log.exception("utterance_failed", room=u.room, participant=u.participant_id)
            finally:
                self._busy.discard(u.speaker)
                self._wake.set()

    async def _restart(self, engine: Transcribe) -> None:
        if not isinstance(engine, ProcessEngine):
            return
        delay = 1.0
        while True:
            try:
                await asyncio.to_thread(engine.start)
                log.info("engine_restarted", pid=engine.pid)
                return
            except Exception as e:  # a model that fails to load again: back off, the other engine serves
                log.error("engine_restart_failed", error=str(e), retry_s=delay)
                await asyncio.sleep(delay)
                delay = min(delay * 2, 30.0)

    async def process(self, u: Utterance, engine: Transcribe | None = None) -> None:
        engine = engine or self._engines[0]
        # Read before each utterance, so a glossary change applies from the next one (005-AC23).
        # Off by default: the prompt hurts small on real speech (spike S10b).
        prompt = await self._api.prompt(u.room) if self._use_prompt else ""
        if prompt is None:
            prompt = self._prompts.get(u.room, "")
        else:
            self._prompts[u.room] = prompt
        t0 = time.monotonic()
        ENGINES_BUSY.inc()
        try:
            line = await asyncio.to_thread(engine, u.audio, prompt)
        finally:
            ENGINES_BUSY.dec()
        TRANSCRIBE.labels(self._engine).observe(time.monotonic() - t0)
        seconds = round(len(u.audio) / SR, 2)
        u.audio = u.audio[:0]  # the samples are no longer needed: drop them now, not at the next utterance
        if not line.text.strip():
            LINES.labels("empty").inc()
            log.info("utterance_empty", room=u.room, seconds=seconds, detected=line.language)
            return
        reason = self._judge(line, seconds, self._rules)
        if reason:
            LINES.labels("implausible").inc()
            IMPLAUSIBLE.labels(reason).inc()
            log.info(
                "line_implausible",
                room=u.room,
                participant=u.participant_id,
                reason=reason,
                seconds=seconds,
                detected=line.language,
                language_probability=round(line.language_probability, 2),
                avg_logprob=round(line.avg_logprob, 2),
                no_speech_prob=round(line.no_speech_prob, 2),
                compression_ratio=round(line.compression_ratio, 2),
                chars=len(line.text),
            )
            return
        ok = await self._api.post_line(u.room, u.participant_id, line.text)
        LINES.labels("posted" if ok else "failed").inc()
        language = guess_language(line.text)
        if ok:
            LANGUAGE.labels(self._engine, language).inc()
        if ok:
            LATENCY.labels(self._engine).observe(time.monotonic() - u.ended)
        # The text is personal data: logs carry its size and language, never the words.
        log.info(
            "line_done",
            room=u.room,
            participant=u.participant_id,
            posted=ok,
            seconds=seconds,
            language=language,
            # Whisper's own label; parakeet gives none.
            detected=line.language,
            chars=len(line.text),
            latency_s=round(time.monotonic() - u.ended, 2),
        )
