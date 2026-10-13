import asyncio
import json
import time

import httpx
import numpy as np
import pytest
from prometheus_client import REGISTRY

from grill_stt.pipeline import Pipeline, RoomAPI, Utterance
from grill_stt.transcribe import Line
from grill_stt.vad import Audio


def metric(name: str, **labels: str) -> float:
    return REGISTRY.get_sample_value(name, labels) or 0.0


def api(handler: httpx.MockTransport) -> RoomAPI:
    client = httpx.AsyncClient(
        base_url="http://room", transport=handler, headers={"Authorization": "Bearer t"}
    )
    return RoomAPI(client)


def utterance(room: str = "r1", pid: int = 7) -> Utterance:
    return Utterance(room, pid, np.ones(16000, np.float32), time.monotonic())


async def test_posts_a_line_as_json_with_the_service_token() -> None:
    seen: list[httpx.Request] = []

    def handle(r: httpx.Request) -> httpx.Response:
        seen.append(r)
        return httpx.Response(201)

    assert await api(httpx.MockTransport(handle)).post_line("r1", 7, "Bonjour")
    (r,) = seen
    assert r.url.path == "/api/rooms/r1/voice/lines"
    assert r.headers["authorization"] == "Bearer t"
    assert r.headers["content-type"] == "application/json"
    assert json.loads(r.content) == {"participant_id": 7, "body": "Bonjour"}


async def test_retries_only_what_was_never_sent(monkeypatch: pytest.MonkeyPatch) -> None:
    real_sleep = asyncio.sleep
    monkeypatch.setattr(asyncio, "sleep", lambda _s: real_sleep(0))
    calls = 0

    def refuse_then_store(r: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if calls < 3:
            raise httpx.ConnectError("refused", request=r)
        return httpx.Response(201)

    assert await api(httpx.MockTransport(refuse_then_store)).post_line("r1", 7, "x")
    assert calls == 3

    calls = 0

    def always_refuse(r: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        raise httpx.ConnectError("refused", request=r)

    assert not await api(httpx.MockTransport(always_refuse)).post_line("r1", 7, "x")
    assert calls == 3

    # A read timeout may come after the room stored the line: no retry, no duplicate.
    calls = 0

    def slow(r: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        raise httpx.ReadTimeout("slow", request=r)

    assert not await api(httpx.MockTransport(slow)).post_line("r1", 7, "x")
    assert calls == 1


@pytest.mark.parametrize("status", [404, 400, 500, 503])
async def test_refused_lines_are_not_retried(status: int) -> None:
    calls = 0

    def handle(_: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(status)

    assert not await api(httpx.MockTransport(handle)).post_line("r1", 7, "x")
    assert calls == 1


async def test_heartbeat_names_the_worker_and_its_rooms() -> None:
    bodies: list[object] = []

    def handle(r: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(r.content))
        return httpx.Response(204 if r.url.path == "/api/stt/heartbeat" else 404)

    assert await api(httpx.MockTransport(handle)).heartbeat(
        "w1", ["a", "b"], {"a": [7, 8]}, "parakeet", False
    )
    assert bodies == [
        {
            "worker": "w1",
            "rooms": ["a", "b"],
            "voices": {"a": [7, 8]},
            "engine": "parakeet",
            "glossary": False,
        }
    ]

    def down(r: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("down", request=r)

    assert not await api(httpx.MockTransport(down)).heartbeat("w1", [], {}, "whisper", True)
    assert not await api(httpx.MockTransport(lambda _r: httpx.Response(401))).heartbeat(
        "w1", [], {}, "whisper", True
    )


async def test_queue_is_bounded_and_its_depth_exported() -> None:
    """001-AC7: overlapping speech queues up to a bound instead of growing without limit."""
    stored = 0

    def handle(r: httpx.Request) -> httpx.Response:
        nonlocal stored
        if r.method == "GET":
            return httpx.Response(200, json={"prompt": ""})
        stored += 1
        return httpx.Response(201)

    p = Pipeline(lambda _a, _p: Line("hello", "en"), api(httpx.MockTransport(handle)), size=2)
    dropped = metric("grill_stt_utterances_dropped_total")
    assert p.submit(utterance()) and p.submit(utterance())
    assert metric("grill_stt_queue_depth") == 2
    assert not p.submit(utterance())
    assert metric("grill_stt_utterances_dropped_total") == dropped + 1

    task = asyncio.create_task(p.run())
    await asyncio.wait_for(p._queue.join(), 5)
    task.cancel()
    assert stored == 2
    assert metric("grill_stt_queue_depth") == 0


async def test_empty_transcripts_are_not_posted_and_audio_is_released() -> None:
    def handle(r: httpx.Request) -> httpx.Response:
        if r.method == "GET":
            return httpx.Response(200, json={"prompt": ""})
        raise AssertionError("posted an empty line")

    seen: list[Audio] = []

    def transcribe(a: Audio, _p: str) -> Line:
        seen.append(a)
        return Line("", "fr")

    empty = metric("grill_stt_lines_total", result="empty")
    u = utterance()
    await Pipeline(transcribe, api(httpx.MockTransport(handle)), size=1).process(u)
    assert metric("grill_stt_lines_total", result="empty") == empty + 1
    assert len(seen[0]) == 16000
    # 001-AC9: the samples are dropped as soon as the line is done, not kept with the utterance.
    assert len(u.audio) == 0


async def test_a_failing_utterance_does_not_stop_the_queue() -> None:
    stored: list[str] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.method == "GET":
            return httpx.Response(200, json={"prompt": ""})
        stored.append(json.loads(r.content)["body"])
        return httpx.Response(201)

    texts = iter(["first", "boom", "third"])

    def transcribe(_a: Audio, _p: str) -> Line:
        t = next(texts)
        if t == "boom":
            raise RuntimeError("model error")
        return Line(t, "en")

    errors = metric("grill_stt_lines_total", result="error")
    p = Pipeline(transcribe, api(httpx.MockTransport(handle)), size=4)
    for _ in range(3):
        p.submit(utterance())
    task = asyncio.create_task(p.run())
    await asyncio.wait_for(p._queue.join(), 5)
    task.cancel()
    assert stored == ["first", "third"]
    assert metric("grill_stt_lines_total", result="error") == errors + 1


async def test_implausible_lines_are_counted_not_posted() -> None:
    """001-AC8: babble the model made of a TTS glitch (spike S5) never reaches the room."""
    stored: list[str] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.method == "GET":
            return httpx.Response(200, json={"prompt": ""})
        stored.append(json.loads(r.content)["body"])
        return httpx.Response(201)

    babble = Line("三二八四八四八四八二八四八三二八四八八", "zh", 0.3, -0.5, 0.2, 3.1)
    texts = iter([babble, Line("Oui.", "en", 0.28, -0.43, 0.26, 0.33)])
    dropped = metric("grill_stt_lines_total", result="implausible")
    repeated = metric("grill_stt_lines_implausible_total", reason="repetition")
    p = Pipeline(lambda _a, _p: next(texts), api(httpx.MockTransport(handle)), size=4)
    await p.process(utterance())
    await p.process(utterance())
    assert stored == ["Oui."]
    assert metric("grill_stt_lines_total", result="implausible") == dropped + 1
    assert metric("grill_stt_lines_implausible_total", reason="repetition") == repeated + 1


async def test_each_utterance_reads_the_room_prompt() -> None:
    """005-AC23: the prompt is read before each utterance, so a glossary change applies from the next
    one; while the room server does not answer, the last prompt read stays in use."""
    prompts = iter(
        [(200, {"prompt": "On parle de Helm."}), (200, {"prompt": "On parle de Argo CD."}), (503, {})]
    )
    asked: list[str] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.method == "GET":
            asked.append(r.url.path)
            code, body = next(prompts)
            return httpx.Response(code, json=body)
        return httpx.Response(201)

    used: list[str] = []

    def transcribe(_a: Audio, prompt: str) -> Line:
        used.append(prompt)
        return Line("ok", "en")

    p = Pipeline(transcribe, api(httpx.MockTransport(handle)), size=4, use_prompt=True)
    for _ in range(3):
        await p.process(utterance())
    assert used == ["On parle de Helm.", "On parle de Argo CD.", "On parle de Argo CD."]
    assert asked == ["/api/stt/rooms/r1/prompt"] * 3


async def test_no_prompt_by_default() -> None:
    """005-AC23 (spike S10b): by default the worker passes Whisper no prompt and does not even ask
    the room server for one; APP_GLOSSARY_PROMPT=on turns it back on."""
    asked: list[str] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.method == "GET":
            asked.append(r.url.path)
            return httpx.Response(200, json={"prompt": "On parle de Helm."})
        return httpx.Response(201)

    used: list[str] = []

    def transcribe(_a: Audio, prompt: str) -> Line:
        used.append(prompt)
        return Line("ok", "en")

    await Pipeline(transcribe, api(httpx.MockTransport(handle)), size=4).process(utterance())
    assert used == [""]
    assert asked == []


async def test_prompt_read_failures() -> None:
    def down(r: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("down", request=r)

    for transport in (
        httpx.MockTransport(down),
        httpx.MockTransport(lambda _r: httpx.Response(200, content=b"not json")),
        httpx.MockTransport(lambda _r: httpx.Response(200, json={"prompt": 3})),
        httpx.MockTransport(lambda _r: httpx.Response(200, json={})),
        httpx.MockTransport(lambda _r: httpx.Response(404, json={"error": "no such room"})),
    ):
        assert await api(transport).prompt("r1") is None
    assert (
        await api(httpx.MockTransport(lambda _r: httpx.Response(200, json={"prompt": ""}))).prompt("r1") == ""
    )
