"""The bounded queue between the audio tracks and the model, and the room API it posts lines to."""

import asyncio
import time
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
TRANSCRIBE = Histogram(
    "grill_stt_transcribe_seconds", "Model time per utterance.", buckets=(0.5, 1, 2, 3, 5, 8, 13, 20)
)
LATENCY = Histogram(
    "grill_stt_line_latency_seconds",
    "From the end of an utterance to its line stored in the room.",
    buckets=(1, 2, 3, 5, 7, 10, 15, 30),
)


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

    async def heartbeat(self, worker: str, rooms: list[str]) -> bool:
        try:
            r = await self._client.post("/api/stt/heartbeat", json={"worker": worker, "rooms": rooms})
        except httpx.HTTPError as e:
            log.warning("heartbeat_failed", error=str(e))
            return False
        if r.status_code != 204:
            log.warning("heartbeat_refused", status=r.status_code)
            return False
        return True


class Pipeline:
    """One consumer drains the queue: one utterance at a time gets every model thread."""

    def __init__(self, transcribe: Callable[[Audio], Line], api: RoomAPI, size: int) -> None:
        self._transcribe = transcribe
        self._api = api
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
        t0 = time.monotonic()
        line = await asyncio.to_thread(self._transcribe, u.audio)
        TRANSCRIBE.observe(time.monotonic() - t0)
        seconds = round(len(u.audio) / SR, 2)
        u.audio = u.audio[:0]  # the samples are no longer needed: drop them now, not at the next utterance
        if not line.text:
            LINES.labels("empty").inc()
            log.info("utterance_empty", room=u.room, seconds=seconds, language=line.language)
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
