# 030 — Camera and screen share in the call

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-06
**Context**: builds on 005 (room audio, deafen), 020 (room layout), 029 (screen capture into the chat). Spike S19 measured the media path.

## Problem

The room's call is audio only. To show their screen or see each other, participants have to open another tool next to the room and lose the room's chat, documents and transcription while they do. A screen capture (029) sends one still image. It cannot follow a live walkthrough.

## Solution

Once in the audio, a participant can turn their camera on and share their screen. The room shows a **Video** stage in the space where a document opens. Its layout depends on what is being shared:

- someone shares their screen: the screen fills the stage, and cameras run in a row of small tiles under it;
- only cameras: they fill the stage as a grid.

Opening the stage is up to each viewer, so a document being read stays open. When someone starts sharing their screen, a strip says so, with a Watch button. On a phone, cameras work, screen sharing does not, and the stage is a **Video** tab. Nothing is recorded, and the personas never see the video: a screen reaches them only as a capture (029).

## Scope

- Camera on/off and screen share start/stop in the audio bar, for anyone in the audio.
- The LiveKit token allows `camera`, `screen_share` and `screen_share_audio` besides `microphone`.
- The Video stage in the viewer area: a screen share large with a camera row, or a camera grid. A participant's own camera shows mirrored.
- Several screen shares at once: the latest is large, the others are tiles that can be promoted with a click.
- A "… is sharing their screen · Watch" strip, and a fullscreen button on a large screen share.
- Phone: a Video tab in the tab bar while any video is live or while it is open, cameras only.
- Bandwidth: `adaptiveStream` and `dynacast` on the room's LiveKit connection, simulcast on cameras (S19).
- Device choice: microphone, camera and, where the browser can route it, speaker. It switches during the call and is remembered per browser.

## Out of scope

- Recording, and storing any frame of video. The only path from a screen to the room's documents is 029's capture.
- Personas or the transcription seeing video. The STT and TTS workers subscribe to audio only and keep doing so.
- A device test page before joining (mic level meter, camera preview): the self tile and the speaking indicator show it once in the call.
- Background blur or replacement, a picture-in-picture window, remote control of a shared screen.
- A per-room or per-instance switch to turn video off. Without LiveKit there is no video, as there is no audio.
- Video in the docs window (`/r/<room>/docs`).
- Room admins turning someone else's camera or screen off. Removing a person from the room (016) still drops all their tracks.

## Acceptance criteria

### Server
- [x] **AC1** `GET /api/rooms/{room}/voice` returns a token whose grant allows exactly the sources `microphone`, `camera`, `screen_share` and `screen_share_audio`. `canPublishData` stays false, `roomAdmin` stays absent, and the token's life stays at most one minute.

### Publishing
- [x] **AC2** In the audio, the audio bar has a camera button ("Turn my camera on" / "Turn my camera off", `aria-pressed`) and, where `navigator.mediaDevices.getDisplayMedia` exists, a screen share button ("Share my screen" / "Stop sharing my screen", `aria-pressed`). The camera button is 44 × 44 px at 390 px, where the screen share button is absent (AC14). Both are hidden until the participant joins the audio. Joining the audio leaves the camera off.
- [x] **AC3** The camera publishes with simulcast at the 720p preset. The screen share publishes at the 1080p, 15 fps screen-share preset, with the tab's or the system's audio when the browser offers it.
- [x] **AC4** Stopping a share from the browser's own "Stop sharing" control unpublishes the track and sets the button back to "Share my screen", as the button does. Refusing the camera or the screen picker leaves the button off and shows nothing for a cancel, and leaves an earlier failure of the same source on screen. A camera blocked in the site settings is not a cancel: the strip says to allow it there, as for the microphone. A real failure (no camera, device busy) shows its reason in the voice error strip, as a mic failure does.
- [x] **AC5** Leaving the audio, losing the connection, or being removed from the room (016) stops the camera and the screen share and releases the devices: every local video track ends with `readyState === 'ended'`.

### Watching
- [x] **AC6** The room connects with `adaptiveStream` and `dynacast` on. A camera tile no bigger than 320 × 180 px receives the low simulcast layer: in the e2e, the received `frameWidth` is at most 640.
- [x] **AC7** From 40rem up, a "Video" button in the room header shows the number of live videos and opens the stage in the viewer area. A document that was open comes back when the stage closes (✕ or a document picked from the list). The button is absent while no video is live. An open stage stays open when the last video stops, saying "Nobody is sharing video right now.", until ✕, a document picked from the list, or leaving the audio.
- [x] **AC8** With a screen share live, the stage shows it large (aspect kept, letterboxed) and the cameras in a row of 160 × 90 px tiles under it. With no screen share, the cameras fill the stage in a grid of equal tiles, aspect kept. Each tile carries the person's name, and the speaking indicator of the People list (005) while they speak. The participant's own camera is mirrored, and only for them.
- [x] **AC9** With two screen shares live, the latest started is large and the other is a tile labelled "<name>'s screen". A click on it makes it large.
- [x] **AC10** When someone else starts a screen share, every participant whose stage is closed sees a strip "<name> is sharing their screen" with a Watch button that opens the stage. It goes away when that share stops or the stage opens. The stage never opens by itself.
- [x] **AC11** A large screen share has a fullscreen button ("Full screen", Escape leaves). Its controls work by keyboard: Tab reaches the tiles, Enter promotes one, Escape closes the stage and gives focus back to the Video button.
- [x] **AC12** Muting the room (005-AC22) mutes the screen-share audio along with the voices. Videos keep playing.
- [x] **AC13** A participant whose stage is closed (or Video tab not shown) receives no video: every remote video publication is unsubscribed, and it is subscribed again when the stage opens.

### Phone
- [x] **AC14** Under 40rem, the tab bar gains a Video tab, with the count of live videos, while any video is live. When the last one stops, the tab goes, unless it is the open tab: then it stays, showing AC7's empty stage, until the participant leaves the stage (another tab, ✕, Escape, a document opened) or leaves the audio. The tab shows the same stage. The screen share button is absent, except to stop a share started in a wider window; the camera button works, and all controls keep the 44 px rule. In the audio at 390 px, the room name in the header keeps at least 6rem of width.

### Devices
- [x] **AC16** In the audio, the audio options menu (`⋯`) has a "Devices" group with one select per kind: Microphone, Camera, and Speaker. Speaker shows only where `HTMLMediaElement.prototype.setSinkId` exists. Each select lists the browser's devices by label (`navigator.mediaDevices.enumerateDevices()`) and marks the one in use. It refreshes when a device is plugged in or out (`MediaDevicesChanged`), and stays reachable and usable at 390 px.
- [x] **AC17** Picking a device switches it during the call (`room.switchActiveDevice`) without leaving the audio. The microphone keeps its mute state, the camera keeps its on/off state, and the others hear and see the new device. On-device transcription (010) follows the new microphone. A failure (device gone, busy) keeps the previous device and shows its reason in the voice error strip.
- [x] **AC18** The choice is kept in this browser per kind (its `deviceId`) and used the next time the participant joins the audio or turns the camera on, in any room. A remembered device that is no longer present falls back to the browser's default, and the select shows the default without an error.
- [x] **AC19** When the device in use is unplugged during the call, the browser's default takes over, the select follows, and the voice error strip names the device that was lost.

### Unchanged
- [x] **AC15** The STT worker and the persona bots subscribe to microphone audio only: no video track and no screen-share audio, so a shared tab's sound is neither transcribed under the sharer's name nor heard by the personas as speech. Their tests assert that with a camera, a screen share and its audio published in the room.

## Phases

### Phase 1 — Publish and watch on a wide screen
- Work: the grant in `getVoice` and its test; the camera and screen buttons; publish options; `adaptiveStream` and `dynacast`; the stage in the viewer area (screen large, camera row, grid, mirrored self-view, names, speaking ring); the Video header button; teardown on leave, disconnect and removal.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC13, AC15
- **Data model impact**: none
- **DoD**: `go test -p 1 ./internal/web/...` green with the widened grant test. A new Playwright `e2e/video/video.spec.js` on its own loopback stack (`e2e/video/run.sh`: LiveKit only, `make e2e-video`, a CI job), Chrome with `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream --auto-select-desktop-capture-source=Entire screen`, with `getDisplayMedia` stubbed by an animated 1280 × 720 canvas `captureStream`: headless Chrome has no screen to capture, as 029 found, with two participants at 1440 px:
  - A turns the camera on, and B's stage shows A's tile at a received width ≤ 640;
  - A shares, and B's stage shows it large with A's camera in the row;
  - A stops from the track's `ended` event, and the button resets;
  - A leaves the audio, and every local video track has `readyState === 'ended'`;
  - B with the stage closed has no subscribed video publication.

  The STT and TTS workers' unit tests are green with video publications present.

### Phase 2 — Device choice
- Work: the Devices group in the audio options menu; switching during the call; the remembered choice with its fallback; unplug handling; on-device transcription following the microphone.
- **Covers**: AC16, AC17, AC18, AC19
- **Data model impact**: none
- **DoD**: Playwright with Chrome's fake devices, two of each (`--use-fake-device-for-media-stream=device-count=2`): real for the microphones and speakers; the second fake camera feeds no frame to the WebRTC encoder, so the init script hands out an animated canvas reporting its `deviceId` when it is asked for exactly. The test:
  - picks the second microphone and camera, and the other participant still hears and sees A, with the new camera's `deviceId` on A's published track;
  - reloads, rejoins, and the second devices are in use;
  - makes a remembered `deviceId` unknown, and A joins on the default with no error;
  - emulates an unplug (stub `enumerateDevices` and fire `devicechange`), and the strip names the device that was lost;
  - opens the menu at 390 px, and the selects work.

  The device STT e2e (010) is green after a microphone switch.

### Phase 3 — Several shares, strip, fullscreen, phone
- Work: promotion between screen shares; the "is sharing" strip; fullscreen; the keyboard path; muting the room also mutes screen audio; the phone Video tab.
- **Covers**: AC9, AC10, AC11, AC12, AC14
- **Data model impact**: none
- **DoD**: `e2e/video/video.spec.js` extended:
  - two shares, the latest large, then a click swaps them;
  - the strip appears for B with the stage closed, and Watch opens it;
  - keyboard only: Tab to a tile, Enter, Escape back to the Video button;
  - with the room muted, the screen-share audio element is muted;
  - at 390 px, the Video tab appears with a count, no screen share button is present, and the tab disappears when the last video stops, unless it is the open tab, which stays with the empty stage.

  Then on the lab (TURN/TLS only), two browsers on different networks, one a phone on mobile data, see each other's camera and a screen share for 5 minutes. The report goes in the tracking file, with freezes and received resolution from the diagnostics sheet.

  Lab result (2026-11-10): a real meeting of more than 15 minutes, Chrome on macOS and Firefox on Linux on two networks, both through TURN; camera 720p and screen share 1080p published and seen. Freezes and received resolution were not recorded, and the phone on mobile data was not tested; shipped on this evidence.

## Data model impact (summary)

None. Video is never stored.

## Open questions

- (none)

## Decisions

- The stage takes the viewer's place rather than the chat's or the sidebar's: it is the only flexible area (docs/design.md §6), and a screen needs the width.
- The stage never opens by itself, because someone reading a document should not have it taken away. A strip with Watch does the asking (AC10).
- The camera is off when joining the audio, so nobody appears on camera by surprise.
- No video switch per room or per instance, since one means one more setting to maintain. LiveKit being configured already gates it, as it does audio. Add one if an instance's bandwidth ever needs it.
- A closed stage unsubscribes from video (AC13). With 8 people, S19 measured 0.5 to 1.4 Mbps per viewer for video alone. Someone who is not watching should not pay for it.
- `screen_share_audio` is granted so a shared video or demo can be heard. It follows the room mute like any voice.
- Devices are chosen in the audio options menu, not a separate settings sheet: that is where the call's other options already live. The choice is per browser, not per profile, because a `deviceId` only means something on the machine that produced it.
- Speaker choice only where `setSinkId` exists: Safari has none, and a select that does nothing would mislead. Also only with two outputs listed: Firefox lists the others only after `selectAudioOutput()`. Found in phase 2 review.
- Screen-share audio stays out of the transcription and the personas' hearing (AC15): it is not the sharer speaking. Found by the phase 1 worker: the workers subscribed to any audio by kind.
- A camera permission blocked for good (`NotAllowedError` with the permission `denied`) shows the site-settings hint (AC4): otherwise every press does nothing with no clue why. Found in phase 1 review round 3.
- The stage does not close by itself when the last video stops (AC7): a camera that drops for a second, an unplug or a network blip, would otherwise close the viewer's stage and leave it closed when the video comes back. Found in phase 2.
- Devices are listed with `navigator.mediaDevices.enumerateDevices()`, not LiveKit's `Room.getLocalDevices`: the latter waits for any capture under way, so an unanswered permission prompt stalled every switch, and it resets the list LiveKit's own default selection compares with.
- A device picked while the camera is off or the mic muted is opened once to test it (AC17), so its light or the OS mic indicator flickers: the price of refusing a busy device at pick time instead of at the next unmute, with the bad choice already remembered.
- On a phone the open Video tab stays when the last video stops (AC14), as the open stage does (AC7). The header's room name keeps 6rem at 390 px: phase 1 found it squeezed to nothing by the added buttons.
- On a phone the transcription toggle lives in the audio options menu (`⋯`), and `⋯` carries its state: the dot while the voice is written down, the danger colour while it is off. This amends 005-AC49 and 010-AC23 for phones: with the camera button added, the room name kept 78 px of the 96 px AC14 asks for. Found in phase 3.
- The video e2e runs on its own loopback LiveKit stack, as persona-voice does, not against the compose stack: CI has no compose stack, and the e2e must gate merges.
- WAN quality is checked by hand on the lab at the end of phase 3. S19 ran on a LAN only, and over a TCP-only path loss hurts more. If the check falls short, the follow-up is exposing ICE/TCP (S4) or UDP, not a code change here.
