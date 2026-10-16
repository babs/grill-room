#!/usr/bin/env python3
"""Spike S14b: does a French voice and an English voice sound like one person?

Cosine similarity of ECAPA speaker embeddings (speechbrain/spkrec-ecapa-voxceleb, Apache-2.0).
Same speaker usually scores above 0.5 to 0.6, different speakers of one sex around 0.2 to 0.4.
Runs in envs/chatterbox, which has torch 2.6 and speechbrain.

usage: timbre.py <a.wav> <b.wav> [<a.wav> <b.wav> ...]   prints one JSON line per pair
       timbre.py --rank <ref.wav> <candidate.wav>...      candidates by similarity to ref
"""

import json
import sys

import soundfile as sf
import torch
import torchaudio
from speechbrain.inference.speaker import EncoderClassifier

ENC = EncoderClassifier.from_hparams(source="speechbrain/spkrec-ecapa-voxceleb", run_opts={"device": "cpu"})


def emb(path: str) -> torch.Tensor:
    data, sr = sf.read(path, dtype="float32", always_2d=True)
    wav = torchaudio.functional.resample(torch.from_numpy(data.mean(axis=1))[None], sr, 16000)
    return ENC.encode_batch(wav).squeeze()


def cos(a: str, b: str) -> float:
    return round(float(torch.nn.functional.cosine_similarity(emb(a), emb(b), dim=0)), 3)


def main() -> None:
    args = sys.argv[1:]
    if args[0] == "--rank":
        ref = args[1]
        for c, s in sorted(((c, cos(ref, c)) for c in args[2:]), key=lambda x: -x[1]):
            print(json.dumps({"ref": ref, "cand": c, "cos": s}))
        return
    for a, b in zip(args[::2], args[1::2], strict=True):
        print(json.dumps({"a": a, "b": b, "cos": cos(a, b)}))


if __name__ == "__main__":
    main()
