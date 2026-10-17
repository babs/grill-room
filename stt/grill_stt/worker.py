"""Joins every active LiveKit room as a hidden participant and feeds each audio track to the pipeline."""

import asyncio
import contextlib
import time
from collections.abc import Coroutine, Sequence
from datetime import timedelta
from typing import Any

import numpy as np
import structlog
from livekit import api, rtc
from livekit.protocol import models
from prometheus_client import Gauge

from .config import Settings
from .pipeline import HEARTBEAT_TIMEOUT_S, Flags, Pipeline, RoomAPI, Utterance
from .vad import SR, Audio, Segmenter, Silero

log = structlog.get_logger()

ROOMS = Gauge("grill_stt_rooms", "LiveKit rooms the worker is in.")
TRACKS = Gauge("grill_stt_tracks", "Audio tracks the worker listens to.")
DEVICE_TRACKS = Gauge(
    "grill_stt_device_tracks", "Subscribed tracks skipped because a device transcribes them."
)
# Frames buffered per track between LiveKit and the VAD: 10 ms each, so 10 s.
FRAME_CAPACITY = 1000


def is_agent(p: object) -> bool:
    """A persona's voice bot joins as an agent participant (spec 013)."""
    return getattr(p, "kind", None) == rtc.ParticipantKind.PARTICIPANT_KIND_AGENT


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
        # The LiveKit room sid each room was listed with when joined.
        self._sids: dict[str, str] = {}
        # Rooms left because only persona bots remained: joined again once LiveKit lists a human
        # there, not on every poll (013-AC19).
        self._agents_only: set[str] = set()
        self._tracks: dict[str, set[asyncio.Task[None]]] = {}
        # Open audio streams by track sid, with their participant's identity and room. LiveKit ends
        # none of them when a track goes: the SFU hands the freed transceiver to the next track it
        # sends, and a stream left open hears that newcomer under the old speaker's name (spike S5).
        self._streams: dict[str, tuple[str, rtc.AudioStream, str]] = {}
        # Read on every frame: a flagged participant's audio is dropped before the VAD (010-AC14).
        self._device: Flags = frozenset()
        self._changed = asyncio.Event()
        self._background: set[asyncio.Task[None]] = set()
        # The pipeline's clock: an utterance's end and its line's latency are read on it.
        self._clock = time.monotonic
        ROOMS.set_function(lambda: len(self.rooms))
        DEVICE_TRACKS.set_function(
            lambda: sum((room, int(identity)) in self._device for identity, _, room in self._streams.values())
        )

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
                await self.reconcile(resp.rooms)
            except Exception as e:  # LiveKit down: keep polling, it comes back
                log.warning("discover_failed", error=str(e))
            await asyncio.sleep(self._s.poll_seconds)

    async def reconcile(self, listed: Sequence[Any]) -> None:
        """Drops the rooms LiveKit closed, then joins the ones with someone in them (005-AC18).

        LiveKit can close an empty room without the SDK telling the worker, and a later join opens a
        new room under the same name: a room gone from the list, under another sid, or no longer
        connected is stale.
        """
        by_name = {r.name: r for r in listed}
        for name, room in list(self.rooms.items()):
            r = by_name.get(name)
            sid = self._sids.get(name)
            if r is None:
                reason = "closed"
            elif sid and r.sid != sid:
                reason = "replaced"
            elif not room.isconnected():
                reason = "connection_lost"
            else:
                continue
            self._forget(name, room, reason)
            self._spawn(room.disconnect())
        self._agents_only &= set(by_name)
        for r in listed:
            if r.name in self.rooms or r.num_participants == 0:
                continue
            if r.name in self._agents_only and not await self._has_human(r.name):
                continue
            await self._join(r.name, r.sid)

    async def _has_human(self, room: str) -> bool:
        """Whether LiveKit lists anyone but agents in room: a count cannot tell a human from a bot."""
        resp = await self._lk.room.list_participants(api.ListParticipantsRequest(room=room))
        return any(p.kind != models.ParticipantInfo.Kind.AGENT for p in resp.participants)

    async def _join(self, name: str, sid: str = "") -> None:
        room = rtc.Room()
        self.rooms[name] = room
        self._sids[name] = sid
        self._agents_only.discard(name)
        self._tracks[name] = set()
        room.on("participant_connected", lambda p: self._skip_agent(name, p))
        room.on("track_published", lambda pub, p: self._subscribe(name, pub, p))
        room.on("track_subscribed", lambda track, pub, p: self._on_track(name, track, pub, p))
        room.on("track_unsubscribed", lambda _track, pub, _p: self._close(pub.sid))
        room.on("track_unpublished", lambda pub, _p: self._close(pub.sid))
        room.on("participant_disconnected", lambda p: self._on_participant_left(name, p))
        room.on("disconnected", lambda reason: self._forget(name, room, reason))
        try:
            # Subscriptions are chosen per participant: a persona's bot is never subscribed (013-AC19).
            await room.connect(self._s.livekit_url, self._token(name), rtc.RoomOptions(auto_subscribe=False))
        except Exception as e:  # one room failing must not stop the others
            log.warning("room_join_failed", room=name, error=str(e))
            self._forget(name, room, "join_failed")
            return
        for p in room.remote_participants.values():
            self._skip_agent(name, p)
            for pub in p.track_publications.values():
                self._subscribe(name, pub, p)
        log.info("room_joined", room=name, participants=len(room.remote_participants))
        self._changed.set()
        self._leave_if_alone(name)

    def _leave_if_alone(self, name: str) -> None:
        """Leaves a room with nobody but persona bots in it (013-AC19)."""
        room = self.rooms.get(name)
        if room is None or any(not is_agent(p) for p in room.remote_participants.values()):
            return
        if room.remote_participants:
            self._agents_only.add(name)
        self._spawn(room.disconnect())  # "disconnected" cleans up

    def _skip_agent(self, room: str, p: rtc.RemoteParticipant) -> None:
        if is_agent(p):
            log.debug("participant_skipped", room=room, identity=p.identity, reason="agent")

    def _subscribe(self, room: str, pub: rtc.RemoteTrackPublication, p: rtc.RemoteParticipant) -> None:
        """Subscribes a human's audio track; a persona bot's never (013-AC19)."""
        if not is_agent(p) and pub.kind == rtc.TrackKind.KIND_AUDIO:
            pub.set_subscribed(True)

    def _forget(self, name: str, room: rtc.Room, reason: object) -> None:
        if self.rooms.get(name) is not room:
            return
        del self.rooms[name]
        self._sids.pop(name, None)
        for task in self._tracks.pop(name, set()):
            task.cancel()
        self._changed.set()
        log.info("room_left", room=name, reason=str(reason))

    def _on_participant_left(self, room: str, p: rtc.RemoteParticipant) -> None:
        # The Python SDK skips track_unsubscribed when the participant is already gone: close here too.
        for sid in [sid for sid, (identity, _, _) in self._streams.items() if identity == p.identity]:
            self._close(sid)
        self._leave_if_alone(room)

    def _close(self, sid: str) -> None:
        """Ends the stream of a track that went away; its listener flushes the speech in progress."""
        entry = self._streams.pop(sid, None)
        if entry is not None:
            self._changed.set()
            self._spawn(entry[1].aclose())

    def _spawn(self, coro: Coroutine[Any, Any, None]) -> None:
        # The event loop keeps only weak references to tasks: hold the fire-and-forget ones.
        task = asyncio.create_task(coro)
        self._background.add(task)
        task.add_done_callback(self._background.discard)

    def _on_track(
        self, room: str, track: rtc.Track, pub: rtc.RemoteTrackPublication, p: rtc.RemoteParticipant
    ) -> None:
        if track.kind != rtc.TrackKind.KIND_AUDIO:
            return
        try:
            participant_id = int(p.identity)
        except ValueError:
            log.warning("track_ignored", room=room, identity=p.identity)
            return
        self._close(pub.sid)  # a resubscription replaces the stream, never adds a second one
        stream = rtc.AudioStream(track, sample_rate=SR, num_channels=1, capacity=FRAME_CAPACITY)
        self._streams[pub.sid] = (p.identity, stream, room)
        self._changed.set()
        task = asyncio.create_task(self._listen(room, participant_id, pub.sid, stream))
        tasks = self._tracks.setdefault(room, set())
        tasks.add(task)
        task.add_done_callback(tasks.discard)

    async def _listen(self, room: str, participant_id: int, sid: str, stream: rtc.AudioStream) -> None:
        """VAD over one participant's track; every utterance it cuts goes to the queue, in memory."""
        TRACKS.inc()
        log.info("track_listening", room=room, participant=participant_id, track=sid)
        try:
            vad = Silero()

            def segmenter() -> Segmenter:
                return Segmenter(
                    vad, min_silence_ms=self._s.vad_min_silence_ms, max_speech_s=self._s.vad_max_speech_s
                )

            def submit(cuts: list[tuple[Audio, float]]) -> None:
                # The speech ended the silence the cut waited for ago; a forced cut waited none.
                for audio, silent_s in cuts:
                    self._pipeline.submit(Utterance(room, participant_id, audio, self._clock() - silent_s))

            # Speech in progress when the flag comes is dropped: the device's capture predates its flag.
            seg = segmenter()
            skipped = False
            async for ev in stream:
                if (room, participant_id) in self._device:
                    skipped = True
                    continue
                if skipped:
                    vad.reset()
                    seg, skipped = segmenter(), False
                    # 010-AC15 measures the handover up to this frame reaching the VAD.
                    log.info("track_resumed", room=room, participant=participant_id, track=sid)
                samples = np.frombuffer(ev.frame.data, np.int16).astype(np.float32)
                samples /= 32768
                submit(seg.feed_timed(samples))
            if not skipped and (room, participant_id) not in self._device:
                submit(seg.flush_timed())
        except Exception:
            # One broken track must not go unnoticed, nor take the room down with it.
            log.exception("track_failed", room=room, participant=participant_id)
        finally:
            TRACKS.dec()
            entry = self._streams.get(sid)
            if entry is not None and entry[1] is stream:
                del self._streams[sid]
                self._changed.set()
            await stream.aclose()
            log.info("track_closed", room=room, participant=participant_id, track=sid)

    def voices(self) -> dict[str, list[int]]:
        """The participants whose track the worker transcribes, by room (005-AC8, 010-AC14)."""
        out: dict[str, set[int]] = {}
        for identity, _, room in self._streams.values():
            if (room, int(identity)) not in self._device:
                out.setdefault(room, set()).add(int(identity))
        return {room: sorted(ids) for room, ids in out.items()}

    async def heartbeat(self) -> None:
        """Tells the room server the worker is up, which rooms and voices it hears, at once on a change."""
        while True:
            self._changed.clear()
            started = self._clock()
            device = await self._api.heartbeat(
                self._s.worker_name,
                sorted(self.rooms),
                self.voices(),
                self._s.engine,
                self._s.uses_glossary,
                timeout=min(self._s.heartbeat_seconds, HEARTBEAT_TIMEOUT_S),
            )
            self.flag(device)
            # Start to start: a slow answer must not stretch the period a cleared flag waits (010-AC15).
            wait = max(0.0, self._s.heartbeat_seconds - (self._clock() - started))
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self._changed.wait(), wait)

    def flag(self, device: Flags) -> None:
        """Skips the participants a device transcribes; a change re-beats at once, voices changed."""
        if device == self._device:
            return
        for room, participant in sorted(device - self._device):
            log.info("track_device", room=room, participant=participant)
        for room, participant in sorted(self._device - device):
            log.info("track_unflagged", room=room, participant=participant)
        self._device = device
        self._changed.set()

    async def close(self) -> None:
        for room in list(self.rooms.values()):
            await room.disconnect()
