"""One grill room's voice: its queue of lines, who speaks in turn, and when a human cuts in.

LiveKit stays outside: a persona's bot is an Out, the clock is injected, so the turn-taking runs in
tests without a call (013-AC21 to AC24).
"""

import asyncio
import threading
import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Protocol

import numpy as np
import structlog

from . import metrics
from .audio import Double, Samples, SilenceCap, Tempo, level
from .engine import Engine
from .textlayer import Span, TextLayer

log = structlog.get_logger()

TRACK_SR = 48000
FRAME = TRACK_SR // 50  # 20 ms
FRAME_S = FRAME / TRACK_SR
# A frame the player waited this long for, mid-sentence, left the track's 100 ms queue dry.
UNDERRUN_S = 0.1


class Out(Protocol):
    """A persona's audio track."""

    async def play(self, frame: bytes) -> None:
        """Queues one 20 ms frame, 48 kHz mono int16; waits while the track's queue is full."""
        ...

    def clear(self) -> None:
        """Drops the audio queued and not yet sent."""
        ...


class Clock:
    def now(self) -> float:
        return time.monotonic()

    async def sleep(self, s: float) -> None:
        await asyncio.sleep(s)


@dataclass
class Line:
    """A line from the room server's stream (013-AC14), stamped when it arrived here."""

    seq: int
    room: str
    persona_id: int
    persona: str
    voice: str
    rate: float
    text: str
    quiet_ms: int
    resume_s: int
    queue_s: int
    received: float = 0.0

    @classmethod
    def parse(cls, d: dict[str, Any], received: float, wall: float | None = None) -> Line:
        """received is the pod's clock at arrival. The line's age at the server (queued_at against
        wall, the wall clock now) moves it back: a line delivered late keeps its room's queue
        maximum from when it was queued (013-AC24)."""
        if "queued_at" in d:
            queued = datetime.fromisoformat(str(d["queued_at"]).replace("Z", "+00:00")).timestamp()
            age = (time.time() if wall is None else wall) - queued
            received -= min(max(age, 0.0), float(d["queue_s"]))
        return cls(
            seq=int(d["seq"]),
            room=str(d["room"]),
            persona_id=int(d["persona_id"]),
            persona=str(d["persona"]),
            voice=str(d["voice"]),
            rate=float(d.get("rate") or 1.0),
            text=str(d["text"]),
            quiet_ms=int(d["quiet_ms"]),
            resume_s=int(d["resume_s"]),
            queue_s=int(d["queue_s"]),
            received=received,
        )


@dataclass
class State:
    state: str = "idle"
    reason: str = ""
    at: float = 0.0


class Framer:
    """Engine audio to the track's 20 ms frames: level, silence cap, tempo, then 48 kHz int16."""

    def __init__(self, sr: int, gain: float, rate: float, cap: SilenceCap) -> None:
        self._gain = gain
        self._cap = cap
        self._tempo = Tempo(sr, rate)
        if sr * 2 != TRACK_SR:
            raise ValueError(f"engine rate {sr}: the track takes exactly twice it")
        self._up = Double()
        self._pending = np.zeros(0, np.int16)

    def _frames(self, x: Samples, last: bool = False) -> list[bytes]:
        y = self._up.feed(x)
        if last:
            y = np.concatenate([y, self._up.flush()])
        pcm = (np.clip(y, -1, 1) * 32767).astype(np.int16)
        self._pending = np.concatenate([self._pending, pcm])
        n = len(self._pending) // FRAME * FRAME
        if last and len(self._pending) > n:
            self._pending = np.pad(self._pending, (0, FRAME - len(self._pending) + n))
            n += FRAME
        frames = [self._pending[i : i + FRAME].tobytes() for i in range(0, n, FRAME)]
        self._pending = self._pending[n:]
        return frames

    def feed(self, x: Samples) -> list[bytes]:
        return self._frames(self._tempo.feed(self._cap.feed(level(x, self._gain))))

    def flush(self) -> list[bytes]:
        return self._frames(
            np.concatenate([self._tempo.feed(self._cap.flush()), self._tempo.flush()]), last=True
        )


@dataclass
class Pace:
    """How fast the pod makes speech, learned from the sentences it made: wall seconds per second of
    track audio (rtf) and track seconds per character at rate 1. The lab's 4-CPU node runs Pocket at
    RTF 0.6 to 4 as its neighbours load it (2026-10-17), so the player cannot assume real time."""

    rtf: float = 0.5  # S14b measured 0.39: no pre-buffer until the pod shows it is slower
    s_per_char: float = 0.07
    # A slower sentence counts at once, a faster one eases the pace back.
    ease: float = 0.3
    # Margin on rtf: a sentence's own pace varies around the average.
    headroom: float = 1.2

    def learn(self, wall_s: float, audio_s: float, chars: int, rate: float) -> None:
        if audio_s < 0.5 or not chars:
            return
        rtf = wall_s / audio_s
        self.rtf = max(rtf, self.rtf + self.ease * (rtf - self.rtf))
        self.s_per_char += self.ease * (audio_s * rate / chars - self.s_per_char)

    def prebuffer_s(self, chars: int, rate: float) -> float:
        """The audio to hold before a sentence of chars starts so the rest is made before the track
        needs it: made at 1/rtf while it plays, it lacks total * (1 - 1/rtf) by the end."""
        total = chars * self.s_per_char / rate
        return total * max(0.0, 1 - 1 / (self.rtf * self.headroom))


@dataclass
class Sentence:
    chars: int = 0
    frames: list[bytes] = field(default_factory=list)
    done: bool = False
    # failed: synthesis broke; the player drops the line as an error instead of waiting.
    failed: bool = False


class RoomVoice:
    """The lines of one room, said one at a time in queue order (013-AC24)."""

    def __init__(
        self,
        room: str,
        engine: Engine,
        layer: Callable[[], TextLayer],
        clock: Clock | None = None,
        out: Callable[[int], Out | None] = lambda _p: None,
        pace: Pace | None = None,
    ) -> None:
        self.room = room
        self.engine = engine
        self.layer = layer
        self.clock = clock or Clock()
        # out finds a persona's track, None while its bot is not in the call.
        self.out = out
        # One per pod: every room's synthesis shares its CPU.
        self.pace = pace or Pace()
        self.listeners = False
        self.humans: set[str] = set()
        # Each bot's latest view of who speaks, by bot.
        self._seen: dict[str, set[str]] = {}
        self.quiet_since = self.clock.now()
        self.changed_at = 0.0
        self._changed = asyncio.Event()
        self._queue: deque[Line] = deque()
        self._new = asyncio.Event()
        self.states: dict[int, State] = {}
        # When a persona's frame last went out: its echo can come back on a listener's track (S16).
        self.played_at = float("-inf")

    # -- inputs ---------------------------------------------------------------------------------

    def add(self, line: Line) -> None:
        self._queue.append(line)
        self._set(line.persona_id, "waiting")
        self._new.set()

    def speakers(self, humans: set[str], bot: str = "") -> None:
        """LiveKit's active speakers as one bot sees them, the bots left out (013-AC21, AC22). The
        room hears a human while any of its bots does: a bot that has not subscribed to someone yet
        names nobody, which must not silence another bot's view."""
        if humans:
            self._seen[bot] = humans
        else:
            self._seen.pop(bot, None)
        humans = set().union(*self._seen.values())
        now = self.clock.now()
        if humans != self.humans:
            log.debug("speakers", room=self.room, humans=sorted(humans), source=bot)
        if humans and not self.humans:
            self.changed_at = now
        if not humans and self.humans:
            self.quiet_since = now
        self.humans = humans
        self._changed.set()

    def set_listeners(self, on: bool) -> None:
        self.listeners = on
        self._changed.set()

    def forget_bot(self, bot: str) -> None:
        """A bot left: its last view of the speakers goes with it."""
        if bot in self._seen:
            self.speakers(set(), bot)

    def wake(self) -> None:
        """A bot joined: a line waiting for it goes on."""
        self._changed.set()

    # -- states ---------------------------------------------------------------------------------

    def _set(self, persona: int, state: str, reason: str = "") -> None:
        self.states[persona] = State(state, reason, self.clock.now())

    def _drop(self, line: Line, reason: str) -> None:
        metrics.DROPPED.labels(reason).inc()
        self._set(line.persona_id, "dropped", reason)
        log.info("line_dropped", room=self.room, persona_id=line.persona_id, seq=line.seq, reason=reason)

    # -- waiting --------------------------------------------------------------------------------

    async def _changed_within(self, timeout: float, *also: asyncio.Event) -> None:
        """Returns on a speaker or listener change, one of also, or timeout on the clock."""
        self._changed.clear()
        waits: set[asyncio.Future[Any]] = {asyncio.ensure_future(e.wait()) for e in (self._changed, *also)}
        waits.add(asyncio.ensure_future(self.clock.sleep(max(timeout, 0.0))))
        try:
            await asyncio.wait(waits, return_when=asyncio.FIRST_COMPLETED)
        finally:
            for w in waits:
                w.cancel()

    async def _quiet(self, quiet_s: float, deadline: float) -> bool:
        """Waits for quiet_s with no human speaking; False once deadline passes first (013-AC21)."""
        while True:
            now = self.clock.now()
            if not self.humans and now - self.quiet_since >= quiet_s:
                return True
            if now >= deadline:
                return False
            left = deadline - now
            if not self.humans:
                left = min(left, quiet_s - (now - self.quiet_since))
            await self._changed_within(min(left, 0.25))

    # -- the loop -------------------------------------------------------------------------------

    async def run(self) -> None:
        while True:
            while not self._queue:
                self._new.clear()
                await self._new.wait()
            line = self._queue.popleft()
            try:
                await self._take(line)
            except asyncio.CancelledError:
                raise
            except Exception:
                log.exception("line_failed", room=self.room, seq=line.seq)
                self._drop(line, "error")

    async def _take(self, line: Line) -> None:
        deadline = line.received + line.queue_s
        if self.clock.now() > deadline:
            self._drop(line, "stale")
            return
        if not self.listeners:
            self._drop(line, "no_listener")
            return
        if not await self._quiet(line.quiet_ms / 1000, deadline):
            self._drop(line, "stale")
            return
        out = self.out(line.persona_id)
        while out is None:
            # The bot joins when a listener comes (013-AC15): wait for it within the line's time.
            if self.clock.now() >= deadline or not self.listeners:
                self._drop(line, "stale" if self.listeners else "no_listener")
                return
            await self._changed_within(0.1)
            out = self.out(line.persona_id)
        metrics.QUEUE_WAIT.observe(self.clock.now() - line.received)
        await self._say(line, out)

    async def _say(self, line: Line, out: Out) -> None:
        plan = self.layer().plan(line.text)
        sentences = [Sentence(chars=sum(len(sp.text) for sp in spans)) for spans in plan]
        made = asyncio.Event()
        producer = asyncio.create_task(self._produce(line, plan, sentences, made))
        # From here, not from arrival: the quiet time and a line ahead in the queue are not the
        # pod's latency (013-AC17).
        started = self.clock.now()
        try:
            self._set(line.persona_id, "speaking")
            log.info(
                "speaking",
                room=self.room,
                persona_id=line.persona_id,
                seq=line.seq,
                voice=line.voice,
                sentences=len(plan),
                lag_s=round(self.clock.now() - line.received, 3),
            )
            first = True
            i = 0
            while i < len(sentences):
                ok = await self._play(sentences[i], out, made, line, started if first else None)
                first = False
                if ok:
                    i += 1
                    continue
                metrics.INTERRUPTED.inc()
                self._set(line.persona_id, "waiting")
                log.info("interrupted", room=self.room, persona_id=line.persona_id, seq=line.seq, sentence=i)
                if not await self._quiet(line.quiet_ms / 1000, self.clock.now() + line.resume_s):
                    self._drop(line, "interrupted")
                    return
                metrics.RESUMED.inc()
                self._set(line.persona_id, "speaking")
                log.info("resumed", room=self.room, persona_id=line.persona_id, seq=line.seq, sentence=i)
            metrics.SPOKEN.inc()
            self._set(line.persona_id, "idle")
            log.info("spoken", room=self.room, persona_id=line.persona_id, seq=line.seq)
        finally:
            producer.cancel()

    async def _play(
        self, s: Sentence, out: Out, made: asyncio.Event, line: Line, started: float | None
    ) -> bool:
        """Plays one sentence from its start; False when a human cut in (013-AC22). It starts once
        enough is made to play to its end without a gap: a slow pod delays the sentence, never
        stutters in it, and a human still stops it at once."""
        j = 0
        need = self.pace.prebuffer_s(s.chars, line.rate)
        while True:
            if s.failed:
                raise RuntimeError("synthesis failed")
            if self.humans:
                out.clear()
                metrics.STOP.observe(max(self.clock.now() - self.changed_at, 0.0))
                return False
            if j < len(s.frames) and (j or s.done or len(s.frames) * FRAME_S >= need):
                await out.play(s.frames[j])
                self.played_at = self.clock.now()
                if started is not None and j == 0:
                    metrics.FIRST_AUDIO.observe(self.clock.now() - started)
                j += 1
                continue
            if s.done:
                return True
            made.clear()
            t0 = self.clock.now()
            await self._changed_within(1.0, made)
            waited = self.clock.now() - t0
            if j > 0 and not self.humans and waited > UNDERRUN_S:
                metrics.UNDERRUNS.inc()
                log.info(
                    "underrun",
                    room=self.room,
                    persona_id=line.persona_id,
                    seq=line.seq,
                    frame=j,
                    waited_s=round(waited, 3),
                )

    async def _produce(
        self, line: Line, plan: list[list[Span]], out: list[Sentence], made: asyncio.Event
    ) -> None:
        """Synthesises the line sentence after sentence, ahead of the player, off the event loop."""
        loop = asyncio.get_running_loop()
        stop = threading.Event()
        cap = SilenceCap(self.engine.sample_rate)
        try:
            for sentence, spans in zip(out, plan, strict=True):
                t0 = self.clock.now()
                for span in spans:
                    framer = Framer(
                        self.engine.sample_rate, self.engine.gain(line.voice, span.lang), line.rate, cap
                    )
                    q: asyncio.Queue[list[bytes] | None] = asyncio.Queue()

                    def work(
                        span: Span = span, framer: Framer = framer, q: asyncio.Queue[list[bytes] | None] = q
                    ) -> None:
                        t0, audio = time.monotonic(), 0
                        try:
                            for chunk in self.engine.stream(line.voice, span.lang, span.text, stop):
                                audio += len(chunk)
                                loop.call_soon_threadsafe(q.put_nowait, framer.feed(chunk))
                            loop.call_soon_threadsafe(q.put_nowait, framer.flush())
                        finally:
                            if audio:
                                metrics.RTF.observe(
                                    (time.monotonic() - t0) / (audio / self.engine.sample_rate)
                                )
                            loop.call_soon_threadsafe(q.put_nowait, None)

                    # An engine faster than any track (the fake one) runs in the loop: no thread to race.
                    fut: asyncio.Future[None]
                    if getattr(self.engine, "inline", False):
                        work()
                        fut = loop.create_future()
                        fut.set_result(None)
                    else:
                        fut = loop.run_in_executor(None, work)
                    while (frames := await q.get()) is not None:
                        sentence.frames.extend(frames)
                        made.set()
                    await fut
                sentence.done = True
                made.set()
                self.pace.learn(
                    self.clock.now() - t0, len(sentence.frames) * FRAME_S, sentence.chars, line.rate
                )
        except Exception:
            log.exception("synthesis_failed", room=self.room, seq=line.seq)
            for sentence in out:
                sentence.failed = True
            made.set()
        finally:
            stop.set()
