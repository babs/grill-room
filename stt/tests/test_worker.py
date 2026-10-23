import asyncio
import json
from collections.abc import Callable
from types import SimpleNamespace
from typing import Any, cast

import httpx
import numpy as np
import pytest
import structlog.testing
from livekit import rtc
from livekit.protocol import models
from prometheus_client import REGISTRY
from pydantic import ValidationError

import grill_stt.worker as worker_mod
from grill_stt.config import Settings
from grill_stt.pipeline import Pipeline, RoomAPI, Utterance
from grill_stt.transcribe import Line
from grill_stt.vad import WINDOW, Segmenter
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
    pipeline = Pipeline([lambda _a, _p: Line("", "en")], room_api, size=1)
    return Worker(settings(**kw), pipeline, room_api, lk=None)  # type: ignore[arg-type] # unused here


def test_plausibility_thresholds_come_from_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("APP_MIN_LANGUAGE_PROBABILITY", "0.7")
    monkeypatch.setenv("APP_SHORT_UTTERANCE_S", "1.5")
    p = settings().plausibility()
    assert (p.min_language_probability, p.short_utterance_s, p.min_avg_logprob) == (0.7, 1.5, -1.0)


def test_cut_search_comes_from_the_environment_and_stays_below_the_cap(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """018-AC3: APP_VAD_CUT_SEARCH_S, default 3 s; a search as long as the cap is refused."""
    assert settings().vad_cut_search_s == 3.0
    monkeypatch.setenv("APP_VAD_CUT_SEARCH_S", "0")
    assert settings().vad_cut_search_s == 0.0
    for bad in (
        {"vad_cut_search_s": -0.1},
        {"vad_cut_search_s": 15},
        {"vad_cut_search_s": 3, "vad_max_speech_s": 2},
    ):
        with pytest.raises(ValidationError):
            settings(**bad)


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


class LoudVAD:
    """A Silero stand-in: loud windows are speech; it counts the windows it judged and its resets."""

    def __init__(self) -> None:
        self.windows = 0
        self.resets = 0

    def __call__(self, windows: Any) -> Any:
        self.windows += len(windows)
        return loud(windows)

    def reset(self) -> None:
        self.resets += 1


def tracks() -> float:
    return REGISTRY.get_sample_value("grill_stt_tracks") or 0.0


def listen(
    monkeypatch: pytest.MonkeyPatch, vad: LoudVAD | None = None, **kw: Any
) -> tuple[Worker, list[FakeStream], list[tuple[int, int]], list[Utterance]]:
    """A worker on FakeStreams; utterances land in a list as (participant, samples), and whole."""
    streams: list[FakeStream] = []

    def stream(track: object, **kw: object) -> FakeStream:
        streams.append(FakeStream(track, **kw))
        return streams[-1]

    monkeypatch.setattr(rtc, "AudioStream", stream)
    monkeypatch.setattr(worker_mod, "Silero", LoudVAD if vad is None else lambda: vad)
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)), **kw)
    submitted: list[tuple[int, int]] = []
    heard: list[Utterance] = []

    def submit(u: Utterance) -> None:
        submitted.append((u.participant_id, len(u.audio)))
        heard.append(u)

    monkeypatch.setattr(w._pipeline, "submit", submit)
    return w, streams, submitted, heard


@pytest.fixture
def listening(monkeypatch: pytest.MonkeyPatch) -> tuple[Worker, list[FakeStream], list[tuple[int, int]]]:
    return listen(monkeypatch)[:3]


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


async def test_an_utterance_ends_when_its_speech_does(monkeypatch: pytest.MonkeyPatch) -> None:
    """Line latency counts from the end of speech: 800 ms before a pause cut, at a forced 15 s cut,
    where the speaker never paused (spike S007-ab: dating it 800 ms early overstated the p95)."""
    streams: list[FakeStream] = []

    def stream(track: object, **kw: object) -> FakeStream:
        streams.append(FakeStream(track, **kw))
        return streams[-1]

    monkeypatch.setattr(rtc, "AudioStream", stream)
    monkeypatch.setattr(worker_mod, "Silero", LoudVAD)
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    w._clock = lambda: 1000.0
    ended: list[float] = []
    monkeypatch.setattr(w._pipeline, "submit", lambda u: ended.append(u.ended))
    subscribe(w, "TR_a", "7")
    await settle(lambda: streams)
    streams[0].frames.put_nowait(speech(1) + bytes(int(0.9 * 16000) * 2))
    streams[0].frames.put_nowait(speech(16))
    await settle(lambda: len(ended) == 2)
    assert [round(1000 - e, 2) for e in ended] == [0.8, 0.0]
    await streams[0].aclose()


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


STANDARD = rtc.ParticipantKind.PARTICIPANT_KIND_STANDARD


def human(identity: str, kind: int = STANDARD, **pubs: Any) -> SimpleNamespace:
    return SimpleNamespace(identity=identity, kind=kind, track_publications=pubs)


class FakePub:
    def __init__(self, kind: int = rtc.TrackKind.KIND_AUDIO) -> None:
        self.kind = kind
        self.subscribed: bool | None = None

    def set_subscribed(self, on: bool) -> None:
        self.subscribed = on


class FakeRoom:
    """An rtc.Room the test connects and closes; it records its disconnect."""

    def __init__(self) -> None:
        self.connected = False
        self.left = False
        self.remote_participants: dict[str, Any] = {"7": human("7")}
        self.handlers: dict[str, Any] = {}

    def on(self, event: str, cb: Any) -> None:
        self.handlers[event] = cb

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


def test_heartbeat_period_defaults_to_two_seconds() -> None:
    """010-AC14: a cleared device flag reaches the worker within one 2 s beat (010-AC15)."""
    assert Settings.model_fields["heartbeat_seconds"].default == 2.0


def device_tracks() -> float:
    return REGISTRY.get_sample_value("grill_stt_device_tracks") or 0.0


def pause() -> bytes:
    return bytes(int(0.9 * 16000) * 2)


def pcm(data: bytes) -> Any:
    samples = np.frombuffer(data, np.int16).astype(np.float32)
    samples /= 32768
    return samples


async def drained(stream: FakeStream) -> None:
    # The listener takes and handles every queued frame in one step: an empty queue means all handled.
    await settle(stream.frames.empty)
    await asyncio.sleep(0)


async def test_a_flagged_track_stays_subscribed_and_its_audio_skips_the_vad(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """010-AC14: the worker keeps the stream, drops the speech in progress, leaves the participant out
    of voices. 010-AC15: once cleared, the next frame goes to a fresh segmenter."""
    streams: list[FakeStream] = []

    def stream(track: object, **kw: object) -> FakeStream:
        streams.append(FakeStream(track, **kw))
        return streams[-1]

    vad = LoudVAD()
    monkeypatch.setattr(rtc, "AudioStream", stream)
    monkeypatch.setattr(worker_mod, "Silero", lambda: vad)
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    submitted: list[tuple[int, int]] = []
    monkeypatch.setattr(w._pipeline, "submit", lambda u: submitted.append((u.participant_id, len(u.audio))))
    subscribe(w, "TR_a", "7")
    subscribe(w, "TR_b", "8")
    await settle(lambda: len(streams) == 2)
    a = streams[0]

    # Cut before the flag: submitted as usual.
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(submitted) == 1)
    whole = submitted[0]
    # The device starts mid-sentence: its capture predates the flag, so the half sentence is its own.
    a.frames.put_nowait(speech(1))
    await drained(a)
    w._changed.clear()
    with structlog.testing.capture_logs() as logs:
        w.flag(frozenset({("r1", 7)}))
    assert logs == [{"event": "track_device", "room": "r1", "participant": 7, "log_level": "info"}]
    assert w._changed.is_set()
    assert w.voices() == {"r1": [8]} and device_tracks() == 1
    seen = vad.windows
    a.frames.put_nowait(pause() + speech(1) + pause())
    await drained(a)
    assert vad.windows == seen and submitted == [whole]
    assert set(w._streams) == {"TR_a", "TR_b"} and not a.closed

    # Cleared: the first frame reaches the VAD, and the half sentence does not come back.
    w._changed.clear()
    with structlog.testing.capture_logs() as logs:
        w.flag(frozenset())
    assert [e["event"] for e in logs] == ["track_unflagged"]
    assert w._changed.is_set() and w.voices() == {"r1": [7, 8]} and device_tracks() == 0
    with structlog.testing.capture_logs() as logs:
        a.frames.put_nowait(pause())
        await drained(a)
        on_first = [e["event"] for e in logs]
        a.frames.put_nowait(pause())
        await drained(a)
    assert on_first == ["track_resumed"]
    assert logs == [
        {"event": "track_resumed", "room": "r1", "participant": 7, "track": "TR_a", "log_level": "info"}
    ]
    assert vad.windows > seen and vad.resets == 1 and submitted == [whole]
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(submitted) == 2)
    fresh = Segmenter(loud).feed(pcm(pause() + speech(1) + pause()))
    assert submitted == [whole, (7, len(fresh[0]))]

    # The same flags again change nothing, and send no extra beat.
    w._changed.clear()
    w.flag(frozenset())
    assert not w._changed.is_set()
    for s in streams:
        await s.aclose()


@pytest.mark.parametrize(("frames_after_flag", "unflag"), [(0, False), (2, False), (2, True)])
async def test_speech_in_progress_when_the_flag_comes_is_dropped(
    listening: tuple[Worker, list[FakeStream], list[tuple[int, int]]], frames_after_flag: int, unflag: bool
) -> None:
    """010-AC14: the speech the flag cut is the device's, even when the track ends flagged, or ends
    after an unflag before any new frame."""
    w, streams, submitted = listening
    base = tracks()
    subscribe(w, "TR_a", "7")
    await settle(lambda: tracks() == base + 1)
    streams[0].frames.put_nowait(speech(1))
    await drained(streams[0])
    w.flag(frozenset({("r1", 7)}))
    for _ in range(frames_after_flag):
        streams[0].frames.put_nowait(speech(1))
    await drained(streams[0])
    if unflag:
        w.flag(frozenset())
    w._close("TR_a")
    await settle(lambda: tracks() == base)
    assert submitted == []


async def test_an_utterance_cut_before_the_flag_still_posts(monkeypatch: pytest.MonkeyPatch) -> None:
    """010-AC14: the flag only stops new audio; the queue posts what was cut before it."""
    streams: list[FakeStream] = []

    def stream(track: object, **kw: object) -> FakeStream:
        streams.append(FakeStream(track, **kw))
        return streams[-1]

    monkeypatch.setattr(rtc, "AudioStream", stream)
    monkeypatch.setattr(worker_mod, "Silero", LoudVAD)
    lines: list[object] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.url.path.endswith("/voice/lines"):
            lines.append(json.loads(r.content))
        return httpx.Response(201)

    room_api = RoomAPI(httpx.AsyncClient(base_url="http://room", transport=httpx.MockTransport(handle)))
    pipeline = Pipeline([lambda _a, _p: Line("hello", "en")], room_api, size=4)
    w = Worker(settings(), pipeline, room_api, lk=None)  # type: ignore[arg-type] # unused here
    subscribe(w, "TR_a", "7")
    await settle(lambda: streams)
    streams[0].frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(pipeline._pending) == 1)
    w.flag(frozenset({("r1", 7)}))
    task = asyncio.create_task(pipeline.run())
    await asyncio.wait_for(pipeline.idle(), 5)
    task.cancel()
    assert lines == [{"participant_id": 7, "body": "hello"}]
    await streams[0].aclose()


def stalled(r: httpx.Request) -> httpx.Response:
    raise httpx.ReadTimeout("stalled", request=r)


@pytest.mark.parametrize(
    "cleared", [lambda _r: httpx.Response(200, json={"device": {}}), lambda _r: httpx.Response(503), stalled]
)
async def test_the_heartbeat_answer_flags_tracks_and_beats_again_at_once(
    listening: tuple[Worker, list[FakeStream], list[tuple[int, int]]],
    cleared: Callable[[httpx.Request], httpx.Response],
) -> None:
    """010-AC14: a 200 flags participant 7; the voices changed, so a second beat leaves without
    waiting the period (30 s here). 010-AC15: a cleared flag is applied as soon as the beat returns;
    a refused or timed-out beat clears it too, as the room's flag lapses meanwhile. The beat times out
    at 2 s, under the room's 3 s lapse, though the period here is 30 s."""
    w, streams, submitted = listening
    flagged = {"device": {"r1": [7]}}
    beats: list[dict[str, Any]] = []

    def handle(r: httpx.Request) -> httpx.Response:
        beats.append(json.loads(r.content))
        return httpx.Response(200, json=flagged) if len(beats) <= 2 else cleared(r)

    w._api = RoomAPI(httpx.AsyncClient(base_url="http://room", transport=httpx.MockTransport(handle)))
    beat = w._api.heartbeat
    timeouts: list[object] = []

    async def timed(*a: Any, **kw: Any) -> frozenset[tuple[str, int]]:
        timeouts.append(kw.get("timeout"))
        return await beat(*a, **kw)

    w._api.heartbeat = timed  # type: ignore[method-assign]
    subscribe(w, "TR_a", "7")
    subscribe(w, "TR_b", "8")
    task = asyncio.create_task(w.heartbeat())
    await settle(lambda: len(beats) == 2)
    assert [b["voices"] for b in beats] == [{"r1": [7, 8]}, {"r1": [8]}]
    assert w._device == frozenset({("r1", 7)})
    # The third beat (any change will do) brings the clear back.
    w._changed.set()
    await settle(lambda: len(beats) == 4)
    assert w._device == frozenset() and beats[3]["voices"] == {"r1": [7, 8]}
    assert set(timeouts) == {2.0}
    streams[0].frames.put_nowait(speech(1) + pause())
    await settle(lambda: submitted)
    assert [pid for pid, _ in submitted] == [7]
    task.cancel()
    for s in streams:
        await s.aclose()


class Stop(Exception):
    pass


@pytest.mark.parametrize(("took", "wait"), [(0.5, 1.5), (0.0, 2.0), (3.0, 0.0)])
async def test_beats_are_spaced_start_to_start(
    monkeypatch: pytest.MonkeyPatch, took: float, wait: float
) -> None:
    """010-AC15: a slow beat does not push back the next one, so a cleared flag arrives within 2 s."""
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)), heartbeat_seconds=2)
    now = [100.0]
    w._clock = lambda: now[0]

    async def slow_beat(*_a: object, **_kw: object) -> frozenset[tuple[str, int]]:
        now[0] += took
        return frozenset()

    waits: list[float] = []

    async def record(aw: Any, timeout: float) -> None:
        aw.close()
        waits.append(timeout)
        raise Stop

    monkeypatch.setattr(w._api, "heartbeat", slow_beat)
    monkeypatch.setattr(asyncio, "wait_for", record)
    with pytest.raises(Stop):
        await w.heartbeat()
    assert waits == [wait]


AGENT = rtc.ParticipantKind.PARTICIPANT_KIND_AGENT


async def test_a_persona_bot_is_never_subscribed(monkeypatch: pytest.MonkeyPatch) -> None:
    """013-AC19: humans' audio is subscribed at join and when published; a bot's never, video never."""
    made: list[FakeRoom] = []
    monkeypatch.setattr(rtc, "Room", lambda: new_room(made))
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    alice_mic, alice_cam, bot_mic = FakePub(), FakePub(rtc.TrackKind.KIND_VIDEO), FakePub()
    room = FakeRoom()
    room.remote_participants = {
        "7": human("7", mic=alice_mic, cam=alice_cam),
        "persona-3": human("persona-3", AGENT, mic=bot_mic),
    }
    monkeypatch.setattr(rtc, "Room", lambda: room)
    with structlog.testing.capture_logs() as logs:
        await w.reconcile([listed("r1", "RM_a", people=2)])
        w._subscribe("r1", late := FakePub(), human("persona-4", AGENT))  # type: ignore[arg-type]
        w._subscribe("r1", bob := FakePub(), human("8"))  # type: ignore[arg-type]
        w._skip_agent("r1", human("persona-4", AGENT))  # type: ignore[arg-type]
    got = (alice_mic.subscribed, alice_cam.subscribed, bot_mic.subscribed, late.subscribed, bob.subscribed)
    assert got == (True, None, None, None, True)
    assert not [e for e in logs if e["event"] == "track_ignored"]
    skipped = [e["identity"] for e in logs if e["event"] == "participant_skipped"]
    assert skipped == ["persona-3", "persona-4"]
    assert all(e["log_level"] == "debug" for e in logs if e["event"] == "participant_skipped")


class FakeLK:
    """LiveKit's room API as the worker lists participants: who is in, by kind."""

    def __init__(self) -> None:
        self.kinds: list[Any] = []
        self.room = self

    async def list_participants(self, _req: object) -> SimpleNamespace:
        return SimpleNamespace(participants=[SimpleNamespace(kind=k) for k in self.kinds])


async def test_a_room_with_only_bots_is_left_and_joined_again_once_a_human_is_listed(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """013-AC19: once only persona bots remain the worker leaves, stays out while LiveKit lists bots
    only, whatever their count, and comes back for a human, even one alone at the count bots had."""
    made: list[FakeRoom] = []
    monkeypatch.setattr(rtc, "Room", lambda: new_room(made))
    w = worker(httpx.MockTransport(lambda _r: httpx.Response(204)))
    lk = FakeLK()
    w._lk = lk  # type: ignore[assignment]
    await w.reconcile([listed("r1", "RM_a", people=2)])
    made[0].remote_participants = {p: human(p, AGENT) for p in ("persona-3", "persona-4")}
    w._leave_if_alone("r1")
    await settle(lambda: made[0].left)
    w._forget("r1", made[0], "left")  # type: ignore[arg-type]
    agent_kind = models.ParticipantInfo.Kind.AGENT
    for kinds in ([agent_kind, agent_kind], [agent_kind]):
        lk.kinds = kinds
        await w.reconcile([listed("r1", "RM_a", people=len(kinds))])
        assert len(made) == 1, "joined a room with only bots in it again"
    # The bot left and a human came, alone: the same count as before, a human now.
    lk.kinds = [models.ParticipantInfo.Kind.STANDARD]
    await w.reconcile([listed("r1", "RM_a", people=1)])
    assert len(made) == 2 and w.rooms["r1"] is cast(object, made[1])


def silence(seconds: float) -> bytes:
    return bytes(int(seconds * 16000) * 2)


async def test_each_utterance_carries_the_gap_before_it(monkeypatch: pytest.MonkeyPatch) -> None:
    """018-AC1: the worker hands each utterance the pause before it on its track, and none to the first
    one after a resume."""
    w, streams, _, heard = listen(monkeypatch)
    subscribe(w, "TR_a", "7")
    await settle(lambda: streams)
    a = streams[0]
    a.frames.put_nowait(speech(1) + silence(1.5) + speech(1) + pause())
    await settle(lambda: len(heard) == 2)
    gap = heard[1].gap_s
    assert heard[0].gap_s is None and gap is not None and abs(gap - 1.5) <= 2 * WINDOW / 16000
    w.flag(frozenset({("r1", 7)}))
    a.frames.put_nowait(pause())
    await drained(a)
    w.flag(frozenset())
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(heard) == 3)
    assert heard[2].gap_s is None and not any(u.forced for u in heard)
    await a.aclose()


async def test_the_cut_search_setting_reaches_the_segmenter(monkeypatch: pytest.MonkeyPatch) -> None:
    """018-AC3: with APP_VAD_CUT_SEARCH_S at 1, a dip 2 s before the 15 s cap is out of reach: the
    forced cut stays at the cap, where the 3 s default would move it to the dip."""
    w, streams, _, heard = listen(monkeypatch, vad_cut_search_s=1)
    subscribe(w, "TR_a", "7")
    await settle(lambda: streams)
    streams[0].frames.put_nowait(speech(13) + silence(0.1) + speech(3) + pause())
    await settle(lambda: len(heard) == 2)
    assert heard[0].forced and len(heard[0].audio) == 469 * WINDOW
    assert heard[1].gap_s == 0.0
    await streams[0].aclose()


async def muted_room(
    monkeypatch: pytest.MonkeyPatch,
) -> tuple[Worker, FakeStream, list[Utterance], Callable[[str], None], list[int]]:
    """A listened track TR_a of participant 7, its track_muted handler, and the forget_gap calls."""
    made: list[FakeRoom] = []
    monkeypatch.setattr(rtc, "Room", lambda: new_room(made))
    forgot: list[int] = []
    forget = Segmenter.forget_gap

    def forget_gap(seg: Segmenter) -> None:
        forgot.append(1)
        forget(seg)

    monkeypatch.setattr(Segmenter, "forget_gap", forget_gap)
    w, streams, _, heard = listen(monkeypatch)
    await w.reconcile([listed("r1", "RM_a")])
    subscribe(w, "TR_a", "7")
    await settle(lambda: streams)
    handler = made[0].handlers["track_muted"]
    return w, streams[0], heard, lambda sid: handler(human("7"), SimpleNamespace(sid=sid)), forgot


async def test_a_mute_forgets_the_gap_and_keeps_the_audio(monkeypatch: pytest.MonkeyPatch) -> None:
    """018-AC1: a muted track may send no frames, so the line after a mute has no gap, even when the
    mute comes mid-utterance; nothing is flushed, cut or lost."""
    w, a, heard, mute, _ = await muted_room(monkeypatch)
    audio = (speech(1) + pause()) * 3 + speech(1) + speech(1) + pause()
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(heard) == 1)
    mute("TR_a")
    a.frames.put_nowait(speech(1) + pause() + speech(1) + pause() + speech(1))
    await drained(a)
    mute("TR_a")
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(heard) == 4)
    assert [u.gap_s for u in heard] == [None, None, 0.896, None]
    assert [len(u.audio) for u in heard] == [len(u) for u in Segmenter(loud).feed(pcm(audio))]
    await a.aclose()


async def test_a_mute_of_another_track_or_a_device_speaker_changes_nothing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """018-AC1: only the muted track forgets its gap, and not while the device transcribes the speaker."""
    w, a, heard, mute, forgot = await muted_room(monkeypatch)
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(heard) == 1)
    mute("TR_b")
    a.frames.put_nowait(speech(1) + pause())
    await settle(lambda: len(heard) == 2)
    assert heard[1].gap_s is not None and forgot == []
    w.flag(frozenset({("r1", 7)}))
    mute("TR_a")
    assert forgot == []
    await a.aclose()
