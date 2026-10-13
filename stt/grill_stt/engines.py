"""Engine processes: each loads one model and transcribes one utterance at a time (spec 007, AC6).

A process, not a thread: a native crash or a hang takes down one engine, which is restarted, and
the other keeps transcribing. The child is a plain `python -m grill_stt.engines`, so it imports only
its engine's runtime, never the worker's LiveKit and HTTP stack.
"""

import os
import subprocess
import sys
from collections.abc import Callable
from multiprocessing.connection import Connection

from .line import Line
from .vad import Audio

Transcribe = Callable[[Audio, str], Line]


class EngineDied(Exception):
    """The engine process exited, broke its pipe or overran its deadline: restart it."""


class ProcessEngine:
    """The parent's handle on one engine process. Calls block: run them in a thread, one at a time."""

    def __init__(self, argv: list[str], timeout_s: float) -> None:
        # argv runs the child; the two pipe fds are appended (see serve).
        self._argv = argv
        self._timeout = timeout_s
        self._proc: subprocess.Popen[bytes] | None = None
        self._to: Connection | None = None
        self._from: Connection | None = None

    @property
    def pid(self) -> int | None:
        return self._proc.pid if self._proc else None

    def alive(self) -> bool:
        return self._proc is not None and self._proc.poll() is None

    def start(self, load_timeout_s: float = 600) -> None:
        """Starts the process and waits until its model is loaded."""
        self.stop()
        child_reads, parent_writes = os.pipe()
        parent_reads, child_writes = os.pipe()
        self._to = Connection(parent_writes, readable=False)
        self._from = Connection(parent_reads, writable=False)
        try:
            fds = (child_reads, child_writes)
            self._proc = subprocess.Popen([*self._argv, *map(str, fds)], pass_fds=fds, env=child_env())
        except BaseException:
            self.stop()
            raise
        finally:
            # The child's ends, once: a second close could hit an fd another thread reopened.
            os.close(child_reads)
            os.close(child_writes)
        try:
            self._receive(load_timeout_s)
        except BaseException:
            self.stop()  # a child that died loading leaves neither a zombie nor open pipes
            raise

    def __call__(self, audio: Audio, prompt: str = "") -> Line:
        if self._to is None:
            raise EngineDied("not started")
        try:
            self._to.send((audio, prompt))
        except OSError as e:
            raise EngineDied(f"send: {e}") from e
        line = self._receive(self._timeout)
        if not isinstance(line, Line):
            raise EngineDied(f"unexpected answer {type(line).__name__}")
        return line

    def _receive(self, timeout: float) -> object:
        assert self._from is not None
        try:
            if not self._from.poll(timeout):
                self.stop()
                raise EngineDied(f"no answer within {timeout:g} s")
            kind, value = self._from.recv()
        except (EOFError, OSError) as e:
            raise EngineDied(f"receive: {e!r}") from e
        if kind == "error":
            # The model raised on this utterance; the process is fine.
            raise RuntimeError(value)
        return value

    def stop(self) -> None:
        for conn in (self._to, self._from):
            if conn is not None:
                conn.close()
        self._to = self._from = None
        if self._proc is not None:
            self._proc.kill()
            self._proc.wait()
            self._proc = None


def child_env() -> dict[str, str]:
    """The worker's environment without its secrets, which an engine never needs. OTel off: under
    opentelemetry-instrument the child would start its own SDK and exporter for no span."""
    env = {k: v for k, v in os.environ.items() if k not in ("APP_ROOM_TOKEN", "APP_LIVEKIT_API_SECRET")}
    env["OTEL_SDK_DISABLED"] = "true"
    return env


def serve(transcribe: Transcribe, read_fd: int, write_fd: int) -> None:
    """The child's loop: ready once loaded, then one answer per utterance until the parent goes."""
    inbox = Connection(read_fd, writable=False)
    outbox = Connection(write_fd, readable=False)
    outbox.send(("ready", os.getpid()))
    while True:
        try:
            audio, prompt = inbox.recv()
        except EOFError:
            return
        try:
            outbox.send(("ok", transcribe(audio, prompt)))
        except Exception as e:  # one utterance the model chokes on is an error, not a dead engine
            outbox.send(("error", f"{type(e).__name__}: {e}"))


def argv(engine: str, model: str, cpu_threads: int) -> list[str]:
    return [sys.executable, "-m", "grill_stt.engines", engine, model, str(cpu_threads)]


def main() -> None:
    from .logs import setup_logging

    engine, model, threads, read_fd, write_fd = sys.argv[1:]
    setup_logging(os.getenv("APP_LOG_LEVEL", "info"))
    transcribe: Transcribe
    if engine == "parakeet":
        from .parakeet import ParakeetTranscriber

        transcribe = ParakeetTranscriber(model, int(threads))
    else:
        from .transcribe import Transcriber

        transcribe = Transcriber(model, int(threads))
    serve(transcribe, int(read_fd), int(write_fd))


if __name__ == "__main__":
    main()
