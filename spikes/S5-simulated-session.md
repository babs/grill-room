# S5: a simulated grill session on the lab

**Verdict: the loop works end to end, but the lab STT is not fit for a real conversation yet.** Three synthetic voices held a 2.5 min FR/EN grill. A real daemon ran two Opus turns. The agent's syntheses, document and annotation reply track what was said closely, and the second turn resumed the session. STT has 7% WER, but it lags far behind speech on the lab node, and it posts lines under the wrong speaker after someone leaves and rejoins the audio. Both STT problems block a real session. The fix iteration solved both; the rerun at the end of this report holds AC7 on every line.

## Setup

- Voices (Piper 1.8, CPU) were chosen by the license on each model card. PM Claire speaks French with `fr_FR-siwis-medium` (CC-BY 4.0). Designer Emma speaks English with `en_GB-cori-medium` (public domain). Dev Marc starts in French on `fr_FR-mls-medium` speaker 84 (CC-BY 4.0) and switches to English on `en_US-joe-medium` (CC0). Piper has no bilingual voice, so his voice changes at the switch. `fr_FR-gilles-low` (CC0) was dropped because it has no nasal-vowel phoneme.
- The script is `s5-simulated-session/session.json`: 18 lines in two rounds, 2:02 then 0:24, with a 1.5 s overlap between Marc and Emma. `make_audio.py` writes one 48 kHz WAV per speaker and round. The 6 WAVs take 41 MB and the voice models 254 MB, none of it committed.
- `s5.spec.js` uses three Chrome instances, each with its own fake mic. Claire uploads `plan.md` and pairs a real daemon through the approval page. Round 1 plays, Emma annotates the plan, and Claire presses the agent button. Then round 2 plays and she presses it again. The mics opened within 90 ms of each other. Round 2 restarts each mic with Leave and Join audio. In the full run all three did it at once; the committed spec does it one at a time, which is how problem 2 reproduces.
- The agent model is Opus, set with `GRILL_ROOM_AGENT_MODEL=opus` on the daemon. That env var is the only way to choose it today: the room offers no model or effort choice. Before each press Claire typed one line asking the agent to batch its tool calls, to stay within the budget.

## STT (full run)

| | WER | words |
|---|---|---|
| Claire, fr | 0.04 | 131 |
| Marc, fr then en | 0.08 | 119 |
| Emma, en | 0.12 | 68 |
| fr / en | 0.07 / 0.07 | 176 / 142 |

- Every script line got a transcript (18/18). The errors are small: "colour" became "color", "I will" became "All", "rend" became "ravele". Marc's switch to English was detected on his first English line (WER 0.04).
- Overlap: Emma's interruption and Marc's line both came through, with WER 0.00 and 0.04. Each person has their own track, so talking at the same time costs nothing.
- Attribution: 30 of 32 lines landed under the right name. The 2 wrong ones are a copy of Claire's last line posted under Marc (see problem 2).
- Latency from end of speech to line was 7.7 s at best, 33.9 s median and 52.3 s max. It grows steadily through round 1, so AC7's 10 s held for the first two lines only.

## Agent (Opus, 7 real requests)

| Turn | button to synthesis | daemon turn | requests | resume | result |
|---|---|---|---|---|---|
| 1 | 18.8 s | 17.6 s | 3 | no | new agent doc `plan.md` (+284 / -9 words vs the upload), 1 thread reply |
| 2 | 15.6 s | 14.1 s | 4 | yes | `plan.md` rev 2 (+45 / -7 words) |

- Turn 1 covers every point raised: tokens instead of a second style sheet, theme passed to the server for the document frame, contrast with dark values for the red and amber lights, `localStorage`, and no flash on load. It asked Marc which style sheets hard-code colours. It also noticed Marc's garbled line: "One line of Marc's (20:05:22) was garbled in the transcript, so I left it out."
- Turn 2 picked up the round-2 decisions (feature flag, on for everyone after one week, Emma delivers the tokens first). It repeated the open questions from turn 1, which shows the resumed session remembered them. It also asked Emma to confirm the misheard "All deliver the color tokens".
- The reply to the annotation is relevant and correct: "Yes, agreed in the room. The revised plan (docs/plan.md) drops the second style sheet: colours are named tokens (CSS custom properties) shared by both themes".

## Problems

1. **STT cannot keep up on the lab node.** Each utterance costs about 6 s whatever its length: lines finish 6 to 7 s apart for both 1.4 s and 5 s utterances. My guess is Whisper's fixed 30 s input window (`beam_size` is already 1). The node has 4 cores (i7-6700) and ran at 86% CPU. Three people talking build a backlog. The queue of 8 dropped 0 utterances in the full run, but 5 in the first voice-only run and 10 in the second. S1-bis measured about 2 s per utterance on the dev machine, so this node needs resizing, as D3 anticipated.
2. **Wrong speaker after a leave and rejoin.** The worker opens an `AudioStream` per subscribed track and never closes it when the track is unsubscribed. When the worker stays in the room while people leave and rejoin the audio, `grill_stt_tracks` goes from 3 to 6. Lines then get duplicated under the wrong name: Claire's under Marc in the full run, and in a voice-only repro Emma's under Claire and Marc's twice under Marc. When the audio room empties, the worker rejoins it fresh and attribution comes out 18/18. My guess is that LiveKit hands the stale stream the next track's audio. The likely fix is to end the listener on `track_unsubscribed`.
3. **The VAD splits sentences.** The 500 ms cut turned 18 script lines into 32 transcript lines, and with a fixed cost per utterance that roughly doubles the STT load.
4. **Two documents share a name.** The agent wrote `docs/plan.md`, so the room lists the upload and the agent document both as "plan.md".
5. **The model is invisible.** No log line or UI element shows which model a turn ran on. Opus is inferred from the env var passed to `--model`.
6. **Hallucinations get posted.** On "Ça demande du travail.", the MLS voice produced 6 s of babble, the same in all 3 runs. Whisper detected `zh` and posted a line of Chinese characters. The input is a TTS artefact, but the worker posts any output, with no confidence or language filter.

## Budget and cleanup

- 7 real Claude requests, all Opus (budget 8). The three voice-only runs used none.
- The test room and both repro rooms are deleted, and the admin API lists 0 rooms. The daemon is stopped, there are no sandbox containers, networks or volumes, and the browsers are closed. The lab deployment is still running.
- Left on disk: `.scratchpad/s5/` (venv, audio, run logs, daemon log).

## Rerun after the fixes (v0.9.0)

Same script, voices and lab node, audio fed at 16 kHz. Agent turns on Opus. The session now runs the AC39 loop: the agent answers Emma's thread, Emma replies, the agent answers again and opens its own annotations, a thread gets closed, and Emma's reply reopens it.

| | S5 full run | rerun |
|---|---|---|
| WER all / fr / en | 0.07 / 0.07 / 0.07 | 0.08 / 0.07 / 0.11 |
| WER Claire / Marc / Emma | 0.04 / 0.08 / 0.12 | 0.03 / 0.11 / 0.15 |
| attribution | 30/32 | 18/18 |
| voice lines per script line | 1.78 (32 for 18) | 1.00 (18 for 18) |
| latency min / median / p95 / max | 7.7 / 33.9 / n/a / 52.3 s | 3.5 / 4.7 / 5.9 / 6.6 s |
| lines over AC7's 10 s | 16 of 18 | 0 |
| queue drops (full run, voice-only runs) | 0, 5, 10 | 0 |
| garbage lines posted / dropped | 1 (Chinese) / 0 | 0 / 0 |
| model time per utterance | about 6 s | 4.0 s mean (71.9 s for 18) |
| `grill_stt_tracks` through leave and rejoin | 3 then 6 | 3 throughout |
| turn 1 / turn 2, press to synthesis | 18.8 / 15.6 s | 21 / 26 s |
| real Claude requests | 7 | 8 |
| model shown on the syntheses | none | `claude-opus-5-5`, twice |

- **Wrong speaker.** The worker now closes a track's stream when the track goes. The real mechanism was not a stale stream fed "the next track's audio" by chance. LiveKit's SFU adds a subscribed track with `AddTrack`, which reuses an inactive transceiver. The client's receiver track for that transceiver is the old object, so a stream left open on it plays the newcomer's audio. In the Python SDK a track stream only ends when its own handle or the track's handle is dropped, never on `track_unsubscribed`. A local voice e2e reproduced it before the fix: Alice's sentence came out under Carol, who had left.
- **Capacity.** On the lab node, offline over the same audio: the old transcriber took 5.7 s per utterance. Sharing the encoder pass between language detection and decoding brought that to 3.9 s, and dropping the temperature fallback to 3.4 s. 3 threads cost 4% over 4. With 800 ms cuts and the old transcriber the median latency would still be 9.7 s, with 7 lines over 10 s. Live, beside LiveKit, a line costs 4.0 s. Batching and a smaller model were not needed.
- **Over-splitting.** At 500 ms the 0.3 s pauses Piper leaves between sentences still cut lines. 600 ms gave 22 utterances, 700 and up gave 18. 800 ms keeps a margin.
- **Garbage.** The MLS glitch on "Ça demande du travail." now lands inside the previous sentence's utterance, and Whisper drops it there. Cut on its own, as at 500 ms, it is dropped by the repetition and language checks (`stt/tests/fixtures/glitch-mls.wav`). Short answers ("Oui", "OK", "Yes", "D'accord") pass, even when Whisper detects "Oui" as English at 0.28.
- **Names and model.** The agent named its document `plan-revised.md`, as the prompt now asks. The model shows on each synthesis.
- **Annotation loop.** Turn 1 replied to Emma, closed her thread, and opened 2 annotations of its own on `plan-revised.md`. Emma's reply reopened her thread. Turn 2 answered her and left the thread open, since the colour mapping still has no owner. It closed its own rollout thread and opened one more. Emma's reply on that thread reopened it.
- **Still rough.** "And I will" is "All" again, and "dark mode" became "ART mode", which the agent questioned in turn 2. Emma's WER is 0.15 on 68 words: two lines account for it, inside run-to-run noise. The lab node CPU is the limit: about 4 s per line leaves room for one talker at a time, with overlaps queued.

Cleanup: the test room is deleted (the lab lists 0 rooms), the daemon is stopped, and no sandbox container, network or volume is left. The lab deployment runs v0.9.0.
