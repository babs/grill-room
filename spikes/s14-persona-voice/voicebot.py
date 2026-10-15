#!/usr/bin/env python3
"""Spike S14 voice bot: speaks the personas' lines into the LiveKit call, one bot participant each.

Polls the room server's speech queue (GRILL_ROOM_PERSONA_VOICE=on) with the STT service token,
synthesises each line sentence by sentence, and publishes it on the persona's own audio track.
One persona speaks at a time per room. A human speaking pauses it: it says the interrupted
sentence again once the room is quiet, or drops the rest of the line past RESUME_WITHIN_S.
"""

import asyncio
import contextlib
import json
import logging
import os
import re
import sys
import time
import urllib.request
from dataclasses import dataclass, field
from datetime import timedelta

import numpy as np
from livekit import api, rtc

from bench import ENGINES, Synth

log = logging.getLogger("voicebot")

ROOM_URL = os.environ["S14_ROOM_URL"]
TOKEN = os.environ["S14_STT_TOKEN"]
LK_URL = os.environ["S14_LIVEKIT_URL"]
LK_KEY = os.environ["S14_LIVEKIT_KEY"]
LK_SECRET = os.environ["S14_LIVEKIT_SECRET"]
ENGINE = os.environ.get("S14_ENGINE", "piper")
THREADS = int(os.environ.get("S14_THREADS", "4"))

# A slot's voice in each language: slot n is the persona ranked n in its room. Same slot, same
# speaker type across languages, so a persona keeps its character when the room switches.
VOICES = {
    "piper": [
        {"fr": "fr_FR-siwis-medium", "en": "en_GB-cori-high"},
        {"fr": "fr_FR-upmc-medium#1", "en": "en_US-bryce-medium"},
        {"fr": "fr_FR-mls-medium#84", "en": "en_US-norman-medium"},
    ],
    "pocket": [
        {"fr": "estelle", "en": "alba"},
        {"fr": "marius", "en": "marius"},
        {"fr": "charles", "en": "charles"},
    ],
}
QUIET_S = 0.8  # silence after a human before a persona speaks or resumes
RESUME_WITHIN_S = 8.0  # an interruption longer than this drops the rest of the line
MAX_AGE_S = 90.0  # a line not started by then is stale: the room has moved on
IDLE_LEAVE_S = 60.0  # a bot with nothing to say leaves the call
FRAME_MS = 20
# The track's rate: engines differ (Piper 22.05 kHz, Pocket 24 kHz), one source per bot takes one.
TRACK_SR = 48000
TARGET_RMS = 0.08  # about -22 dBFS

FRENCH = re.compile(r"\b(le|la|les|des|une|est|et|pour|dans|sur|que|qui|pas|j'ai)\b", re.I)


def language(text: str) -> str:
    return "fr" if len(FRENCH.findall(text)) > len(text.split()) / 10 else "en"


def sentences(text: str) -> list[str]:
    return [s.strip() for s in re.findall(r"[^.!?]+[.!?]*", text) if s.strip()]


class Voices:
    """Loaded engines by voice id; synthesis runs in a worker thread, one at a time."""

    def __init__(self) -> None:
        self._synth: dict[tuple[str, str], tuple[int, Synth]] = {}
        self._lock = asyncio.Lock()

    async def synth(self, voice: str, lang: str, text: str) -> np.ndarray:
        """int16 mono at TRACK_SR."""
        async with self._lock:
            if (voice, lang) not in self._synth:
                self._synth[(voice, lang)] = await asyncio.to_thread(ENGINES[ENGINE], voice, lang, THREADS)
            sr, fn = self._synth[(voice, lang)]
            pcm = await asyncio.to_thread(lambda: np.concatenate(list(fn(text))))
        # Voices come out at different loudness (marius about a third of estelle): level each sentence.
        rms = float(np.sqrt(np.mean(pcm**2))) or 1.0
        pcm = pcm * min(TARGET_RMS / rms, 0.95 / max(float(np.abs(pcm).max()), 1e-6))
        data = (np.clip(pcm, -1, 1) * 32767).astype(np.int16)
        rs = rtc.AudioResampler(sr, TRACK_SR, quality=rtc.AudioResamplerQuality.HIGH)
        frames = rs.push(rtc.AudioFrame(data.tobytes(), sr, 1, len(data))) + rs.flush()
        return np.concatenate([np.frombuffer(f.data, np.int16) for f in frames])


@dataclass
class Bot:
    room: rtc.Room
    source: rtc.AudioSource
    last: float = field(default_factory=time.monotonic)


class RoomVoice:
    """The personas of one grill room: their queue, their bots, and who is talking."""

    def __init__(self, name: str, voices: Voices) -> None:
        self.name = name
        self.voices = voices
        self.queue: asyncio.Queue[dict] = asyncio.Queue()
        self.bots: dict[int, Bot] = {}
        self.humans: set[str] = set()
        self.human_change = asyncio.Event()
        self.quiet_since = time.monotonic()

    def _on_speakers(self, speakers: list[rtc.Participant]) -> None:
        humans = {p.identity for p in speakers if not p.identity.startswith("persona-")}
        log.debug("speakers %s", [p.identity for p in speakers])
        if humans and not self.humans:
            log.info("human_speaking %s", json.dumps({"room": self.name, "who": sorted(humans)}))
        if not humans and self.humans:
            self.quiet_since = time.monotonic()
        self.humans = humans
        self.human_change.set()

    async def _bot(self, item: dict) -> Bot:
        pid = item["persona_id"]
        bot = self.bots.get(pid)
        if bot is not None and bot.room.isconnected():
            return bot
        tok = (
            api.AccessToken(LK_KEY, LK_SECRET)
            .with_identity(f"persona-{pid}")
            .with_name(item["persona"])
            .with_kind("agent")
            .with_grants(
                api.VideoGrants(
                    room_join=True,
                    room=self.name,
                    can_publish=True,
                    can_subscribe=True,
                    can_publish_data=False,
                    can_publish_sources=["microphone"],
                )
            )
            .with_ttl(timedelta(minutes=5))
            .to_jwt()
        )
        room = rtc.Room()
        room.on("active_speakers_changed", self._on_speakers)
        await room.connect(LK_URL, tok, rtc.RoomOptions(auto_subscribe=True))
        source = rtc.AudioSource(TRACK_SR, 1, queue_size_ms=200)
        track = rtc.LocalAudioTrack.create_audio_track(f"voice-{pid}", source)
        await room.local_participant.publish_track(
            track, rtc.TrackPublishOptions(source=rtc.TrackSource.SOURCE_MICROPHONE)
        )
        bot = Bot(room, source)
        self.bots[pid] = bot
        log.info("bot_joined %s", json.dumps({"room": self.name, "persona": item["persona"]}))
        return bot

    @staticmethod
    def _voice(item: dict, lang: str) -> str:
        slots = VOICES[ENGINE]
        return slots[item["slot"] % len(slots)][lang]

    async def _wait_quiet(self) -> float:
        """Waits until no human has spoken for QUIET_S; returns how long it waited."""
        start = time.monotonic()
        while self.humans or time.monotonic() - self.quiet_since < QUIET_S:
            self.human_change.clear()
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self.human_change.wait(), QUIET_S / 4)
        return time.monotonic() - start

    async def _play(self, bot: Bot, data: np.ndarray) -> bool:
        """Plays one sentence; False when a human cut in."""
        n = TRACK_SR * FRAME_MS // 1000
        for i in range(0, len(data), n):
            if self.humans:
                bot.source.clear_queue()
                return False
            chunk = data[i : i + n]
            if len(chunk) < n:
                chunk = np.pad(chunk, (0, n - len(chunk)))
            await bot.source.capture_frame(rtc.AudioFrame(chunk.tobytes(), TRACK_SR, 1, n))
        await bot.source.wait_for_playout()
        return not self.humans

    async def run(self) -> None:
        while True:
            try:
                item = await asyncio.wait_for(self.queue.get(), IDLE_LEAVE_S)
            except TimeoutError:
                await self.leave()
                continue
            age = time.time() - item["at_unix"]
            if age > MAX_AGE_S:
                log.info(
                    "line_stale %s", json.dumps({"room": self.name, "persona": item["persona"], "age_s": round(age)})
                )
                continue
            try:
                await self.say(item)
            except Exception:
                log.exception("line_failed")

    async def say(self, item: dict) -> None:
        bot = await self._bot(item)
        lang = language(item["text"])
        voice = self._voice(item, lang)
        await self._wait_quiet()
        log.info(
            "speaking %s",
            json.dumps(
                {
                    "room": self.name,
                    "persona": item["persona"],
                    "voice": voice,
                    "lang": lang,
                    "lag_s": round(time.time() - item["at_unix"], 2),
                }
            ),
        )
        # Synthesis of the next sentence overlaps playback of the current one.
        parts = sentences(item["text"])
        nxt = asyncio.create_task(self.voices.synth(voice, lang, parts[0]))
        for i in range(len(parts)):
            pcm = await nxt
            if i + 1 < len(parts):
                nxt = asyncio.create_task(self.voices.synth(voice, lang, parts[i + 1]))
            while not await self._play(bot, pcm):
                log.info("interrupted %s", json.dumps({"room": self.name, "persona": item["persona"], "sentence": i}))
                waited = await self._wait_quiet()
                if waited > RESUME_WITHIN_S:
                    log.info(
                        "line_dropped %s",
                        json.dumps({"room": self.name, "persona": item["persona"], "waited_s": round(waited, 1)}),
                    )
                    nxt.cancel()
                    return
                log.info(
                    "resumed %s",
                    json.dumps(
                        {"room": self.name, "persona": item["persona"], "sentence": i, "waited_s": round(waited, 1)}
                    ),
                )
        bot.last = time.monotonic()
        log.info("spoken %s", json.dumps({"room": self.name, "persona": item["persona"]}))

    async def leave(self) -> None:
        for pid, bot in list(self.bots.items()):
            await bot.room.disconnect()
            del self.bots[pid]
            log.info("bot_left %s", json.dumps({"room": self.name, "persona_id": pid}))


def fetch(after: int) -> tuple[list[dict], int]:
    req = urllib.request.Request(
        f"{ROOM_URL}/api/stt/speech?after={after}", headers={"Authorization": f"Bearer {TOKEN}"}
    )
    with urllib.request.urlopen(req, timeout=5) as r:
        body = json.load(r)
        return body["items"] or [], body["last"]


async def main() -> None:
    logging.basicConfig(
        level=os.environ.get("S14_LOG_LEVEL", "INFO"), stream=sys.stdout, format="%(asctime)s %(name)s %(message)s"
    )
    voices = Voices()
    rooms: dict[str, RoomVoice] = {}
    tasks: set[asyncio.Task[None]] = set()
    # Lines queued before the bot started are old news: start after the latest.
    _, after = await asyncio.to_thread(fetch, 0)
    log.info("started %s", json.dumps({"engine": ENGINE, "after": after}))
    while True:
        try:
            items, last = await asyncio.to_thread(fetch, after)
        except Exception as e:  # the server restarting must not stop the bot
            log.warning("poll_failed %s", e)
            items, last = [], after
        if last < after:
            # The server restarted: its numbering began again, and every line it has is new.
            log.info("server_restarted %s", json.dumps({"after": after, "last": last}))
            after = 0
            continue
        for it in items:
            after = it["seq"]
            it["at_unix"] = time.time()  # the server's clock may differ; age from receipt
            rv = rooms.get(it["room"])
            if rv is None:
                rv = rooms[it["room"]] = RoomVoice(it["room"], voices)
                t = asyncio.create_task(rv.run())
                tasks.add(t)
            log.info(
                "line_received %s",
                json.dumps(
                    {"room": it["room"], "persona": it["persona"], "source": it["source"], "text": it["text"]},
                    ensure_ascii=False,
                ),
            )
            rv.queue.put_nowait(it)
        await asyncio.sleep(0.5)


if __name__ == "__main__":
    asyncio.run(main())
