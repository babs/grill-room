"""Entry point: python -m grill_stt."""

import asyncio
import contextlib
import logging
import os
import signal
import sys

import httpx
import structlog
from aiohttp import web
from livekit import api
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from .config import Settings
from .pipeline import Pipeline, RoomAPI
from .transcribe import Transcriber
from .worker import Worker, http_url

log = structlog.get_logger()


def setup_logging(level: str) -> None:
    renderer: structlog.typing.Processor = (
        structlog.dev.ConsoleRenderer() if sys.stdout.isatty() else structlog.processors.JSONRenderer()
    )
    structlog.configure(
        processors=[
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso", utc=True),
            # A traceback becomes one escaped field: one JSON object per line.
            structlog.processors.dict_tracebacks,
            renderer,
        ],
        wrapper_class=structlog.make_filtering_bound_logger(logging.getLevelNamesMapping()[level.upper()]),
    )


async def serve_http(port: int, ready: asyncio.Event) -> web.AppRunner:
    async def healthz(_: web.Request) -> web.Response:
        # 503 while the model loads: nothing can be transcribed yet.
        if not ready.is_set():
            return web.json_response({"status": "loading"}, status=503)
        return web.json_response({"status": "ok"})

    async def metrics(_: web.Request) -> web.Response:
        return web.Response(body=generate_latest(), headers={"Content-Type": CONTENT_TYPE_LATEST})

    app = web.Application()
    app.router.add_get("/healthz", healthz)
    app.router.add_get("/metrics", metrics)
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    await web.TCPSite(runner, "0.0.0.0", port).start()  # all interfaces: a container port
    return runner


async def run(s: Settings) -> None:
    ready = asyncio.Event()
    runner = await serve_http(s.port, ready)
    log.info("model_loading", model=s.model, cpu_threads=s.cpu_threads)
    transcribe = await asyncio.to_thread(Transcriber, s.model, s.cpu_threads)
    ready.set()
    log.info("model_ready")
    client = httpx.AsyncClient(
        base_url=s.room_url,
        timeout=httpx.Timeout(10.0, connect=5.0),
        headers={"Authorization": f"Bearer {s.room_token.get_secret_value()}"},
    )
    lk = api.LiveKitAPI(http_url(s.livekit_url), s.livekit_api_key, s.livekit_api_secret.get_secret_value())
    room_api = RoomAPI(client)
    pipeline = Pipeline(transcribe, room_api, s.queue_size)
    worker = Worker(s, pipeline, room_api, lk)
    try:
        async with asyncio.TaskGroup() as tg:
            tg.create_task(pipeline.run())
            tg.create_task(worker.discover())
            tg.create_task(worker.heartbeat())
    finally:
        await worker.close()
        await lk.aclose()
        await client.aclose()
        await runner.cleanup()


def main() -> None:
    s = Settings()  # type: ignore[call-arg] # pydantic-settings reads the required fields from the env
    setup_logging(s.log_level)
    log.info(
        "startup",
        version=os.getenv("VERSION", "v0.0.0"),
        commit_hash=os.getenv("COMMIT_HASH", "0000000"),
        build_timestamp=os.getenv("BUILD_TIMESTAMP", "1970-01-01T00:00:00"),
        project_url=os.getenv("PROJECT_URL", "https://github.com/babs/grill-room"),
        worker=s.worker_name,
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
