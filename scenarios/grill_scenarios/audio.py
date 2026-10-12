"""Piper speech for the voice rounds: one WAV per speaker and round, on the round's shared timeline."""

import hashlib
import urllib.request
import wave
from collections.abc import Callable
from pathlib import Path
from typing import Any

import numpy as np
import numpy.typing as npt

from .schema import Scenario, VoiceRound

# The rate the STT worker reads, and the one spike S5's rerun fed.
SR = 16000
PEAK = 0.5
# Below the STT worker's 800 ms VAD cut, so a multi-sentence line stays one utterance.
SENTENCE_GAP_S = 0.3
TAIL_S = 3.0
VOICES_URL = "https://huggingface.co/rhasspy/piper-voices/resolve/main"

Audio = npt.NDArray[np.float32]
Synth = Callable[[str, int | None, str], Audio]


def resample(a: Audio, sr_in: int) -> Audio:
    """Band-limited resampling to SR through the spectrum: no aliasing from Piper's 22 kHz output."""
    if sr_in == SR:
        return a
    n = round(len(a) * SR / sr_in)
    spec = np.fft.rfft(a)[: n // 2 + 1]
    return (np.fft.irfft(spec, n) * (n / len(a))).astype(np.float32)


def timeline(rnd: VoiceRound, gap: float, durations: list[float]) -> list[tuple[float, float]]:
    """Start and end of each line in seconds: a line waits its gap after the floor holder's end;
    a negative gap overlaps, and an overlapping line does not push the next one back."""
    t = rnd.lead - gap
    out = []
    for line, d in zip(rnd.lines, durations, strict=True):
        start = t + (gap if line.gap is None else line.gap)
        out.append((round(start, 3), round(start + d, 3)))
        t = max(t, start + d)
    return out


def voice_file(cache: Path, model: str) -> Path:
    region, name, quality = model.split("-")
    for suffix in (".onnx", ".onnx.json"):
        target = cache / f"{model}{suffix}"
        if not target.exists():
            url = f"{VOICES_URL}/{region[:2]}/{region}/{name}/{quality}/{model}{suffix}"
            with urllib.request.urlopen(url, timeout=120) as r:
                data = r.read()
            target.with_suffix(target.suffix + ".part").write_bytes(data)
            target.with_suffix(target.suffix + ".part").rename(target)
    return cache / f"{model}.onnx"


def piper(cache: Path) -> Synth:
    """A synthesiser over cached Piper models; each line's clip is cached by voice and text."""
    from piper import PiperVoice, SynthesisConfig

    voices: dict[str, Any] = {}

    def synth(model: str, speaker: int | None, text: str) -> Audio:
        key = hashlib.sha256(f"{model}\0{speaker}\0{SR}\0{text}".encode()).hexdigest()[:32]
        clip = cache / "clips" / f"{key}.npy"
        if clip.exists():
            cached: Audio = np.load(clip).astype(np.float32)
            return cached
        if model not in voices:
            voices[model] = PiperVoice.load(voice_file(cache / "voices", model))
        parts: list[Audio] = []
        for chunk in voices[model].synthesize(text, SynthesisConfig(speaker_id=speaker)):
            parts += [
                resample(chunk.audio_float_array.astype(np.float32), chunk.sample_rate),
                np.zeros(int(SENTENCE_GAP_S * SR), np.float32),
            ]
        audio = np.concatenate(parts[:-1])
        # Piper's levels differ per voice; browsers apply AGC, so normalise like a live mic.
        audio = (audio * (PEAK / max(float(np.abs(audio).max()), 1e-6))).astype(np.float32)
        clip.parent.mkdir(parents=True, exist_ok=True)
        np.save(clip, audio)
        return audio

    return synth


def write_wav(path: Path, audio: Audio) -> None:
    pcm = (np.clip(audio, -1, 1) * 32767).astype("<i2")
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def render(s: Scenario, out: Path, synth: Synth) -> list[dict[str, Any]]:
    """Writes <out>/<speaker>-r<n>.wav for every voice round and returns each round's lines with
    their timings, as the driver and the scoring read them."""
    out.mkdir(parents=True, exist_ok=True)
    people = {p.name: p for p in s.participants}
    speakers = [p.name for p in s.participants if p.voices]
    rounds = []
    for n, rnd in enumerate((st.voice for st in s.steps if st.voice), 1):
        clips = []
        for ln in rnd.lines:
            v = people[ln.by].voices[ln.lang]
            clips.append(synth(v.model, v.speaker, ln.text))
        times = timeline(rnd, s.gap, [len(c) / SR for c in clips])
        total = int((max(e for _, e in times) + TAIL_S) * SR)
        tracks = {}
        for who in speakers:
            track = np.zeros(total, np.float32)
            for ln, clip, (start, _) in zip(rnd.lines, clips, times, strict=True):
                if ln.by == who:
                    i = int(start * SR)
                    track[i : i + len(clip)] += clip
            path = out / f"{who}-r{n}.wav"
            write_wav(path, track)
            tracks[who] = str(path)
        lines = [
            {
                "by": ln.by,
                "lang": ln.lang,
                "text": ln.text,
                "start": a,
                "end": b,
                "overlap": (ln.gap or 0) < 0,
            }
            for ln, (a, b) in zip(rnd.lines, times, strict=True)
        ]
        rounds.append({"round": n, "tracks": tracks, "lines": lines, "seconds": total / SR})
    return rounds
