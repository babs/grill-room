"""The room server side: lines and rooms from its event stream, the pod's heartbeat back (013-AC14)."""

import asyncio
import json
from collections.abc import Callable
from typing import Any

import httpx
import structlog

log = structlog.get_logger()

# The server pings every 15 s: a stream silent for longer is dead, reconnect.
READ_TIMEOUT_S = 40.0
MAX_BACKOFF_S = 10.0


class ServerLink:
    def __init__(
        self,
        client: httpx.AsyncClient,
        on_rooms: Callable[[list[dict[str, Any]]], None],
        on_line: Callable[[dict[str, Any]], None],
    ) -> None:
        self._client = client
        self._on_rooms = on_rooms
        self._on_line = on_line
        # The last line taken: a reconnect resumes after it (Last-Event-ID).
        self.last = 0
        self.connected = False

    def dispatch(self, event: str, data: str, eid: str) -> None:
        try:
            body = json.loads(data)
            if event == "rooms":
                self._on_rooms(body["rooms"])
            elif event == "line":
                self._on_line(body)
        except Exception:
            # One bad event must not end the stream: a reconnect would replay the same line forever.
            log.exception("event_refused", kind=event, id=eid)
        if event == "line" and eid.isdigit():
            self.last = int(eid)

    async def _stream(self) -> None:
        headers = {"Last-Event-ID": str(self.last)} if self.last else {}
        timeout = httpx.Timeout(10.0, read=READ_TIMEOUT_S)
        async with self._client.stream("GET", "/api/tts/events", headers=headers, timeout=timeout) as r:
            r.raise_for_status()
            self.connected = True
            log.info("stream_open", after=self.last)
            event, data, eid = "", "", ""
            async for line in r.aiter_lines():
                if line == "":
                    if data:
                        self.dispatch(event, data, eid)
                    event, data, eid = "", "", ""
                elif line.startswith("event: "):
                    event = line[7:]
                elif line.startswith("data: "):
                    data = line[6:]
                elif line.startswith("id: "):
                    eid = line[4:]

    async def events(self) -> None:
        backoff = 1.0
        while True:
            try:
                await self._stream()
                backoff = 1.0
            except asyncio.CancelledError:
                raise
            except Exception as e:  # the server restarting must not stop the pod
                log.warning("stream_failed", error=str(e) or type(e).__name__)
            self.connected = False
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, MAX_BACKOFF_S)

    async def beat(self, body: dict[str, Any]) -> None:
        try:
            r = await self._client.post("/api/tts/heartbeat", json=body, timeout=httpx.Timeout(5.0))
            r.raise_for_status()
        except httpx.HTTPError as e:
            log.warning("heartbeat_failed", error=str(e) or type(e).__name__)
