#!/usr/bin/env python3
"""Spike S14: one TTS engine, one language, per process, so peak RSS is that engine's alone.

Prints one JSON line of measures and writes a WAV per voice. Run under taskset; --threads must
match the pinned cores, since onnxruntime and torch size their pools from the machine, not the mask.
"""

import argparse
import functools
import json
import os
import resource
import time
import wave
from collections.abc import Callable, Iterator
from pathlib import Path

import numpy as np

TEXTS = {
    "fr": [
        "J'ai relu la spec.",
        "Le webhook GitHub renvoie une erreur 502 quand le pod Kubernetes redémarre, "
        "donc j'ai ajouté un retry avec un backoff exponentiel.",
        "Au sujet du commentaire d'Alice sur le budget, j'ai répondu que le cluster PostgreSQL "
        "coûte environ trois cents euros par mois.",
        "J'ai ouvert deux notes sur la section OAuth, le refresh token n'expire jamais "
        "et la latence p95 de LiveKit dépasse deux cents millisecondes.",
    ],
    "en": [
        "I read the spec again.",
        "The GitHub webhook returns a 502 when the Kubernetes pod restarts, "
        "so I added a retry with exponential backoff.",
        "About Alice's comment on the budget, I replied that the PostgreSQL cluster "
        "costs about three hundred euros a month.",
        "I opened two notes on the OAuth section: the refresh token never expires, "
        "and the p95 latency of LiveKit is over two hundred milliseconds.",
    ],
}

# The same text with numbers and product names spelled the way they are said: what a pronunciation
# step before the synthesizer would produce (spike S14, the jargon test).
TEXTS["fr_said"] = [
    t.replace("502", "cinq cent deux")
    .replace("p95", "p quatre-vingt-quinze")
    .replace("LiveKit", "Live Kit")
    .replace("GitHub", "Git Hub")
    .replace("OAuth", "O Auth")
    for t in TEXTS["fr"]
]
TEXTS["en_said"] = [
    t.replace("502", "five oh two")
    .replace("p95", "p ninety-five")
    .replace("LiveKit", "Live Kit")
    .replace("OAuth", "O Auth")
    for t in TEXTS["en"]
]


MODELS = Path(os.environ.get("S14_MODELS", "models"))

# A synth function takes one sentence and yields float32 mono chunks at the engine's rate.
Synth = Callable[[str], Iterator[np.ndarray]]


def ort_options(threads: int):  # type: ignore[no-untyped-def]
    import onnxruntime as ort

    o = ort.SessionOptions()
    o.intra_op_num_threads = threads
    o.inter_op_num_threads = 1
    return o


def piper_engine(voice: str, lang: str, threads: int) -> tuple[int, Synth]:
    import onnxruntime as ort
    from piper import PiperVoice, SynthesisConfig
    from piper.config import PiperConfig
    from piper.voice import ESPEAK_DATA_DIR

    name, _, spk = voice.partition("#")
    path = MODELS / "piper" / f"{name}.onnx"
    cfg = PiperConfig.from_dict(json.loads(Path(f"{path}.json").read_text()))
    sess = ort.InferenceSession(str(path), sess_options=ort_options(threads), providers=["CPUExecutionProvider"])
    pv = PiperVoice(config=cfg, session=sess, espeak_data_dir=Path(ESPEAK_DATA_DIR), download_dir=Path.cwd())
    sc = SynthesisConfig(speaker_id=int(spk) if spk else None)

    def synth(text: str) -> Iterator[np.ndarray]:
        for c in pv.synthesize(text, sc):
            yield c.audio_float_array.astype(np.float32)

    return cfg.sample_rate, synth


def kokoro_engine(voice: str, lang: str, threads: int, model_file: str = "kokoro-v1.0.onnx") -> tuple[int, Synth]:
    import onnxruntime as ort
    from kokoro_onnx import Kokoro

    model = MODELS / "kokoro" / model_file
    sess = ort.InferenceSession(str(model), sess_options=ort_options(threads), providers=["CPUExecutionProvider"])
    k = Kokoro.from_session(sess, str(MODELS / "kokoro" / "voices-v1.0.bin"))
    klang = {"fr": "fr-fr", "en": "en-us"}[lang]
    if voice.startswith("b"):
        klang = "en-gb" if lang == "en" else klang

    def synth(text: str) -> Iterator[np.ndarray]:
        audio, _ = k.create(text, voice=voice, speed=1.0, lang=klang)
        yield audio.astype(np.float32)

    return 24000, synth


@functools.cache
def _pocket_model(language: str):  # type: ignore[no-untyped-def]
    """One model per language in a process: voices differ only by their state."""
    from pocket_tts import TTSModel

    return TTSModel.load_model(language=language)


def pocket_engine(voice: str, lang: str, threads: int, suffix: str = "") -> tuple[int, Synth]:
    import torch

    torch.set_num_threads(threads)
    # Shared across voices: callers must not generate on two voices at once (not thread-safe).
    model = _pocket_model({"fr": "french", "en": "english_2026-09"}[lang[:2]] + suffix)
    state = model.get_state_for_audio_prompt(voice)

    def synth(text: str) -> Iterator[np.ndarray]:
        for c in model.generate_audio_stream(state, text):
            yield c.numpy().astype(np.float32)

    return model.sample_rate, synth


def supertonic_engine(voice: str, lang: str, threads: int) -> tuple[int, Synth]:
    from supertonic import TTS

    tts = TTS(auto_download=True, intra_op_num_threads=threads, inter_op_num_threads=1)
    style = tts.get_voice_style(voice_name=voice)

    def synth(text: str) -> Iterator[np.ndarray]:
        wav, _ = tts.synthesize(text, voice_style=style, lang=lang[:2])
        yield np.asarray(wav, dtype=np.float32).reshape(-1)

    return tts.sample_rate, synth


ENGINES: dict[str, Callable[[str, str, int], tuple[int, Synth]]] = {
    "piper": piper_engine,
    "kokoro": kokoro_engine,
    "kokoro_int8": functools.partial(kokoro_engine, model_file="kokoro-v1.0.int8.onnx"),
    "pocket": pocket_engine,
    # The 24-layer variants: higher quality, slower, per the README.
    "pocket24": functools.partial(pocket_engine, suffix="_24l"),
    "supertonic": supertonic_engine,
}


def cpu_s() -> float:
    u = resource.getrusage(resource.RUSAGE_SELF)
    return u.ru_utime + u.ru_stime


def rss_mb() -> float:
    return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024


def write_wav(path: Path, sr: int, audio: np.ndarray) -> None:
    pcm = (np.clip(audio, -1, 1) * 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(pcm.tobytes())


def run(engine: str, lang: str, voice: str, threads: int, out: Path | None) -> dict[str, object]:
    t0 = time.perf_counter()
    sr, synth = ENGINES[engine](voice, lang, threads)
    for _ in synth("Ok."):  # warm-up: first-call allocations are not per-utterance latency
        pass
    load_s = time.perf_counter() - t0
    rss_loaded = rss_mb()
    pieces, ttfa, synth_s = [], [], 0.0
    cpu0 = cpu_s()
    for sentence in TEXTS[lang]:
        start = time.perf_counter()
        first = None
        for chunk in synth(sentence):
            if first is None:
                first = time.perf_counter() - start
            pieces.append(chunk)
        synth_s += time.perf_counter() - start
        ttfa.append(first or 0.0)
        pieces.append(np.zeros(int(sr * 0.25), np.float32))
    audio = np.concatenate(pieces)
    dur = len(audio) / sr
    if out is not None:
        out.parent.mkdir(parents=True, exist_ok=True)
        write_wav(out, sr, audio)
    return {
        "engine": engine,
        "lang": lang,
        "voice": voice,
        "threads": threads,
        "load_s": round(load_s, 2),
        "ttfa_first_s": round(ttfa[0], 3),
        "ttfa_long_s": round(max(ttfa), 3),
        "rtf": round(synth_s / dur, 3),
        # Core-seconds per second of audio, all threads: what sizing needs, unlike wall-clock RTF.
        "cpu_per_audio_s": round((cpu_s() - cpu0) / dur, 3),
        "audio_s": round(dur, 1),
        "rss_loaded_mb": round(rss_loaded),
        "rss_peak_mb": round(rss_mb()),
        "wav": str(out) if out else None,
    }


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("engine", choices=sorted(ENGINES))
    p.add_argument("lang", choices=sorted(TEXTS))
    p.add_argument("voice")
    p.add_argument("--threads", type=int, default=4)
    p.add_argument("--out", type=Path)
    a = p.parse_args()
    print(json.dumps(run(a.engine, a.lang, a.voice, a.threads, a.out)), flush=True)


if __name__ == "__main__":
    main()
