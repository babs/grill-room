import asyncio
import json
from types import SimpleNamespace
from typing import Any, cast

import httpx
import numpy as np
import pytest
from livekit import rtc
from prometheus_client import REGISTRY

import grill_stt.worker as worker_mod
from grill_stt.config import Settings
from grill_stt.pipeline import Pipeline, RoomAPI
from grill_stt.transcribe import Line
from grill_stt.worker import Worker, http_url


def settings(**kw: Any) -> Settings:
    base: dict[str, Any] = {
        "room_url": "http://room",
        "room_token": "t",
        "livekit_url": "ws://livekit:7880",
        "livekit_api_key": "k",
        "livekit_api_secret": "s" * 32,
        "worker_name": "w1",
        "heartbeat_seconds": 30,
    }
    return Settings(**(base | kw))


def worker(handler: httpx.MockTransport, **kw: Any) -> Worker:
    room_api = RoomAPI(httpx.AsyncClient(base_url="http://room", transport=handler))
    pipeline = Pipeline(lambda _a, _p: Line("", "en"), room_api, size=1)
    return Worker(settings(**kw), pipeline, room_api, lk=None)  # type: ignore[arg-type] # unused here


def test_plausibility_thresholds_come_from_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("APP_MIN_LANGUAGE_PROBABILITY", "0.7")
    monkeypatch.setenv("APP_SHORT_UTTERANCE_S", "1.5")
    p = settings().plausibility()
    assert (p.min_language_probability, p.short_utterance_s, p.min_avg_logprob) == (0.7, 1.5, -1.0)


def test_http_url() -> None:
    assert http_url("wss://livekit.example") == "https://livekit.example"
    assert http_url("ws://livekit:7880") == "http://livekit:7880"


async def test_heartbeat_goes_out_at_once_when_the_rooms_change() -> None:
    beats: list[bytes] = []

    def handle(r: httpx.Request) -> httpx.Response:
        beats.append(r.content)
        return httpx.Response(204)

    w = worker(httpx.MockTransport(handle))
    task = asyncio.create_task(w.heartbeat())
    while not beats:
        await asyncio.sleep(0.01)
    room = SimpleNamespace(remote_participants={})
    w.rooms["r1"] = room  # type: ignore[assignment]
    w._changed.set()
    for _ in range(100):
        if len(beats) == 2:
            break
        await asyncio.sleep(0.01)
    # heartbeat_seconds is 30: the second beat came from the change, not the timer.
    tail = b',"engine":"parakeet","glossary":false}'
    assert beats == [
        b'{"worker":"w1","rooms":[],"voices":{}' + tail,
        b'{"worker":"w1","rooms":["r1"],"voices":{}' + tail,
    ]
    task.cancel()


@pytest.mark.parametrize(
    ("kw", "engine", "glossary"),
    [
        ({}, "parakeet", False),
        # 007-AC14: parakeet never uses the glossary, even with the prompt setting on.
        ({"engine": "parakeet", "glossary_prompt": True}, "parakeet", False),
        ({"engine": "whisper"}, "whisper", False),
        ({"engine": "whisper", "glossary_prompt": True}, "whisper", True),
    ],
)
async def test_heartbeat_says_the_engine_and_whether_it_reads_the_glossary(
    kw: dict[str, Any], engine: str, glossary: bool
) -> None:
    """007-AC14: the room hides the glossary tab while no worker reads the glossary."""
    beats: list[dict[str, Any]] = []

    def handle(r: httpx.Request) -> httpx.Response:
        beats.append(json.loads(r.content))
        return httpx.Response(204)

    task = asyncio.create_task(worker(httpx.MockTransport(handle), **kw).heartbeat())
    while not beats:
        await asyncio.sleep(0.01)
    task.cancel()
    assert (beats[0]["engine"], beats[0]["glossary"]) == (engine, glossary)


async def test_forget_drops_the_room_and_its_tracks_once() -> None:
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    room = SimpleNamespace()
    w.rooms["r1"] = room  # type: ignore[assignment]
    listening = asyncio.create_task(asyncio.sleep(60))
    w._tracks["r1"] = {listening}
    # A stale "disconnected" from an earlier connection to the same room changes nothing.
    w._forget("r1", SimpleNamespace(), "stale")  # type: ignore[arg-type]
    assert "r1" in w.rooms
    w._forget("r1", room, "left")  # type: ignore[arg-type]
    assert "r1" not in w.rooms and "r1" not in w._tracks
    await asyncio.sleep(0)
    assert listening.cancelled()
    assert w._changed.is_set()


@pytest.mark.parametrize(
    ("kind", "identity"), [(rtc.TrackKind.KIND_VIDEO, "7"), (rtc.TrackKind.KIND_AUDIO, "stt-other")]
)
async def test_only_participant_audio_is_transcribed(kind: int, identity: str) -> None:
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    subscribe(w, "TR_a", identity, kind)
    assert not w._tracks.get("r1")


async def test_leaves_a_room_once_alone() -> None:
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    left = asyncio.Event()

    async def disconnect() -> None:
        left.set()

    w.rooms["r1"] = SimpleNamespace(remote_participants={"7": object()}, disconnect=disconnect)  # type: ignore[assignment]
    w._leave_if_alone("r1")
    await asyncio.sleep(0)
    assert not left.is_set()
    w.rooms["r1"].remote_participants = {}  # type: ignore[misc]
    w._leave_if_alone("r1")
    await asyncio.wait_for(left.wait(), 1)


class FakeStream:
    """An rtc.AudioStream fed by the test: aclose ends the iteration, as LiveKit's EOS does."""

    def __init__(self, track: object, **_kw: object) -> None:
        self.track = track
        self.frames: asyncio.Queue[bytes | None] = asyncio.Queue()
        self.closed = False

    def __aiter__(self) -> FakeStream:
        return self

    async def __anext__(self) -> SimpleNamespace:
        data = await self.frames.get()
        if data is None:
            raise StopAsyncIteration
        return SimpleNamespace(frame=SimpleNamespace(data=data))

    async def aclose(self) -> None:
        if not self.closed:
            self.closed = True
            self.frames.put_nowait(None)


def speech(seconds: float) -> bytes:
    """Loud samples the stand-in VAD below calls speech."""
    return np.full(int(seconds * 16000), 16000, np.int16).tobytes()


def loud(windows: Any) -> Any:
    return np.where(np.abs(windows).mean(axis=1) > 0.3, 0.9, 0.0).astype(np.float32)


def tracks() -> float:
    return REGISTRY.get_sample_value("grill_stt_tracks") or 0.0


@pytest.fixture
def listening(monkeypatch: pytest.MonkeyPatch) -> tuple[Worker, list[FakeStream], list[tuple[int, int]]]:
    """A worker on FakeStreams; utterances land in a list as (participant, samples)."""
    streams: list[FakeStream] = []

    def stream(track: object, **kw: object) -> FakeStream:
        streams.append(FakeStream(track, **kw))
        return streams[-1]

    monkeypatch.setattr(rtc, "AudioStream", stream)
    monkeypatch.setattr(worker_mod, "Silero", lambda: loud)
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    submitted: list[tuple[int, int]] = []
    monkeypatch.setattr(w._pipeline, "submit", lambda u: submitted.append((u.participant_id, len(u.audio))))
    return w, streams, submitted


def subscribe(w: Worker, sid: str, identity: str, kind: int = rtc.TrackKind.KIND_AUDIO) -> None:
    """Fires the worker's track_subscribed handler for one track of room r1."""
    track, pub, p = SimpleNamespace(kind=kind), SimpleNamespace(sid=sid), SimpleNamespace(identity=identity)
    w._on_track("r1", track, pub, p)  # type: ignore[arg-type]


async def settle(cond: Any) -> None:
    for _ in range(200):
        if cond():
            return
        await asyncio.sleep(0.005)
    raise AssertionError("condition not reached")


async def test_a_track_that_goes_ends_its_stream_and_flushes_its_speech(
    listening: tuple[Worker, list[FakeStream], list[tuple[int, int]]],
) -> None:
    """001-AC7: an unsubscribed track's stream closes, so a reused transceiver cannot speak under its name."""
    w, streams, submitted = listening
    base = tracks()
    subscribe(w, "TR_a", "7")
    subscribe(w, "TR_b", "8")
    await settle(lambda: tracks() == base + 2)
    # 7 stops mid-sentence and leaves: the half utterance still reaches the queue under 7.
    streams[0].frames.put_nowait(speech(1))
    await asyncio.sleep(0.01)
    w._close("TR_a")
    await settle(lambda: tracks() == base + 1)
    assert streams[0].closed and not streams[1].closed
    assert submitted and all(pid == 7 for pid, _ in submitted)
    assert set(w._streams) == {"TR_b"}
    # Closing a track twice, or one never seen, changes nothing.
    w._close("TR_a")
    w._close("TR_x")
    assert tracks() == base + 1
    w.rooms["r1"] = SimpleNamespace()  # type: ignore[assignment]
    w._forget("r1", w.rooms["r1"], "left")
    await settle(lambda: tracks() == base)
    assert streams[1].closed and not w._streams


async def test_a_participant_who_leaves_closes_every_stream_of_theirs(
    listening: tuple[Worker, list[FakeStream], list[tuple[int, int]]],
) -> None:
    """001-AC7: participant_disconnected closes the streams the SDK sent no track_unsubscribed for."""
    w, streams, _ = listening
    base = tracks()
    w.rooms["r1"] = SimpleNamespace(remote_participants={"8": object()})  # type: ignore[assignment]
    for sid, identity in (("TR_a", "7"), ("TR_b", "7"), ("TR_c", "8")):
        subscribe(w, sid, identity)
    await settle(lambda: tracks() == base + 3)
    w._on_participant_left("r1", SimpleNamespace(identity="7"))  # type: ignore[arg-type]
    await settle(lambda: tracks() == base + 1)
    assert [s.closed for s in streams] == [True, True, False]
    # A resubscription to the same track replaces its stream instead of adding one.
    subscribe(w, "TR_c", "8")
    await settle(lambda: streams[2].closed)
    await settle(lambda: tracks() == base + 1)
    assert w._streams["TR_c"][1] is cast(object, streams[3])
    for s in streams:
        await s.aclose()
    await settle(lambda: tracks() == base)


async def test_a_listener_that_fails_to_start_still_closes_its_stream(
    listening: tuple[Worker, list[FakeStream], list[tuple[int, int]]], monkeypatch: pytest.MonkeyPatch
) -> None:
    w, streams, _ = listening
    base = tracks()

    def broken() -> None:
        raise RuntimeError("no VAD model")

    monkeypatch.setattr(worker_mod, "Silero", broken)
    subscribe(w, "TR_a", "7")
    await settle(lambda: streams[0].closed)
    assert not w._streams and tracks() == base


async def test_voices_follow_the_subscribed_tracks(
    listening: tuple[Worker, list[FakeStream], list[tuple[int, int]]],
) -> None:
    """005-AC8: the heartbeat's voices are the participants whose track a stream is open on, per room."""
    w, streams, _ = listening
    w._changed.clear()
    subscribe(w, "TR_a", "7")
    subscribe(w, "TR_b", "8")
    subscribe(w, "TR_c", "8")
    assert w._changed.is_set()
    assert w.voices() == {"r1": [7, 8]}
    w._changed.clear()
    w._close("TR_a")
    assert w._changed.is_set() and w.voices() == {"r1": [8]}
    w._on_participant_left("r1", SimpleNamespace(identity="8"))  # type: ignore[arg-type]
    assert w.voices() == {}
    for s in streams:
        await s.aclose()


class FakeRoom:
    """An rtc.Room the test connects and closes; it records its disconnect."""

    def __init__(self) -> None:
        self.connected = False
        self.left = False
        self.remote_participants = {"7": object()}

    def on(self, _event: str, _cb: Any) -> None:
        pass

    async def connect(self, *_a: object) -> None:
        self.connected = True

    def isconnected(self) -> bool:
        return self.connected

    async def disconnect(self) -> None:
        self.left = True


def new_room(made: list[FakeRoom]) -> FakeRoom:
    made.append(FakeRoom())
    return made[-1]


def listed(name: str, sid: str, people: int = 1) -> SimpleNamespace:
    return SimpleNamespace(name=name, sid=sid, num_participants=people)


@pytest.mark.parametrize("how", ["closed", "same_name", "dropped"])
async def test_a_room_livekit_closed_is_dropped_and_joined_again(
    monkeypatch: pytest.MonkeyPatch, how: str
) -> None:
    """005-AC18: LiveKit closes an empty room under the worker, people come back, the worker rejoins."""
    made: list[FakeRoom] = []
    monkeypatch.setattr(rtc, "Room", lambda: new_room(made))
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    await w.reconcile([listed("r1", "RM_a")])
    assert w.rooms["r1"] is cast(object, made[0])
    # Nothing changed: the room stays as it is.
    await w.reconcile([listed("r1", "RM_a")])
    assert len(made) == 1
    w._changed.clear()
    if how == "closed":
        # Gone from the list: dropped, and not listening any more.
        await w.reconcile([])
        assert "r1" not in w.rooms and w._changed.is_set()
        await w.reconcile([listed("r1", "RM_b")])
    elif how == "same_name":
        # The poll never saw it gone: a new room under the same name has another sid.
        await w.reconcile([listed("r1", "RM_b")])
    else:
        # The SDK lost the connection without a "disconnected" event.
        made[0].connected = False
        await w.reconcile([listed("r1", "RM_a")])
    await settle(lambda: made[0].left)
    assert len(made) == 2 and w.rooms["r1"] is cast(object, made[1])
    # A stale "disconnected" from the old room leaves the new one alone.
    w._forget("r1", made[0], "late")  # type: ignore[arg-type]
    assert w.rooms["r1"] is cast(object, made[1])


async def test_an_empty_listed_room_is_not_joined(monkeypatch: pytest.MonkeyPatch) -> None:
    made: list[FakeRoom] = []
    monkeypatch.setattr(rtc, "Room", lambda: new_room(made))
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    await w.reconcile([listed("r1", "RM_a", people=0)])
    assert not made and not w.rooms


def test_glossary_prompt_is_opt_in(monkeypatch: pytest.MonkeyPatch) -> None:
    """005-AC23 (spike S10b): off unless APP_GLOSSARY_PROMPT says on."""
    assert settings().glossary_prompt is False
    monkeypatch.setenv("APP_GLOSSARY_PROMPT", "on")
    assert settings().glossary_prompt is True
