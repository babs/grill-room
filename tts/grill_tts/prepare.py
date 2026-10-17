#!/usr/bin/env python3
"""Build step, offline after fetch.py: what the pod would otherwise compute at every start.

- the voices Pocket ships no state for (the CML-TTS readers), encoded once per language model;
- each voice's gain, from a render of its sample sentence (013-AC15);
- that render as the voice's sample, Opus, which the pod hands to the room server (013-AC27).

    prepare.py <models dir>
"""

import json
import sys
import threading
from pathlib import Path

import numpy as np
import soundfile as sf

from .audio import gain_for, level
from .engine import POCKET_LANG, Pocket, catalogue
from .fetch import split

SAMPLE = {
    "fr": "Bonjour, voici ma voix. Je dirai dans l'appel ce que j'ai fait, en quelques phrases.",
    "en": "Hello, this is my voice. I will say in the call what I did, in a few sentences.",
}


def main() -> None:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    from huggingface_hub import hf_hub_download
    from pocket_tts import TTSModel, export_model_state

    out = Path(sys.argv[1])
    for lang, name in POCKET_LANG.items():
        model = TTSModel.load_model(language=name)
        (out / "voices" / lang).mkdir(parents=True, exist_ok=True)
        for v in catalogue():
            if v.get("pocket"):
                continue
            repo, path, rev = split("hf://" + v["source"])
            state = model.get_state_for_audio_prompt(
                Path(hf_hub_download(repo_id=repo, filename=path, revision=rev))
            )
            export_model_state(state, out / "voices" / lang / f"{v['id']}.safetensors")
            print(f"encoded {v['id']}/{lang}", flush=True)
        del model
    engine = Pocket(out, threads=2)
    gains: dict[str, float] = {}
    (out / "samples").mkdir(exist_ok=True)
    for v in catalogue():
        for lang in POCKET_LANG:
            audio = np.concatenate(list(engine.stream(v["id"], lang, SAMPLE[lang], threading.Event())))
            g = gains[f"{v['id']}/{lang}"] = round(gain_for(audio), 4)
            sf.write(
                out / "samples" / f"{v['id']}-{lang}.ogg",
                level(audio, g),
                engine.sample_rate,
                format="OGG",
                subtype="OPUS",
            )
            print(f"sampled {v['id']}/{lang} gain {g}", flush=True)
    (out / "voices" / "gains.json").write_text(json.dumps(dict(sorted(gains.items())), indent=2) + "\n")


if __name__ == "__main__":
    main()
