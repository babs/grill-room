import { test, expect } from './fixtures.js';
import { roomWith } from './helpers.js';

// The e2e server has no LiveKit: this LiveKit stand-in keeps the room page's own mic handling and
// takes the mic from navigator.mediaDevices.getUserMedia on each enable, as LiveKit's restart does.
const fakeLiveKit = `window.LivekitClient = (() => {
  const names = new Proxy({}, { get: (_, k) => k });
  class Room {
    constructor() {
      this.state = 'connected';
      this.canPlaybackAudio = true;
      this.remoteParticipants = new Map();
      let track = null;
      this.localParticipant = {
        isMicrophoneEnabled: false,
        getTrackPublication: () => (track ? { track: { mediaStreamTrack: track } } : undefined),
        setMicrophoneEnabled: async (on) => {
          if (on) track = (await navigator.mediaDevices.getUserMedia({ audio: true })).getAudioTracks()[0];
          this.localParticipant.isMicrophoneEnabled = on;
        },
      };
    }
    on() { return this; }
    async connect() {}
    async disconnect() {}
    async startAudio() {}
  }
  return { Room, RoomEvent: names, TrackEvent: names, Track: { Source: { Microphone: 'microphone' } }, DisconnectReason: { CLIENT_INITIATED: 1 } };
})();
`;

// Firefox's text for a denied mic: the one the lab showed raw.
const raw = 'The request is not allowed by the user agent or the platform in the current context.';

async function voicePage(browser, request) {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await page.context().route('**/static/vendor/livekit-client.umd.js', (r) => r.fulfill({ contentType: 'text/javascript', body: fakeLiveKit }));
  await page.context().route(`**/api/rooms/${room.id}/voice`, (r) => r.fulfill({ json: { url: 'ws://127.0.0.1:9', token: 't' } }));
  await page.context().route(`**/api/rooms/${room.id}`, async (r) => {
    const res = await r.fetch();
    return r.fulfill({ response: res, json: { ...(await res.json()), voice: true } });
  });
  // window.denyMic rejects getUserMedia as a browser does when the site may not use the mic.
  await page.context().addInitScript((text) => {
    window.denyMic = false;
    navigator.mediaDevices.getUserMedia = async () => {
      if (window.denyMic) throw new DOMException(text, 'NotAllowedError');
      return new AudioContext().createMediaStreamDestination().stream;
    };
  }, raw);
  await page.reload();
  await expect(page.locator('#voice-join')).toBeVisible();
  return page;
}

// A denied unmute shows a banner; the next unmute that works clears it (the lab's banner stayed
// with the mic live).
test('a mic error clears once the mic is live', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const banner = page.locator('#voice-error');
  await page.locator('#voice-join').click();
  await expect(page.locator('#voice-mute')).toHaveText('Mute');
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveText('Unmute');

  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-mute').click();
  await expect(banner).toBeVisible();
  await expect(page.locator('#voice-mute')).toHaveText('Unmute');

  await page.evaluate(() => { window.denyMic = false; });
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveText('Mute');
  await expect(banner).toBeHidden();
});

// Joining with the mic denied: the participant listens, told how to allow it; the banner closes
// with its ✕, and a later unmute that works leaves no banner either.
test('a mic denied at join is dismissible and clears on unmute', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const banner = page.locator('#voice-error');
  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-join').click();
  await expect(banner).toContainText('Allow it in the site settings');
  await expect(banner).toContainText('You can still listen.');
  await expect(banner).not.toContainText(raw);
  await expect(banner.locator('[data-icon=alert] svg')).toHaveCount(1);
  await banner.getByRole('button', { name: 'Dismiss' }).click();
  await expect(banner).toBeHidden();

  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-mute').click();
  await expect(banner).toContainText('Allow it in the site settings');
  await expect(banner).not.toContainText(raw);
  await page.evaluate(() => { window.denyMic = false; });
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveText('Mute');
  await expect(banner).toBeHidden();
});
