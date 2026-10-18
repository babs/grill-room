"""Entry point: python -m grill_tts."""

import asyncio
import contextlib
import os
import signal
import sys
import time
from typing import Any

import httpx
import structlog
from aiohttp import web
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import ValidationError

from . import metrics
from .bots import Bots, TrackOut
from .client import ServerLink
from .config import Settings
from .engine import Engine, Fake
from .logs import setup_logging
from .room import Clock, Line, Pace, RoomVoice
from .textlayer import Entry, TextLayer, load_seed

log = structlog.get_logger()

# A dropped state shows this long, then the persona reads idle again.
DROPPED_SHOWN_S = 10.0


def load_engine(s: Settings) -> Engine:
    if s.engine == "fake":
        return Fake()
    from .engine import Pocket

    return Pocket(s.models_dir, s.cpu_threads)


class Pod:
    def __init__(self, s: Settings, engine: Engine, link_client: httpx.AsyncClient) -> None:
        self._s = s
        self.engine = engine
        self.clock = Clock()
        self.pace = Pace()
        self.layer = TextLayer(load_seed())
        self.voices: dict[str, RoomVoice] = {}
        self._tasks: set[asyncio.Task[None]] = set()
        self.bots = Bots(s, self.voice)
        self.link = ServerLink(link_client, self.bots.sync, self.line, self.lexicon)

    def voice(self, room: str) -> RoomVoice:
        rv = self.voices.get(room)
        if rv is None:

            def out(persona: int) -> TrackOut | None:
                return self.bots.out(room, persona)

            rv = self.voices[room] = RoomVoice(
                room, self.engine, lambda: self.layer, self.clock, out, self.pace
            )
            task = asyncio.create_task(rv.run())
            self._tasks.add(task)
        return rv

    def lexicon(self, entries: list[dict[str, Any]]) -> None:
        """The room server's lexicon replaces the seed, for the next sentence (013-AC31)."""
        self.layer = TextLayer(Entry(**e) for e in entries)
        log.info("lexicon_loaded", terms=len(entries))

    def line(self, d: dict[str, Any]) -> None:
        line = Line.parse(d, self.clock.now())
        metrics.RECEIVED.inc()
        log.info(
            "line_received",
            room=line.room,
            persona_id=line.persona_id,
            seq=line.seq,
            words=len(line.text.split()),
        )
        self.voice(line.room).add(line)

    def states(self) -> list[dict[str, Any]]:
        now = self.clock.now()
        out = []
        for room, rv in self.voices.items():
            for persona, st in rv.states.items():
                state, reason = st.state, st.reason
                if state == "dropped" and now - st.at > DROPPED_SHOWN_S:
                    state, reason = "idle", ""
                if state == "idle" and (room, persona) not in self.bots.bots:
                    continue
                out.append(
                    {"room": room, "persona_id": persona, "state": state}
                    | ({"reason": reason} if reason else {})
                )
        return out

    async def heartbeat(self) -> None:
        while True:
            started = time.monotonic()
            self.bots.sync(self.bots.roster)
            await self.link.beat(
                {
                    "pod": self._s.pod_name[:64],
                    "rooms": sorted(self.bots.rooms),
                    "bots": sorted(f"{r}/persona-{p}" for r, p in self.bots.bots),
                    "states": self.states(),
                }
            )
            await asyncio.sleep(max(0.0, self._s.heartbeat_seconds - (time.monotonic() - started)))

    async def close(self) -> None:
        for t in self._tasks:
            t.cancel()
        await self.bots.close()


async def serve_http(port: int, ready: asyncio.Event) -> web.AppRunner:
    async def healthz(_: web.Request) -> web.Response:
        # 503 while the models load: nothing can be said yet.
        if not ready.is_set():
            return web.json_response({"status": "loading"}, status=503)
        return web.json_response({"status": "ok"})

    async def metrics_page(_: web.Request) -> web.Response:
        return web.Response(body=generate_latest(), headers={"Content-Type": CONTENT_TYPE_LATEST})

    app = web.Application()
    app.router.add_get("/healthz", healthz)
    app.router.add_get("/metrics", metrics_page)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    await web.TCPSite(runner, "0.0.0.0", port).start()  # all interfaces: a container port
    return runner


async def run(s: Settings) -> None:
    client = httpx.AsyncClient(
        base_url=s.room_url, headers={"Authorization": f"Bearer {s.room_token.get_secret_value()}"}
    )
    ready = asyncio.Event()
    runner = await serve_http(s.port, ready)
    pod: Pod | None = None
    try:
        log.info("model_loading", engine=s.engine, cpu_threads=s.cpu_threads)
        started = time.monotonic()
        engine = await asyncio.to_thread(load_engine, s)
        log.info("model_ready", engine=s.engine, load_s=round(time.monotonic() - started, 1))
        pod = Pod(s, engine, client)
        ready.set()
        async with asyncio.TaskGroup() as tg:
            tg.create_task(pod.link.events())
            tg.create_task(pod.heartbeat())
            tg.create_task(pod.link.samples(s.models_dir / "samples"))
    finally:
        if pod is not None:
            await pod.close()
        await client.aclose()
        await runner.cleanup()


def main() -> None:
    try:
        s = Settings()  # type: ignore[call-arg] # pydantic-settings reads the required fields from the env
    except ValidationError as e:
        setup_logging("info")
        log.error(
            "config_invalid",
            errors=[f"APP_{'_'.join(map(str, x['loc'])).upper()}: {x['msg']}" for x in e.errors()],
        )
        sys.exit(2)
    setup_logging(s.log_level)
    log.info(
        "startup",
        version=os.getenv("VERSION", "v0.0.0"),
        commit_hash=os.getenv("COMMIT_HASH", "0000000"),
        build_timestamp=os.getenv("BUILD_TIMESTAMP", "1970-01-01T00:00:00"),
        project_url=os.getenv("PROJECT_URL", "https://github.com/babs/grill-room"),
        pod=s.pod_name,
        engine=s.engine,
    )

    async def guarded() -> None:
        task = asyncio.current_task()
        assert task is not None
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, task.cancel)
        with contextlib.suppress(asyncio.CancelledError):
            await run(s)
        log.info("shutdown")

    asyncio.run(guarded())


if __name__ == "__main__":
    main()
