import asyncio

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
