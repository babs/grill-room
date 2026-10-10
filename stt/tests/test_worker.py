import asyncio
from types import SimpleNamespace
from typing import Any

import httpx
import pytest
from livekit import rtc

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


def worker(handler: httpx.MockTransport) -> Worker:
    room_api = RoomAPI(httpx.AsyncClient(base_url="http://room", transport=handler))
    pipeline = Pipeline(lambda _a: Line("", "en"), room_api, size=1)
    return Worker(settings(), pipeline, room_api, lk=None)  # type: ignore[arg-type] # unused here


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
    assert beats == [b'{"worker":"w1","rooms":[]}', b'{"worker":"w1","rooms":["r1"]}']
    task.cancel()


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
    w._on_track("r1", SimpleNamespace(kind=kind), SimpleNamespace(identity=identity))  # type: ignore[arg-type]
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
