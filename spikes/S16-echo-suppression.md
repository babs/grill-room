# S16: server-side suppression of a persona's own echo

With a listener on speakers, Chrome's echo canceller sometimes lets the persona's voice back into the listener's microphone. The tts pod's onset detector (`Onset` in `tts/grill_tts/audio.py`, fed by `Bots._listen`) takes it for a human cutting in, and the persona stutters. The question: is there a simple gate in the tts pod that removes most of these cuts and keeps 013-AC22 (stop within 300 ms of a real barge-in)? Ran on 2026-10-18 on a throwaway branch, `spike/echo-suppress`, not pushed.

Tags: **[M]** measured live, **[O]** offline replay of recorded audio, **[U]** unverified.

## 1. Verdict

**Go, for a narrow fix.** It takes about 20 lines and no new dependency, and it only covers the common case.

- **Most self-echo cuts are a warm-up leak.** Apart from the mid-call full leaks below, every echo cut came 1.0-3.1 s into the first persona audio the listener's track heard. That's Chrome's echo canceller converging. The leak is a burst of 40-80 ms at -15 to -30 dBFS: as loud as real speech, but shorter [M, O].
- **Recommended: a stricter onset on a listener's track during its first 4 s of persona audio.** While a persona plays (any frame out within the last 1.5 s) and the track has heard under 4 s of it, an onset needs 9 loud 10 ms frames in the last 16 instead of 4 in 6. After the warm-up the detector is unchanged.
- **Result, echo.spec.js with echo cancellation on:** runs with a self-echo cut went from 5 of 12 to 2 of 12. Uncapped went from 4 of 6 to 0 of 6, and at 1 CPU from 1 of 6 to 2 of 6. One of the two failures at 1 CPU is a different failure mode (see the next point) [M].
- **AC22 holds.** The turn-taking spec stayed green 3 of 3, with a p95 stop of 120, 150 and 140 ms from heard. The baseline was 130 and 122 ms [M].
- **What it does not fix: a full mid-call leak under CPU starvation.** Twice at 1 CPU, Chrome's canceller collapsed mid-call. The listener track then carried the persona at -9 to -18 dBFS median, and the cuts repeated on every resume. No level or duration gate separates that from speech, and libwebrtc's AEC3 in the pod only halved it offline [O].
- **Rejected:**
  - a raised absolute threshold, because leaks peak above quiet speech;
  - a gate relative to the playback level, because it blocks normal speech;
  - the same duration gate applied all call long: AC22 p95 rose to 230-330 ms, and AC21 failed once;
  - server-side AEC3, which is too much plumbing for a partial win.

## 2. Setup

- The `persona-voice/run.sh` stack: LiveKit, the STT image, and the tts pod from source. echo.spec.js runs the tts pod on Pocket. turn-taking.spec.js runs it on the fake engine with `CI=1`, so stops are measured from when Alice hears Bob, as in CI.
- The echo Chrome container ran uncapped or capped at 1 CPU. Every stack ran under a 6 GB scope at `nice 19` / `ionice -c3`, one at a time, never next to another agent's persona-voice stack.
- **Recorder.** An env-gated hook in the pod dumped, with wall-clock timestamps:
  - every 20 ms frame each bot sent;
  - every frame of every listener track the bots heard (16 kHz).

  Replaying `Onset` on these recordings reproduced every live cut exactly. So the candidate gates were first compared offline on the same audio, then the best two ran live.

## 3. Measurements

### 3.1 echo.spec.js, echo-on half (live)

| Gate | Echo CPU | Runs | Runs with a self-echo cut | Cuts |
|---|---|---|---|---|
| none (baseline) | uncapped | 6 | 4 | 6 |
| none (baseline) | 1 | 6 | 1 | 1 |
| 9 of 16, always while playing | uncapped | 6 | 0 | 0 |
| 9 of 16, always while playing | 1 | 5 (+1 infra) | 3 | 9, one run a mid-call full leak |
| **9 of 16, first 4 s only** | uncapped | 6 | **0** | 0 |
| **9 of 16, first 4 s only** | 1 | 6 | **2** | 6: 1 warm-up, 5 from one mid-call full leak |

- The infra failure: the echo browser container never came up.
- The echo-off half still caught its own loop in every run that reached it. That loop is loud and continuous, so the gate leaves it alone, and the spec needs no change.

### 3.2 turn-taking.spec.js, AC21-AC24 (live, fake engine, from heard)

| Gate | Result | AC22 p95 (ms) |
|---|---|---|
| none (baseline) | 2 green, 1 setup flake (2nd persona never joined) | 130, 122 |
| 9 of 16, always while playing | 1 green, 1 AC22 fail, 1 AC21 fail | 330, 230, 270 |
| **9 of 16, first 4 s only** | **3 green** | **120, 150, 140** |

The always-on gate fails on the fixture's quiet clip (-37 to -45 dBFS, around the -42 dBFS floor). It needs up to 170 ms more to start, and once Grill resumed over Bob (AC21).

### 3.3 Offline replay of the 7 baseline echo cuts

| Candidate | Echo cuts left of 7 | Added onset delay on 47 real barge-ins (p50 / p95 / max ms) |
|---|---|---|
| absolute floor -36 / -32 / -28 dBFS while playing | 6 / 6 / 4 | n/a, leaks reach -15 dBFS and the fixture's speech median is -29 |
| floor = loudest playback in the echo window -12 dB | 1 | rejected: blocks most normal speech (playback peaks -11 dBFS) |
| 8 of 10 frames | 2 | 41 / 766 / 779 |
| 8 of 12 | 2 | 41 / 200 / 226 |
| **9 of 16** | **2** | **50 / 111 / 171** |
| libwebrtc AEC3 (`rtc.AudioProcessingModule`) | 2 | not measured; on the two mid-call full leaks: 4 to 2 and 5 to 2 |

- The 2 cuts left by 9 of 16 come from one run, whose later leaks lasted about 200 ms.
- AEC3 also removed the echo-off loop entirely. That would make echo.spec.js's off half fail.

## 4. Recommended change

- `Onset` takes a `playing` flag. While it is set, an onset needs 9 loud frames in the last 16 instead of 4 in 6.
- `RoomVoice` stamps `played_at` on each frame `_play` hands to a track, so one room-level value covers all of its bots.
- `Bots._listen` counts how long its track has heard persona audio: a frame counts if the room played within the last 1.5 s. It sets `playing` while that count is under 4 s.

Size: about 20 lines in `audio.py`, `room.py` and `bots.py`, plus about 30 lines of unit tests (a 60 ms burst ignored during warm-up, the same burst caught after it). No new dependency, no wire or config change.

## 5. Risks

- **A real barge-in in a listener's first 4 s of persona audio is slower:** +50 ms typical, up to +170 ms for quiet speech [O]. AC22 holds on the spec's sample, but a quiet speaker cutting in that early can still be talked over once.
- **Tuned on one rig.** The constants come from Chrome's canceller on a container PipeWire loop: the 40-80 ms bursts, the 1.0-3.1 s warm-up, the 1.5 s echo window. Real speakers, rooms and other browsers may leak longer or later [U].
- **The warm-up count belongs to the track.** A listener who rejoins gets a fresh count, which matches a new `getUserMedia`. A device change mid-call (headset to speakers) restarts Chrome's convergence but not the count [U].
- **Mid-call full leaks remain.** They showed up only with the echo browser at 1 CPU, in 2 of 11 valid capped runs, at 4-5 cuts each. Any fix there means real echo cancellation in the pod: a per-room far-end mix fed to one AEC3 per listener track, with double-talk behavior to verify against AC22. That is no longer "without a headache".

## 6. Not done

- No live run of AEC3 in the pod; the offline replay was enough to rule it out on cost.
- No run at 0.5 CPU, where the earlier evidence failed 4 of 4.
- The warm-up gate was not run in CI's `--repeat-each=3`.
