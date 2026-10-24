import asyncio
import json
import time

import httpx
import numpy as np
import pytest
import structlog.testing
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


async def test_a_line_carries_the_gap_before_it_only_when_known() -> None:
    """018-AC1: gap_s goes with the line; 018-AC8: without one the body is the pre-018 shape."""
    seen: list[dict[str, object]] = []

    def handle(r: httpx.Request) -> httpx.Response:
        seen.append(json.loads(r.content))
        return httpx.Response(201)

    room_api = api(httpx.MockTransport(handle))
    assert await room_api.post_line("r1", 7, "a", 0.64)
    assert await room_api.post_line("r1", 7, "b", 0.0)
    assert await room_api.post_line("r1", 7, "c", None)
    assert seen == [
        {"participant_id": 7, "body": "a", "gap_s": 0.64},
        {"participant_id": 7, "body": "b", "gap_s": 0.0},
        {"participant_id": 7, "body": "c"},
    ]


async def test_line_done_logs_the_gap_and_the_forced_cut() -> None:
    """018-AC9: the worker's line_done says the pause before the line and whether its cut was forced."""
    sent: list[dict[str, object]] = []

    def handle(r: httpx.Request) -> httpx.Response:
        sent.append(json.loads(r.content))
        return httpx.Response(201)

    p = Pipeline([lambda _a, _p: Line("ok", "en")], api(httpx.MockTransport(handle)), size=1)
    u = Utterance("r1", 7, np.ones(16000, np.float32), time.monotonic(), gap_s=0.0, forced=True)
    with structlog.testing.capture_logs() as logs:
        await p.process(u)
        await p.process(utterance())
    done = [(e["gap_s"], e["forced"]) for e in logs if e["event"] == "line_done"]
    assert done == [(0.0, True), (None, False)]
    assert [m.get("gap_s") for m in sent] == [0.0, None]


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

    assert (
        await api(httpx.MockTransport(handle)).heartbeat("w1", ["a", "b"], {"a": [7, 8]}, "parakeet", False)
        == frozenset()
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

    assert await api(httpx.MockTransport(down)).heartbeat("w1", [], {}, "whisper", True) == frozenset()


@pytest.mark.parametrize(
    ("status", "body", "flags"),
    [
        (200, {"device": {"a": [7, 8], "b": [9]}}, frozenset({("a", 7), ("a", 8), ("b", 9)})),
        (200, {"device": {}}, frozenset()),
        (200, {"device": {"a": []}}, frozenset()),
        # A server from before spec 010 answers 204: skip nobody.
        (204, None, frozenset()),
        # Garbage: skip nobody.
        (200, None, frozenset()),
        (200, {}, frozenset()),
        (200, {"device": [7]}, frozenset()),
        (200, {"device": {"a": 7}}, frozenset()),
        (200, {"device": {"a": ["7"]}}, frozenset()),
        (200, {"device": {"a": [7, True]}}, frozenset()),
        (200, {"device": {"a": [[7]]}}, frozenset()),
        (200, [1], frozenset()),
        # Refused: the room's flags lapse meanwhile, so skip nobody.
        (401, None, frozenset()),
        (500, {"device": {"a": [7]}}, frozenset()),
    ],
)
async def test_heartbeat_answer_names_the_participants_a_device_transcribes(
    status: int, body: object, flags: frozenset[tuple[str, int]]
) -> None:
    """010-AC14: the 200 answer's device map; 204, an older server, flags nobody."""

    def handle(_r: httpx.Request) -> httpx.Response:
        if body is None:
            return httpx.Response(status, content=b"not json" if status == 200 else b"")
        return httpx.Response(status, json=body)

    assert await api(httpx.MockTransport(handle)).heartbeat("w1", ["a"], {}, "parakeet", False) == flags


async def test_an_unreadable_device_map_is_logged_once_per_bad_spell() -> None:
    answers = iter([{"device": 1}, {"device": 1}, {"device": {"a": [7]}}, {"device": "x"}, {"device": 1}])
    room_api = api(httpx.MockTransport(lambda _r: httpx.Response(200, json=next(answers))))
    with structlog.testing.capture_logs() as logs:
        got = [await room_api.heartbeat("w1", [], {}, "parakeet", False) for _ in range(5)]
    assert got == [frozenset()] * 2 + [frozenset({("a", 7)})] + [frozenset()] * 2
    assert [e["event"] for e in logs] == ["heartbeat_device_unreadable"] * 2


async def test_a_beat_whose_answer_trickles_in_gives_up_at_its_timeout() -> None:
    """010-AC15: httpx times each read alone; a body dripping in under that must still end the beat
    in time, flagging nobody, before the room's 3 s flag lapse."""
    body = b'{"device": {"a": [7]}}'

    async def drip(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        await reader.readuntil(b"\r\n\r\n")
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n")
        writer.write(b"Content-Length: %d\r\n\r\n" % len(body))
        try:
            for byte in body:
                writer.write(bytes([byte]))
                await writer.drain()
                await asyncio.sleep(0.1)
        except ConnectionError:
            pass
        finally:
            writer.close()

    server = await asyncio.start_server(drip, "127.0.0.1", 0)
    port = server.sockets[0].getsockname()[1]
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{port}", timeout=5.0) as client:
            started = time.monotonic()
            flags = await RoomAPI(client).heartbeat("w1", ["a"], {}, "parakeet", False, timeout=0.3)
            took = time.monotonic() - started
    finally:
        server.close()
        await asyncio.wait_for(server.wait_closed(), 5)
    # The whole body takes 2.2 s; each read waits only 0.1 s.
    assert flags == frozenset() and took < 1.3


async def test_a_deduped_line_counts_as_posted() -> None:
    """010-AC16: the room dropped our copy of a line the device already stored; not a refusal."""
    room_api = api(httpx.MockTransport(lambda _r: httpx.Response(200, json={"deduped": True})))
    with structlog.testing.capture_logs() as logs:
        assert await room_api.post_line("r1", 7, "x")
    assert [(e["event"], e["log_level"]) for e in logs] == [("line_deduped", "info")]


async def test_queue_is_bounded_and_its_depth_exported() -> None:
    """001-AC7: overlapping speech queues up to a bound instead of growing without limit. 007-AC8:
    a drop is counted and announced in the room, once per speaker per 30 s."""
    lines: list[str] = []
    notices: list[tuple[str, object]] = []

    def handle(r: httpx.Request) -> httpx.Response:
        if r.url.path.endswith("/voice/dropped"):
            notices.append((r.url.path, json.loads(r.content)["participant_id"]))
        else:
            lines.append(json.loads(r.content)["body"])
        return httpx.Response(201)

    now = [100.0]
    p = Pipeline(
        [lambda _a, _p: Line("hello", "en")], api(httpx.MockTransport(handle)), size=2, clock=lambda: now[0]
    )
    dropped = metric("grill_stt_utterances_dropped_total")
    assert p.submit(utterance()) and p.submit(utterance())
    assert metric("grill_stt_queue_depth") == 2
    assert not p.submit(utterance(pid=7))
    assert not p.submit(utterance(pid=7))  # same speaker within 30 s: counted, not announced again
    assert not p.submit(utterance(pid=8))
    now[0] += 31
    assert not p.submit(utterance(pid=7))
    assert metric("grill_stt_utterances_dropped_total") == dropped + 4
    await asyncio.wait_for(asyncio.gather(*p._background), 5)
    assert notices == [("/api/rooms/r1/voice/dropped", 7), ("/api/rooms/r1/voice/dropped", 8)] + [
        ("/api/rooms/r1/voice/dropped", 7)
    ]

    task = asyncio.create_task(p.run())
    await asyncio.wait_for(p.idle(), 5)
    task.cancel()
    assert lines == ["hello", "hello"]
    assert metric("grill_stt_queue_depth") == 0


async def test_drop_notice_route_and_its_failures() -> None:
    """007-AC8: the notice goes to the STT route; a refusal or an outage is logged, not raised."""
    seen: list[httpx.Request] = []

    def handle(r: httpx.Request) -> httpx.Response:
        seen.append(r)
        return httpx.Response(201)

    assert await api(httpx.MockTransport(handle)).post_dropped("r1", 7)
    assert seen[0].url.path == "/api/rooms/r1/voice/dropped" and json.loads(seen[0].content) == {
        "participant_id": 7
    }

    def down(r: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("down", request=r)

    assert not await api(httpx.MockTransport(down)).post_dropped("r1", 7)
    assert not await api(httpx.MockTransport(lambda _r: httpx.Response(404))).post_dropped("r1", 7)


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
    await Pipeline([transcribe], api(httpx.MockTransport(handle)), size=1).process(u)
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
    p = Pipeline([transcribe], api(httpx.MockTransport(handle)), size=4)
    for _ in range(3):
        p.submit(utterance())
    task = asyncio.create_task(p.run())
    await asyncio.wait_for(p.idle(), 5)
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
    p = Pipeline([lambda _a, _p: next(texts)], api(httpx.MockTransport(handle)), size=4)
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

    p = Pipeline([transcribe], api(httpx.MockTransport(handle)), size=4, use_prompt=True)
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

    await Pipeline([transcribe], api(httpx.MockTransport(handle)), size=4).process(utterance())
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
