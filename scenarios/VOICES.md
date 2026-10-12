# Piper voices

`grill-scenario` runs a voice only if this table lists it under CC0, CC-BY or public domain (004-AC18). It reads the first two columns, so keep the voice name in backticks and the license alone in its cell. The license is the one on the voice's model card, for the dataset it was trained on.

| Voice | License | Dataset | Note |
|---|---|---|---|
| `fr_FR-siwis-medium` | CC-BY-4.0 | SIWIS French Speech Synthesis Database | Fine-tuned from `en_US-lessac-medium` |
| `fr_FR-mls-medium` | CC-BY-4.0 | Multilingual LibriSpeech, OpenSLR 94 | 125 speakers; scenarios use speaker 84 |
| `en_US-joe-medium` | CC0 | OHF-Voice voice-datasets | Fine-tuned from `en_US-lessac-medium` |
| `en_GB-cori-medium` | public domain | LibriVox recordings | |

Model cards: `https://huggingface.co/rhasspy/piper-voices/blob/main/<lang>/<locale>/<name>/<quality>/MODEL_CARD`.

Two of these voices start from `en_US-lessac-medium`, whose card points to a license of its own, the Blizzard 2013 Lessac terms, which is none of the three allowed. Their own cards name only their fine-tuning data, and the gate checks the card, so it lets them through. Read the Lessac terms before shipping audio made with them.

`fr_FR-gilles-low` (CC0) is left out on purpose: it cannot say French nasal vowels (spike S5).
