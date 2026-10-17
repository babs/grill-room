"""The personas' LiveKit participants: one per speaking persona of a room with a listener (013-AC15)."""

import asyncio
import contextlib
from collections.abc import Callable
from dataclasses import dataclass
from datetime import timedelta
from typing import Any

import structlog
from livekit import api, rtc

from . import metrics
from .config import Settings
from .room import FRAME, TRACK_SR, RoomVoice

log = structlog.get_logger()

# The track's queue: what a stop can still let out is at most this, so keep it short (013-AC22).
QUEUE_MS = 100


def is_agent(p: Any) -> bool:
    return getattr(p, "kind", None) == rtc.ParticipantKind.PARTICIPANT_KIND_AGENT or str(
        getattr(p, "identity", "")
    ).startswith("persona-")


class TrackOut:
    """A bot's microphone track, as room.Out."""

    def __init__(self, source: rtc.AudioSource) -> None:
        self._source = source

    async def play(self, frame: bytes) -> None:
        await self._source.capture_frame(rtc.AudioFrame(frame, TRACK_SR, 1, FRAME))

    def clear(self) -> None:
        self._source.clear_queue()


@dataclass
class Bot:
    room: rtc.Room
    out: TrackOut
    leave: asyncio.Task[None] | None = None


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

        def speakers(ps: list[rtc.Participant]) -> None:
            voice.speakers({p.identity for p in ps if not is_agent(p)})

        def subscribe(pub: rtc.RemoteTrackPublication, p: rtc.RemoteParticipant) -> None:
            # LiveKit names a human as speaker to a bot only once it hears their track (S14).
            if not is_agent(p) and pub.kind == rtc.TrackKind.KIND_AUDIO:
                pub.set_subscribed(True)

        room.on("active_speakers_changed", speakers)
        room.on("track_published", subscribe)
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
            with contextlib.suppress(Exception):
                await room.disconnect()
            return
        finally:
            self._joining.pop(key, None)
        self.bots[key] = Bot(room, TrackOut(source))
        metrics.BOTS.set(len(self.bots))
        metrics.BOTS_JOINED.inc()
        log.info("bot_joined", room=room_name, persona_id=persona)
        voice.wake()

    def _gone(self, key: tuple[str, int], room: rtc.Room, reason: object) -> None:
        bot = self.bots.get(key)
        if bot is None or bot.room is not room:
            return
        del self.bots[key]
        if bot.leave is not None and bot.leave is not asyncio.current_task():
            bot.leave.cancel()
        metrics.BOTS.set(len(self.bots))
        log.info("bot_left", room=key[0], persona_id=key[1], reason=str(reason))
        if not any(r == key[0] for r, _ in self.bots):
            # No bot hears the room any more: a speaker it named last must not block its next line.
            self._voice(key[0]).speakers(set())

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
