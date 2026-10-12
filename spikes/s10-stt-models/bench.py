#!/usr/bin/env python3
"""S10: CPU speech-to-text on mixed French/English technical speech.

  bench.py audio <dir>                            Piper renders script.json into <dir>/audio.npz
  bench.py run <dir> <backend> <model> <glossary> [--threads N] [--every K]
                                                  one model over every K-th line, ROW/RUN lines on stdout
  bench.py report <log>... [--misses] [--every K] WER, term accuracy, language, timing per run
  bench.py selftest                               checks the scoring

backend: whisper (faster-whisper int8), onnx (onnx-asr int8: Parakeet, Canary), kyutai and granite
(transformers on torch CPU).
glossary: none | hot | prompt | both. Whisper: hotwords, initial_prompt, or both. Granite: prompt adds
its "Keywords:" list. Other backends ignore it.
`audio` and `run` need the lab Job's venvs (job.sh); `report` needs only the standard library.
"""

import argparse
import json
import re
import resource
import sys
import time
import unicodedata
import urllib.request
import zlib
from collections import defaultdict
from pathlib import Path

SCRIPT = Path(__file__).with_name("script.json")
VOICES = "https://huggingface.co/rhasspy/piper-voices/resolve/main"
GAP_S = 0.3  # Piper's pause between sentences, under the worker's 800 ms VAD cut
PAD_S = 0.2  # the worker's Segmenter.pad_ms around each utterance
VAD_CUT_S = 0.8  # silence the worker waits for before it ends an utterance
AC7_S = 10.0


def load_script() -> dict:
    return json.loads(SCRIPT.read_text())


# --- audio -----------------------------------------------------------------------------------------


def resample(a, sr_in: int, sr_out: int):
    import numpy as np

    # FFT resampling band-limits the signal; np.interp would alias Piper's 8-11 kHz into the 16 kHz band.
    n_out = round(len(a) * sr_out / sr_in)
    spec = np.fft.rfft(a)
    keep = n_out // 2 + 1
    spec = spec[:keep] if len(spec) >= keep else np.pad(spec, (0, keep - len(spec)))
    return (np.fft.irfft(spec, n_out) * (n_out / len(a))).astype(np.float32)


def cmd_audio(out: Path) -> None:
    import numpy as np
    from piper import PiperVoice, SynthesisConfig

    s = load_script()
    vdir = out / "voices"
    vdir.mkdir(parents=True, exist_ok=True)
    voices = {}
    arrays = {}
    for line in s["lines"]:
        v = s["voices"][line["voice"]]
        if v["model"] not in voices:
            region, name, quality = v["model"].split("-")
            for suffix in (".onnx", ".onnx.json"):
                target = vdir / f"{v['model']}{suffix}"
                if not target.exists():
                    url = f"{VOICES}/{region[:2]}/{region}/{name}/{quality}/{v['model']}{suffix}"
                    with urllib.request.urlopen(url, timeout=120) as r:
                        target.write_bytes(r.read())
            voices[v["model"]] = PiperVoice.load(vdir / f"{v['model']}.onnx")
        parts, sr = [], 22050
        for chunk in voices[v["model"]].synthesize(
            line["text"], SynthesisConfig(speaker_id=v.get("speaker_id"))
        ):
            sr = chunk.sample_rate
            parts += [chunk.audio_float_array.astype(np.float32), np.zeros(int(GAP_S * sr), np.float32)]
        pad = np.zeros(int(PAD_S * sr), np.float32)
        a = np.concatenate([pad, *parts[:-1], pad])
        a *= 0.5 / max(float(np.abs(a).max()), 1e-6)
        arrays[f"{line['id']}_16k"] = resample(a, sr, 16000)
        arrays[f"{line['id']}_24k"] = resample(a, sr, 24000)
        print(f"audio {line['id']} {len(a) / sr:.2f}s", flush=True)
    np.savez(out / "audio.npz", **arrays)


# --- backends ----------------------------------------------------------------------------------------


class Whisper:
    """Mirrors stt/grill_stt/transcribe.py: one encoder pass for detection and decoding, greedy, T=0."""

    sr = 16000

    def __init__(self, model: str, threads: int, glossary: str, terms: list[str]) -> None:
        import numpy as np
        from faster_whisper import WhisperModel

        self.np = np
        self.m = WhisperModel(model, device="cpu", compute_type="int8", cpu_threads=threads)
        words = ", ".join(terms)
        prompt = f"Glossaire : {words}." if glossary in ("prompt", "both") else None
        hot = words if glossary in ("hot", "both") else None
        _, info = self.m.transcribe(
            np.zeros(16000, np.float32),
            language="en",
            beam_size=1,
            temperature=0.0,
            without_timestamps=True,
            condition_on_previous_text=False,
            vad_filter=False,
            initial_prompt=prompt,
            hotwords=hot,
        )
        self.opts = info.transcription_options
        tok = self.m.hf_tokenizer
        self.prompt_tokens = sum(len(tok.encode(" " + p).ids) for p in (prompt, hot) if p)

    def __call__(self, audio, lang: str) -> dict:
        from faster_whisper.audio import pad_or_trim
        from faster_whisper.tokenizer import Tokenizer

        m = self.m
        features = m.feature_extractor(audio)
        first = features[:, : min(features.shape[-1] - 1, m.feature_extractor.nb_max_frames)]
        encoded = m.encode(pad_or_trim(first))
        (token, lang_prob), *_ = m.model.detect_language(encoded)[0]
        detected = token[2:-2]
        tokenizer = Tokenizer(m.hf_tokenizer, True, task="transcribe", language=detected)
        segments = list(m.generate_segments(features, tokenizer, self.opts, False, encoded))
        kept = [s for s in segments if not (s.no_speech_prob > 0.6 and s.avg_logprob < -1.0)]
        return {
            "hyp": " ".join(t for t in (s.text.strip() for s in kept) if t),
            "lang": detected,
            "lang_prob": lang_prob,
            "avg_logprob": min((s.avg_logprob for s in kept), default=0.0),
            "no_speech": max((s.no_speech_prob for s in kept), default=0.0),
        }


class Onnx:
    """Parakeet and Canary through onnx-asr: ONNX Runtime, no NeMo or torch."""

    sr = 16000
    prompt_tokens = 0

    def __init__(self, model: str, threads: int, glossary: str, terms: list[str]) -> None:
        import onnx_asr
        import onnxruntime as rt

        so = rt.SessionOptions()
        so.intra_op_num_threads = threads
        so.inter_op_num_threads = 1
        self.m = onnx_asr.load_model(model, quantization="int8", sess_options=so)
        # Canary has no language detection: it is given the line's true language (an oracle).
        self.canary = "canary" in model

    def __call__(self, audio, lang: str) -> dict:
        kw = {"language": lang} if self.canary else {}
        return {"hyp": str(self.m.recognize(audio, sample_rate=16000, **kw)).strip()}


class Kyutai:
    """Kyutai STT through transformers, fp32: the CPU has no native bf16."""

    sr = 24000
    prompt_tokens = 0

    def __init__(self, model: str, threads: int, glossary: str, terms: list[str]) -> None:
        import numpy as np
        import torch
        from transformers import KyutaiSpeechToTextForConditionalGeneration, KyutaiSpeechToTextProcessor

        torch.set_num_threads(threads)
        self.np, self.torch = np, torch
        self.proc = KyutaiSpeechToTextProcessor.from_pretrained(model)
        self.m = KyutaiSpeechToTextForConditionalGeneration.from_pretrained(model, torch_dtype=torch.float32)

    def __call__(self, audio, lang: str) -> dict:
        # The model writes text 0.5 s behind the audio; trailing silence lets it finish the last words.
        a = self.np.concatenate([audio, self.np.zeros(self.sr, self.np.float32)])
        with self.torch.inference_mode():
            out = self.m.generate(**self.proc(audio=a, return_tensors="pt"))
        return {"hyp": self.proc.batch_decode(out, skip_special_tokens=True)[0].strip()}


class Granite:
    """IBM Granite Speech through transformers, bf16 to stay in RAM; `prompt`/`both` add its keyword list."""

    sr = 16000

    def __init__(self, model: str, threads: int, glossary: str, terms: list[str]) -> None:
        import torch
        from transformers import AutoModelForSpeechSeq2Seq, AutoProcessor

        torch.set_num_threads(threads)
        self.torch = torch
        self.proc = AutoProcessor.from_pretrained(model)
        self.m = AutoModelForSpeechSeq2Seq.from_pretrained(model, torch_dtype=torch.bfloat16)
        ask = "<|audio|>can you transcribe the speech into a written format?"
        if glossary in ("prompt", "both"):
            ask += " Keywords: " + ", ".join(terms)
        tok = self.proc.tokenizer
        self.prompt = tok.apply_chat_template(
            [{"role": "user", "content": ask}], tokenize=False, add_generation_prompt=True
        )
        self.prompt_tokens = len(tok(self.prompt)["input_ids"])

    def __call__(self, audio, lang: str) -> dict:
        wav = self.torch.from_numpy(audio).unsqueeze(0)
        inputs = self.proc(self.prompt, wav, device="cpu", return_tensors="pt")
        with self.torch.inference_mode():
            out = self.m.generate(**inputs, max_new_tokens=200, do_sample=False, num_beams=1)
        n = inputs["input_ids"].shape[-1]
        return {"hyp": self.proc.tokenizer.batch_decode(out[:, n:], skip_special_tokens=True)[0].strip()}


BACKENDS = {"whisper": Whisper, "onnx": Onnx, "kyutai": Kyutai, "granite": Granite}


def cpu_info() -> dict:
    lines = Path("/proc/cpuinfo").read_text().splitlines()
    model = next((ln.split(":", 1)[1].strip() for ln in lines if ln.startswith("model name")), "")
    try:
        quota, period = Path("/sys/fs/cgroup/cpu.max").read_text().split()
        limit = "none" if quota == "max" else f"{int(quota) / int(period):g}"
    except OSError:
        limit = "unknown"
    return {"cpu_model": model, "cpu_limit": limit}


def cmd_run(a: argparse.Namespace) -> None:
    import numpy as np

    s = load_script()
    audio = np.load(Path(a.dir) / "audio.npz")
    lines = s["lines"][:: a.every]
    t0 = time.perf_counter()
    engine = BACKENDS[a.backend](a.model, a.threads, a.glossary, sorted(s["terms"] + s["distractors"]))
    load_s = time.perf_counter() - t0
    key = f"{a.backend}:{a.model}:{a.glossary}:{a.threads}"
    first = lines[0]
    engine(audio[f"{first['id']}_{engine.sr // 1000}k"], first["lang"])  # warm-up, not timed
    for line in lines:
        x = audio[f"{line['id']}_{engine.sr // 1000}k"]
        t = time.perf_counter()
        out = engine(x, line["lang"])
        dt = time.perf_counter() - t
        row = {"run": key, "id": line["id"], "dur": len(x) / engine.sr, "infer": dt} | out
        print("ROW", json.dumps(row, ensure_ascii=False), flush=True)
    rss_mb = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024
    info = {"run": key, "load_s": load_s, "peak_rss_mb": rss_mb, "prompt_tokens": engine.prompt_tokens}
    print("RUN", json.dumps(info | cpu_info()), flush=True)


# --- report ------------------------------------------------------------------------------------------


def words(s: str) -> list[str]:
    # Lowercase, punctuation, apostrophes and hyphens become spaces: "l'egress" -> "l egress".
    s = unicodedata.normalize("NFKC", s).lower().replace("’", "'")
    return re.sub(r"[^\w]+", " ", s).split()


def edits(ref: list[str], hyp: list[str]) -> int:
    prev = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        cur = [i]
        for j, h in enumerate(hyp, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r != h)))
        prev = cur
    return prev[-1]


def contains(hay: list[str], needle: list[str]) -> bool:
    n = len(needle)
    return any(hay[i : i + n] == needle for i in range(len(hay) - n + 1))


def posted(r: dict) -> bool:
    """The stt worker's plausibility checks (pipeline.implausible); the repetition one for every model."""
    text = r["hyp"].encode()
    if text and len(text) / len(zlib.compress(text)) > 2.4:
        return False
    if "avg_logprob" not in r:
        return True
    unsure = r["lang_prob"] < 0.5
    return not (
        r["avg_logprob"] < -1.0
        or (r["no_speech"] > 0.6 and unsure)
        or (unsure and r["dur"] - 2 * PAD_S >= 2.0)
    )


def pct(xs: list[float], q: float) -> float:
    xs = sorted(xs)
    return xs[min(len(xs) - 1, round(q * (len(xs) - 1)))]


def cmd_report(logs: list[str], misses: bool, every: int) -> None:
    s = load_script()
    # --every K scores every run on the subset a `run --every K` saw, so slow models compare fairly.
    ref = {ln["id"]: ln for ln in s["lines"][::every]}
    rows, runs = defaultdict(list), {}
    for log in logs:
        for raw in Path(log).read_text().splitlines():
            if raw.startswith("ROW "):
                r = json.loads(raw[4:])
                if r["id"] in ref:
                    rows[r["run"]].append(r)
            elif raw.startswith("RUN "):
                r = json.loads(raw[4:])
                runs[r["run"]] = r
    print(
        "| run | WER all / fr / fr-siwis / fr-mls / en | terms all / fr / en | lang ok | dropped "
        "| infer p50 / p95 s | RTF | latency p95 s | AC7 | RAM MB | prompt tok |"
    )
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for key, rs in rows.items():
        err, tot = defaultdict(int), defaultdict(int)
        hit, seen = defaultdict(int), defaultdict(int)
        missed = []
        lang_ok = lang_n = dropped = 0
        for r in rs:
            line = ref[r["id"]]
            ok_post = posted(r)
            dropped += not ok_post
            rw, hw = words(line["text"]), words(r["hyp"]) if ok_post else []
            for g in ("all", line["lang"], line["voice"]):
                err[g] += edits(rw, hw)
                tot[g] += len(rw)
            for term in s["terms"]:
                tw = words(term)
                if contains(rw, tw):
                    ok = contains(hw, tw)
                    for g in ("all", line["lang"]):
                        seen[g] += 1
                        hit[g] += ok
                    if not ok:
                        missed.append(f"{r['id']}:{term}")
            if r.get("lang"):
                lang_n += 1
                lang_ok += r["lang"] == line["lang"]
        infer = [r["infer"] for r in rs]
        rtf = sum(infer) / sum(r["dur"] for r in rs)
        lat95 = VAD_CUT_S + pct(infer, 0.95)
        # One talker speaks back to back: past RTF 1 the queue grows and AC7 fails however short the line.
        ac7 = "yes" if lat95 <= AC7_S and rtf < 1 else "no"
        info = runs.get(key, {})

        def ratio(n, d, g):
            return f"{n[g] / d[g]:.2f}" if d[g] else "-"

        print(
            f"| {key} | {' / '.join(ratio(err, tot, g) for g in ('all', 'fr', 'fr1', 'fr2', 'en'))} "
            f"| {ratio(hit, seen, 'all')} / {ratio(hit, seen, 'fr')} / {ratio(hit, seen, 'en')} "
            f"| {f'{lang_ok}/{lang_n}' if lang_n else '-'} | {dropped}/{len(rs)} "
            f"| {pct(infer, 0.5):.2f} / {pct(infer, 0.95):.2f} | {rtf:.2f} | {lat95:.1f} | {ac7} "
            f"| {info.get('peak_rss_mb', 0):.0f} | {info.get('prompt_tokens', 0)} |"
        )
        if misses:
            print(f"<!-- {key} missed: {' '.join(missed)} -->")


def cmd_selftest() -> None:
    assert words("L'egress du Helm-chart, P95 !") == ["l", "egress", "du", "helm", "chart", "p95"]
    assert edits(words("on fait le rollout"), words("On fait le roll-out")) == 2
    assert edits([], ["a"]) == 1 and edits(["a"], []) == 1
    assert contains(words("Le Helm chart expose"), words("Helm chart"))
    assert not contains(words("le helm-charte"), words("Helm chart"))
    assert not contains(words("des pods"), words("pod"))
    base = {"hyp": "Il faut ajouter un feature flag.", "dur": 4.0}
    assert posted(base)
    assert not posted(base | {"hyp": "vas-y, " * 40})
    whisper = base | {"lang_prob": 0.9, "avg_logprob": -0.3, "no_speech": 0.0}
    assert posted(whisper)
    assert not posted(whisper | {"avg_logprob": -1.2})
    assert not posted(whisper | {"lang_prob": 0.3})
    assert posted(whisper | {"lang_prob": 0.3, "dur": 2.0})  # a short "oui" is spared
    print("selftest ok")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("audio").add_argument("dir")
    r = sub.add_parser("run")
    r.add_argument("dir")
    r.add_argument("backend", choices=BACKENDS)
    r.add_argument("model")
    r.add_argument("glossary", choices=["none", "hot", "prompt", "both"])
    r.add_argument("--threads", type=int, default=4)
    r.add_argument("--every", type=int, default=1)
    rep = sub.add_parser("report")
    rep.add_argument("logs", nargs="+")
    rep.add_argument("--misses", action="store_true")
    rep.add_argument("--every", type=int, default=1)
    sub.add_parser("selftest")
    a = p.parse_args()
    if a.cmd == "audio":
        cmd_audio(Path(a.dir))
    elif a.cmd == "run":
        cmd_run(a)
    elif a.cmd == "report":
        cmd_report(a.logs, a.misses, a.every)
    else:
        cmd_selftest()


if __name__ == "__main__":
    sys.exit(main())
