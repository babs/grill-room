import json

import pytest

from grill_tts.engine import catalogue
from grill_tts.fetch import LOCK, key, split, wanted


# 013-AC10: models.lock.json pins exactly the files the pod loads, each by revision and sha256: a
# voice added to the catalogue without a pin, or a pin nothing loads, fails here before any build.
def test_lock_covers_what_the_pod_loads() -> None:
    lock = json.loads(LOCK.read_text())
    keys = {key(*f) for f in wanted()}
    assert keys == set(lock)
    assert all(len(h) == 64 and int(h, 16) >= 0 for h in lock.values())
    for v in catalogue():
        if v.get("pocket"):
            assert any(f"/embeddings/{v['pocket']}.safetensors@" in k for k in keys), v["id"]
        else:
            assert v["source"] in keys, v["id"]
    assert sum("/model.safetensors@" in k and k.startswith("kyutai/pocket-tts/") for k in keys) == 2


def test_split() -> None:
    assert split("hf://kyutai/tts-voices/cml-tts/fr/a.wav@abc") == (
        "kyutai/tts-voices",
        "cml-tts/fr/a.wav",
        "abc",
    )
    for bad in ("hf://kyutai/tts-voices/a.wav", "hf://@abc"):
        with pytest.raises(ValueError):
            split(bad)
