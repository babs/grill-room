"""The personas' LiveKit participants: one per speaking persona of a room with a listener (013-AC15)."""

import asyncio
import contextlib
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

import numpy as np
import structlog
from livekit import api, rtc

from . import metrics
from .audio import Onset
from .config import Settings
from .room import FRAME, TRACK_SR, RoomVoice

log = structlog.get_logger()

# The track's queue: what a stop can still let out is at most this, so keep it short (013-AC22).
QUEUE_MS = 100
# A persona's echo comes back on a listener's track within this after its frame went out (S16).
ECHO_S = 1.5
# Onsets are judged at this rate: enough for speech energy, cheap to decode.
ONSET_SR = 16000


def is_agent(p: Any) -> bool:
    return getattr(p, "kind", None) == rtc.ParticipantKind.PARTICIPANT_KIND_AGENT or str(
        getattr(p, "identity", "")
    ).startswith("persona-")


class TrackOut:
    """A bot's microphone track, as room.Out: a pump sends a frame every 20 ms, the line's or
    silence, so the stream never pauses. A stream that restarts at each line makes the listener's
    jitter buffer grow again, which delays the persona's first words and its stop (013-AC22)."""

    SILENCE = bytes(FRAME * 2)

    def __init__(self, source: rtc.AudioSource) -> None:
        self._source = source
        # One frame waits here: play() returns once the pump took it, so the line keeps its pace.
        self._next: asyncio.Queue[bytes] = asyncio.Queue(maxsize=1)
        self._pump = asyncio.create_task(self._run())

    async def _run(self) -> None:
        while True:
            try:
                frame = self._next.get_nowait()
            except asyncio.QueueEmpty:
                frame = self.SILENCE
            await self._source.capture_frame(rtc.AudioFrame(frame, TRACK_SR, 1, FRAME))

    async def play(self, frame: bytes) -> None:
        """Hands one frame to the pump; raises once the bot left, so the line drops as an error
        instead of waiting on a pump that is gone."""
        if self._pump.done():
            raise ConnectionError("the persona's bot left the call")
        put = asyncio.ensure_future(self._next.put(frame))
        await asyncio.wait({put, self._pump}, return_when=asyncio.FIRST_COMPLETED)
        if not put.done():
            put.cancel()
            raise ConnectionError("the persona's bot left the call")

    def clear(self) -> None:
        while not self._next.empty():
            self._next.get_nowait()
        self._source.clear_queue()

    def stop(self) -> None:
        self._pump.cancel()


@dataclass
class Bot:
    room: rtc.Room
    out: TrackOut
    leave: asyncio.Task[None] | None = None
    # Each human's onset task, by identity.
    listens: dict[str, asyncio.Task[None]] = field(default_factory=dict)


class Bots:
    """Joins and leaves the bots as the room server's roster says, and feeds each room's speakers."""

    def __init__(self, s: Settings, voice: Callable[[str], RoomVoice]) -> None:
        self._s = s
        self._voice = voice
        self.bots: dict[tuple[str, int], Bot] = {}
        self._joining: dict[tuple[str, int], asyncio.Task[None]] = {}
        self._tasks: set[asyncio.Task[None]] = set()
        self.rooms: set[str] = set()
        # The last roster: the server sends one only on a change, so a failed join or a bot LiveKit
        # dropped is retried from it on each heartbeat.
        self.roster: list[dict[str, Any]] = []
        metrics.HUMAN_TRACKS.set_function(self.human_tracks)

    def human_tracks(self) -> int:
        """The humans' audio tracks the bots are subscribed to, over every bot."""
        return sum(
            1
            for bot in self.bots.values()
            for p in bot.room.remote_participants.values()
            if not is_agent(p)
            for pub in p.track_publications.values()
            if pub.kind == rtc.TrackKind.KIND_AUDIO and pub.subscribed
        )

    def out(self, room: str, persona: int) -> TrackOut | None:
        bot = self.bots.get((room, persona))
        return bot.out if bot is not None and bot.room.isconnected() else None

    def sync(self, roster: list[dict[str, Any]]) -> None:
        """Brings the bots in line with the roster: joins the missing, leaves the absent later."""
        self.roster = roster
        wanted = {(r["room"], int(p["id"])): str(p["name"]) for r in roster for p in r["personas"]}
        rooms = {r["room"] for r in roster}
        for room in self.rooms | rooms:
            self._voice(room).set_listeners(room in rooms)
        self.rooms = rooms
        for key, name in wanted.items():
            bot = self.bots.get(key)
            if bot is not None and bot.leave is not None:
                bot.leave.cancel()
                bot.leave = None
            if bot is None and key not in self._joining:
                self._joining[key] = self._spawn(self._join(key, name))
        for key, bot in self.bots.items():
            if key not in wanted and bot.leave is None:
                bot.leave = self._spawn(self._leave_later(key, bot))

    def _spawn(self, coro: Any) -> asyncio.Task[None]:
        task: asyncio.Task[None] = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return task

    def _token(self, room: str, persona: int, name: str) -> str:
        grants = api.VideoGrants(
            room_join=True,
            room=room,
            can_publish=True,
            can_publish_data=False,
            can_subscribe=True,
            can_publish_sources=["microphone"],
        )
        return (
            api.AccessToken(self._s.livekit_api_key, self._s.livekit_api_secret.get_secret_value())
            .with_identity(f"persona-{persona}")
            .with_name(name)
            .with_kind("agent")
            .with_grants(grants)
            .with_ttl(timedelta(minutes=5))
            .to_jwt()
        )

    async def _join(self, key: tuple[str, int], name: str) -> None:
        room_name, persona = key
        voice = self._voice(room_name)
        room = rtc.Room()
        listens: dict[str, asyncio.Task[None]] = {}

        def speakers(ps: list[rtc.Participant]) -> None:
            voice.speakers({p.identity for p in ps if not is_agent(p)}, f"persona-{persona}")

        def subscribe(pub: rtc.RemoteTrackPublication, p: rtc.RemoteParticipant) -> None:
            # LiveKit names a human as speaker to a bot only once it hears their track (S14).
            if not is_agent(p) and pub.kind == rtc.TrackKind.KIND_AUDIO:
                pub.set_subscribed(True)

        def heard(track: rtc.Track, _pub: rtc.RemoteTrackPublication, p: rtc.RemoteParticipant) -> None:
            if not is_agent(p) and track.kind == rtc.TrackKind.KIND_AUDIO:
                vad = f"vad:persona-{persona}:{p.identity}"
                listens[p.identity] = self._spawn(self._listen(voice, vad, p.identity, track))

        def unheard(p: rtc.RemoteParticipant) -> None:
            # The SDK never ends a gone track's stream: its onset would hold the room "speaking".
            if (task := listens.pop(p.identity, None)) is not None:
                task.cancel()

        room.on("active_speakers_changed", speakers)
        room.on("track_published", subscribe)
        room.on("track_subscribed", heard)
        room.on("track_unsubscribed", lambda _track, _pub, p: unheard(p))
        room.on("participant_disconnected", unheard)
        room.on("disconnected", lambda reason: self._gone(key, room, reason))
        try:
            await room.connect(
                self._s.livekit_url,
                self._token(room_name, persona, name),
                rtc.RoomOptions(auto_subscribe=False),
            )
            for p in room.remote_participants.values():
                for pub in p.track_publications.values():
                    subscribe(pub, p)
            source = rtc.AudioSource(TRACK_SR, 1, queue_size_ms=QUEUE_MS)
            track = rtc.LocalAudioTrack.create_audio_track(f"voice-{persona}", source)
            await room.local_participant.publish_track(
                track, rtc.TrackPublishOptions(source=rtc.TrackSource.SOURCE_MICROPHONE)
            )
        except Exception as e:  # one bot failing must not stop the others; the next roster retries
            log.warning("bot_join_failed", room=room_name, persona_id=persona, error=str(e))
            for task in listens.values():
                task.cancel()
            with contextlib.suppress(Exception):
                await room.disconnect()
            return
        finally:
            self._joining.pop(key, None)
        self.bots[key] = Bot(room, TrackOut(source), listens=listens)
        metrics.BOTS.set(len(self.bots))
        metrics.BOTS_JOINED.inc()
        log.info("bot_joined", room=room_name, persona_id=persona)
        voice.wake()

    async def _listen(self, voice: RoomVoice, key: str, identity: str, track: rtc.Track) -> None:
        """Onsets on a human's track, as soon as the audio shows them (013-AC22)."""
        stream = rtc.AudioStream(track, sample_rate=ONSET_SR, num_channels=1)
        onset = Onset()
        try:
            async for ev in stream:
                frame = ev.frame
                changed = onset.feed(
                    np.frombuffer(frame.data, np.int16),
                    1000 * frame.samples_per_channel / ONSET_SR,
                    voice.clock.now() - voice.played_at < ECHO_S,
                )
                if changed is not None:
                    voice.speakers({identity} if changed else set(), key)
        finally:
            voice.forget_bot(key)
            await stream.aclose()

    def _gone(self, key: tuple[str, int], room: rtc.Room, reason: object) -> None:
        bot = self.bots.get(key)
        if bot is None or bot.room is not room:
            return
        del self.bots[key]
        bot.out.stop()
        for task in bot.listens.values():
            task.cancel()
        if bot.leave is not None and bot.leave is not asyncio.current_task():
            bot.leave.cancel()
        metrics.BOTS.set(len(self.bots))
        log.info("bot_left", room=key[0], persona_id=key[1], reason=str(reason))
        # Its view of the speakers goes: a human it named last must not block the room's next line.
        self._voice(key[0]).forget_bot(f"persona-{key[1]}")

    async def _leave_later(self, key: tuple[str, int], bot: Bot) -> None:
        await asyncio.sleep(self._s.leave_after_s)
        # Only the bot this timer was set for: a newer one under the same key stays.
        if self.bots.get(key) is bot:
            await bot.room.disconnect()
            self._gone(key, bot.room, "idle")

    async def close(self) -> None:
        for task in list(self._tasks):
            task.cancel()
        for bot in list(self.bots.values()):
            with contextlib.suppress(Exception):
                await bot.room.disconnect()
