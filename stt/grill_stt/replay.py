"""Replays WAV tracks in real time through the worker's own segmenter, engines and filter (spec 007, AC16).

usage: python -m grill_stt.replay <manifest.json> <out.jsonl> --set S [--talkers N]

The manifest lists tracks, `{"tracks": [{"id", "set", "item", "lang", "wav"}]}`, WAV paths relative
to it, 16 kHz mono PCM16. Tracks of the set play N at a time, each as one speaker talking live, the
longest first; groups follow each other. Every stored line becomes one JSON row: track, item,
text, its end in the track (`arrival`), its length and its latency from the end of speech. A last
row sums up drops, filter outcomes and the pod's peak memory. Settings come from the APP_*
environment as in the worker: engine, engine processes and threads, queue size, filter.
"""

import argparse
import asyncio
import contextvars
import json
import time
import wave
from pathlib import Path
from typing import Any

import numpy as np
from prometheus_client import REGISTRY

from .__main__ import engines
from .config import Settings
from .engines import Transcribe
from .logs import setup_logging
from .pipeline import Pipeline, RoomAPI, Utterance
from .vad import SR, Segmenter, Silero

FRAME = SR // 100  # LiveKit hands the worker 10 ms frames
# What each utterance carried in, by object: the pipeline only hands the sink the room and speaker.
CURRENT: contextvars.ContextVar[dict[str, Any]] = contextvars.ContextVar("current")


class Sink(RoomAPI):
    """The room server's place: records each line with the utterance it came from."""

    def __init__(self, out: list[dict[str, Any]]) -> None:
        self._out = out
        self.dropped: list[str] = []

    async def post_line(self, room: str, participant_id: int, text: str) -> bool:
        meta = CURRENT.get()
        self._out.append(meta | {"text": text, "latency": time.monotonic() - meta["ended"]})
        return True

    async def post_dropped(self, room: str, participant_id: int) -> bool:
        self.dropped.append(room)
        return True

    async def prompt(self, room: str) -> str | None:
        return ""


class ReplayPipeline(Pipeline):
    def __init__(self, engines: list[Transcribe], sink: Sink, s: Settings) -> None:
        super().__init__(engines, sink, s.queue_size, s.plausibility(), use_prompt=False, engine=s.engine)
        self.meta: dict[int, dict[str, Any]] = {}

    async def process(self, u: Utterance, engine: Transcribe | None = None) -> None:
        CURRENT.set(self.meta.pop(id(u)))
        await super().process(u, engine)


def frames(path: Path) -> int:
    with wave.open(str(path)) as w:
        return int(w.getnframes())


def read(path: Path) -> np.ndarray:
    with wave.open(str(path)) as w:
        if (w.getframerate(), w.getnchannels(), w.getsampwidth()) != (SR, 1, 2):
            raise SystemExit(f"{path}: want 16 kHz mono PCM16")
        pcm = np.frombuffer(w.readframes(w.getnframes()), "<i2")
    return pcm.astype(np.float32) / np.float32(32768)


async def play(p: ReplayPipeline, track: dict[str, Any], audio: np.ndarray, pid: int, s: Settings) -> None:
    """One speaker live: 10 ms frames on the wall clock, cut by the worker's Segmenter."""
    seg = Segmenter(Silero(), min_silence_ms=s.vad_min_silence_ms, max_speech_s=s.vad_max_speech_s)
    pause = s.vad_min_silence_ms / 1000
    loop = asyncio.get_running_loop()
    start = loop.time()

    def submit(u: np.ndarray, at: float, ended: float) -> None:
        utt = Utterance(track["id"], pid, u, ended)
        p.meta[id(utt)] = {k: track[k] for k in ("id", "set", "item", "lang")} | {
            "arrival": round(at, 3),
            "dur": round(len(u) / SR, 3),
            "ended": ended,
        }
        if not p.submit(utt):
            p.meta.pop(id(utt))

    for i in range(0, len(audio), FRAME):
        for u in seg.feed(audio[i : i + FRAME]):
            submit(u, (i + FRAME) / SR, time.monotonic() - pause)
        # Absolute schedule: a late frame does not push the rest of the track back.
        delay = start + (i + FRAME) / SR - loop.time()
        if delay > 0:
            await asyncio.sleep(delay)
    for u in seg.flush():
        submit(u, len(audio) / SR, time.monotonic())


def working_set() -> int | None:
    """The cgroup's memory minus its inactive file cache: the figure kubectl top and evictions use.
    memory.peak also counts the page cache of the corpus and the mmapped model, up to the limit."""
    try:
        current = int(Path("/sys/fs/cgroup/memory.current").read_text())
        stat = dict(line.split() for line in Path("/sys/fs/cgroup/memory.stat").read_text().splitlines())
    except OSError, ValueError:
        return None
    return current - int(stat.get("inactive_file", 0))


async def sample_working_set(peak: list[int]) -> None:
    while True:
        ws = working_set()
        if ws is not None and ws > peak[0]:
            peak[0] = ws
        await asyncio.sleep(1)


def peak_memory() -> int | None:
    """The cgroup's peak memory, the figure a memory limit is checked against."""
    for f in ("/sys/fs/cgroup/memory.peak", "/sys/fs/cgroup/memory/memory.max_usage_in_bytes"):
        try:
            return int(Path(f).read_text())
        except OSError, ValueError:
            continue
    return None


def counters() -> dict[str, float]:
    out = {}
    for m in REGISTRY.collect():
        for smp in m.samples:
            if smp.name in (
                "grill_stt_lines_total",
                "grill_stt_lines_implausible_total",
                "grill_stt_utterances_dropped_total",
            ):
                key = smp.name + "".join(f".{v}" for v in smp.labels.values())
                out[key] = smp.value
    return out


async def replay(manifest: Path, out: Path, cset: str, talkers: int) -> None:
    s = Settings(room_url="-", room_token="-", livekit_url="-", livekit_api_key="-", livekit_api_secret="-")  # type: ignore[arg-type]
    tracks = [t for t in json.loads(manifest.read_text())["tracks"] if t["set"] == cset]
    # Longest first, read per group: the whole corpus in memory would inflate the measured peak.
    tracks.sort(key=lambda t: -frames(manifest.parent / t["wav"]))
    rows: list[dict[str, Any]] = []
    sink = Sink(rows)
    p = ReplayPipeline(list(engines(s)), sink, s)
    t0 = time.monotonic()
    await p.start()
    serving = asyncio.create_task(p.run())
    ws_peak = [0]
    sampling = asyncio.create_task(sample_working_set(ws_peak))
    try:
        for g in range(0, len(tracks), talkers):
            group = tracks[g : g + talkers]
            audio = [read(manifest.parent / t["wav"]) for t in group]
            await asyncio.gather(
                *(play(p, t, a, k, s) for k, (t, a) in enumerate(zip(group, audio, strict=True)))
            )
            del audio
            await p.idle()
            print(
                f"group {g // talkers + 1}: {[t['id'] for t in group]} lines so far {len(rows)}", flush=True
            )
    finally:
        serving.cancel()
        sampling.cancel()
        p.stop()
    with out.open("w") as f:
        for r in rows:
            r.pop("ended")
            r["latency"] = round(r["latency"], 3)
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
        summary = {
            "summary": True,
            "set": cset,
            "talkers": talkers,
            "engine": s.engine,
            "processes": s.engine_processes,
            "threads": s.cpu_threads,
            "tracks": len(tracks),
            "lines": len(rows),
            "drop_notices": len(sink.dropped),
            "counters": counters(),
            "peak_memory": peak_memory(),
            "peak_working_set": ws_peak[0] or None,
            "wall_s": round(time.monotonic() - t0, 1),
        }
        f.write(json.dumps(summary) + "\n")
    print(json.dumps(summary), flush=True)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("manifest", type=Path)
    ap.add_argument("out", type=Path)
    ap.add_argument("--set", required=True)
    ap.add_argument("--talkers", type=int, default=1)
    a = ap.parse_args()
    setup_logging("warning")
    asyncio.run(replay(a.manifest, a.out, a.set, a.talkers))


if __name__ == "__main__":
    main()
