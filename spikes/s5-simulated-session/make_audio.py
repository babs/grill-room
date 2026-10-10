#!/usr/bin/env python3
"""Synthesise the S5 session with Piper: one WAV per speaker and round on a shared timeline.

Writes <out>/<speaker>-r<round>.wav (48 kHz mono PCM16, silence while the speaker is not talking)
and fills each line's start and end in session.json, in seconds from the start of its round's audio.
Voice models are downloaded into <out>/voices on first use.
Usage: uv run ./make_audio.py <out dir>
"""

import json
import sys
import urllib.request
import wave
from pathlib import Path

import numpy as np
from piper import PiperVoice, SynthesisConfig

SR = 48000
PEAK = 0.5
# Under the STT worker's 500 ms VAD cut, so a multi-sentence line stays one utterance.
SENTENCE_GAP_S = 0.3
TAIL_S = 3.0
VOICES = "https://huggingface.co/rhasspy/piper-voices/resolve/main"
SESSION = Path(__file__).with_name("session.json")


def voice_file(dirpath: Path, model: str) -> Path:
    region, name, quality = model.split("-")
    for suffix in (".onnx", ".onnx.json"):
        target = dirpath / f"{model}{suffix}"
        if not target.exists():
            url = f"{VOICES}/{region[:2]}/{region}/{name}/{quality}/{model}{suffix}"
            with urllib.request.urlopen(url, timeout=120) as r:
                target.write_bytes(r.read())
    return dirpath / f"{model}.onnx"


def synth(voice: PiperVoice, text: str, speaker_id: int | None) -> np.ndarray:
    parts: list[np.ndarray] = []
    for chunk in voice.synthesize(text, SynthesisConfig(speaker_id=speaker_id)):
        a = chunk.audio_float_array.astype(np.float32)
        t_in = np.arange(len(a)) / chunk.sample_rate
        # ponytail: linear upsampling; its imaging above 11 kHz is inaudible to Whisper at 16 kHz.
        parts += [np.interp(np.arange(0, t_in[-1], 1 / SR), t_in, a), np.zeros(int(SENTENCE_GAP_S * SR))]
    audio = np.concatenate(parts[:-1])
    # Piper's levels differ per voice; browsers apply AGC, so normalise like a live mic.
    return (audio * (PEAK / max(float(np.abs(audio).max()), 1e-6))).astype(np.float32)


def write_wav(path: Path, audio: np.ndarray) -> None:
    pcm = (np.clip(audio, -1, 1) * 32767).astype("<i2")
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    out = Path(sys.argv[1])
    (out / "voices").mkdir(parents=True, exist_ok=True)
    session = json.loads(SESSION.read_text())
    loaded: dict[str, PiperVoice] = {}
    for n, rnd in enumerate(session["rounds"], 1):
        t = rnd["lead_s"] - session["gap_s"]
        clips: list[tuple[str, float, np.ndarray]] = []
        for line in rnd["lines"]:
            v = session["speakers"][line["speaker"]]["voices"][line["lang"]]
            if v["model"] not in loaded:
                loaded[v["model"]] = PiperVoice.load(voice_file(out / "voices", v["model"]))
            audio = synth(loaded[v["model"]], line["text"], v.get("speaker_id"))
            start = t + line.get("gap_s", session["gap_s"])
            line["start"], line["end"] = round(start, 3), round(start + len(audio) / SR, 3)
            clips.append((line["speaker"], start, audio))
            # An overlapping line does not push the next one back: the floor holder keeps going.
            t = max(t, line["end"])
        total = int((t + TAIL_S) * SR)
        for speaker in session["speakers"]:
            track = np.zeros(total, np.float32)
            for who, start, audio in clips:
                if who == speaker:
                    i = int(start * SR)
                    track[i : i + len(audio)] += audio
            write_wav(out / f"{speaker}-r{n}.wav", track)
        print(f"round {n}: {total / SR:.1f} s, {len(clips)} lines")
    SESSION.write_text(json.dumps(session, ensure_ascii=False, indent=1) + "\n")


if __name__ == "__main__":
    main()
