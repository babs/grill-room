# Spike S14: personas speak in the audio call

Asked 2026-10-15, for spec 013. Research plus a prototype behind `GRILL_ROOM_PERSONA_VOICE=on`, off by default. Not a product merge.

## Verdict

**Go for spec 013**, with Pocket TTS (Kyutai) as the engine, provided its voices pass your listening test. Pocket TTS is CPU-only, ships French and English, and its weights are CC-BY-4.0 with MIT code. It streams its first audio in about 0.1 s and runs at 0.34 of real time on 4 cores, 0.47 on a single core.

- **Runner-up: Supertonic 3.** It came out the most intelligible in the STT round trip, but it needs twice Pocket's CPU here and has 10 fixed voices. Its OpenRAIL-M licence carries use restrictions plus a clause that lets the licensor restrict usage "remotely or otherwise". Pick it only if it sounds clearly better to you.
- **Low-CPU fallback: Piper.** It costs a sixth of Pocket's CPU, but its French is the least intelligible of the four and it sounds robotic.
- **Out: Kokoro.** It has one French voice and waits 4.4 s before the first audio of a long sentence. Also out: every non-commercial model (XTTS, F5-TTS, Voxtral TTS, Fish/OpenAudio), and NeuTTS Nano French, whose licence cannot be read without accepting its gate.

The prototype runs end to end on loopback: turn result, `spoken.md`, Pocket TTS, then one LiveKit bot per persona. Two personas spoke in two voices and a human cut one off; it went quiet and repeated its sentence once he stopped. The STT worker transcribed only the human. The demo test passed 4 runs in a row on the final code, 7 in all.

The design holds, with three findings the spec must carry:

1. **The bot must subscribe to the humans' audio to see them speak.** LiveKit 1.13.9 reported only the bot itself as an active speaker until the bot subscribed.
2. **Text needs a pronunciation pass.** Every engine stumbled on `502`, `p95` and `LiveKit`. Spelling those out improved Pocket's round trip, for example English WER 0.25 to 0.19.
3. **The prompt alone does not hold every rule.** Haiku broke the one-question rule in 1 of 3 turns and miscounted its annotations in another. The server sanitises `spoken.md` before speaking it.

## Setup

- Workstation: i7-14700F. Every number below is on **4 pinned E-cores** (`taskset -c 16-19`, 4.2 GHz Gracemont, AVX2, no AVX-512), with each engine's thread pool set to 4. On 4 P-cores, Piper was 2.7x faster and Pocket 1.6x. The lab's 4-vCPU i7-6700 workers were not measured; do that before you size the pod.
- Another agent's Chrome was using up to 4.5 cores elsewhere on the box during the runs. Each engine got 3 timed runs, and the tables give the median with its range.
- Text: four sentences per language, a first-person turn summary full of jargon (webhook, 502, Kubernetes, retry, backoff, PostgreSQL, OAuth, p95, LiveKit).
- Measures: time to the first audio chunk for the short first sentence and for the longest one, RTF (synthesis time over audio time), peak RSS, and core-seconds per audio second from `getrusage`, which counts every thread.
- Intelligibility proxy: each sample went through the room's own STT, parakeet-ultra via `grill_stt.replay`, giving a WER and how many of 12 jargon words came back. This measures intelligibility, not naturalness.
- Haiku requests used: **6** (3 turns x 2 prompt versions).

## 1. Engines

### Shortlist, with sources

| Engine | Licence (checked) | FR / EN | Kept? |
|---|---|---|---|
| Piper 1.8 (OHF-Voice `piper1-gpl`) | runtime GPL-3.0 (PyPI `piper-tts` 1.8.0); each voice under its own card, see §2 | yes / yes | yes |
| Kokoro 82M v1.0 | Apache-2.0 (HF card) | 1 FR voice / 28 EN | yes, ruled out on results |
| Pocket TTS 3.3.0 (Kyutai) | weights CC-BY-4.0 (HF card, gated for voice cloning only), code MIT (GitHub) | yes / yes, plus de, it, es, pt, nl | yes |
| Supertonic 3 (Supertone) | BigScience OpenRAIL-M (model LICENSE): commercial use allowed, use restrictions, licensor may restrict usage "remotely or otherwise" | yes / yes, 31 languages | yes |
| Chatterbox Multilingual | MIT | yes / yes | no: 0.5B, GPU-oriented, no CPU real-time claim |
| Qwen3-TTS 0.6B | Apache-2.0 | yes / yes | no: "GPU preferred", 0.6B on 4 cores |
| NeuTTS Nano French | "NeuTTS Open License 1.0", gated, unreadable before accepting | FR | no: licence unknown. NeuTTS Air (Apache-2.0) is English only |
| XTTS-v2 (Coqui) | Coqui Public Model License: non-commercial | yes / yes | excluded |
| F5-TTS | CC-BY-NC-4.0 | yes / yes | excluded |
| Voxtral TTS 4B (Mistral) | CC-BY-NC-4.0 | yes / yes | excluded |
| Fish Speech / OpenAudio | CC-BY-NC-SA | yes / yes | excluded |

The non-commercial models are out because their licences forbid commercial use, and a room server that anyone may self-host, including a company, cannot depend on that.

Piper and Kokoro both use espeak-ng (GPL-3.0) for phonemes. Self-hosting is fine under the GPL. Shipping the library inside a public image means offering its source too.

### Speed and memory (4 E-cores, 4 threads, median of 3)

| Engine | Lang | Voice | First audio, short sentence | First audio, longest sentence | RTF (range) | Peak RSS |
|---|---|---|---|---|---|---|
| Piper | fr | siwis | 0.07 s | 0.48 s | 0.048 (0.048-0.053) | 333 MB |
| Piper | en | bryce | 0.10 s | 0.55 s | 0.048 (0.048-0.051) | 366 MB |
| Kokoro fp32 | fr | ff_siwis | 0.74 s | 4.86 s | 0.455 | 668 MB |
| Kokoro int8 | en | af_heart | 0.62 s | 4.14 s | 0.411 | 428 MB |
| **Pocket TTS** | fr | estelle | **0.09 s** | **0.11 s** | 0.338 (0.336-0.384) | 1129 MB |
| **Pocket TTS** | en | alba | **0.08 s** | **0.13 s** | 0.339 (0.338-0.365) | 1131 MB |
| Supertonic 3 | fr | F1 | 1.90 s | 5.19 s | 0.510 | 528 MB |
| Supertonic 3 | en | F1 | 2.19 s | 5.19 s | 0.525 | 527 MB |
| Pocket TTS 24-layer | fr | estelle | 0.24 s | 0.37 s | 1.09: slower than real time | 2.6 GB |

Pocket streams within a sentence, so its first audio does not grow with sentence length. The others synthesise a whole sentence first. Supertonic pays about 1.5 s per call whatever the length; its 8 flow steps took 0.07 to 0.36 s each here.

CPU per audio second, all threads, single runs:

| Engine | 1 thread | 2 threads | 4 threads |
|---|---|---|---|
| Piper (fr) | 0.13 core-s (RTF 0.13) | 0.15 (RTF 0.07) | 0.20 (RTF 0.05) |
| Pocket TTS (fr) | 0.73 core-s (RTF 0.47) | 1.10 (RTF 0.34) | 1.05 (RTF 0.35) |
| Kokoro int8 (fr) | | | 1.67 (RTF 0.42) |
| Supertonic 3 (fr) | | | 2.04 (RTF 0.51) |

### Intelligibility: STT round trip

Mean over the sampled voices. WER is against the text read; jargon is out of 12.

| Engine | FR WER | FR jargon | EN WER | EN jargon |
|---|---|---|---|---|
| Piper (7 FR, 5 EN voices) | 0.29 (0.25-0.33) | 4.0 | 0.19 (0.13-0.27) | 7.2 |
| Kokoro (FR: ff_siwis only) | 0.25 | 5.0 | 0.15 (0.13-0.18) | 9.5 |
| Pocket TTS (7 FR, 6 EN) | 0.22 (0.19-0.26) | 3.7 | 0.21 (0.16-0.25) | 5.5 |
| Supertonic 3 (5 + 5) | 0.19 (0.14-0.23) | 4.6 | 0.12 (0.10-0.15) | 10.2 |

Kokoro's English voices reading French score WER 0.49 and 0.71, so it has one usable French voice.

What went wrong is mostly jargon. Pocket said "52" for 502, "pain dive" for p95 and "Livit" for LiveKit. Piper turned "PostgreSQL cluster" into "crusté poster skull". With numbers and names spelled the way people say them ("cinq cent deux", "p quatre-vingt-quinze", "Live Kit"), the scores became:

| Sample | Written as is | Spelled out |
|---|---|---|
| Pocket fr estelle | 0.23, 5/12 | 0.20, 5/12 |
| Pocket en alba | 0.25, 7/12 | 0.19, 8/12 |
| Supertonic fr M2 | 0.14, 7/12 | 0.16, 6/12 |
| Piper fr siwis | 0.28, 4/12 | 0.28, 5/12 |

Pocket samples at random, so these single runs move by a few points from one run to the next. The direction still argues for a pronunciation step: numbers to words in the line's language, plus a per-room lexicon. The room glossary from spec 007 is the natural place for it.

### Naturalness

I cannot judge it by ear. The samples are in the folder handed over with this report (not committed), with an `index.md` that links each file to its engine, voice, licence, RTF and WER. Start with the demo, then `fr-pocket-estelle`, `fr-supertonic-M2`, `fr-piper-fr_FR-siwis-medium` and `fr-kokoro-ff_siwis`, then the English set.

## 2. One voice per persona

### What each engine offers under an acceptable licence

| Engine | French | English |
|---|---|---|
| Piper | 4 models: siwis (F, CC-BY), upmc jessica and pierre (F and M, CC-BY-SA), mls (125 LibriVox speakers, CC-BY, uneven quality), tom (M, AGPL-3.0, avoid). Siwis and upmc start from the Lessac voice, whose Blizzard terms are none of CC0, CC-BY or public domain (`scenarios/VOICES.md`). About 4 to 6 good, distinct voices | 10+ clean single-speaker voices (cori, bryce, norman, ljspeech, john and kristin are public domain; joe, mike and reza CC0) plus libritts_r (904 speakers, CC-BY) |
| Kokoro | 1 (ff_siwis) | about 6 graded B- or better (af_heart, af_bella, af_nicole, bf_emma, ...) |
| Pocket TTS | 25 catalogue voices run on the French model: 27 minus cosette (Expresso) and jean (EARS), both non-commercial. Licences: CC0 (estelle, marius, javert, giovanni, lola), CC-BY-4.0 (alba, the VCTK set, daan), public domain (the LibriVox set). Only estelle is a French speaker; the others carry their own timbre into French. Accepting the model's gate unlocks cloning: 35 French CML-TTS speakers (CC-BY) and the CC0 voice donations | the same 25 |
| Supertonic 3 | 10 fixed styles (F1-F5, M1-M5), the same in every language. New voices need the paid Voice Builder | the same 10 |

Pocket's catalogue embeddings come from an ungated repo, so no account is needed. Cloning from a WAV needs the gated weights, which means accepting Kyutai's terms on Hugging Face. **That acceptance is your decision.** The terms forbid impersonation without consent, among other uses.

### How a persona gets its voice

- A **persona setting `voice`**, empty by default. Empty means **by order**: the persona's rank in the room, by id, picks a slot, and each slot pairs one French voice with one English voice of the same type. The prototype does this (`Speech.Slot`, `VOICES` in `voicebot.py`). A persona added later takes the next slot, so nobody's voice changes when another persona joins.
- A room setting can override that per persona later. Two personas never share a voice while slots remain.

### Language

**Follow the text, not the persona.** Every persona already writes in the room's language (its text says so), and `spoken.md` is in the language of its synthesis. The engine needs only to know which language model to load. The prototype guesses French or English from stopwords, and the server's fallback summary does the same. A room that switches language mid-session keeps each persona's character, because the slot pairs a voice for each language.

## 3. What is spoken

**Spoken: only `spoken.md`, which the persona writes in the same turn.** It holds the gist of its chat message and its actions, in the first person, in 60 words. When it writes none, the server builds a summary from the turn's report: "I changed the payment spec. I opened 2 notes. I answered a note." The documents are never read out. The raw synthesis is not read either: a 300-word Markdown synthesis would hold the floor for two minutes. Open question 1 asks whether the synthesis should be read when it is short and plain.

No extra model call is needed: `spoken.md` is a new workspace output like `replies.json`. The daemon sends its content because `turn.NeedsContent` now lists it.

The server makes it safe to speak whatever the agent wrote (`spokenText`): it removes Markdown marks, link targets, bare URLs and short ids (`Alice ·a1b2` becomes `Alice`, `#14` becomes `14`), then cuts at the end of the sentence that reaches 80 words. Tests cover six input shapes, and removing either the id or the mark step turns them red.

### Prompt addition (final, v3)

It goes into the workspace list of the prompt only when the flag is on. Source: `spikes/s14-persona-voice/prompt/spoken-v3.md`.

```
- The room may be on an audio call. Write spoken.md: what you say out loud in the call after this turn. A speech synthesizer reads it in your voice, word for word, so write it the way you would say it.
  - Say what you did, in the first person: "J'ai commenté la section budget." "I changed the spec. Retries now stop after three tries." When you answered someone, say whose point it was and what you answered: "About Alice's note on the budget, I replied that the figure is monthly."
  - Say only what your files and your synthesis say. Give a number only when you wrote it.
  - One question at most, at the end, addressed by name to the person who can answer it.
  - 60 words at most, in the language of your synthesis. Short sentences, most under 15 words, ended by full stops. No colons, dashes or brackets.
  - Plain words only. No Markdown, list, link, file name, path, number sign, id, code, quotation marks or emoji. Write symbols and units as words: "300 euros a month", not "300 €/month". Say "the payment spec", not "docs/spec-paiement.md", and "Alice", not "Alice ·a1b2".
  - Never read a document or your synthesis out. Say what changed and why it matters.
  - No greeting, thanks, recap phrase or filler.
  - Nothing worth saying aloud: leave spoken.md out.
```

Rules applied, from the no-slop bar: no opener, closer, thanks or recap phrase. Short sentences that end in a full stop, because a speech synthesiser reads a colon or a dash as a stumble. Nothing that cannot be said aloud (Markdown, paths, ids, URLs, emoji, quotes). Names said the way people say them. At most one question, to a named person. Facts only from the turn.

### Haiku test: 3 realistic turns, 6 requests

The turns are in `prompt/turns.json`: Grill on a Stripe webhook spec (French), Secretary on a retro (English), Reviewer on an English RFC in a French room. Each run used `claude -p --model haiku`, the persona text as the appended system prompt, no tools and no user settings. The debug log loaded 0 user skills, so the user's own rules did not reach the model. `prompt_test.py` checks each output mechanically.

**v1** (`spoken-v1.md`, the requested shape with a 60-word cap):

| Turn | Output | Problems found |
|---|---|---|
| Grill, fr | J'ai ajouté une section sur l'idempotence du webhook : Stripe ne garantit pas l'unicité des événements, donc on élimine les doublons. J'ai aussi répondu à Alice : les 300 euros par mois couvrent les trois environnements. Question : que fait-on si la commande est annulée avant l'arrivée du webhook ? | 3 colons used as connectors; "Question :" as a label; sentences of 21 words |
| Secretary, en | I recorded that releases move to Thursdays and the staging freeze is dropped, and I ticked the "What went wrong" item. I also noted Raj's rollback runbook due on the 24th and Dana's post-mortem. Lee, did you take the PagerDuty rota check? I marked it to confirm. | quotation marks; a 21-word sentence |
| Reviewer, fr | J'ai annoté la section 4 de la RFC. Pour les refresh tokens, j'ai proposé une rotation à chaque usage : la session reste ouverte pour le mobile. J'ai aussi signalé le scope admin trop large, PKCE optionnel et le stockage local. Bob, la rotation te convient ? Quel délai pour révoquer un jeton ? | 2 questions; a colon; "stockage local" loses the meaning of localStorage |

v1 was clean on slop (no filler, no greeting, nothing unspeakable beyond the above) and kept the first person. Its faults are the ones a listener notices: connector colons, quotes, long sentences, two questions.

**v2** (`spoken-v2.md`: sentences of 15 words or fewer, no colons, dashes, brackets or quotes, symbols as words, one question at most, the user's "About Alice's note" pattern):

| Turn | Output | Problems found |
|---|---|---|
| Grill, fr | J'ai ajouté l'idempotence à la spec de paiement. Stripe ne garantit pas l'unicité des événements. On dédoublonne sur l'identifiant de l'événement, gardé sept jours. Sur la note d'Alice, les trois cents euros sont le total mensuel. La production seule en prend cent quatre-vingts. Que se passe-t-il si le webhook arrive sur une commande annulée ? Alice, tu tranches ? | **2 questions**; exactly 60 words; drops "j'ai répondu que" |
| Secretary, en | I updated the minutes and ticked the What went wrong item on the agenda. Releases move to Thursdays, as Dana said. The staging freeze is dropped, as Raj said. Raj writes the rollback runbook by October 24. Dana books the post mortem. Lee, can you confirm you will check the PagerDuty rota? | none |
| Reviewer, fr | Marie, j'ai relu la section quatre de la RFC. J'ai posé cinq annotations. Un jeton volé resterait valide sans limite. Bob, j'ai répondu sur le mobile. Une rotation à chaque usage garde la session ouverte. Elle coupe aussi un jeton volé dès sa deuxième utilisation. Bob, quel délai est prévu pour la révocation ? | **"cinq annotations": it opened 4**; reads choppy |

v2 fixed every formatting fault: 0 colons, quotes or Markdown across the 3 turns, and the longest sentence fell to 14 words. It introduced a staccato rhythm and kept two content faults: a second question, and a wrong count. v3 relaxes the sentence rule to "most under 15 words", restates "one question at most, at the end", and adds "say only what your files and your synthesis say; give a number only when you wrote it". **v3 is untested: the 6-request budget was spent.** The first real turns on the lab will test it, and the server's sanitiser is the safety net for format, not for facts.

## 4. Call behaviour

| Need | Design (prototype status) |
|---|---|
| One bot per persona | The voice service joins the room's LiveKit room as `persona-<id>`, with the persona's name and participant kind `agent`, allowed to publish the microphone source only. It joins when the persona has a line and leaves after 60 s idle. **Built.** |
| Silent while a human speaks | LiveKit active speakers. The bot pauses within one 20 ms frame of the update, clears its buffered audio, waits for 0.8 s of quiet, then **says the interrupted sentence again**. Past 8 s of interruption it drops the rest of the line; a line not started within 90 s is dropped as stale. **Built**; the resume path ran in the demo, the drop paths did not. |
| Detection latency | Bob's speech began about 0.25 s before the bot logged `human_speaking`. He stopped at about +3.6 s, and the bot resumed 4.4 to 4.8 s after the interruption. |
| **Finding: speaker updates need a subscription** | With `auto_subscribe=False`, LiveKit 1.13.9 sent the bot active-speaker updates that listed only itself, never Bob. With the humans' tracks subscribed, Bob appeared within 0.25 s. The spec must subscribe one listener per room to the humans' audio (bandwidth only; no decoding is needed). |
| One persona at a time | A FIFO per room. Secretary's line waited 16 to 26 s behind Grill's and started 0.1 to 0.2 s after Grill's ended. **Built and asserted.** |
| Per-listener mute | The audio menu lists "Hear <persona>" for each bot in the call. Unticking sets that participant's volume to 0, kept per room in `localStorage`. **Built**; the demo asserts the `<audio>` volume. Unsubscribing would also save the bandwidth; that is a spec choice. |
| STT ignores persona tracks | **Already true, by accident.** The worker parses the LiveKit identity as a participant id, so `persona-6` fails, logs `track_ignored` and is skipped. It still subscribes first, and logs a warning per track. The spec should make it skip `kind == agent` before subscribing, and leave a room where only bots remain. |
| Device mode (spec 010) | It transcribes the local microphone track only (010-AC9), so a persona's track never reaches it. Acoustic echo is the AEC's job, as for any remote participant. |
| People list | Persona identities match no participant row, so personas do not appear as people. The audio menu's "N voices are being written down" counted the 2 humans only (screenshot checked). A speaking indicator on the persona's button is left to the spec. |
| Volume | Pocket voices come out at different loudness (marius about a third of estelle). The bot levels each sentence to about -22 dBFS, with a peak cap. **Built.** |

### Echo

The claim holds in Chrome, from sources; I could not test it here. Chrome's AEC3 takes as its reference the audio the page plays through media elements, and LiveKit's `track.attach()` plays remote tracks through an `<audio>` element. Audio played through WebAudio is not in the reference ([atmoky known issues](https://docs.atmoky.com/known-issues)). A persona track is an ordinary remote WebRTC track, so it is cancelled exactly as a human's is. Firefox and Safari were not checked.

**The fake-mic setup cannot test echo.** Chrome's fake device plays a file into the capture, with no acoustic path from the speakers back to the mic. A real test needs a PipeWire null sink whose monitor feeds a virtual source, with Chrome playing to the first and capturing from the second. The test would then compare the outgoing track's level while a persona speaks, with `echoCancellation` on and off. That loads modules into your audio session (reversible with `pactl unload-module`), so it waits for your go-ahead.

## 5. Prototype

Behind `GRILL_ROOM_PERSONA_VOICE=on`, off by default. The flag needs LiveKit configured.

| Piece | Change |
|---|---|
| `internal/turn` | `SpokenFile`, `NeedsContent("spoken.md")`, `Outcome.Spoken` (4 KB cap, UTF-8). Test `TestSpokenFile` |
| `internal/web/speech.go` | in-memory queue of 200 lines; `GET /api/stt/speech?after=<seq>` (STT token) answers `{items, last}`, so a bot notices a server restart; `spokenText` sanitiser; `turnSummary` fallback (fr/en). Tests `TestSpokenText`, `TestTurnSummary`, `TestSpeechQueue`, `TestPromptAsksForSpokenOnlyWithTheFlag`, `TestSpeechFromTurns` |
| `internal/web/workspace.go`, `turns.go` | the prompt rules when the flag is on; queue the line after `CompleteTurn` |
| `cmd/grill-room` | reads `GRILL_ROOM_PERSONA_VOICE` |
| `internal/web/ui/room.js` | "Hear <persona>" toggles in the audio menu |
| README, `docs/agent-workspace.md` | the route, the variable, the `spoken.md` row |
| `spikes/s14-persona-voice/voicebot.py` | the bot: polls, synthesises with any `bench.py` engine, publishes at 48 kHz, queues, pauses, levels |

### Demo, run on loopback

Stack: the worktree's server with the flag on, an isolated LiveKit and STT worker (containers `grill-room-test-s14-*`), and the bot running Pocket TTS on 2 threads. Then `demo.spec.js`:

1. It creates a room and adds Secretary, then pairs a fake host daemon that answers each turn with a canned `spoken.md` through the real result route. It asserts the job's prompt contains `Write spoken.md`.
2. Alice (silent fake mic) joins the audio. Grill and then Secretary are pressed.
3. When Grill's bot is speaking in Alice's call, Bob joins with a fake mic that plays French speech 2 s later.
4. Asserted: the bot logged `interrupted` then `resumed` for Grill; Secretary started only after Grill finished. Then the "Hear Grill" toggle set Grill's audio to volume 0. Every voice line in the chat is Bob's, and none carries a persona's words.

Result: **passed 4 runs in a row on the final code**, 7 in all. The 2 failures between them were the bot crashing on the new response shape and a broken room page, see the self-check. The server log shows `speech_queued` with source `spoken`. The STT worker logged `track_ignored` for each `persona-*` track and one `line_done` for Bob. The recording, `demo-two-personas-human-interrupts.wav`, is what Alice heard: Grill (estelle, French), Bob cutting in, Grill repeating its sentence, then Secretary (marius, English).

## 6. Costs and placement

**CPU per spoken minute** (E-cores; P-cores are 1.6x to 2.7x faster):

| Engine | 1 thread | 4 threads |
|---|---|---|
| Piper | 8 core-s | 12 core-s |
| Pocket TTS | 44 core-s | 63 core-s |
| Kokoro int8 | | 100 core-s |
| Supertonic 3 | | 122 core-s |

A 60-word line is about 20 s of audio, so it costs about 15 core-seconds on Pocket with one thread.

**Where TTS runs:**

| Option | For | Against |
|---|---|---|
| In the STT pod | already in every LiveKit room, already holds the keys and the token | STT is sized to 4 CPUs for about four people talking at once (S10d); a speaking persona would take a core from it at the worst moment |
| **A new voice pod (recommended)** | its own CPU budget and scaling; same trust as the STT worker (LiveKit keys, STT-class token); can share the STT image's base | one more deployment |
| The Go server | no new pod | no Go TTS: cgo onnxruntime or a subprocess; the server is a single SQLite replica and should stay light |

**Sizing for Pocket:** one process holds one model per language, whatever the number of voices. Both languages took 1.4 GB RSS on the bot; two French voices took 1.0 GB. One room speaks at a time, so the need is one core per room speaking at once: RTF 0.47 on one thread leaves twice real time. Start the pod at 2 CPU and 2.5 GiB, which covers two rooms talking at once, and measure on a lab node first. Piper fits in 0.5 CPU and 512 MiB for the same load.

## Draft acceptance criteria for spec 013

- **AC1** `GRILL_ROOM_PERSONA_VOICE=on` (off by default, needs LiveKit) makes the prompt ask for `spoken.md` with the rules of S14 v3. Off, neither the prompt nor the routes change.
- **AC2** The daemon sends `spoken.md` content. The server keeps at most 4 KB of UTF-8 and sanitises it: no Markdown, link, URL or short id is spoken, and the line is cut at a sentence end by 80 words. A turn without `spoken.md` speaks a first-person summary of its revisions, new documents, annotations and replies, in the synthesis's language, or nothing when it changed nothing. The raw synthesis is never spoken (see open question 1).
- **AC3** A pronunciation step runs before synthesis: numbers become words in the line's language, and the room glossary (spec 007) can carry a spoken form per term.
- **AC4** Each persona speaks as its own LiveKit participant `persona-<id>`, kind `agent`, named after the persona, allowed to publish microphone audio only. It joins when it has a line and leaves after 60 s of silence.
- **AC5** A persona's voice is its `voice` setting, or by default the slot of its rank in the room. A slot pairs a French and an English voice. No two personas of a room share a slot while slots remain. Only voices whose licence is CC0, CC-BY, CC-BY-SA or public domain ship, listed with their source as in `scenarios/VOICES.md`.
- **AC6** The language of a line follows its text, not the persona.
- **AC7** One persona speaks at a time per room, in FIFO order. A line not started within 90 s is dropped and logged. A line is spoken only while at least one human is in the call; otherwise it is dropped, so a bot never opens a LiveKit room by itself (the prototype does not check this).
- **AC8** While a human is an active speaker, the persona is silent within 300 ms. It says the interrupted sentence again after 0.8 s of quiet, or drops the rest of the line after 8 s of interruption. The voice service subscribes to the humans' audio in each room it speaks in, as it must to see them speak (S14).
- **AC9** The audio menu lists "Hear <persona>" for each persona in the call. Unticking it silences that persona for this participant in this room only, and the choice survives a reload.
- **AC10** The STT worker skips participants of kind `agent` before subscribing and leaves a room where only agents remain. Device mode is unchanged.
- **AC11** Levels: each sentence is levelled to about -22 dBFS.
- **AC12** Echo: with a persona speaking and a human silent, the human's outgoing track stays under a threshold to be set, in Chrome, on a loopback echo path (the PipeWire test above).
- **AC13** Metrics: lines queued, spoken, interrupted, dropped (with reason), synthesis seconds, and queue wait.
- **AC14** The voice service is its own deployment with a CPU and memory budget from a lab measurement. It speaks every room's lines in real time with one room speaking per core.

## Open questions for you

1. Should the chat text be spoken when it is short and plain (say under 60 words with no Markdown), or always only `spoken.md` as the prototype does?
2. Pocket's voice cloning needs Kyutai's gated weights, which means accepting their terms on Hugging Face. Should we accept them for the 35 French CML-TTS voices, or stay on the 25 ungated catalogue voices (one native French)?
3. Engine: your ear decides between Pocket (recommended), Supertonic 3 and Piper.
4. May I run the PipeWire echo test (AC12) on this workstation? It loads two modules into your audio session, removed afterwards with `pactl unload-module`.

## Reproduce

Everything runs from `spikes/s14-persona-voice/`. Python 3.13, because kokoro-onnx 0.6.1 caps Python below 3.14. `uv sync` installs the CPU torch build through the pytorch-cpu index.

```sh
# Models: Piper voices and Kokoro files under $S14_MODELS (piper/, kokoro/); Pocket TTS and Supertonic
# download from Hugging Face on first use.
S14_MODELS=<models dir> ./run_bench.sh <samples dir> <results.jsonl>        # 3 timed runs + one WAV per voice
.venv/bin/python summarize.py <results.jsonl>                                # median table
# STT round trip: 16 kHz copies + a manifest, then the worker's replay in its image
docker run --rm -v <dir>:/data --entrypoint python grill-stt:local -m grill_stt.replay /data/manifest.json /data/out.jsonl --set s14 --talkers 4
.venv/bin/python asr_wer.py <dir>/out.jsonl
./prompt_test.py spoken-v3.md --out <scratch>/results.jsonl                 # 3 haiku requests
# Demo: LiveKit on 127.0.0.1:7890, the server on 127.0.0.1:7695 with GRILL_ROOM_PERSONA_VOICE=on and
# GRILL_ROOM_STT_TOKEN, an STT worker on the same LiveKit, then:
./run_bot.sh <demo.env> <bot.log> pocket
./run_demo.sh <demo dir>          # demo.env (LK_KEY, LK_SECRET, STT_TOKEN, ADMIN_TOKEN), silent.wav, human.wav, bot.log
```

Raw results: `results/bench.jsonl`, `results/cpu.jsonl`, `results/wer.md`, `results/wer-said.md`, `prompt/haiku-results.jsonl`.

## Sources

- Kokoro: [model card](https://huggingface.co/hexgrad/Kokoro-82M), [VOICES.md](https://huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md)
- Pocket TTS: [GitHub README](https://github.com/kyutai-labs/pocket-tts), [model card](https://huggingface.co/kyutai/pocket-tts), [voices and their licences](https://huggingface.co/kyutai/tts-voices)
- Supertonic 3: [model card and LICENSE](https://huggingface.co/Supertone/supertonic-3)
- Piper: [voices](https://huggingface.co/rhasspy/piper-voices) (each `MODEL_CARD`), [piper-tts on PyPI](https://pypi.org/project/piper-tts/)
- Non-commercial: [XTTS-v2](https://huggingface.co/coqui/XTTS-v2), [F5-TTS](https://huggingface.co/SWivid/F5-TTS), [Voxtral TTS](https://huggingface.co/mistralai/Voxtral-4B-TTS-2603)
- Others: [Chatterbox](https://huggingface.co/ResembleAI/chatterbox), [Qwen3-TTS](https://huggingface.co/Qwen/Qwen3-TTS-12Hz-0.6B-Base), [NeuTTS](https://github.com/neuphonic/neutts)
- Chrome AEC and media elements: [atmoky known issues](https://docs.atmoky.com/known-issues), [Chrome: more native echo cancellation](https://developer.chrome.com/blog/more-native-echo-cancellation/)

## Self-check

- The room page broke, and the browser caught it. A scripted edit to `room.js` deleted `voiceParticipant()`, which left the room page with no personas and no people. `node --check` and the UI type check both passed; a page load in Chrome showed the `ReferenceError`. Fixed, then the demo passed 4 runs in a row.
- Two bugs found by running for real: the speech route answered `null` for an empty queue, and a restarted server reset the bot's sequence numbers. Both fixed; the second was checked live by restarting the server between two runs.
- Not done: the lab measurement, the echo test, the drop paths (90 s stale, 8 s interruption) in a live run, and v3 of the prompt on a model.
- Review (my-review) found that the word cap did not hold on unpunctuated text: 600 words passed. It also found that the README missed `last`, and that the bot loaded one Pocket model per voice. All three are fixed, the first with a test that fails without the fix.
