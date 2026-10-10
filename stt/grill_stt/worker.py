"""Joins every active LiveKit room as a hidden participant and feeds each audio track to the pipeline."""

import asyncio
import contextlib
import time
from datetime import timedelta

import numpy as np
import structlog
from livekit import api, rtc
from prometheus_client import Gauge

from .config import Settings
from .pipeline import Pipeline, RoomAPI, Utterance
from .vad import SR, Segmenter, Silero

log = structlog.get_logger()

ROOMS = Gauge("grill_stt_rooms", "LiveKit rooms the worker is in.")
TRACKS = Gauge("grill_stt_tracks", "Audio tracks the worker listens to.")
# Frames buffered per track between LiveKit and the VAD: 10 ms each, so 10 s.
FRAME_CAPACITY = 1000


def http_url(ws_url: str) -> str:
    return ws_url.replace("ws", "http", 1)


class Worker:
    def __init__(self, s: Settings, pipeline: Pipeline, room_api: RoomAPI, lk: api.LiveKitAPI) -> None:
        self._s = s
        self._pipeline = pipeline
        self._api = room_api
        self._lk = lk
        self._identity = f"stt-{s.worker_name}"
        self.rooms: dict[str, rtc.Room] = {}
        self._tracks: dict[str, set[asyncio.Task[None]]] = {}
        self._changed = asyncio.Event()
        # The event loop keeps only weak references to tasks: hold the fire-and-forget ones.
        self._background: set[asyncio.Task[None]] = set()
        ROOMS.set_function(lambda: len(self.rooms))

    def _token(self, room: str) -> str:
        grants = api.VideoGrants(
            room_join=True,
            room=room,
            hidden=True,
            can_publish=False,
            can_publish_data=False,
            can_subscribe=True,
        )
        return (
            api.AccessToken(self._s.livekit_api_key, self._s.livekit_api_secret.get_secret_value())
            .with_identity(self._identity)
            .with_name("Transcription")
            .with_grants(grants)
            .with_ttl(timedelta(minutes=5))
            .to_jwt()
        )

    async def discover(self) -> None:
        """Joins each room with someone in it; LiveKit rooms only exist for grill rooms."""
        while True:
            try:
                resp = await self._lk.room.list_rooms(api.ListRoomsRequest())
                for r in resp.rooms:
                    if r.name not in self.rooms and r.num_participants > 0:
                        await self._join(r.name)
            except Exception as e:  # LiveKit down: keep polling, it comes back
                log.warning("discover_failed", error=str(e))
            await asyncio.sleep(self._s.poll_seconds)

    async def _join(self, name: str) -> None:
        room = rtc.Room()
        self.rooms[name] = room
        self._tracks[name] = set()
        room.on("track_subscribed", lambda track, _pub, p: self._on_track(name, track, p))
        room.on("participant_disconnected", lambda _p: self._leave_if_alone(name))
        room.on("disconnected", lambda reason: self._forget(name, room, reason))
        try:
            await room.connect(self._s.livekit_url, self._token(name), rtc.RoomOptions(auto_subscribe=True))
        except Exception as e:  # one room failing must not stop the others
            log.warning("room_join_failed", room=name, error=str(e))
            self._forget(name, room, "join_failed")
            return
        log.info("room_joined", room=name, participants=len(room.remote_participants))
        self._changed.set()
        self._leave_if_alone(name)

    def _leave_if_alone(self, name: str) -> None:
        room = self.rooms.get(name)
        if room is not None and not room.remote_participants:
            task = asyncio.create_task(room.disconnect())  # "disconnected" cleans up
            self._background.add(task)
            task.add_done_callback(self._background.discard)

    def _forget(self, name: str, room: rtc.Room, reason: object) -> None:
        if self.rooms.get(name) is not room:
            return
        del self.rooms[name]
        for task in self._tracks.pop(name, set()):
            task.cancel()
        self._changed.set()
        log.info("room_left", room=name, reason=str(reason))

    def _on_track(self, room: str, track: rtc.Track, p: rtc.RemoteParticipant) -> None:
        if track.kind != rtc.TrackKind.KIND_AUDIO:
            return
        try:
            participant_id = int(p.identity)
        except ValueError:
            log.warning("track_ignored", room=room, identity=p.identity)
            return
        task = asyncio.create_task(self._listen(room, participant_id, track))
        tasks = self._tracks.setdefault(room, set())
        tasks.add(task)
        task.add_done_callback(tasks.discard)

    async def _listen(self, room: str, participant_id: int, track: rtc.Track) -> None:
        """VAD over one participant's track; every utterance it cuts goes to the queue, in memory."""
        seg = Segmenter(Silero())
        pause = seg.min_silence_ms / 1000
        stream = rtc.AudioStream(track, sample_rate=SR, num_channels=1, capacity=FRAME_CAPACITY)
        TRACKS.inc()
        log.info("track_listening", room=room, participant=participant_id)
        try:
            async for ev in stream:
                samples = np.frombuffer(ev.frame.data, np.int16).astype(np.float32)
                samples /= 32768
                for audio in seg.feed(samples):
                    self._pipeline.submit(Utterance(room, participant_id, audio, time.monotonic() - pause))
            for audio in seg.flush():
                self._pipeline.submit(Utterance(room, participant_id, audio, time.monotonic()))
        except Exception:
            # One broken track must not go unnoticed, nor take the room down with it.
            log.exception("track_failed", room=room, participant=participant_id)
        finally:
            TRACKS.dec()
            await stream.aclose()

    async def heartbeat(self) -> None:
        """Tells the room server the worker is up and which rooms it hears, at once on every change."""
        while True:
            self._changed.clear()
            await self._api.heartbeat(self._s.worker_name, sorted(self.rooms))
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self._changed.wait(), self._s.heartbeat_seconds)

    async def close(self) -> None:
        for room in list(self.rooms.values()):
            await room.disconnect()
