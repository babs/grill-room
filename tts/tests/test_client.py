import asyncio
import json
from typing import Any

import httpx
import pytest
from pydantic import SecretStr

from grill_tts.__main__ import DROPPED_SHOWN_S, Pod
from grill_tts.client import ServerLink
from grill_tts.config import Settings
from grill_tts.engine import Fake
from grill_tts.room import State


def sse(*events: tuple[str, str, dict[str, Any]]) -> bytes:
    out = ": ping\n\n"
    for name, eid, data in events:
        out += (f"id: {eid}\n" if eid else "") + f"event: {name}\ndata: {json.dumps(data)}\n\n"
    return out.encode()


LINE = {
    "seq": 7,
    "room": "r1",
    "persona_id": 3,
    "persona": "Grill",
    "voice": "alba",
    "rate": 1.0,
    "text": "Hi.",
    "quiet_ms": 1000,
    "resume_s": 20,
    "queue_s": 40,
}


# 013-AC14: rooms and lines come off the stream; a reconnect asks to resume after the last line.
async def test_stream_and_resume() -> None:
    seen: list[Any] = []
    asked: list[str | None] = []

    def handle(r: httpx.Request) -> httpx.Response:
        asked.append(r.headers.get("Last-Event-ID"))
        assert r.headers["Authorization"] == "Bearer tok"
        if len(asked) == 1:
            return httpx.Response(
                200,
                content=sse(("rooms", "", {"rooms": [{"room": "r1", "personas": []}]}), ("line", "7", LINE)),
            )
        return httpx.Response(200, content=sse(("line", "8", LINE | {"seq": 8})))

    client = httpx.AsyncClient(
        base_url="http://room", transport=httpx.MockTransport(handle), headers={"Authorization": "Bearer tok"}
    )
    link = ServerLink(
        client, lambda rooms: seen.append(("rooms", rooms)), lambda d: seen.append(("line", d["seq"]))
    )
    await link._stream()
    await link._stream()
    assert seen == [("rooms", [{"room": "r1", "personas": []}]), ("line", 7), ("line", 8)]
    assert asked == [None, "7"] and link.last == 8


# A line the pod cannot read is skipped, not replayed on every reconnect.
async def test_a_bad_line_is_skipped() -> None:
    seen: list[int] = []
    link = ServerLink(
        httpx.AsyncClient(base_url="http://room"), lambda _r: None, lambda d: seen.append(d["seq"])
    )
    link.dispatch("line", "{not json", "9")
    link.dispatch("line", json.dumps({"no": "seq"}), "10")
    link.dispatch("line", json.dumps(LINE | {"seq": 11}), "11")
    assert seen == [11] and link.last == 11


# The server restarting must not end the pod: a failed stream is retried.
async def test_events_retry(monkeypatch: pytest.MonkeyPatch) -> None:
    calls = 0

    def handle(_r: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(503)

    async def no_wait(_s: float) -> None:
        if calls >= 3:
            raise asyncio.CancelledError

    monkeypatch.setattr("grill_tts.client.asyncio.sleep", no_wait)
    link = ServerLink(
        httpx.AsyncClient(base_url="http://room", transport=httpx.MockTransport(handle)), print, print
    )
    with pytest.raises(asyncio.CancelledError):
        await link.events()
    assert calls == 3 and not link.connected


def settings() -> Settings:
    return Settings(
        room_url="http://room",
        room_token=SecretStr("t"),
        livekit_url="ws://lk",
        livekit_api_key="k",
        livekit_api_secret=SecretStr("s" * 32),
        engine="fake",
    )


# 013-AC14, 013-AC29: the heartbeat reports waiting and speaking states, a drop for a while with its
# reason, and leaves out idle personas with no bot.
async def test_pod_states() -> None:
    pod = Pod(settings(), Fake(), httpx.AsyncClient(base_url="http://room"))
    pod.line(LINE)
    rv = pod.voices["r1"]
    now = pod.clock.now()
    rv.states = {
        3: State("waiting", "", now),
        4: State("dropped", "stale", now),
        5: State("dropped", "stale", now - DROPPED_SHOWN_S - 1),
        6: State("idle", "", now),
    }
    assert pod.states() == [
        {"room": "r1", "persona_id": 3, "state": "waiting"},
        {"room": "r1", "persona_id": 4, "state": "dropped", "reason": "stale"},
    ]
    await pod.close()
