import { test, expect } from './fixtures.js';
import { roomWith, playSTT, noOverflow } from './helpers.js';

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
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');

  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-mute').click();
  await expect(banner).toBeVisible();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');

  await page.evaluate(() => { window.denyMic = false; });
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
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
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
  await expect(banner).toBeHidden();
});

const bar = ['#voice-mute', '#voice-deaf', '#voice-stt', '#voice-more', '#voice-leave'];

// tipUnder waits for the styled tip to say text, then checks it sits below el. The pointer left
// where Join audio was may have opened another icon's tip first.
async function tipUnder(page, el, text) {
  const tip = page.locator('#person-tip');
  await expect(tip).toBeVisible();
  await expect(tip).toHaveText(text);
  const [t, b] = [await tip.boundingBox(), await el.boundingBox()];
  expect(t.y, 'tip below its icon').toBeGreaterThanOrEqual(b.y + b.height);
}

// 005-AC49: once in the audio, the header is a row of icon buttons, each named, each with its styled
// tip below it on hover and on keyboard focus; the toggles carry aria-pressed. ⋯ holds only the
// rare options, as rows aligned with its head.
test('the audio bar: icon toggles with their tips below', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  await page.setViewportSize({ width: 1440, height: 900 });
  const room = await page.evaluate(() => roomID);
  const me = Number(await page.evaluate(() => myID));
  // A worker transcribes Alice: the transcription toggle carries its dot.
  await playSTT(page, room, { listening: true, transcribed: [me], engines: ['parakeet'] });
  await page.locator('#voice-join').click();
  for (const sel of bar) {
    const b = page.locator(sel);
    await expect(b).toBeVisible();
    await expect(b).toHaveAttribute('aria-label', /\w/);
    await expect(b).toHaveAttribute('data-tip', /\w/);
    await expect(b.locator('svg')).toHaveCount(1);
    await expect(b).toHaveText('');
  }
  await expect(page.locator('#voice-join')).toBeHidden();
  const mute = page.locator('#voice-mute');
  const deaf = page.locator('#voice-deaf');
  const stt = page.locator('#voice-stt');
  await expect(mute).toHaveAttribute('aria-pressed', 'false');
  await expect(deaf).toHaveAttribute('aria-pressed', 'false');
  await expect(stt).toHaveAttribute('aria-pressed', 'true');
  await expect(stt).toHaveClass(/captured/);

  // Hover shows the tip under the icon, with the words of its state.
  await mute.hover();
  await tipUnder(page, mute, 'Your mic is on. Press to mute it.');
  await mute.click();
  await expect(mute).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#person-tip')).toHaveText('Your mic is muted. Press to speak again.');
  // Keyboard focus shows it at once.
  await page.keyboard.press('Tab');
  await expect(deaf).toBeFocused();
  await tipUnder(page, deaf, 'Mute the room: stop hearing the others.');
  await page.keyboard.press('Enter');
  await expect(deaf).toHaveAttribute('aria-pressed', 'true');
  await expect(deaf).toHaveAttribute('data-tip', /Press to hear them again/);
  await page.keyboard.press('Enter');
  await expect(deaf).toHaveAttribute('aria-pressed', 'false');
  await page.keyboard.press('Tab');
  await tipUnder(page, stt, /^Your voice is being written down\. Press to stop/);

  // ⋯ holds no button: the checkbox rows start at the head's text, their words in one column.
  await page.locator('#voice-more').click();
  const menu = page.locator('#voice-menu');
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('button')).toHaveCount(0);
  await expect(menu.locator('#stt-status')).toHaveText('1 voice is being written down · yours is one of them');
  const x = await page.evaluate(() => {
    const head = document.getElementById('stt-status');
    const box = document.querySelector('#voice-keepmic input');
    return { head: head.getBoundingClientRect().left + parseFloat(getComputedStyle(head).paddingLeft), box: box.getBoundingClientRect().left };
  });
  expect(Math.abs(x.head - x.box)).toBeLessThanOrEqual(1);
  await page.keyboard.press('Escape');

  await page.locator('#voice-leave').click();
  await expect(page.locator('#voice-join')).toBeVisible();
  for (const sel of bar) await expect(page.locator(sel)).toBeHidden();

  // On a phone the bar fits the header row, every target 44 px; Leave audio moves into ⋯ so the
  // room's name keeps its first word.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator('#voice-join').click();
  const phoneBar = bar.filter((sel) => sel !== '#voice-leave');
  await expect(page.locator('#voice-more')).toBeVisible();
  await expect(page.locator('#voice-leave')).toBeHidden();
  for (const sel of phoneBar) {
    const b = await page.locator(sel).boundingBox();
    expect(b.width, sel).toBeGreaterThanOrEqual(44);
    expect(b.height, sel).toBeGreaterThanOrEqual(44);
  }
  expect(await noOverflow(page)).toEqual([]);
  // One row with the room's name: nothing wrapped under it.
  const name = await page.locator('#room-open').boundingBox();
  expect((await page.locator('#room-open .name').boundingBox()).width).toBeGreaterThanOrEqual(80);
  for (const sel of [...phoneBar, '#me']) {
    const b = await page.locator(sel).boundingBox();
    expect(Math.abs(b.y + b.height / 2 - (name.y + name.height / 2)), sel + ' on the name row').toBeLessThanOrEqual(2);
  }
  await page.locator('#voice-more').click();
  await menu.getByRole('button', { name: 'Leave audio', exact: true }).click();
  await expect(page.locator('#voice-join')).toBeVisible();
});

// 010-AC23: turning the transcription of my voice off holds across a reload and stops this
// device's transcription engine.
test('the transcription of my voice turns off and stays off', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const room = await page.evaluate(() => roomID);
  const stt = page.locator('#voice-stt');
  const meOff = async () => (await (await page.request.get(`/api/rooms/${room}`)).json()).me.stt_off;
  await page.locator('#voice-join').click();
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(true);
  await stt.click();
  await expect(stt).toHaveAttribute('aria-pressed', 'false');
  await expect(stt).toHaveAttribute('data-tip', /not written down, and the others see it/);
  // The device engine follows: it runs only while my voice is to be transcribed.
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(false);
  expect(await meOff()).toBe(true);

  await page.reload();
  await page.locator('#voice-join').click();
  await expect(stt).toHaveAttribute('aria-pressed', 'false');
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(false);
  await stt.click();
  await expect(stt).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(true);
  expect(await meOff()).toBeUndefined();
});
