#!/usr/bin/env python3
"""Spike S14b: synthesise the test set with one engine, one voice and one normalisation mode.

Runs inside the engine's own venv (engines pin incompatible torch versions), so it imports only
numpy and soundfile at the top. Reads texts.json from normalize.py, writes <out>/<id>.wav per
sentence and prints one JSON line per sentence, then a summary line.

usage: synth.py <engine> <voice> <mode> <out dir> [--threads 4] [--only id,id]
"""

import argparse
import json
import os
import resource
import time
from collections.abc import Callable, Iterator
from pathlib import Path

import numpy as np
import soundfile as sf

HERE = Path(__file__).parent
# Synth(lang, text) yields float32 mono chunks at the engine's rate.
Synth = Callable[[str, str], Iterator[np.ndarray]]


def pocket(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import torch
    from pocket_tts import TTSModel

    torch.set_num_threads(threads)
    models = {
        "fr": TTSModel.load_model(language="french"),
        "en": TTSModel.load_model(language=os.environ.get("S14B_POCKET_EN", "english_2026-09")),
    }
    # "fr|en" pairs two prompts; one prompt alone is the same speaker in both languages.
    fr_voice, _, en_voice = voice.partition("|")
    voices = {"fr": fr_voice, "en": en_voice or fr_voice}
    states = {lg: m.get_state_for_audio_prompt(voices[lg]) for lg, m in models.items()}

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        for c in models[lang].generate_audio_stream(states[lang], text):
            yield c.numpy().astype(np.float32)

    return models["fr"].sample_rate, synth, {"fr", "en"}


def supertonic(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    from supertonic import TTS

    tts = TTS(auto_download=True, intra_op_num_threads=threads, inter_op_num_threads=1)
    fr_voice, _, en_voice = voice.partition("|")
    styles = {
        "fr": tts.get_voice_style(voice_name=fr_voice),
        "en": tts.get_voice_style(voice_name=en_voice or fr_voice),
    }

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        wav, _ = tts.synthesize(text, voice_style=styles[lang], lang=lang)
        yield np.asarray(wav, dtype=np.float32).reshape(-1)

    return tts.sample_rate, synth, {"fr", "en"}


def sopro(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import torch
    from sopro import SoproTTS

    torch.set_num_threads(threads)
    q = "int8" if os.environ.get("S14B_SOPRO_INT8") == "1" else None
    tts = SoproTTS.from_pretrained("samuel-vitorino/sopro-v2-turbo", device="cpu", quantization=q)
    # Cloning only: the voice is a reference clip, the same one for both languages.
    ref = tts.prepare_reference(ref_audio_path=voice, stream=True)

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        for c in tts.stream(text, ref=ref, lang=lang):
            yield c.reshape(-1).numpy().astype(np.float32)

    return tts.sample_rate, synth, {"fr", "en"}


def kokoro(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import re

    import onnxruntime as ort
    from kokoro_onnx import Kokoro

    d = Path(os.environ["S14_MODELS"]) / "kokoro"
    o = ort.SessionOptions()
    o.intra_op_num_threads, o.inter_op_num_threads = threads, 1
    sess = ort.InferenceSession(str(d / "kokoro-v1.0.onnx"), sess_options=o, providers=["CPUExecutionProvider"])
    k = Kokoro.from_session(sess, str(d / "voices-v1.0.bin"))
    # "ff_siwis|bf_emma": its one French voice for French, an English one for English.
    a, _, b = voice.partition("|")
    voices = {"fr": a, "en": b} if a == "ff_siwis" else {"en": a}
    if a == "ff_siwis" and not b:
        voices["en"] = a
    accent = "en-gb" if voices["en"].startswith("b") else "en-us"
    chunk = os.environ.get("S14B_KOKORO_CHUNK", "1") == "1"

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        # Kokoro renders a whole call before returning: cutting at clause ends is what streams.
        for part in re.split(r"(?<=[,.;?!])\s+", text) if chunk else [text]:
            audio, _ = k.create(part, voice=voices[lang], speed=1.0, lang=accent if lang == "en" else "fr-fr")
            yield audio.astype(np.float32)

    return 24000, synth, set(voices)


def piper(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import onnxruntime as ort
    from piper import PiperVoice
    from piper.config import PiperConfig
    from piper.voice import ESPEAK_DATA_DIR

    path = Path(os.environ["S14_MODELS"]) / "piper" / f"{voice}.onnx"
    cfg = PiperConfig.from_dict(json.loads(Path(f"{path}.json").read_text()))
    o = ort.SessionOptions()
    o.intra_op_num_threads, o.inter_op_num_threads = threads, 1
    sess = ort.InferenceSession(str(path), sess_options=o, providers=["CPUExecutionProvider"])
    pv = PiperVoice(config=cfg, session=sess, espeak_data_dir=Path(ESPEAK_DATA_DIR), download_dir=Path.cwd())

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        for c in pv.synthesize(text):
            yield c.audio_float_array.astype(np.float32)

    return cfg.sample_rate, synth, {voice[:2]}


def chatterbox(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import torch
    from chatterbox.mtl_tts import ChatterboxMultilingualTTS

    torch.set_num_threads(threads)
    m = ChatterboxMultilingualTTS.from_pretrained(device="cpu")
    m.prepare_conditionals(voice)

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        yield m.generate(text, language_id=lang).squeeze(0).numpy().astype(np.float32)

    return m.sr, synth, {"fr", "en"}


def kyutai(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import torch
    from moshi.models import loaders
    from moshi.models.tts import TTSModel

    torch.set_num_threads(threads)
    nq = int(os.environ.get("S14B_KYUTAI_NQ", "32"))
    info = loaders.CheckpointInfo.from_hf_repo("kyutai/tts-1.6b-en_fr")
    # float32: bf16 matmuls have no fast path on these CPUs.
    m = TTSModel.from_checkpoint_info(info, n_q=nq, temp=0.6, device="cpu", dtype=torch.float32)
    vp = m.get_voice_path(voice)
    attrs = m.make_condition_attributes([vp], cfg_coef=2.0)

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        # Batch generation; the first audio a streaming server could send is the first decoded frame
        # after the text-to-audio delay, so yield it as soon as that frame exists.
        entries = [m.prepare_script([text], padding_between=1)]
        frames: list[torch.Tensor] = []
        first_at: list[float] = []

        def on_frame(f: torch.Tensor) -> None:
            if (f != -1).all():
                frames.append(f)
                if len(frames) == m.delay_steps + 3:
                    first_at.append(time.perf_counter())

        res = m.generate(entries, [attrs], on_frame=on_frame)
        with m.mimi.streaming(1), torch.no_grad():
            pcm = [torch.clip(m.mimi.decode(f[:, 1:, :]), -1, 1) for f in res.frames[m.delay_steps :]][2:]
        audio = torch.cat([p[0, 0] for p in pcm[: res.end_steps[0]]]).numpy()
        FIRST_AT.extend(first_at)
        yield audio.astype(np.float32)

    return m.mimi.sample_rate, synth, {"fr", "en"}


def melo(voice: str, threads: int) -> tuple[int, Synth, set[str]]:
    import torch
    from melo.api import TTS

    torch.set_num_threads(threads)
    m = TTS(language="FR", device="cpu")
    spk = m.hps.data.spk2id[voice]

    def synth(lang: str, text: str) -> Iterator[np.ndarray]:
        yield m.tts_to_file(text, spk, None, speed=1.0, quiet=True).astype(np.float32)

    return m.hps.data.sampling_rate, synth, {"fr"}


ENGINES = {
    "pocket": pocket,
    "kokoro": kokoro,
    "sopro": sopro,
    "supertonic": supertonic,
    "piper": piper,
    "chatterbox": chatterbox,
    "kyutai": kyutai,
    "melo": melo,
}
# Set by engines that synthesise a whole line but know when streaming could have started.
FIRST_AT: list[float] = []


def trim(audio: np.ndarray, sr: int) -> np.ndarray:
    """Cut leading and trailing silence: 10 ms frames under -40 dB of the span's peak."""
    hop = sr // 100
    n = len(audio) // hop
    if n == 0:
        return audio
    rms = np.sqrt((audio[: n * hop].reshape(n, hop) ** 2).mean(axis=1))
    loud = np.flatnonzero(rms > rms.max() * 0.01)
    return audio[max(loud[0] - 2, 0) * hop : (loud[-1] + 3) * hop]


def resample(audio: np.ndarray, src: int, dst: int) -> np.ndarray:
    if src == dst:
        return audio
    from math import gcd

    from scipy.signal import resample_poly

    g = gcd(src, dst)
    return resample_poly(audio, dst // g, src // g).astype(np.float32)


def level(audio: np.ndarray, target_db: float = -22.0) -> np.ndarray:
    """Same loudness for every span, so a language switch does not jump in volume (S14 levels lines alike)."""
    rms = float(np.sqrt(np.mean(audio**2))) or 1e-9
    return audio * (10 ** (target_db / 20) / rms)


def cpu_s() -> float:
    u = resource.getrusage(resource.RUSAGE_SELF)
    return u.ru_utime + u.ru_stime


def build(spec: str, threads: int) -> tuple[int, Synth, set[str]]:
    engine, _, voice = spec.partition(":")
    return ENGINES[engine](voice, threads)


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("mode")
    p.add_argument("out", type=Path)
    p.add_argument("--fr", help="engine:voice for French lines and spans")
    p.add_argument("--en", help="engine:voice for English lines and spans; default: --fr's engine")
    p.add_argument("--threads", type=int, default=4)
    p.add_argument("--only", default="")
    a = p.parse_args()
    texts = json.loads((HERE / "texts.json").read_text())[a.mode]
    ids = a.only.split(",") if a.only else list(texts)

    t0 = time.perf_counter()
    specs = {"fr": a.fr or a.en, "en": a.en or a.fr}
    engines: dict[str, tuple[int, Synth, set[str]]] = {}
    if specs["fr"].split(":")[0] == specs["en"].split(":")[0]:
        # One engine for both languages: load it once; "voice|voice" pairs its two voices.
        fe, _, fv = specs["fr"].partition(":")
        ev = specs["en"].partition(":")[2]
        built = build(f"{fe}:{fv}|{ev}" if fv != ev else specs["fr"], a.threads)
        engines = dict.fromkeys(built[2], built)
    else:
        for lg, spec in specs.items():
            engines[lg] = build(spec, a.threads)
    sr = engines["fr" if "fr" in engines else "en"][0]
    for lg, (_, synth, _) in engines.items():  # first-call allocations are not per-line latency
        for _ in synth(lg, "Ok, c'est parti." if lg == "fr" else "Okay, let's go."):
            pass
    load_s = time.perf_counter() - t0
    a.out.mkdir(parents=True, exist_ok=True)
    tot_synth = tot_audio = 0.0
    cpu0 = cpu_s()
    for sid in ids:
        spans = texts[sid]
        if any(lg not in engines for lg, _ in spans):
            # No voice for that language: read the line in the one language there is.
            spans = [[next(iter(engines)), " ".join(t for _, t in spans)]]
        FIRST_AT.clear()
        start = time.perf_counter()
        first = None
        pieces = []
        for lg, text in spans:
            esr, synth, _ = engines[lg]
            span = []
            for chunk in synth(lg, text):
                if first is None:
                    first = (FIRST_AT[0] if FIRST_AT else time.perf_counter()) - start
                span.append(chunk)
            audio = resample(np.concatenate(span), esr, sr)
            if len(spans) > 1:
                # Engines pad each call with silence; between spans of one sentence that reads as a pause.
                audio = np.concatenate([level(trim(audio, sr)), np.zeros(int(sr * 0.04), np.float32)])
            pieces.append(audio)
        took = time.perf_counter() - start
        audio = level(np.concatenate(pieces))
        sf.write(a.out / f"{sid}.wav", np.clip(audio, -1, 1), sr, subtype="PCM_16")
        dur = len(audio) / sr
        tot_synth, tot_audio = tot_synth + took, tot_audio + dur
        print(
            json.dumps(
                {
                    "id": sid,
                    "ttfa_s": round(first or 0, 3),
                    "synth_s": round(took, 2),
                    "audio_s": round(dur, 2),
                    "spans": len(spans),
                }
            ),
            flush=True,
        )
    print(
        json.dumps(
            {
                "out": a.out.name,
                "fr": specs["fr"],
                "en": specs["en"],
                "mode": a.mode,
                "threads": a.threads,
                "n": len(ids),
                "load_s": round(load_s, 1),
                "rtf": round(tot_synth / tot_audio, 3),
                # Core-seconds per audio second, every thread: what sizing needs, unlike wall-clock RTF.
                "cpu_per_audio_s": round((cpu_s() - cpu0) / tot_audio, 2),
                "rss_peak_mb": round(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024),
            }
        ),
        flush=True,
    )


if __name__ == "__main__":
    main()
