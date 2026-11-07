import asyncio
from typing import Any

import numpy as np
import pytest

from grill_tts.bots import TrackOut


class Source:
    """An AudioSource that takes frames at once, like a track with room in its queue."""

    def __init__(self) -> None:
        self.frames = 0
        self.cleared = 0

    async def capture_frame(self, _frame: object) -> None:
        self.frames += 1
        await asyncio.sleep(0.001)

    def clear_queue(self) -> None:
        self.cleared += 1


# 013-AC22: the track sends silence between lines; once its bot left, play() raises at once and
# also while it waits, so the room drops the line instead of hanging.
async def test_track_out_pump_and_stop() -> None:
    src = Source()
    out = TrackOut(src)  # type: ignore[arg-type]
    await out.play(b"\x00" * 1920)
    while src.frames < 5:
        await asyncio.sleep(0.001)
    out.clear()
    assert src.cleared == 1
    out.stop()
    await asyncio.sleep(0)
    with pytest.raises(ConnectionError):
        await asyncio.wait_for(out.play(b"\x00" * 1920), 1)
    blocked = TrackOut(src)  # type: ignore[arg-type]
    blocked._next.put_nowait(b"x")  # the slot is taken: the next play() waits on the pump
    waiting = asyncio.ensure_future(blocked.play(b"y"))
    await asyncio.sleep(0)
    blocked.stop()
    with pytest.raises(ConnectionError):
        await asyncio.wait_for(waiting, 1)


class Frame:
    def __init__(self, amp: float) -> None:
        self.data = (np.sin(np.arange(160) / 3) * amp * 32767).astype(np.int16).tobytes()
        self.samples_per_channel = 160


# 013-AC35: a listener who rejoins gets a new track, so the echo warm-up (S16) starts over: a burst
# 1 s into the new track is ignored even though the old one heard the persona for 4.5 s.
async def test_listen_warm_up_resets_on_rejoin(monkeypatch: pytest.MonkeyPatch) -> None:
    from livekit import rtc

    from grill_tts.bots import Bots
    from grill_tts.room import Clock, RoomVoice

    class Now(Clock):
        t = 0.0

        def now(self) -> float:
            return self.t

    clock = Now()
    voice = RoomVoice("r1", None, lambda: None, clock)  # type: ignore[arg-type, return-value]
    calls: list[set[str]] = []
    monkeypatch.setattr(voice, "speakers", lambda humans, _bot="": calls.append(humans))
    frames: list[float] = []

    class Stream:
        def __init__(self, *_a: object, **_k: object) -> None:
            pass

        async def __aiter__(self):  # type: ignore[no-untyped-def]
            for amp in frames:
                clock.t += 0.01
                voice.played_at = clock.t  # a persona plays all along
                yield type("Ev", (), {"frame": Frame(amp)})

        async def aclose(self) -> None:
            pass

    monkeypatch.setattr(rtc, "AudioStream", Stream)
    burst = [0.14] * 6 + [0.0] * 50
    listen = Bots._listen
    frames[:] = [0.0] * 450 + burst
    await listen(None, voice, "vad:persona-1:alice", "alice", None)  # type: ignore[arg-type]
    assert {"alice"} in calls, "past 4.5 s of persona audio the burst did not start speech"
    calls.clear()
    frames[:] = [0.0] * 100 + burst
    await listen(None, voice, "vad:persona-1:alice", "alice", None)  # type: ignore[arg-type]
    assert {"alice"} not in calls, "the rejoined track kept the old track's warm-up"


# A human who leaves mid-speech must not hold the room: the SDK never ends a gone track's stream, so
# its onset stays "speaking" unless the bot stops listening when the track or the human goes.
@pytest.mark.parametrize(
    "leave", ["participant_disconnected", "track_unsubscribed", "bot_disconnected", "join_failed"]
)
async def test_leaver_mid_onset_frees_the_quiet(monkeypatch: pytest.MonkeyPatch, leave: str) -> None:
    from livekit import rtc

    from grill_tts.bots import Bots
    from grill_tts.room import RoomVoice

    from .test_client import settings

    class Room:
        def __init__(self) -> None:
            self.on_: dict[str, object] = {}
            self.remote_participants: dict[str, object] = {}
            self.local_participant = self

        def on(self, ev: str, fn: object) -> None:
            self.on_[ev] = fn

        def emit(self, ev: str, *a: object) -> None:
            if ev in self.on_:
                self.on_[ev](*a)  # type: ignore[operator]

        async def connect(self, *_a: object, **_k: object) -> None:
            if leave == "join_failed":
                self.emit("track_subscribed", track, mic, bob)

        async def publish_track(self, *_a: object, **_k: object) -> None:
            if leave == "join_failed":
                while not voice.humans:
                    await asyncio.sleep(0.001)
                raise ConnectionError("track publication timed out")

        async def disconnect(self) -> None:
            pass

        def isconnected(self) -> bool:
            return True

    class Stream:
        def __init__(self, *_a: object, **_k: object) -> None:
            pass

        async def __aiter__(self):  # type: ignore[no-untyped-def]
            for _ in range(10):
                yield type("Ev", (), {"frame": Frame(0.3)})
            await asyncio.Event().wait()  # like the SDK: no end of stream once the track is gone

        async def aclose(self) -> None:
            pass

    room = Room()
    monkeypatch.setattr(rtc, "Room", lambda: room)
    monkeypatch.setattr(rtc, "AudioSource", lambda *_a, **_k: Source())
    monkeypatch.setattr(rtc.LocalAudioTrack, "create_audio_track", staticmethod(lambda *_a: None))
    monkeypatch.setattr(rtc, "AudioStream", Stream)
    voice = RoomVoice("r1", None, lambda: None)  # type: ignore[arg-type, return-value]
    bots = Bots(settings(), lambda _r: voice)
    bob = type("P", (), {"identity": "bob", "kind": None})()
    track = type("T", (), {"kind": rtc.TrackKind.KIND_AUDIO})()
    mic = type("Pub", (), {"kind": rtc.TrackKind.KIND_AUDIO, "source": rtc.TrackSource.SOURCE_MICROPHONE})()
    bots.sync([{"room": "r1", "personas": [{"id": 1, "name": "Grill"}]}])
    if leave != "join_failed":
        while not bots.bots:
            await asyncio.sleep(0.001)
        room.emit("track_subscribed", track, mic, bob)
    while not voice.humans:
        await asyncio.sleep(0.001)
    if leave == "participant_disconnected":
        room.emit(leave, bob)
    elif leave == "track_unsubscribed":
        room.emit(leave, track, mic, bob)
    elif leave == "bot_disconnected":
        room.emit("disconnected", "network")
    quiet_s = 0.2
    try:
        assert await asyncio.wait_for(voice._quiet(quiet_s, voice.clock.now() + 20), quiet_s + 1.0)
    finally:
        await bots.close()


# A deleted room's bot must not rejoin on the next heartbeat, whose roster still lists the room: the
# join would recreate the LiveKit room. A later roster that lists it again does rejoin.
async def test_deleted_room_is_not_rejoined(monkeypatch: pytest.MonkeyPatch) -> None:
    from livekit import rtc

    from grill_tts.bots import Bots
    from grill_tts.room import RoomVoice

    from .test_client import settings

    joins = 0

    class Room:
        def __init__(self) -> None:
            self.on_: dict[str, object] = {}
            self.remote_participants: dict[str, object] = {}
            self.local_participant = self

        def on(self, ev: str, fn: object) -> None:
            self.on_[ev] = fn

        async def connect(self, *_a: object, **_k: object) -> None:
            nonlocal joins
            joins += 1

        async def publish_track(self, *_a: object, **_k: object) -> None:
            pass

        async def disconnect(self) -> None:
            pass

        def isconnected(self) -> bool:
            return True

    rooms: list[Room] = []

    def new_room() -> Room:
        rooms.append(Room())
        return rooms[-1]

    monkeypatch.setattr(rtc, "Room", new_room)
    monkeypatch.setattr(rtc, "AudioSource", lambda *_a, **_k: Source())
    monkeypatch.setattr(rtc.LocalAudioTrack, "create_audio_track", staticmethod(lambda *_a: None))
    voice = RoomVoice("r1", None, lambda: None)  # type: ignore[arg-type, return-value]
    bots = Bots(settings(), lambda _r: voice)
    roster = [{"room": "r1", "personas": [{"id": 1, "name": "Grill"}]}]

    async def joined(n: int) -> None:
        while joins < n or not bots.bots:
            await asyncio.sleep(0.001)

    try:
        bots.sync(roster)
        await asyncio.wait_for(joined(1), 1)
        rooms[-1].on_["disconnected"](rtc.DisconnectReason.ROOM_DELETED)  # type: ignore[operator]
        assert not bots.bots
        bots.sync(bots.roster)  # the heartbeat
        await asyncio.sleep(0.01)
        assert joins == 1 and not bots.bots, "the bot rejoined its deleted room"
        bots.sync(roster)
        await asyncio.wait_for(joined(2), 1)
    finally:
        await bots.close()


# 030-AC15: a human's camera, screen share and its audio are never subscribed nor listened to, at
# join or when published later; the microphone is.
async def test_bot_subscribes_to_the_microphone_only(monkeypatch: pytest.MonkeyPatch) -> None:
    from livekit import rtc

    from grill_tts.bots import Bots
    from grill_tts.room import RoomVoice

    from .test_client import settings

    class Pub:
        def __init__(self, kind: int, source: int) -> None:
            self.kind = kind
            self.source = source
            self.subscribed = False

        def set_subscribed(self, on: bool) -> None:
            self.subscribed = on

    audio, video, src = rtc.TrackKind.KIND_AUDIO, rtc.TrackKind.KIND_VIDEO, rtc.TrackSource
    shared = [
        (video, src.SOURCE_CAMERA),
        (video, src.SOURCE_SCREENSHARE),
        (audio, src.SOURCE_SCREENSHARE_AUDIO),
    ]
    mic = Pub(audio, src.SOURCE_MICROPHONE)
    at_join = [Pub(k, s) for k, s in shared]
    later = [Pub(k, s) for k, s in shared]
    pubs = {"m": mic, **{f"p{i}": p for i, p in enumerate(at_join)}}
    alice = type("P", (), {"identity": "7", "kind": None, "track_publications": pubs})()

    class Room:
        def __init__(self) -> None:
            self.on_: dict[str, object] = {}
            self.remote_participants: dict[str, object] = {"7": alice}
            self.local_participant = self

        def on(self, ev: str, fn: object) -> None:
            self.on_[ev] = fn

        async def connect(self, *_a: object, **_k: object) -> None:
            pass

        async def publish_track(self, *_a: object, **_k: object) -> None:
            pass

        async def disconnect(self) -> None:
            pass

        def isconnected(self) -> bool:
            return True

    room = Room()
    monkeypatch.setattr(rtc, "Room", lambda: room)
    monkeypatch.setattr(rtc, "AudioSource", lambda *_a, **_k: Source())
    monkeypatch.setattr(rtc.LocalAudioTrack, "create_audio_track", staticmethod(lambda *_a: None))
    voice = RoomVoice("r1", None, lambda: None)  # type: ignore[arg-type, return-value]
    bots = Bots(settings(), lambda _r: voice)
    try:
        bots.sync([{"room": "r1", "personas": [{"id": 1, "name": "Grill"}]}])
        while not bots.bots:
            await asyncio.sleep(0.001)
        for pub in later:
            room.on_["track_published"](pub, alice)  # type: ignore[operator]
        assert mic.subscribed and not any(p.subscribed for p in at_join + later)
        # A subscription that lands anyway is not listened to as Alice speaking.
        for pub in at_join:
            pub.subscribed = True
            room.on_["track_subscribed"](type("T", (), {"kind": pub.kind})(), pub, alice)  # type: ignore[operator]
        assert not next(iter(bots.bots.values())).listens
        # The heartbeat's count of human tracks counts the microphone only.
        assert bots.human_tracks() == 1
    finally:
        await bots.close()


# 032-AC10: the roster's time keeper has its voice and rate's interjections made, on each sync so a
# voice or rate edit is made too; a persona that does not cut in has none.
async def test_only_the_time_keeper_is_warmed() -> None:
    from grill_tts.bots import Bots
    from grill_tts.room import RoomVoice

    from .test_client import settings

    voice = RoomVoice("r1", None, lambda: None)  # type: ignore[arg-type, return-value]
    warmed: list[tuple[str, float]] = []
    bots = Bots(settings(), lambda _r: voice, lambda v, r: warmed.append((v, r)))

    async def no_join(*_a: object) -> None:
        pass

    bots._join = no_join  # type: ignore[method-assign, assignment]

    def roster(keeper_voice: str, rate: float) -> list[dict[str, Any]]:
        return [
            {
                "room": "r1",
                "personas": [
                    {"id": 1, "name": "Grill", "voice": "marius", "rate": 1.0, "cut_in_s": 0},
                    {"id": 2, "name": "Timekeeper", "voice": keeper_voice, "rate": rate, "cut_in_s": 20},
                ],
            }
        ]

    bots.sync(roster("alba", 1.0))
    bots.sync(roster("alba", 1.2))
    bots.sync(roster("estelle", 1.2))
    assert [w for w in warmed if w[0] == "marius"] == []
    assert ("alba", 1.0) in warmed and ("alba", 1.2) in warmed and ("estelle", 1.2) in warmed
    await bots.close()
