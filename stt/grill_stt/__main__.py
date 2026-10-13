"""Entry point: python -m grill_stt."""

import asyncio
import contextlib
import os
import signal
import sys
from collections.abc import Callable
from pathlib import Path

import httpx
import structlog
from aiohttp import web
from livekit import api
from prometheus_client import CONTENT_TYPE_LATEST, Gauge, generate_latest
from pydantic import ValidationError

from .config import Settings
from .engines import ProcessEngine, argv
from .logs import setup_logging
from .pipeline import Pipeline, RoomAPI
from .worker import Worker, http_url

log = structlog.get_logger()

ENGINE = Gauge(
    "grill_stt_engine_info", "The running engine, its model and revision.", ["engine", "model", "revision"]
)


def engine(s: Settings) -> tuple[str, str]:
    """The model path and revision of the configured engine."""
    if s.engine == "parakeet":
        return s.parakeet_model, s.parakeet_revision
    return s.model, s.whisper_revision


def engine_info(s: Settings) -> None:
    model, revision = engine(s)
    ENGINE.labels(s.engine, Path(model).name, revision).set(1)


def engines(s: Settings) -> list[ProcessEngine]:
    model, _ = engine(s)
    return [
        ProcessEngine(argv(s.engine, model, s.cpu_threads), s.engine_timeout_s)
        for _ in range(s.engine_processes)
    ]


async def serve_http(port: int, ready: asyncio.Event, healthy: Callable[[], bool]) -> web.AppRunner:
    async def healthz(_: web.Request) -> web.Response:
        # 503 while the models load: nothing can be transcribed yet.
        if not ready.is_set():
            return web.json_response({"status": "loading"}, status=503)
        if not healthy():
            return web.json_response({"status": "no engine up for 60 s"}, status=503)
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
    client = httpx.AsyncClient(
        base_url=s.room_url,
        timeout=httpx.Timeout(10.0, connect=5.0),
        headers={"Authorization": f"Bearer {s.room_token.get_secret_value()}"},
    )
    room_api = RoomAPI(client)
    pipeline = Pipeline(
        engines(s), room_api, s.queue_size, s.plausibility(), use_prompt=s.uses_glossary, engine=s.engine
    )
    ready = asyncio.Event()
    runner = await serve_http(s.port, ready, pipeline.healthy)
    lk = api.LiveKitAPI(http_url(s.livekit_url), s.livekit_api_key, s.livekit_api_secret.get_secret_value())
    worker = Worker(s, pipeline, room_api, lk)
    try:
        model, revision = engine(s)
        log.info(
            "model_loading",
            engine=s.engine,
            model=model,
            revision=revision,
            processes=s.engine_processes,
            cpu_threads=s.cpu_threads,
        )
        await pipeline.start()
        engine_info(s)
        ready.set()
        log.info("model_ready", engine=s.engine)
        async with asyncio.TaskGroup() as tg:
            tg.create_task(pipeline.run())
            tg.create_task(worker.discover())
            tg.create_task(worker.heartbeat())
    finally:
        pipeline.stop()
        await worker.close()
        await lk.aclose()
        await client.aclose()
        await runner.cleanup()


def main() -> None:
    try:
        s = Settings()  # type: ignore[call-arg] # pydantic-settings reads the required fields from the env
    except ValidationError as e:
        # One JSON line naming each bad setting and what it accepts, e.g. APP_ENGINE's two values.
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
        worker=s.worker_name,
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
