import asyncio

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
