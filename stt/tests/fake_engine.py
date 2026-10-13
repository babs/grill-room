"""A stand-in engine process for the pool tests: the real protocol, no model.

audio[0] picks the behaviour: 2 dies mid-utterance, 3 hangs; anything else answers "line <audio[1]>".
"""

import os
import sys
import threading

from grill_stt.engines import serve
from grill_stt.line import Line
from grill_stt.vad import Audio

DIE, HANG = 2.0, 3.0


def transcribe(audio: Audio, _prompt: str) -> Line:
    if audio[0] == DIE:
        os._exit(3)
    if audio[0] == HANG:
        threading.Event().wait()
    return Line(f"line {float(audio[1]):g}", "en")


if __name__ == "__main__":
    serve(transcribe, int(sys.argv[1]), int(sys.argv[2]))
