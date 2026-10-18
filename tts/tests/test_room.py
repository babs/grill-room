import asyncio
from collections.abc import Callable
from datetime import datetime
from typing import Any

import numpy as np
import pytest

from grill_tts import metrics
from grill_tts.engine import Fake
from grill_tts.room import FRAME, TRACK_SR, Clock, Line, Pace, RoomVoice, Sentence
from grill_tts.textlayer import TextLayer, load_seed

LAYER = TextLayer(load_seed())


class FakeClock(Clock):
    """Virtual time: sleeps resolve only when the test advances the clock."""

    def __init__(self) -> None:
        self.t = 1000.0
        self._waiters: list[tuple[float, asyncio.Future[None]]] = []

    def now(self) -> float:
        return self.t

    async def sleep(self, s: float) -> None:
        if s <= 0:
            await asyncio.sleep(0)
            return
        fut: asyncio.Future[None] = asyncio.get_running_loop().create_future()
        self._waiters.append((self.t + s, fut))
        await fut

    def tick(self, dt: float) -> None:
        self.t += dt
        due = [w for w in self._waiters if w[0] <= self.t + 1e-9]
        self._waiters = [w for w in self._waiters if w[0] > self.t + 1e-9]
        for _, f in due:
            if not f.done():
                f.set_result(None)


class FakeOut:
    """A track that plays a frame in 20 ms of virtual time and records what it played and when."""

    def __init__(self, clock: FakeClock, name: str, log: list[tuple[str, float, bytes]]) -> None:
        self.clock, self.name, self.log = clock, name, log
        self.clears: list[float] = []

    async def play(self, frame: bytes) -> None:
        self.log.append((self.name, self.clock.now(), frame))
        await self.clock.sleep(0.02)

    def clear(self) -> None:
        self.clears.append(self.clock.now())


class Rig:
    def __init__(self) -> None:
        self.clock = FakeClock()
        self.log: list[tuple[str, float, bytes]] = []
        self.outs = {1: FakeOut(self.clock, "grill", self.log), 2: FakeOut(self.clock, "secretary", self.log)}
        self.rv = RoomVoice("r1", Fake(), lambda: LAYER, self.clock, lambda p: self.outs.get(p))
        self.rv.set_listeners(True)
        self.task = asyncio.create_task(self.rv.run())

    def line(self, persona: int, text: str, **kw: Any) -> Line:
        d: dict[str, Any] = {
            "seq": len(self.log) + persona,
            "room": "r1",
            "persona_id": persona,
            "persona": "P",
            "voice": "alba",
            "rate": 1.0,
            "text": text,
            "quiet_ms": 1000,
            "resume_s": 20,
            "queue_s": 40,
        }
        line = Line.parse(d | kw, self.clock.now())
        self.rv.add(line)
        return line

    async def until(self, cond: Callable[[], bool], virtual_s: float = 60.0) -> None:
        """Advances virtual time in 10 ms steps until cond holds; the fake engine runs in the loop."""
        end = self.clock.t + virtual_s
        while not cond():
            if self.clock.t > end:
                raise AssertionError("condition not reached in virtual time")
            for _ in range(5):
                await asyncio.sleep(0)
            if not getattr(self.rv.engine, "inline", False):
                # A threaded engine hands its frames over in real time.
                await asyncio.sleep(0.001)
            self.clock.tick(0.01)

    def state(self, persona: int) -> tuple[str, str]:
        s = self.rv.states.get(persona)
        return (s.state, s.reason) if s else ("", "")

    def idle(self, persona: int) -> Callable[[], bool]:
        return lambda: self.state(persona) == ("idle", "")

    def played(self, name: str) -> list[tuple[float, bytes]]:
        return [(t, f) for n, t, f in self.log if n == name]

    async def close(self) -> None:
        self.task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await self.task


def count(c: Any, *labels: str) -> float:
    return float((c.labels(*labels) if labels else c)._value.get())


# 013-AC24: one persona at a time, in queue order; the second waits, then starts right after.
async def test_fifo_across_two_personas() -> None:
    r = Rig()
    r.line(1, "First line here. And a second sentence.")
    r.line(2, "Then mine.")
    await r.until(lambda: r.state(1)[0] == "speaking")
    assert r.state(2) == ("waiting", "")
    await r.until(lambda: r.state(2) == ("idle", ""))
    names = [n for n, _, _ in r.log]
    assert names == sorted(names, key=lambda n: n != "grill"), "secretary spoke before grill finished"
    assert r.state(1) == ("idle", "")
    await r.close()


# 013-AC21: nothing starts while a human speaks, nor before the quiet time after them.
async def test_starts_only_after_the_quiet_time() -> None:
    r = Rig()
    r.rv.speakers({"7"})
    r.line(1, "Hello there.", quiet_ms=1500)
    await r.until(lambda: r.clock.t >= 1003)
    assert not r.log and r.state(1) == ("waiting", "")
    r.rv.speakers(set())
    stopped = r.clock.t
    await r.until(lambda: bool(r.log))
    assert r.log[0][1] - stopped == pytest.approx(1.5, abs=0.03)
    await r.close()


def first_frames(played: list[tuple[float, bytes]]) -> list[bytes]:
    return [f for _, f in played]


# 013-AC22, AC23: a human cuts in, the audio clears at once; quiet again within the window, the
# persona says the interrupted sentence again from its start, then goes on.
async def test_interrupt_and_resume_the_sentence() -> None:
    r = Rig()
    resumed = count(metrics.RESUMED)
    r.line(1, "One two three four. Five six.")
    await r.until(lambda: len(r.played("grill")) >= 10)
    r.rv.speakers({"7"})
    cut = r.clock.t
    await r.until(lambda: bool(r.outs[1].clears))
    assert r.outs[1].clears[0] - cut <= 0.021, "stopped later than one frame after the speaker update"
    n = len(r.played("grill"))
    await r.until(lambda: r.clock.t >= cut + 3)
    assert len(r.played("grill")) == n, "played while the human spoke"
    r.rv.speakers(set())
    await r.until(lambda: r.state(1) == ("idle", ""))
    frames = first_frames(r.played("grill"))
    assert frames[n] == frames[0], "did not restart the interrupted sentence from its start"
    assert count(metrics.RESUMED) == resumed + 1
    await r.close()


# 013-AC23: past the resume window the rest of the line is dropped as interrupted; the next line goes.
async def test_interrupted_past_the_window_drops() -> None:
    r = Rig()
    before = count(metrics.DROPPED, "interrupted")
    r.line(1, "One two three four five six seven.", resume_s=5)
    r.line(2, "Mine now.")
    await r.until(lambda: len(r.played("grill")) >= 5)
    r.rv.speakers({"7"})
    await r.until(lambda: r.clock.t >= 1000 + 8)
    assert r.state(1) == ("dropped", "interrupted")
    r.rv.speakers(set())
    await r.until(lambda: r.state(2) == ("idle", ""))
    assert count(metrics.DROPPED, "interrupted") == before + 1
    await r.close()


# 013-AC24: a line not started within its queue maximum is stale; with nobody listening, no_listener.
async def test_stale_and_no_listener() -> None:
    r = Rig()
    r.rv.speakers({"7"})
    r.line(1, "Too late.", queue_s=10)
    await r.until(lambda: r.state(1)[0] == "dropped", virtual_s=15)
    assert r.state(1) == ("dropped", "stale") and not r.log
    r.rv.speakers(set())
    r.rv.set_listeners(False)
    r.line(2, "Nobody hears.")
    await r.until(lambda: r.state(2)[0] == "dropped")
    assert r.state(2) == ("dropped", "no_listener") and not r.log
    await r.close()


def silent_runs(frames: list[bytes]) -> list[float]:
    pcm = np.frombuffer(b"".join(frames), np.int16).astype(np.float32) / 32768
    w = TRACK_SR // 100
    runs, run = [], 0
    for i in range(0, len(pcm) - w + 1, w):
        if np.sqrt(np.mean(np.square(pcm[i : i + w]))) < 10 ** (-50 / 20):
            run += w
        elif run:
            runs.append(run / TRACK_SR)
            run = 0
    return runs


# 013-AC38: the fake engine leaves 1 s after each sentence; the track never carries more than 300 ms.
# 013-AC37: at rate 1.3 the same line is 1/1.3 as long, within 5 %.
async def test_pauses_capped_and_rate_on_the_track() -> None:
    lengths = {}
    for rate in (1.0, 1.3):
        r = Rig()
        r.line(1, "Alpha beta. Gamma delta. Epsilon.", rate=rate)
        await r.until(r.idle(1))
        frames = first_frames(r.played("grill"))
        assert all(len(f) == FRAME * 2 for f in frames)
        assert max(silent_runs(frames)) <= 0.3 + 0.011
        lengths[rate] = len(frames)
        await r.close()
    assert lengths[1.3] / lengths[1.0] == pytest.approx(1 / 1.3, rel=0.05)


# 013-AC24: a line that waited its queue maximum behind another persona's long line is dropped as
# stale when its turn comes, without a sound.
async def test_stale_behind_a_long_line() -> None:
    r = Rig()
    r.line(1, " ".join(["word"] * 40) + ".")
    r.line(2, "Old news.", queue_s=10)
    await r.until(lambda: r.state(2)[0] == "dropped" or bool(r.played("secretary")), virtual_s=40)
    assert r.state(2) == ("dropped", "stale") and not r.played("secretary")
    await r.close()


class Broken(Fake):
    def stream(self, voice: str, lang: Any, text: str, stop: Any) -> Any:
        raise RuntimeError("engine down")


# 013-AC24: a line whose synthesis breaks is dropped as an error, and the next one goes.
async def test_engine_failure_drops_as_error() -> None:
    r = Rig()
    r.rv.engine = Broken()
    r.line(1, "Never said.")
    await r.until(lambda: r.state(1)[0] == "dropped")
    assert r.state(1) == ("dropped", "error") and not r.played("grill")
    r.rv.engine = Fake()
    r.line(2, "Said.")
    await r.until(r.idle(2))
    await r.close()


# 013-AC24: a line delivered late keeps its queue maximum from when the server queued it; a
# server clock ahead never makes it younger, and its age never exceeds the queue maximum.
def test_line_age_counts_from_queued_at() -> None:
    d: dict[str, Any] = {
        "seq": 1,
        "room": "r",
        "persona_id": 1,
        "persona": "P",
        "voice": "alba",
        "rate": 1.0,
        "text": "x",
        "quiet_ms": 1000,
        "resume_s": 20,
        "queue_s": 40,
    }
    now = datetime.fromisoformat("2027-01-15T08:00:00+00:00").timestamp()
    for queued, want in (
        ("2027-01-15T07:59:30Z", 470.0),
        ("2027-01-15T08:00:05Z", 500.0),
        ("2027-01-15T07:00:00Z", 460.0),
    ):
        assert Line.parse(d | {"queued_at": queued}, 500.0, wall=now).received == pytest.approx(want), queued
    assert Line.parse(d, 500.0).received == 500.0


# 013-AC17: first audio counts from the start of speaking, not the quiet time before it.
async def test_first_audio_counts_from_speaking() -> None:
    r = Rig()
    before = metrics.FIRST_AUDIO._sum.get()
    r.rv.speakers({"7"})
    r.line(1, "Hello there.")
    await r.until(lambda: r.clock.t >= 1005)
    r.rv.speakers(set())
    await r.until(r.idle(1))
    assert metrics.FIRST_AUDIO._sum.get() - before < 0.5, "the 5 s of waiting counted as first-audio latency"
    await r.close()


class Threaded(Fake):
    inline = False


# The engine runs on a worker thread, as Pocket does; the line still plays whole, in order.
async def test_threaded_engine() -> None:
    r = Rig()
    r.rv.engine = Threaded()
    r.line(1, "One two. Three four.")
    await r.until(r.idle(1))
    inline = Rig()
    inline.line(1, "One two. Three four.")
    await inline.until(inline.idle(1))
    assert first_frames(r.played("grill")) == first_frames(inline.played("grill"))
    await r.close()
    await inline.close()


# 013-AC21, AC22: the room hears a human while any of its bots does; a bot that names nobody, or
# leaves, does not silence another bot's view.
async def test_speakers_are_the_union_of_the_bots_views() -> None:
    r = Rig()
    r.rv.speakers({"7"}, "persona-1")
    r.rv.speakers(set(), "persona-2")
    assert r.rv.humans == {"7"}
    r.rv.speakers({"8"}, "persona-2")
    assert r.rv.humans == {"7", "8"}
    r.rv.forget_bot("persona-1")
    assert r.rv.humans == {"8"}
    quiet = r.clock.t
    r.rv.speakers(set(), "persona-2")
    assert r.rv.humans == set() and r.rv.quiet_since == quiet
    await r.close()


async def slow_sentence(
    r: Rig, pace: Pace, chunks: int = 25, every_s: float = 0.24, human_at: int | None = None
) -> tuple[Sentence, bool, float]:
    """Plays one sentence of 50 characters while the test makes it like a slow engine: an 80 ms chunk
    (4 frames) every every_s of virtual time; a human starts speaking after chunk human_at."""
    r.rv.pace = pace
    d = {"seq": 1, "room": "r1", "persona_id": 2, "persona": "P", "voice": "alba", "text": "y" * 50}
    line = Line.parse(d | {"quiet_ms": 0, "resume_s": 20, "queue_s": 40}, r.clock.now())
    s, made = Sentence(chars=50), asyncio.Event()
    t0 = r.clock.t
    play = asyncio.create_task(r.rv._play(s, r.outs[2], made, line, t0))

    def at(t: float) -> Callable[[], bool]:
        return lambda: r.clock.t >= t - 1e-9

    for k in range(chunks):
        await r.until(at(t0 + (k + 1) * every_s))
        s.frames.extend([bytes(2 * FRAME)] * 4)
        made.set()
        if k == human_at:
            r.rv.speakers({"7"})
            cut = r.clock.t
            await r.until(play.done, 1.0)
            return s, play.result(), cut
    s.done = True
    made.set()
    await r.until(play.done)
    return s, play.result(), t0


# Synthesis at 3x real time (the lab, 2026-10-17): the sentence waits until enough is made to play
# to its end without a gap, instead of stuttering every chunk.
async def test_slow_synthesis_plays_the_sentence_without_gaps() -> None:
    r = Rig()
    before = count(metrics.UNDERRUNS)
    _, ok, t0 = await slow_sentence(r, Pace(rtf=3.0, s_per_char=0.04))
    times = [t for t, _ in r.played("secretary")]
    assert ok and len(times) == 100
    gaps = [b - a for a, b in zip(times, times[1:], strict=False)]
    assert max(gaps) <= 0.021, f"a gap of {max(gaps):.2f} s inside the sentence"
    assert count(metrics.UNDERRUNS) == before
    await r.close()


# A pod faster than real time does not hold the sentence back: first audio stays at the first chunk.
async def test_fast_synthesis_starts_at_the_first_chunk() -> None:
    r = Rig()
    _, ok, t0 = await slow_sentence(r, Pace(rtf=0.5, s_per_char=0.04), every_s=0.04)
    assert ok and r.played("secretary")[0][0] - t0 <= 0.041
    await r.close()


# 013-AC22: a human who starts speaking while the sentence still fills its buffer stops it at once.
async def test_onset_during_the_prebuffer_stops_at_once() -> None:
    r = Rig()
    _, ok, cut = await slow_sentence(r, Pace(rtf=3.0, s_per_char=0.04), human_at=3)
    assert not ok and not r.played("secretary")
    assert r.outs[2].clears and r.outs[2].clears[0] - cut <= 0.011
    await r.close()


# The pace jumps to a slower RTF at once, eases back when the pod speeds up, and ignores scraps.
def test_pace_learns_pessimistically() -> None:
    p = Pace(rtf=0.5, s_per_char=0.06)
    assert p.prebuffer_s(50, 1.0) == 0.0
    p.learn(wall_s=6.0, audio_s=2.0, chars=50, rate=1.0)
    assert p.rtf == 3.0 and p.s_per_char == pytest.approx(0.06 + 0.3 * (0.04 - 0.06))
    assert p.prebuffer_s(50, 1.0) == pytest.approx(50 * p.s_per_char * (1 - 1 / (3.0 * 1.2)))
    p.learn(wall_s=1.0, audio_s=2.0, chars=50, rate=1.0)
    assert 0.5 < p.rtf < 3.0
    p.learn(wall_s=9.0, audio_s=0.1, chars=2, rate=1.0)
    assert p.rtf < 3.0, "a 100 ms scrap moved the pace"
