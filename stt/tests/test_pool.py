"""The engine pool: parallel speakers, per-speaker order, dead engines, health and wait metrics."""

import asyncio
import json
import sys
import threading
import time
from pathlib import Path

import httpx
import numpy as np
import pytest
from prometheus_client import REGISTRY, generate_latest
from structlog.testing import capture_logs

from grill_stt.engines import EngineDied, ProcessEngine, child_env
from grill_stt.line import Line
from grill_stt.pipeline import Pipeline, RoomAPI, Utterance
from grill_stt.vad import Audio

from .fake_engine import DIE, HANG

STT = Path(__file__).parents[1]


def metric(name: str, **labels: str) -> float:
    return REGISTRY.get_sample_value(name, labels) or 0.0


def room(posted: list[tuple[int, str]]) -> RoomAPI:
    def handle(r: httpx.Request) -> httpx.Response:
        body = json.loads(r.content)
        posted.append((body["participant_id"], body.get("body", "")))
        return httpx.Response(201)

    return RoomAPI(httpx.AsyncClient(base_url="http://room", transport=httpx.MockTransport(handle)))


def utt(pid: int, n: float, marker: float = 0.0) -> Utterance:
    audio = np.zeros(16000, np.float32)
    audio[0], audio[1] = marker, n
    return Utterance("r1", pid, audio, time.monotonic())


class Gate:
    """An in-process engine whose calls block until released, recording who was inside when."""

    def __init__(self) -> None:
        self.entered = threading.Semaphore(0)
        self.release = threading.Semaphore(0)
        self.inside: list[int] = []
        self.started: list[int] = []
        self.overlap = False
        self._lock = threading.Lock()

    def __call__(self, audio: Audio, _prompt: str) -> Line:
        pid = int(audio[2])
        with self._lock:
            self.overlap |= pid in self.inside
            self.inside.append(pid)
            self.started.append(pid)
        self.entered.release()
        assert self.release.acquire(timeout=10), "never released"
        with self._lock:
            self.inside.remove(pid)
        return Line(f"line {float(audio[1]):g}", "en")


def gated(pid: int, n: float) -> Utterance:
    u = utt(pid, n)
    u.audio[2] = pid
    return u


async def entered(gate: Gate, n: int = 1) -> None:
    for _ in range(n):
        assert await asyncio.to_thread(gate.entered.acquire, True, 10), "no engine took an utterance"


async def test_two_speakers_run_in_parallel() -> None:
    """007-AC6: each engine takes the next utterance when free: two speakers, two engines at once."""
    posted: list[tuple[int, str]] = []
    gate = Gate()
    p = Pipeline([gate, gate], room(posted), size=8)
    busy = metric("grill_stt_engines_busy")
    task = asyncio.create_task(p.run())
    p.submit(gated(1, 1))
    p.submit(gated(2, 2))
    await entered(gate, 2)
    assert sorted(gate.inside) == [1, 2]
    assert metric("grill_stt_engines_busy") == busy + 2
    gate.release.release(2)
    await asyncio.wait_for(p.idle(), 10)
    task.cancel()
    assert sorted(posted) == [(1, "line 1"), (2, "line 2")]


async def test_one_speaker_posts_in_speech_order() -> None:
    """007-AC7: a speaker's next utterance waits for their previous one; another speaker passes it."""
    posted: list[tuple[int, str]] = []
    gate = Gate()
    p = Pipeline([gate, gate], room(posted), size=8)
    task = asyncio.create_task(p.run())
    p.submit(gated(1, 1))
    await entered(gate)
    p.submit(gated(1, 2))
    p.submit(gated(2, 3))
    # The free engine skips speaker 1's second line for speaker 2's.
    await entered(gate)
    assert gate.started == [1, 2]
    gate.release.release(2)
    await entered(gate)
    assert gate.started == [1, 2, 1]
    gate.release.release()
    await asyncio.wait_for(p.idle(), 10)
    task.cancel()
    assert not gate.overlap
    assert [text for pid, text in posted if pid == 1] == ["line 1", "line 2"]


def fake_engines(n: int, timeout_s: float = 30) -> list[ProcessEngine]:
    return [ProcessEngine([sys.executable, "-m", "tests.fake_engine"], timeout_s) for _ in range(n)]


@pytest.fixture
def in_stt(monkeypatch: pytest.MonkeyPatch) -> None:
    """The fake engine runs as `python -m tests.fake_engine`, from stt/."""
    monkeypatch.chdir(STT)


@pytest.mark.usefixtures("in_stt")
async def test_a_dead_engine_is_restarted_and_the_other_carries_on() -> None:
    """007-AC6: an engine process that dies costs its utterance (an error) and is restarted; the
    other engine finishes its own utterance meanwhile."""
    posted: list[tuple[int, str]] = []
    engines = fake_engines(2)
    p = Pipeline(engines, room(posted), size=8)
    await asyncio.wait_for(p.start(), 30)
    pids = {e.pid for e in engines}
    errors = metric("grill_stt_lines_total", result="error")
    task = asyncio.create_task(p.run())
    try:
        p.submit(utt(1, 1, DIE))
        p.submit(utt(2, 2))
        await asyncio.wait_for(p.idle(), 30)
        assert posted == [(2, "line 2")]
        assert metric("grill_stt_lines_total", result="error") == errors + 1
        # The restart happens before the engine takes anything else: the next line proves it.
        p.submit(utt(1, 3))
        p.submit(utt(2, 4))
        await asyncio.wait_for(p.idle(), 30)
        assert sorted(posted[1:]) == [(1, "line 3"), (2, "line 4")]
        assert all(e.alive() for e in engines)
        assert len({e.pid for e in engines} - pids) == 1
        # One dies while idle: it is restarted before its next utterance, which is not lost.
        engines[0]._proc.kill()  # type: ignore[union-attr]
        await asyncio.to_thread(engines[0]._proc.wait)  # type: ignore[union-attr]
        for n in (5, 6, 7):
            p.submit(utt(n, n))
        await asyncio.wait_for(p.idle(), 30)
        assert sorted(posted[3:]) == [(5, "line 5"), (6, "line 6"), (7, "line 7")]
        assert metric("grill_stt_lines_total", result="error") == errors + 1
        assert all(e.alive() for e in engines)
    finally:
        task.cancel()
        p.stop()


@pytest.mark.usefixtures("in_stt")
async def test_a_hung_engine_is_killed_at_its_deadline() -> None:
    """007-AC6: an engine that does not answer within APP_ENGINE_TIMEOUT_S is killed, then restarted."""
    (e,) = fake_engines(1, timeout_s=0.5)
    await asyncio.to_thread(e.start, 30)
    try:
        with pytest.raises(EngineDied, match="no answer"):
            await asyncio.to_thread(e, utt(1, 1, HANG).audio, "")
        assert not e.alive()
        with pytest.raises(EngineDied, match="not started"):
            e(utt(1, 1).audio, "")
        await asyncio.to_thread(e.start, 30)
        assert (await asyncio.to_thread(e, utt(1, 5).audio, "")).text == "line 5"
    finally:
        e.stop()


async def test_an_engine_that_dies_loading_leaves_nothing_behind(monkeypatch: pytest.MonkeyPatch) -> None:
    """A child that exits before ready is reaped and its pipes closed; the secrets never reach it."""
    monkeypatch.setenv("APP_ROOM_TOKEN", "secret-token")
    leak = "import os, sys; sys.exit(7 if 'APP_ROOM_TOKEN' in os.environ else 0)"
    for code in ("import sys; sys.exit(3)", leak):
        e = ProcessEngine([sys.executable, "-c", code], 30)
        with pytest.raises(EngineDied):
            await asyncio.to_thread(e.start, 30)
        assert e.pid is None and not e.alive()
    assert "APP_ROOM_TOKEN" not in child_env() and child_env()["OTEL_SDK_DISABLED"] == "true"


@pytest.mark.usefixtures("in_stt")
async def test_health_fails_once_no_engine_was_up_for_60_s() -> None:
    """007-AC6: /healthz fails when no engine process has been up for 60 s."""
    now = [0.0]
    engines = fake_engines(2)
    p = Pipeline(engines, room([]), size=8, clock=lambda: now[0])
    assert not p.healthy()  # nothing loaded yet
    await asyncio.wait_for(p.start(), 30)
    assert p.healthy()
    engines[0].stop()
    now[0] = 1000
    assert p.healthy()  # one is up
    engines[1].stop()
    now[0] = 1059
    assert p.healthy()
    now[0] = 1061
    assert not p.healthy()


async def test_queue_wait_is_measured_and_a_slow_one_warned() -> None:
    """007-AC9, AC19: the wait from the VAD cut to an engine, with a warning past 10 s."""
    now = [0.0]
    posted: list[tuple[int, str]] = []
    gate = Gate()
    p = Pipeline([gate], room(posted), size=8, clock=lambda: now[0])
    count = metric("grill_stt_queue_wait_seconds_count")
    within = metric("grill_stt_queue_wait_seconds_bucket", le="10.0")
    p.submit(gated(1, 1))
    p.submit(gated(2, 2))
    with capture_logs() as logs:
        task = asyncio.create_task(p.run())
        await entered(gate)
        now[0] = 12.5
        gate.release.release()
        await entered(gate)
        gate.release.release()
        await asyncio.wait_for(p.idle(), 10)
        task.cancel()
    assert metric("grill_stt_queue_wait_seconds_count") == count + 2
    assert metric("grill_stt_queue_wait_seconds_bucket", le="10.0") == within + 1
    slow = [e for e in logs if e["event"] == "queue_wait_slow"]
    assert [(e["participant"], e["wait_s"], e["log_level"]) for e in slow] == [(2, 12.5, "warning")]


async def test_metrics_expose_every_ac9_series() -> None:
    """007-AC9: every queue and engine series is on /metrics, labelled by engine where it says so."""
    p = Pipeline([lambda _a, _p: Line("ok", "en")], room([]), size=1, engine="parakeet")
    p.submit(utt(1, 1))
    p.submit(utt(1, 2))  # dropped
    task = asyncio.create_task(p.run())
    await asyncio.wait_for(p.idle(), 10)
    task.cancel()
    text = generate_latest().decode()
    for series in (
        "grill_stt_queue_depth ",
        "grill_stt_queue_wait_seconds_bucket{",
        'grill_stt_transcribe_seconds_bucket{engine="parakeet"',
        'grill_stt_line_latency_seconds_bucket{engine="parakeet"',
        "grill_stt_engines_busy ",
        "grill_stt_utterances_dropped_total ",
    ):
        assert series in text, series
