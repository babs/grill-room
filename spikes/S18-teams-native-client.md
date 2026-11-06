# S18: a native Teams client in a meeting, one transcript per speaker

**Verdict: pending the live run.** The tools are ready and were checked offline. A real meeting decides it. The question: can a client with no browser, based on the reverse-engineered [ost](https://github.com/eisbaw/ost) protocol core, join a scheduled Teams meeting by its link, receive the audio, and say who spoke when?

Tags: **[M]** measured here, **[S]** read in code or docs, **[U]** unverified.

## 1. What already exists

| Project | Calling | Meetings | Per-speaker audio |
|---|---|---|---|
| eisbaw/ost (Rust, MIT) | Echo-bot and 1:1 calls live. Channel group call via `create_conversation` then `join_conversation_with_sdp` [S] | No join link. The group path takes a thread id from an env var [S] | No. The first SSRC is logged, and every packet goes to one recorder [S] |
| lucavoit/teams-core (ost fork) | Declared audio SSRC fix, live-checked against the Echo bot (1251 sent / 409 received). TURN key fix [S] | `parse_join_url` gives the thread id. Meeting join marked "NOT live-verified" [S] | No [S] |
| Dreamescaper/teamtrim (vendors ost) | Calendar join hands a thread id to the same two-phase call. Media is a loopback stub, calls "unverified" [S] | Same path, never run live [S] | No [S] |
| brandonp2412/microslop (ost fork) | Live 1:1 audio. Trouter fixes: ack format `6:::ID+[]`, acked events `5:ID::JSON`, late `callAcceptance` acks against 408/10056 [S] | "Meeting-link joining is not established" [S] | No. It registers `csrcInfo` and `dominantSpeakerInfo` callbacks but never reads them [S] |
| EionRobb/purple-teams PR #99 (C) | 1:1 calls, personal accounts, Opus and AES-256-GCM SDES [S] | "Group calls are not supported" [S] | No [S] |

Every one of them builds on ost, and none has joined a meeting live. ost at `0892144` fails to build (a `[u8; 20623]` type on a 20476-byte dictionary). teams-core needs libdbus [M].

## 2. What Microsoft documents

- **[MS-RTP] RTP packets** ([spec](https://learn.microsoft.com/en-us/openspecs/office_protocols/ms-rtp/3b8dc3c6-34b8-4827-9b38-3b00154f471c)) [S]:
  - A mixer sends one audio stream.
  - Its CSRC list holds the contributors' MSIs (media source ids), not SSRCs.
  - "The first CSRC in the list SHOULD be the dominant speaker at the moment in which the packet was generated."
- **[MS-RTP] Dominant Speaker History** ([spec](https://learn.microsoft.com/en-us/openspecs/office_protocols/ms-rtp/c0b73312-fc2b-4bf8-a5fe-b1948e0077d6)) [S]:
  - An RTCP feedback message with the current dominant MSI and up to 10 earlier ones.
  - It is negotiated by `x-message app send:dsh recv:dsh`, which ost's offer already carries.
- **Graph** ([mediaStream](https://learn.microsoft.com/en-us/graph/api/resources/mediastream?view=graph-rest-1.0)) [S]:
  - `participant.mediaStreams[].sourceId` is the MSI, which ties a CSRC to a name.
  - Whether the Teams client roster (Trouter `rosterUpdate`) carries the same field is [U].
- **Unmixed audio is documented for the official SDKs only** [S]:
  - The Graph media bot gets up to 4 dominant speakers (`ReceiveUnmixedMeetingAudio`).
  - ACS audio streaming gets the same, 4 channels.
  - The wire form of either is [U].

The likely outcome is one mixed stream, with the speaker carried by the first CSRC on every 20 ms packet. That is enough for a per-speaker transcript: cut the mix by dominant speaker. Overlapping speech goes to whoever dominates.

## 3. Hypotheses the run decides

| # | Hypothesis | Read it in |
|---|---|---|
| H1 | The two-phase call, given the meeting thread, `messageId` and `meetingInfo {organizerId, tenantId}` from the link, joins the meeting | `client.log` (`call_accepted=true`), `phase1.json`, `phase2.json` |
| H2 | Audio arrives as one PCMU stream, not one SSRC per speaker | `summary.md`, Streams table |
| H3 | Its packets carry CSRC lists, and the first entry follows who speaks | `summary.md`, CSRC table; listen to `csrc-*.wav` |
| H4 | A Trouter body (roster, `csrcInfo`, `dominantSpeakerInfo`) maps those values to names | `summary.md`, source ids table; `speakers.json` |
| H5 | The call holds for a 15-minute meeting | `client.log` (no `call_ended` before the end) |

## 4. What the patch changes

`s18-teams-native/ost-meeting-capture.patch`, applied on ost `0892144`:

- **Meeting join.** `call-test --meeting <link>` parses a `meetup-join` link. It accepts the plain, `/v2/?meetingjoin=true#` and Safe Links forms. Short `/meet/<id>` links are refused with a hint. The parse gives the thread id, `messageId`, organizer `Oid` and tenant `Tid`, which fill `groupChat.messageId` and `meetingInfo`.
- **Capture.** `--capture <dir>` writes:
  - `rtp.jsonl`: one row per received packet (SSRC, seq, timestamp, PT, CSRC list, header extension), with the payloads in `ssrc-<n>.pay`;
  - `rtcp.jsonl`: decrypted RTCP in hex, where DSH lives;
  - `trouter.jsonl`: every Trouter frame with its callback path and decoded body;
  - the offer and answer SDPs, and the phase 1 and phase 2 responses.
- **SRTP state per SSRC.** ost kept one inbound rollover counter for all SSRCs. With interleaved sequence spaces it guessed wrong and dropped packets at the auth tag without a log. Two tests fail without the fix and pass with it [M].
- **Fixes taken from the forks:**
  - the dictionary type, so ost builds;
  - our audio sent with the SSRC the offer declares (teams-core, checked live);
  - Trouter acks as `6:::ID+[]`, and acked events parsed (microslop, seen live).
- **Silence out.** In a meeting the client sends silent PCMU frames. Without a microphone, ost falls back to its 1 kHz test tone, which the whole meeting would hear.
- **Nothing in `/tmp`.** With `--capture`, ost's own `/tmp/received_audio.*` dumps are off. Meeting audio stays in the capture directory.
- **Helpers.**
  - `meeting-check <link>` parses a link offline.
  - `meetings` lists calendar events from 2 h ago to 24 h ahead with their `onlineMeeting.joinUrl` (Graph `calendarView`). For a short link it asks Graph `onlineMeetings?$filter=JoinWebUrl eq ...` for the thread and organizer, and prints the long form. Whether the Teams client id's Graph token may read either is [U].
- **Staying in the call.** The client keeps serving Trouter for the whole call:
  - heartbeats every 30 s;
  - acks for any late `callAcceptance`;
  - capture of every frame.

  The call ends on Ctrl-C, at the end of the duration, or when the server ends it. A second Ctrl-C flushes the capture and quits at once, without hanging up: the account stays in the roster until Teams times it out.
- **Knobs left open for the live run:**
  - `S18_PHASE1_MEETING_INFO=1` also sends `meetingInfo` in phase 1;
  - `TEAMS_PARTITION`, `TEAMS_REGION` and `TEAMS_RING` replace the hardcoded `amer03`/`amer`/`general` headers, in case the tenant is not in AMER.

`analyze.py` turns a capture into `summary.md`, one WAV per SSRC and one per first-CSRC (16 kHz, all on the capture's time origin), `speakers.json` and a manifest for `grill_stt.replay`.

## 5. Checked offline [M]

- The patched client builds from a clean clone in `rust:1.99.1-bookworm`. All 100 tests pass, 9 of them new:
  - 6 for the link forms, including the long link built from Graph parsing back;
  - 2 for SRTP with interleaved SSRCs;
  - 1 for the RTP row (CSRCs, extension, padding).
- `test_analyze.py` covers:
  - a sequence and timestamp wrap mid-stream;
  - a late packet across the wrap;
  - a 70 s gap with a sender clock jump;
  - sequence numbers reused, as after 21.8 min of packets;
  - a stream starting 1 s late;
  - a cut last line;
  - the first-CSRC split, the roster names, and the manifest.

  Removing the capture-time fallback or the signed timestamp step makes it fail.
- End to end without Teams: the voice fixture `e2e/voice/fixtures/fr-en.wav` went through the capture format. Steps:
  1. Resampled to 8 kHz and encoded as PCMU, in one stream.
  2. The first-CSRC set to MSI 2886 for the first half and 2890 for the second, with a roster naming them.
  3. Run through `analyze`, then `transcribe` (`grill-stt:local`, Parakeet).

  All 4 sentences came back attributed to the right speaker, one word off in the first ("dans" for "sur").

## 6. Running it in a meeting

From the repository root, on a Linux host with Docker:

```sh
spikes/s18-teams-native/run.sh build      # once: clone, patch, build and test (about 5 min, 4 CPUs)
spikes/s18-teams-native/run.sh login      # device code: open the printed URL, enter the code, sign in
spikes/s18-teams-native/run.sh meetings   # find the meeting's long join link
spikes/s18-teams-native/run.sh check '<meetup-join link>'     # offline: thread, organizer, tenant
spikes/s18-teams-native/run.sh join '<meetup-join link>'      # 30 min max, Ctrl-C hangs up
spikes/s18-teams-native/run.sh transcribe .scratchpad/s18/runs/<timestamp>   # needs make stt-image
spikes/s18-teams-native/run.sh logout     # deletes the tokens
```

- **Tokens.** `login` always signs in afresh: the Teams, IC3 and Graph tokens expire on their own clocks, about an hour for some. Sign in right before the meeting.
- **Lobby.** The client waits 300 s to be admitted, then gives up.
- **The link.** The client takes the long `.../l/meetup-join/19%3ameeting_...` form only. Recent invites often show a short `teams.microsoft.com/meet/<id>?p=...` link, which redirects to a launcher page and is resolved by the web client [M]. Try `meetings` first. Otherwise copy the link from an older invite of the same recurring meeting. Run `check` on it before the meeting starts.

- **Account.** The client joins as the signed-in account, so that person appears a second time in the roster. Use your own account while you are also in the meeting from Teams, or a second account invited to it. The MCU likely does not send an endpoint its own audio [U], so the official client should be the one with the microphone. This client only sends silence.
- **Consent.** Tell the meeting before you capture it. Captures hold everyone's voice and stay under `.scratchpad/s18/` (git-ignored). Delete a run when you are done with it.
- **If the join fails:**

  | Symptom in `client.log` | Try |
  |---|---|
  | `Phase 1 epconv failed (4xx)` | `S18_PHASE1_MEETING_INFO=1 run.sh join ...` |
  | Region or routing error | `TEAMS_REGION=emea TEAMS_PARTITION=<value> run.sh join ...` (values [U]: read `region`/`partition` in the authz response) |
  | `call_accepted=false` after 300 s | Nobody admitted it from the lobby: admit the second endpoint from Teams, or join with an account the lobby lets in |
  | `IC3 token expired` / `No valid Graph token` | `run.sh login` again |
  | `Remote audio SSRC` never logged | Media did not flow: ICE failed. This client gathers host and STUN candidates only, no TURN |
  | Login refused | The tenant's Conditional Access blocks the client id. The spike stops there for that tenant |

## 7. Risks

- **Tenant policy.** A tenant admin can block this client: Conditional Access on its client id, or guest and lobby rules.
- **Protocol churn.** Every endpoint, header and capability bitmask here was copied from web client traffic. Any change on Microsoft's side breaks the join with no notice.
- **Audio quality.** PCMU at 8 kHz is telephone band. Parakeet handled it on the fixture. A real meeting adds the mixer's processing and overlapping speech.

## 8. If it works

The bridge is small next to the rest of grill-room:
1. Join.
2. Decrypt.
3. Cut the mix by first CSRC.
4. Publish each speaker as a LiveKit participant.

The STT worker then transcribes each one unchanged (spec 007). That would be a Go port of the parts used here: signaling, ICE, SDES SRTP and the RTP demux. `pion/srtp` takes SDES keys and the LiveKit Go SDK publishes tracks.
