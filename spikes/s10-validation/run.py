#!/usr/bin/env python3
"""One engine over one corpus; one JSON line per utterance in /data/out/<tag>.<set>.jsonl.

usage: run.py <engine> <set> <glossary> [--threads N] [--tag T] [--track K]
engine: fw:<model> | tcpp:<gguf name>[:-l xx] | sherpa:<dir under $D/sherpa>
glossary: none | on  (Whisper: room sentence as initial_prompt; tcpp: --vocabulary; sherpa: hotwords)
--track K: only the K-th longest speaker track of the set (the 2-stream runs give each process one track).
"""
import argparse
import json
import os
import subprocess
import time
from dataclasses import replace
from pathlib import Path

import numpy as np

D = Path(os.environ.get("D", "/data"))
SR = 16000


def glossary(corpus: str) -> tuple[str, list[str]]:
    """The room prompt (production sentence) and its terms for one corpus."""
    terms = json.loads((D / "corpus" / "glossary.json").read_text())[corpus]
    sentence = "Avec Alice et Grill, on parle de " + ", ".join(terms[:-1]) + " et " + terms[-1] + "."
    return sentence, terms


class FW:
    """stt/grill_stt/transcribe.py on the room-ux branch: one encoder pass, greedy, T=0, prompt per room."""

    def __init__(self, model: str, threads: int, prompt: str | None) -> None:
        from faster_whisper import WhisperModel

        path = model
        if model.startswith("bofenghuang/"):
            from huggingface_hub import snapshot_download

            path = snapshot_download(model, allow_patterns=["ctranslate2/*"]) + "/ctranslate2"
        self.m = WhisperModel(path, device="cpu", compute_type="int8", cpu_threads=threads)
        _, info = self.m.transcribe(np.zeros(SR, np.float32), language="en", beam_size=1, temperature=0.0,
                                    without_timestamps=True, condition_on_previous_text=False, vad_filter=False)
        self.opts = replace(info.transcription_options, initial_prompt=prompt)

    def __call__(self, audio: np.ndarray) -> dict:
        from faster_whisper.audio import pad_or_trim
        from faster_whisper.tokenizer import Tokenizer

        m = self.m
        features = m.feature_extractor(audio)
        first = features[:, : min(features.shape[-1] - 1, m.feature_extractor.nb_max_frames)]
        encoded = m.encode(pad_or_trim(first))
        (token, prob), *_ = m.model.detect_language(encoded)[0]
        lang = token[2:-2]
        tok = Tokenizer(m.hf_tokenizer, True, task="transcribe", language=lang)
        segs = list(m.generate_segments(features, tok, self.opts, False, encoded))
        kept = [s for s in segs if not (s.no_speech_prob > 0.6 and s.avg_logprob < -1.0)]
        return {"hyp": " ".join(s.text.strip() for s in kept if s.text.strip()), "lang": lang, "lang_prob": prob,
                "avg_logprob": min((s.avg_logprob for s in kept), default=0.0),
                "no_speech": max((s.no_speech_prob for s in kept), default=0.0),
                "compression": max((s.compression_ratio for s in kept), default=1.0)}


class Sherpa:
    """A NeMo TDT int8 export through sherpa-onnx; hotwords need modified_beam_search and the model's bpe.vocab."""

    def __init__(self, name: str, threads: int, terms: list[str] | None, score: float, tag: str) -> None:
        import sherpa_onnx

        d = D / "sherpa" / name
        kw = {}
        if terms:
            # Per run: two parallel processes must not rewrite each other's file.
            hw = D / "sherpa" / f"hotwords.{tag}.txt"
            hw.write_text("".join(f"{t}\n" for t in terms))
            kw = {"hotwords_file": str(hw), "hotwords_score": score, "modeling_unit": "bpe",
                  "bpe_vocab": str(d / "bpe.vocab")}
        self.r = sherpa_onnx.OfflineRecognizer.from_transducer(
            encoder=str(d / "encoder.int8.onnx"), decoder=str(d / "decoder.int8.onnx"), joiner=str(d / "joiner.int8.onnx"),
            tokens=str(d / "tokens.txt"), num_threads=threads, model_type="nemo_transducer", feature_dim=128,
            decoding_method="modified_beam_search", max_active_paths=4, **kw)

    def __call__(self, audio: np.ndarray) -> dict:
        s = self.r.create_stream()
        s.accept_waveform(SR, audio)
        self.r.decode_stream(s)
        return {"hyp": s.result.text, "lang": ""}


def run_tcpp(spec: str, utts: list[dict], threads: int, terms: list[str] | None, out) -> None:
    """transcribe.cpp CLI in batch mode, one utterance per run (--batch-size 1): its own per-file timings."""
    name, *extra = spec.split(":")
    gguf = next((D / "hf").glob(f"hub/models--handy-computer--*/snapshots/*/{name}"))
    paths = [D / "wav" / f"{u['id']}.wav" for u in utts]  # written by prep.py, shared by parallel runs
    lst = D / f"batch.{os.getpid()}.list"
    lst.write_text("".join(f"{p}\n" for p in paths))
    cmd = [str(D / "tcpp" / "build" / "bin" / "transcribe-cli"), "-q", "-m", str(gguf), "--threads", str(threads), "--batch", str(lst),
           "--batch-jsonl", "--batch-size", "1", *" ".join(extra).split()]
    if terms:
        cmd += ["--vocabulary", ",".join(terms)]
    t0 = time.perf_counter()
    p = subprocess.run(cmd, capture_output=True, text=True, check=False)  # rc checked below
    wall = time.perf_counter() - t0
    rows = [json.loads(x) for x in p.stdout.splitlines() if x.startswith("{")]
    by = {Path(r["file"]).stem: r for r in rows if "file" in r}
    if p.returncode or not by:
        raise SystemExit(f"tcpp failed rc={p.returncode}: {p.stderr[-800:]}")
    for u in utts:
        r = by.get(u["id"], {})
        sec = (r.get("mel_ms", 0) + r.get("encode_ms", 0) + r.get("decode_ms", 0)) / 1000
        out.write(json.dumps(u | {"hyp": r.get("text", ""), "infer": sec, "error": r.get("error")}, ensure_ascii=False) + "\n")
    print(f"tcpp wall {wall:.1f}s for {len(utts)} files; stderr tail: {p.stderr[-300:]!r}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("engine")
    ap.add_argument("set")
    ap.add_argument("glossary", choices=["none", "on"])
    ap.add_argument("--threads", type=int, default=4)
    ap.add_argument("--tag", default="")
    ap.add_argument("--hot-score", type=float, default=1.5)
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--track", type=int, default=-1)
    a = ap.parse_args()
    meta = json.loads((D / "corpus" / f"{a.set}.json").read_text())
    z = np.load(D / "corpus" / f"{a.set}.npz")
    utts = meta["utts"][: a.limit or None]
    if a.track >= 0:
        speech: dict[str, float] = {}
        for u in utts:
            speech[u["item"]] = speech.get(u["item"], 0.0) + u["dur"]
        item = sorted(speech, key=lambda i: -speech[i])[a.track]
        utts = [u for u in utts if u["item"] == item]
    sentence, terms = glossary(a.set)
    on = a.glossary == "on"
    tag = a.tag or f"{a.engine.replace('/', '_').replace(':', '_').replace(' ', '')}.{a.glossary}.t{a.threads}"
    if a.track >= 0:
        tag += f".k{a.track}"
    od = D / ("smoke" if a.limit else "out")
    od.mkdir(exist_ok=True)
    import resource

    with open(od / f"{tag}.{a.set}.jsonl", "w") as out:
        t0 = time.perf_counter()
        if a.engine.startswith("tcpp:"):
            run_tcpp(a.engine[5:], utts, a.threads, terms if on else None, out)
            rss = resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss / 1024
        else:
            if a.engine.startswith("fw:"):
                eng = FW(a.engine[3:], a.threads, sentence if on else None)
            elif a.engine.startswith("sherpa:"):
                eng = Sherpa(a.engine[7:], a.threads, terms if on else None, a.hot_score, tag)
            else:
                raise SystemExit(f"unknown engine {a.engine}")
            load = time.perf_counter() - t0
            eng(z[utts[0]["id"]])  # warm-up
            for u in utts:
                s = time.perf_counter()
                r = eng(z[u["id"]])
                out.write(json.dumps(u | r | {"infer": time.perf_counter() - s}, ensure_ascii=False) + "\n")
                out.flush()
            print(f"load {load:.1f}s")
            rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024
    (od / f"{tag}.{a.set}.meta").write_text(json.dumps({"rss_mb": rss, "wall": time.perf_counter() - t0}))
    print(f"done {tag} {a.set} rss={rss:.0f}MB wall={time.perf_counter() - t0:.0f}s", flush=True)


if __name__ == "__main__":
    main()
