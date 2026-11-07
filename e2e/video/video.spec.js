import { createHmac } from 'node:crypto';
import { test, expect } from '../fixtures.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const shots = 'test-results/video';

// Headless Chrome cannot capture a screen, even with --auto-select-desktop-capture-source (029's
// screen-capture.spec.js): getDisplayMedia shares an animated 1280 × 720 canvas instead, and keeps
// the constraints it was asked for in window.displayConstraints. The camera is Chrome's fake one.
// window.cameraRefusal and window.screenRefusal make either reject with that DOMException name and
// the message window.refusalMessage ('refused' by default), once only with window.refuseOnce,
// window.mediaDelay (ms) holds either answer back, window.screenAudio adds the tab's sound to the
// screen. Every video track handed out is in window.gotVideo. A device whose id is in
// window.unplugged fails when asked for exactly; window.micFail (ms) fails the next microphone
// capture that long after it is asked.
// Chrome's second fake camera (fake_device_1) never feeds the WebRTC encoder: its framesEncoded
// stays put. Asked for exactly, it hands out an animated canvas that reports its deviceId instead.
function stubMedia() {
  window.gotVideo = [];
  const wait = (ms) => new Promise((r) => setTimeout(r, ms || 0));
  const canvasStream = (fps) => {
    const canvas = Object.assign(document.createElement('canvas'), { width: 1280, height: 720 });
    const g = canvas.getContext('2d');
    let n = 0;
    const timer = setInterval(() => { g.fillStyle = `hsl(${(n++ * 7) % 360} 60% 50%)`; g.fillRect(0, 0, 1280, 720); }, 1000 / fps);
    const stream = canvas.captureStream(fps);
    const t = stream.getVideoTracks()[0];
    const stop = t.stop.bind(t);
    t.stop = () => { clearInterval(timer); stop(); };
    return stream;
  };
  const gum = MediaDevices.prototype.getUserMedia;
  const devices = MediaDevices.prototype.enumerateDevices;
  MediaDevices.prototype.getUserMedia = async function (c) {
    const asked = [c?.audio?.deviceId?.exact, c?.video?.deviceId?.exact];
    if (asked.some((id) => id && window.unplugged?.includes(id))) throw new DOMException('Requested device not found', 'NotFoundError');
    if (!c?.video) {
      const ms = window.micFail;
      if (!ms) return gum.call(this, c);
      window.micFail = 0;
      await wait(ms);
      throw new DOMException('busy', 'NotReadableError');
    }
    await wait(window.mediaDelay);
    if (window.cameraRefusal) {
      const name = window.cameraRefusal;
      if (window.refuseOnce) window.cameraRefusal = '';
      throw new DOMException(window.refusalMessage ?? 'refused', name);
    }
    const second = (await devices.call(this)).filter((d) => d.kind === 'videoinput')[1]?.deviceId;
    let stream;
    if (second && c.video.deviceId?.exact === second && !c.audio) {
      stream = canvasStream(20);
      const t = stream.getVideoTracks()[0];
      const settings = t.getSettings.bind(t);
      t.getSettings = () => ({ ...settings(), deviceId: second });
    } else {
      stream = await gum.call(this, c);
    }
    window.gotVideo.push(...stream.getVideoTracks());
    return stream;
  };
  MediaDevices.prototype.getDisplayMedia = async function (c) {
    window.displayConstraints = c;
    await wait(window.mediaDelay);
    if (window.screenRefusal) throw new DOMException(window.refusalMessage ?? 'refused', window.screenRefusal);
    const stream = canvasStream(15);
    window.gotVideo.push(...stream.getVideoTracks());
    if (window.screenAudio) stream.addTrack(new AudioContext().createMediaStreamDestination().stream.getAudioTracks()[0]);
    return stream;
  };
}

async function newRoom(request, name = 'Video') {
  return (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
}

// person joins the room under name, Chrome's fake camera and microphone allowed.
async function person(browser, request, room, name, { noScreen = false, noLiveKit = false } = {}) {
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await browser.newContext({ permissions: ['microphone', 'camera'] });
  ctx.setDefaultTimeout(20_000);
  await ctx.addInitScript(snoozePush, snoozeKey);
  await ctx.addInitScript(stubMedia);
  if (noScreen) await ctx.addInitScript(() => { delete MediaDevices.prototype.getDisplayMedia; });
  const page = await ctx.newPage();
  if (noLiveKit) await page.route('**/vendor/livekit-client.umd.js', (r) => r.abort());
  const url = new URL(inv.url);
  await page.goto(url.pathname + url.hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

async function joinAudio(page) {
  await page.getByRole('button', { name: 'Join audio' }).click();
  await expect(page.getByRole('button', { name: 'Audio options' })).toBeVisible();
}

// press clicks the toggle named name and waits for it to read after: pressed once turned on. Enabled
// again means toggleVideo has finished, its error strip update included; aria-pressed can come first.
async function press(page, name, after) {
  await page.getByRole('button', { name, exact: true }).click();
  const on = ['Turn my camera off', 'Stop sharing my screen'].includes(after);
  const b = page.getByRole('button', { name: after, exact: true });
  await expect(b).toHaveAttribute('aria-pressed', String(on));
  await expect(b).toBeEnabled();
}

const videoButton = (page) => page.locator('#video-open');
const gotVideo = (page) => page.evaluate(() => window.gotVideo.map((t) => t.readyState));
const remoteVideo = (page) => page.evaluate(() => window.grillDiag.remoteVideo());
const subscribed = async (page) => (await remoteVideo(page)).map((v) => v.subscribed);
// holds waits until read() gives want, then checks it still does 2 s later: what it rules out (an
// autoSubscribe, a late track) would land within that.
async function holds(page, read, want) {
  await expect.poll(read).toEqual(want);
  await page.waitForTimeout(2000);
  expect(await read()).toEqual(want);
}
// unwatched: page has the one video publication, unsubscribed, and keeps it so.
const unwatched = (page) => holds(page, () => subscribed(page), [false]);
const sources = async (page) => (await remoteVideo(page)).map((v) => v.source).sort();
const tile = (page, box, caption) => page.locator(`${box} .tile`).filter({ has: page.locator('figcaption', { hasText: new RegExp(`^${caption}$`) }) });

// 030-AC2, 030-AC14: the buttons come with the audio, the camera off; no screen button without
// getDisplayMedia, nor on a phone, where the camera's is 44 × 44 px.
test('camera and screen buttons', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  await expect(a.locator('#voice-cam')).toBeHidden();
  await expect(a.locator('#voice-screen')).toBeHidden();
  await joinAudio(a);
  await expect(a.getByRole('button', { name: 'Turn my camera on', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toHaveAttribute('aria-pressed', 'false');
  expect(await a.evaluate(() => window.grillDiag.localVideo())).toEqual([]);
  // Focus left on purpose, the button still there, is not taken to the audio options.
  await a.locator('#voice-screen').focus();
  await a.evaluate(() => document.activeElement.blur());
  await holds(a, () => a.evaluate(() => document.activeElement === document.body), true);
  // Under 40rem the screen button is absent (030-AC14): its focus moves to the audio options.
  await a.locator('#voice-screen').focus();
  await a.setViewportSize({ width: 390, height: 844 });
  await expect(a.locator('#voice-more')).toBeFocused();
  const box = await a.locator('#voice-cam').boundingBox();
  expect(box.width).toBeGreaterThanOrEqual(44);
  expect(box.height).toBeGreaterThanOrEqual(44);
  await expect(a.locator('#voice-screen')).toBeHidden();
  await a.screenshot({ path: `${shots}/bar-phone.png` });

  const c = await person(browser, request, room, 'Carol', { noScreen: true });
  await joinAudio(c);
  await expect(c.getByRole('button', { name: 'Turn my camera on', exact: true })).toBeVisible();
  await expect(c.locator('#voice-screen')).toBeHidden();

  // 030-AC7: with no video live, the stage asks nothing of LiveKit: the People list still draws
  // when its bundle failed to load.
  const d = await person(browser, request, room, 'Dora', { noLiveKit: true });
  await expect(d.locator('#participants')).toContainText('Alice');
  await expect(d.locator('#participants')).toContainText('Dora');
  await expect(videoButton(d)).toBeHidden();
});

// 030-AC6, 030-AC7, 030-AC8, 030-AC13: the Video button counts live videos; a closed stage receives none, also
// for a camera already on when the viewer joins; the stage shows a camera with its name, mirrored
// for its owner only; closing it, or picking a document, brings the document back.
test('camera tile and the stage', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await b.locator('#upload-input').setInputFiles({ name: 'plan.md', mimeType: 'text/markdown', buffer: Buffer.from('# Plan\n') });
  await b.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(b.locator('#doc-name')).toHaveText('plan.md');

  await press(a, 'Turn my camera on', 'Turn my camera off');
  await expect(videoButton(a)).toHaveText('Video 1');
  // 030-AC13: B joins with A's camera already on; stage closed, the publication is there and not received.
  await joinAudio(b);
  await expect(videoButton(b)).toHaveText('Video 1');
  await unwatched(b);

  await videoButton(b).click();
  await expect(b.locator('#stage')).toBeVisible();
  await expect(b.locator('#doc-frame')).toBeHidden();
  await expect(tile(b, '#stage-tiles', 'Alice')).toBeVisible();
  await expect.poll(async () => (await remoteVideo(b))[0]).toMatchObject({ subscribed: true, source: 'camera' });
  await expect.poll(async () => (await remoteVideo(b))[0].width).toBeGreaterThan(0);
  await expect(tile(b, '#stage-tiles', 'Alice')).not.toHaveClass(/mirror/);
  const transform = (p) => p.locator('#stage-tiles .tile video').evaluate((v) => getComputedStyle(v).transform);
  expect(await transform(b)).toBe('none');

  await videoButton(a).click();
  await expect(tile(a, '#stage-tiles', 'Alice')).toHaveClass(/mirror/);
  expect(await transform(a)).toBe('matrix(-1, 0, 0, 1, 0, 0)');

  // 030-AC7: a document uploaded while B watches goes to the list and leaves the stage open.
  await b.locator('#upload-input').setInputFiles({ name: 'notes.md', mimeType: 'text/markdown', buffer: Buffer.from('# Notes\n') });
  await expect(b.locator('#documents').getByRole('button', { name: 'notes.md' })).toBeVisible();
  await expect(b.locator('#uploads [aria-label="Added"]')).toHaveCount(1);
  await expect(b.locator('#stage')).toBeVisible();
  await expect(b.locator('#doc-name')).toHaveText('plan.md');

  // ✕ closes the stage: the document is back, the video unsubscribed.
  await b.getByRole('button', { name: 'Close the video' }).click();
  await expect(b.locator('#stage')).toBeHidden();
  await expect(b.locator('#doc-name')).toHaveText('plan.md');
  await expect(b.locator('#doc-frame')).toBeVisible();
  await expect(b.locator('#stage-tiles .tile')).toHaveCount(0);
  await unwatched(b);

  // A document picked from the list closes it too.
  await videoButton(b).click();
  await expect(b.locator('#stage')).toBeVisible();
  await b.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(b.locator('#stage')).toBeHidden();
  await expect(b.locator('#doc-frame')).toBeVisible();

  // Off releases the camera. The last video gone, A's open stage stays open and says so, the focus
  // on the Video button moving to its ✕, and the camera on again shows in it; B's, closed, gets it
  // back too (030-AC5, 030-AC7, 030-AC8).
  await videoButton(a).focus();
  await videoButton(b).focus();
  await a.evaluate(() => toggleVideo(true));
  await expect(a.getByRole('button', { name: 'Turn my camera on', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(a.locator('#stage-close')).toBeFocused();
  await expect(videoButton(b)).toBeHidden();
  // B's stage is closed: its focus goes to the audio options, not the page.
  await expect(b.locator('#voice-more')).toBeFocused();
  await expect(videoButton(a)).toBeHidden();
  await expect(a.locator('#stage')).toBeVisible();
  await expect(a.locator('#stage-empty')).toBeVisible();
  await expect(a.locator('#stage-empty')).toHaveText('Nobody is sharing video right now.');
  await expect(a.locator('#stage .tile')).toHaveCount(0);
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['ended']);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await a.evaluate(() => window.grillDiag.localVideo())).toEqual(['live']);
  await expect(tile(a, '#stage-tiles', 'Alice')).toBeVisible();
  await expect(a.locator('#stage-empty')).toBeHidden();
  await expect(videoButton(b)).toHaveText('Video 1');
  await videoButton(b).click();
  await expect(tile(b, '#stage-tiles', 'Alice')).toBeVisible();
  await expect.poll(async () => (await remoteVideo(b))[0].width).toBeGreaterThan(0);
  // Leaving the audio closes the stage (030-AC7).
  await a.getByRole('button', { name: 'Leave audio' }).click();
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['ended']);
  await expect(a.locator('#stage')).toBeHidden();
  // The Leave button goes with the audio: its focus moves to Join audio.
  await expect(a.locator('#voice-join')).toBeFocused();
  // Leaving without the Leave button (a removal does) while on the Video button: the focus lands on
  // Join audio, the audio options being gone too (030-AC7).
  await b.locator('#stage-close').click();
  await joinAudio(a);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await expect(videoButton(b)).toHaveText('Video 1');
  await videoButton(b).focus();
  await b.evaluate(() => leaveVoice());
  await expect(b.locator('#voice-join')).toBeFocused();
  // The same with the stage open and its ✕ focused.
  await joinAudio(b);
  await expect(videoButton(b)).toHaveText('Video 1');
  await videoButton(b).click();
  await b.locator('#stage-close').focus();
  await b.evaluate(() => leaveVoice());
  await expect(b.locator('#stage')).toBeHidden();
  await expect(b.locator('#voice-join')).toBeFocused();
});

// 030-AC3, 030-AC4, 030-AC6, 030-AC8: a shared screen shows large with the camera in a 160 × 90 row, on the low
// simulcast layer; the share ending from the browser resets the button; a refusal shows nothing,
// a real failure its reason.
test('screen share', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(videoButton(b)).toHaveText('Video 2');
  // 030-AC3: the camera at 720p in three simulcast layers, the 720p preset's bitrate on top; the
  // screen asked at 1080p and 15 fps, sent at the 1080p/15 fps preset's bitrate and rate.
  const published = await a.evaluate(() => window.grillDiag.published());
  const camPub = published.find((v) => v.source === 'camera');
  expect(camPub).toMatchObject({ simulcasted: true, layers: 3, settings: { width: 1280, height: 720 } });
  expect(camPub.encodings).toHaveLength(3);
  expect(Math.max(...camPub.encodings.map((e) => e.maxBitrate))).toBe(1_700_000);
  const screenPub = published.find((v) => v.source === 'screen_share');
  expect(screenPub.encodings.at(-1)).toMatchObject({ maxBitrate: 2_500_000, maxFramerate: 15 });
  expect(await a.evaluate(() => window.displayConstraints)).toMatchObject({ audio: true, video: { width: { ideal: 1920 }, height: { ideal: 1080 }, frameRate: 15 } });

  await videoButton(b).click();
  const screen = tile(b, '#stage-main', "Alice's screen");
  await expect(screen).toBeVisible();
  expect(await screen.locator('video').evaluate((v) => getComputedStyle(v).objectFit)).toBe('contain');
  const cam = tile(b, '#stage-tiles', 'Alice');
  await expect(cam).toBeVisible();
  expect(await cam.boundingBox()).toMatchObject({ width: 160, height: 90 });
  // 030-AC6: a 160 × 90 tile gets a low layer, and keeps it once the bandwidth estimate has ramped
  // up: without adaptiveStream, the top layer (1280) arrives here within about 12 s.
  const camWidth = async () => (await remoteVideo(b)).find((v) => v.source === 'camera')?.width || 0;
  await expect.poll(camWidth).toBeGreaterThan(0);
  for (let i = 0; i < 15; i++) {
    expect(await camWidth()).toBeLessThanOrEqual(640);
    await b.waitForTimeout(1000);
  }
  await expect.poll(async () => (await remoteVideo(b)).find((v) => v.source === 'screen_share')?.width || 0).toBeGreaterThan(0);
  await b.screenshot({ path: `${shots}/stage-screen.png` });

  // 030-AC4: the browser's own "Stop sharing" ends the track.
  await a.evaluate(() => {
    const t = voice.localParticipant.getTrackPublication(LivekitClient.Track.Source.ScreenShare).track.mediaStreamTrack;
    t.dispatchEvent(new Event('ended'));
    t.stop();
  });
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(b.locator('#stage-main .tile')).toHaveCount(0);
  await expect(videoButton(b)).toHaveText('Video 1');
  // Back to the grid: the camera tile is no longer 160 px wide.
  await expect.poll(async () => (await cam.boundingBox()).width).toBeGreaterThan(160);
  await b.screenshot({ path: `${shots}/stage-grid.png` });

  await a.evaluate(() => { window.screenRefusal = 'NotAllowedError'; });
  await a.getByRole('button', { name: 'Share my screen', exact: true }).click();
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toBeEnabled();
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(a.locator('#voice-error')).toBeHidden();

  await a.evaluate(() => { window.screenRefusal = 'NotReadableError'; });
  await a.getByRole('button', { name: 'Share my screen', exact: true }).click();
  await expect(a.locator('#voice-error')).toContainText('Screen sharing unavailable: refused.');
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toHaveAttribute('aria-pressed', 'false');
  // 030-AC4: the camera working says nothing about the screen: its failure stays.
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await expect(a.locator('#voice-error')).toContainText('Screen sharing unavailable: refused.');

  // 030-AC4: the screen's audio fails to publish after its video did: no screen stays published.
  await a.evaluate(() => {
    window.screenRefusal = '';
    window.screenAudio = true;
    window.gotVideo = [];
    const lp = voice.localParticipant;
    const publish = lp.publishTrack.bind(lp);
    // The audio's publish fails once the test says so: B has seen the screen by then.
    lp.publishTrack = (t, o) => (t.source !== LivekitClient.Track.Source.ScreenShareAudio ? publish(t, o)
      : new Promise((_, reject) => { window.failAudio = () => reject(new Error('audio refused')); }));
  });
  await a.getByRole('button', { name: 'Share my screen', exact: true }).click();
  await expect.poll(() => sources(b)).toEqual(['camera', 'screen_share']);
  await expect.poll(() => a.evaluate(() => !!window.failAudio)).toBe(true);
  await a.evaluate(() => window.failAudio());
  await expect(a.locator('#voice-error')).toContainText('Screen sharing unavailable: audio refused.');
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toHaveAttribute('aria-pressed', 'false');
  expect((await a.evaluate(() => window.grillDiag.published())).map((v) => v.source)).toEqual(['camera']);
  expect(await gotVideo(a)).toEqual(['ended']);
  await expect(videoButton(b)).toHaveText('Video 1');
  await holds(b, () => sources(b), ['camera']);
  await expect(b.locator('#stage-main .tile')).toHaveCount(0);
});

// 030-AC4: a refused camera leaves the button off and shows nothing, unless the site settings block
// it; a camera that fails, an AbortError included, says why.
test('camera refusal and failure', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  // A failure path that throws past toggleVideo looks silent on screen: only the page error tells.
  const pageErrors = [];
  a.on('pageerror', (e) => pageErrors.push(e.message));
  await joinAudio(a);
  const camOff = a.getByRole('button', { name: 'Turn my camera on', exact: true });
  await a.evaluate(() => {
    window.cameraRefusal = 'NotAllowedError';
    const query = navigator.permissions.query.bind(navigator.permissions);
    // Not async: Firefox throws at once for an unknown permission name, before any promise.
    navigator.permissions.query = (d) => {
      if (d.name !== 'camera') return query(d);
      if (window.cameraPermission === 'throw') throw new TypeError('camera is not a permission name');
      return Promise.resolve({ state: window.cameraPermission });
    };
    window.cameraPermission = 'prompt';
  });
  await camOff.click();
  await expect(camOff).toBeEnabled();
  await expect(camOff).toHaveAttribute('aria-pressed', 'false');
  await expect(a.locator('#voice-error')).toBeHidden();

  // A browser that cannot query the camera permission (Firefox) reads a refusal as a cancel.
  await a.evaluate(() => { window.cameraPermission = 'throw'; });
  await camOff.click();
  await expect(camOff).toBeEnabled();
  await expect(a.locator('#voice-error')).toBeHidden();
  await a.evaluate(() => new Promise((r) => setTimeout(r, 0)));
  expect(pageErrors).toEqual([]);

  await a.evaluate(() => { window.cameraPermission = 'denied'; });
  await camOff.click();
  await expect(a.locator('#voice-error')).toContainText('Your browser blocks the camera here. Allow it in the site settings (the icon left of the address), then try again.');
  await expect(camOff).toHaveAttribute('aria-pressed', 'false');
  await a.locator('#voice-error-close').click();

  await a.evaluate(() => { window.cameraRefusal = 'AbortError'; });
  await camOff.click();
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: refused.');
  await expect(camOff).toHaveAttribute('aria-pressed', 'false');

  // A silent cancel leaves the failure it did not fix on screen.
  await a.evaluate(() => { window.cameraRefusal = 'NotAllowedError'; window.cameraPermission = 'prompt'; });
  await camOff.click();
  await expect(camOff).toBeEnabled();
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: refused.');

  // A failure with no message names its kind.
  await a.evaluate(() => { window.cameraRefusal = 'NotReadableError'; window.refusalMessage = ''; });
  await camOff.click();
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: NotReadableError.');
  await a.evaluate(() => { delete window.refusalMessage; });

  await a.evaluate(() => { window.cameraRefusal = 'NotReadableError'; });
  await camOff.click();
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: refused.');
  await expect(camOff).toHaveAttribute('aria-pressed', 'false');
  expect(await a.evaluate(() => window.grillDiag.published())).toEqual([]);

  // A camera that then works settles the failure it reported.
  await a.evaluate(() => { window.cameraRefusal = ''; });
  await camOff.click();
  await expect(a.getByRole('button', { name: 'Turn my camera off', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(a.locator('#voice-error')).toBeHidden();
});

// 030-AC5: leaving, or the connection going, ends every local video track and resets the bar.
test('leaving releases the camera and the screen', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  for (const leave of ['button', 'disconnect']) {
    await joinAudio(a);
    await press(a, 'Turn my camera on', 'Turn my camera off');
    await press(a, 'Share my screen', 'Stop sharing my screen');
    await videoButton(a).click();
    await expect(a.locator('#stage .tile')).toHaveCount(2);
    expect(await a.evaluate(() => window.grillDiag.localVideo())).toEqual(['live', 'live']);
    if (leave === 'button') await a.getByRole('button', { name: 'Leave audio' }).click();
    else await a.evaluate(() => voice.disconnect());
    await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['ended', 'ended']);
    await expect(a.locator('#stage')).toBeHidden();
    await expect(a.locator('#stage .tile')).toHaveCount(0);
    await expect(videoButton(a)).toBeHidden();
    await expect(a.locator('#voice-cam')).toBeHidden();
    await expect(a.locator('#voice-screen')).toBeHidden();
    await expect(a.getByRole('button', { name: 'Join audio' })).toBeVisible();
  }

  // A leave while the camera or the screen picker has not answered yet: what they hand over after
  // is stopped, nothing is published.
  for (const name of ['Turn my camera on', 'Share my screen']) {
    await joinAudio(a);
    await a.evaluate(() => { window.gotVideo = []; window.mediaDelay = 1500; });
    await a.getByRole('button', { name, exact: true }).click();
    await a.getByRole('button', { name: 'Leave audio' }).click();
    await holds(a, () => gotVideo(a), ['ended']);
    await a.evaluate(() => { window.mediaDelay = 0; });
  }
});

// 030-AC5: removed from the room (016), A's camera and screen end, and B's stage drops A's tiles.
test('removal releases the camera and the screen', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(videoButton(b)).toHaveText('Video 2');
  await videoButton(b).click();
  await expect(b.locator('#stage .tile')).toHaveCount(2);
  expect(await a.evaluate(() => window.grillDiag.localVideo())).toEqual(['live', 'live']);

  const id = await a.evaluate(() => myID);
  const r = await request.delete(`/api/rooms/${room.id}/participants/${id}`, { headers: admin });
  expect(r.status(), await r.text()).toBeLessThan(300);
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['ended', 'ended']);
  await expect(b.locator('#stage .tile')).toHaveCount(0);
  await expect(videoButton(b)).toBeHidden();
  // 030-AC7: B's open stage stays, empty.
  await expect(b.locator('#stage-empty')).toBeVisible();
});

// Devices (030-AC16): Chrome's fake devices, two of each kind, the mic and the speaker also with a
// "default" entry.
const deviceIds = (page, kind) => page.evaluate(async (k) => (await navigator.mediaDevices.enumerateDevices())
  .filter((d) => d.kind === k && d.deviceId !== 'default').map((d) => d.deviceId), kind);
const devSelect = (page, name) => page.getByRole('combobox', { name, exact: true });
// devices opens the audio options menu, the pointer off it so no tip covers a select.
async function devices(page) {
  await page.mouse.move(0, 0);
  if (!await page.locator('#voice-menu').evaluate((m) => m.matches(':popover-open'))) await page.getByRole('button', { name: 'Audio options' }).click();
  await expect(page.getByRole('group', { name: 'Devices' })).toBeVisible();
}
const micDevice = (page) => page.evaluate(() => voice.localParticipant.getTrackPublication(LivekitClient.Track.Source.Microphone).track.mediaStreamTrack.getSettings().deviceId);
// The device of the track on-device transcription (010) taps, 'stale' unless it is the one published.
const sttMic = (page) => page.evaluate(() => {
  const t = window.grillMic;
  if (!t) return undefined;
  return t === voice.localParticipant.getTrackPublication(LivekitClient.Track.Source.Microphone)?.track.mediaStreamTrack ? t.getSettings().deviceId : 'stale';
});
const camDevice = async (page) => (await page.evaluate(() => window.grillDiag.published())).find((v) => v.source === 'camera')?.settings.deviceId;
const sinks = (page) => page.locator('#voice-audio audio').evaluateAll((els) => els.map((e) => e.sinkId));
// flowing checks that page keeps receiving its first remote track of kind.
const inbound = (page, kind) => page.evaluate(async (k) => {
  const pub = [...voice.remoteParticipants.values()].flatMap((p) => [...p.trackPublications.values()]).find((p) => p.kind === k && p.track);
  const stats = await pub?.track.getRTCStatsReport();
  return [...(stats?.values() || [])].find((s) => s.type === 'inbound-rtp')?.bytesReceived || 0;
}, kind);
async function flowing(page, kind) {
  const before = await inbound(page, kind);
  await expect.poll(() => inbound(page, kind)).toBeGreaterThan(before);
}

// 030-AC16, 030-AC17, 030-AC18: A switches mic, camera and speaker mid-call, B still hears and sees
// A; the mic keeps its mute and the camera its off state; the choice outlives a reload, and a
// remembered device that is gone gives the default without an error.
test('device choice', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  const mics = await deviceIds(a, 'audioinput');
  const cams = await deviceIds(a, 'videoinput');
  const outs = await deviceIds(a, 'audiooutput');
  expect([mics.length, cams.length, outs.length]).toEqual([2, 2, 2]);

  // 030-AC16: each kind listed by label, the one in use selected.
  await devices(a);
  await expect(devSelect(a, 'Microphone').locator('option')).toHaveText(['Fake Default Audio Input', 'Fake Audio Input 1', 'Fake Audio Input 2']);
  await expect(devSelect(a, 'Camera').locator('option')).toHaveText(['fake_device_0', 'fake_device_1']);
  await expect(devSelect(a, 'Speaker').locator('option')).toHaveCount(3);
  await expect(devSelect(a, 'Microphone')).toHaveValue('default');
  await expect(devSelect(a, 'Speaker')).toHaveValue('default');
  expect(await micDevice(a)).toBe('default');

  // 030-AC17: a muted mic stays muted across a switch, and comes back on the new one; on-device
  // transcription (010) taps that one.
  await a.getByRole('button', { name: 'Mute my mic' }).click();
  await expect(a.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');
  await devices(a);
  await devSelect(a, 'Microphone').selectOption(mics[1]);
  await expect(devSelect(a, 'Microphone')).toHaveValue(mics[1]);
  await holds(a, () => a.locator('#voice-mute').getAttribute('aria-pressed'), 'true');
  expect(await sttMic(a)).toBeUndefined();
  await a.locator('#voice-mute').click();
  await expect(a.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
  await expect.poll(() => micDevice(a)).toBe(mics[1]);
  await expect.poll(() => sttMic(a)).toBe(mics[1]);
  await flowing(b, 'audio');

  // 030-AC17: a camera that is off stays off when switched, and turns on with the new one.
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await camDevice(a)).toBe(cams[0]);
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[1]);
  await holds(a, () => a.evaluate(() => window.grillDiag.localVideo()), ['ended']);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await camDevice(a)).toBe(cams[1]);
  await videoButton(b).click();
  await expect(tile(b, '#stage-tiles', 'Alice')).toBeVisible();
  await expect.poll(async () => (await remoteVideo(b))[0].width).toBeGreaterThan(0);
  await flowing(b, 'video');

  // 030-AC17: switched while on, the camera stays on, and B keeps seeing it.
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[0]);
  await expect.poll(() => camDevice(a)).toBe(cams[0]);
  await expect(a.locator('#voice-cam')).toHaveAttribute('aria-pressed', 'true');
  await flowing(b, 'video');
  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect.poll(() => camDevice(a)).toBe(cams[1]);
  await flowing(b, 'video');

  // 030-AC17: the speaker routes every voice A hears.
  await devSelect(a, 'Speaker').selectOption(outs[1]);
  await expect.poll(() => sinks(a)).toEqual([outs[1]]);
  await expect(a.locator('#voice-error')).toBeHidden();

  // 030-AC18: kept in this browser; the next join, here after a reload, uses them.
  await a.reload();
  await joinAudio(a);
  await expect.poll(() => micDevice(a)).toBe(mics[1]);
  await expect.poll(() => sinks(a)).toEqual([outs[1]]);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await camDevice(a)).toBe(cams[1]);
  await devices(a);
  await expect(devSelect(a, 'Microphone')).toHaveValue(mics[1]);
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[1]);
  await expect(devSelect(a, 'Speaker')).toHaveValue(outs[1]);

  // 030-AC18: a remembered device no longer there: the default, and no error.
  await a.evaluate(() => {
    for (const kind of ['audioinput', 'videoinput', 'audiooutput']) localStorage.setItem('grill-room-device:' + kind, 'unplugged-' + kind);
  });
  await a.reload();
  await joinAudio(a);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await micDevice(a)).toBe('default');
  expect(await camDevice(a)).toBe(cams[0]);
  await devices(a);
  await expect(devSelect(a, 'Microphone')).toHaveValue('default');
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  await expect(devSelect(a, 'Speaker')).toHaveValue('default');
  await expect.poll(() => sinks(a)).toEqual(['']);
  await holds(a, () => a.locator('#voice-error').isVisible(), false);
});

// 030-AC16, 030-AC17: at 390 px the selects are 44 px high in 16 px type, inside the screen, and
// work; a switch that fails keeps the device in use, puts the select back and says why, and a
// later switch of that kind that works clears it.
test('device switch on a phone, and its failure', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  await a.setViewportSize({ width: 390, height: 844 });
  await joinAudio(a);
  const mics = await deviceIds(a, 'audioinput');
  const cams = await deviceIds(a, 'videoinput');
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await devices(a);
  for (const name of ['Microphone', 'Camera', 'Speaker']) {
    const s = devSelect(a, name);
    await expect(s).toBeInViewport({ ratio: 1 });
    const box = await s.boundingBox();
    expect(box.height).toBeGreaterThanOrEqual(44);
    expect(box.x + box.width).toBeLessThanOrEqual(390);
    expect(await s.evaluate((el) => getComputedStyle(el).fontSize)).toBe('16px');
  }
  await a.screenshot({ path: `${shots}/devices-phone.png` });
  await devSelect(a, 'Microphone').selectOption(mics[0]);
  await expect.poll(() => micDevice(a)).toBe(mics[0]);

  await a.evaluate(() => { window.cameraRefusal = 'NotReadableError'; window.refuseOnce = true; });
  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: refused.');
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  await expect.poll(() => camDevice(a)).toBe(cams[0]);
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['live']);
  await expect(a.locator('#voice-cam')).toHaveAttribute('aria-pressed', 'true');
  expect(await a.evaluate(() => localStorage.getItem('grill-room-device:videoinput'))).toBeNull();
  // A mic switch that works says nothing about the camera: its failure stays.
  await devSelect(a, 'Microphone').selectOption(mics[1]);
  await expect.poll(() => micDevice(a)).toBe(mics[1]);
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: refused.');

  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect.poll(() => camDevice(a)).toBe(cams[1]);
  await expect(a.locator('#voice-error')).toBeHidden();

  // 030-AC17: two quick picks: the first fails late, and the second is the one that stays.
  await a.evaluate(() => { window.micFail = 1500; });
  await devSelect(a, 'Microphone').selectOption(mics[0]);
  await devSelect(a, 'Microphone').selectOption('default');
  await expect.poll(() => a.evaluate(() => window.micFail)).toBe(0);
  await expect.poll(() => micDevice(a), { timeout: 10_000 }).toBe('default');
  await holds(a, () => micDevice(a), 'default');
  await expect(devSelect(a, 'Microphone')).toHaveValue('default');
  expect(await a.evaluate(() => localStorage.getItem('grill-room-device:audioinput'))).toBe('default');
  await expect(a.locator('#voice-error')).toBeHidden();

  // 030-AC17: a camera that is off is tried when picked: a busy one is refused at once, the
  // select goes back, and nothing is remembered.
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await a.evaluate(() => { window.cameraRefusal = 'NotReadableError'; window.refuseOnce = true; });
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[0]);
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: refused.');
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[1]);
  expect(await a.evaluate(() => localStorage.getItem('grill-room-device:videoinput'))).toBe(cams[1]);
  await expect(a.locator('#voice-cam')).toHaveAttribute('aria-pressed', 'false');
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await camDevice(a)).toBe(cams[1]);

  // 030-AC17: a camera that answers the try only after 10 s is given up, its late capture stopped,
  // and the next pick works.
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await a.evaluate(() => { window.gotVideo = []; window.mediaDelay = 11_000; });
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[0]);
  await expect(a.locator('#voice-error')).toContainText('Camera unavailable: it did not answer.', { timeout: 15_000 });
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[1]);
  expect(await a.evaluate(() => localStorage.getItem('grill-room-device:videoinput'))).toBe(cams[1]);
  await expect.poll(() => a.evaluate(() => window.gotVideo.map((t) => t.readyState)), { timeout: 5_000 }).toEqual(['ended']);
  await a.evaluate(() => { window.mediaDelay = 0; });
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[0]);
  await expect(a.locator('#voice-error')).toBeHidden();
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  expect(await a.evaluate(() => localStorage.getItem('grill-room-device:videoinput'))).toBe(cams[0]);
  // Left from the audio options menu, the phone's Leave: its focus moves to Join audio, not the page.
  await devices(a);
  await a.locator('#voice-leave-menu').click();
  await expect(a.locator('#voice-join')).toBeFocused();
});

// 030-AC19: the devices behind Chrome's default mic and speaker, then the mic and the camera
// picked, then every camera, go away mid-call: enumerateDevices stops listing them (Chrome relabels
// its default entries instead), their tracks end, and devicechange fires. The default takes over,
// the camera stays on, B's stage stays open through it (030-AC7), the select follows, and the strip
// names the device lost. A device made the default by a plug-in is no loss, a camera listed first
// does not take a pick's place (030-AC17), and a camera back after all were gone stays off.
test('device unplugged', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await a.addInitScript(() => {
    window.unplugged = [];
    // Chrome names its default entries after the device behind them.
    window.defaults = { audioinput: 'Default - Fake Audio Input 2', audiooutput: 'Default - Fake Audio Output 2' };
    window.camsReversed = false;
    const list = MediaDevices.prototype.enumerateDevices;
    MediaDevices.prototype.enumerateDevices = async function () {
      const all = (await list.call(this)).filter((d) => !window.unplugged.includes(d.deviceId)).map((d) => ({
        deviceId: d.deviceId, groupId: d.groupId, kind: d.kind,
        label: d.deviceId === 'default' ? window.defaults[d.kind] : d.label,
      }));
      const cams = all.filter((d) => d.kind === 'videoinput');
      return [...all.filter((d) => d.kind !== 'videoinput'), ...(window.camsReversed ? cams.reverse() : cams)];
    };
  });
  // The first camera remembered from an earlier call (030-AC18).
  const firstCam = (await deviceIds(a, 'videoinput'))[0];
  await a.evaluate((id) => localStorage.setItem('grill-room-device:videoinput', id), firstCam);
  await a.reload();
  await joinAudio(a);
  await joinAudio(b);
  const mics = await deviceIds(a, 'audioinput');
  const cams = await deviceIds(a, 'videoinput');
  const outs = await deviceIds(a, 'audiooutput');
  // change unplugs gone, replugs back, relabels defaults, reverses the cameras' order, and ends
  // the local tracks capturing from ends.
  const change = ({ gone = [], back = [], labels = {}, reversed, ends = [] }) => a.evaluate(([g, k, l, r, e]) => {
    window.unplugged = window.unplugged.filter((id) => !k.includes(id)).concat(g);
    Object.assign(window.defaults, l);
    if (r !== undefined) window.camsReversed = r;
    for (const pub of voice.localParticipant.trackPublications.values()) {
      const t = pub.track?.mediaStreamTrack;
      if (t && e.includes(t.getSettings().deviceId)) {
        t.stop();
        t.dispatchEvent(new Event('ended'));
      }
    }
    navigator.mediaDevices.dispatchEvent(new Event('devicechange'));
  }, [gone, back, labels, reversed, ends]);
  const strip = a.locator('#voice-error');
  const dismiss = async () => {
    await a.keyboard.press('Escape');
    await a.locator('#voice-error-close').click();
  };
  await devices(a);
  await expect(devSelect(a, 'Microphone')).toHaveValue('default');

  await change({ labels: { audioinput: 'Default - Fake Audio Input 1' } });
  await holds(a, () => strip.isVisible(), false);
  await change({ gone: [mics[0]], labels: { audioinput: 'Default - Fake Audio Input 2' }, ends: ['default'] });
  await expect(strip).toContainText('Fake Audio Input 1 was unplugged: using the default microphone.');
  await expect(devSelect(a, 'Microphone')).toHaveValue('default');
  await expect.poll(() => sttMic(a)).toBe('default');
  await flowing(b, 'audio');
  await dismiss();

  // The speaker, never picked.
  await change({ gone: [outs[1]], labels: { audiooutput: 'Default - Fake Audio Output 1' } });
  await expect(strip).toContainText('Fake Audio Output 2 was unplugged: using the default speaker.');
  await dismiss();

  await press(a, 'Turn my camera on', 'Turn my camera off');
  await videoButton(b).click();
  // LiveKit's own selection moves to a camera newly listed first: back to the remembered one (030-AC17).
  expect(await camDevice(a)).toBe(cams[0]);
  await change({});
  await change({ reversed: true });
  await holds(a, () => camDevice(a), cams[0]);
  await change({ reversed: false });
  await devices(a);
  // The same with a camera picked in this call.
  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect.poll(() => camDevice(a)).toBe(cams[1]);
  await devSelect(a, 'Camera').selectOption(cams[0]);
  await expect.poll(() => camDevice(a)).toBe(cams[0]);
  await change({});
  await change({ reversed: true });
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  await holds(a, () => camDevice(a), cams[0]);
  await change({ reversed: false });

  await devSelect(a, 'Microphone').selectOption(mics[1]);
  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect.poll(() => micDevice(a)).toBe(mics[1]);
  await expect.poll(() => camDevice(a)).toBe(cams[1]);

  await change({ gone: [mics[1]], ends: [mics[1]] });
  await expect(strip).toContainText('Fake Audio Input 2 was unplugged: using the default microphone.');
  await expect(devSelect(a, 'Microphone')).toHaveValue('default');
  await expect(devSelect(a, 'Microphone').locator('option')).toHaveCount(1);
  await expect.poll(() => micDevice(a)).toBe('default');
  await expect.poll(() => sttMic(a)).toBe('default');
  await flowing(b, 'audio');

  // LiveKit's own restart asks for the camera gone, fails and turns it off: it comes back on the
  // default, and B's stage, open throughout, shows it again.
  await change({ gone: [cams[1]], ends: [cams[1]] });
  await expect(strip).toContainText('fake_device_1 was unplugged: using the default camera.');
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  await expect.poll(() => camDevice(a)).toBe(cams[0]);
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['live']);
  await expect(a.locator('#voice-cam')).toHaveAttribute('aria-pressed', 'true');
  await expect(b.locator('#stage')).toBeVisible();
  await expect(tile(b, '#stage-tiles', 'Alice')).toBeVisible();
  await expect.poll(async () => (await remoteVideo(b))[0]?.width || 0).toBeGreaterThan(0);
  await flowing(b, 'video');

  // The last camera goes: the camera is off, with no default to name; plugged back, it stays off.
  await change({ gone: [cams[0]], ends: [cams[0]] });
  await expect(strip).toContainText('fake_device_0 was unplugged.');
  await expect(strip).not.toContainText('using the default');
  await expect(a.locator('#voice-cam')).toHaveAttribute('aria-pressed', 'false');
  await change({ back: [cams[0]] });
  await devices(a);
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  await holds(a, () => a.locator('#voice-cam').getAttribute('aria-pressed'), 'false');
});

// 030-AC17, 030-AC18: a camera picked before it is published, beside a screen share or while it
// starts, is the one it turns on with.
test('camera picked before it is on', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  await joinAudio(a);
  const cams = await deviceIds(a, 'videoinput');
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[1]);
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[1]);
  const tried = (await gotVideo(a)).length;
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await camDevice(a)).toBe(cams[1]);
  // Captured once, on that camera: not on the default first, then switched.
  await holds(a, async () => (await gotVideo(a)).slice(tried), ['live']);

  // Picked while the camera is starting on the device before.
  await a.getByRole('button', { name: 'Leave audio' }).click();
  await joinAudio(a);
  await a.evaluate(() => { window.mediaDelay = 1500; });
  await a.getByRole('button', { name: 'Turn my camera on', exact: true }).click();
  await devices(a);
  await devSelect(a, 'Camera').selectOption(cams[0]);
  await expect(a.getByRole('button', { name: 'Turn my camera off', exact: true })).toBeEnabled();
  await expect.poll(() => camDevice(a), { timeout: 10_000 }).toBe(cams[0]);
  await devices(a);
  await expect(devSelect(a, 'Camera')).toHaveValue(cams[0]);
  expect(await a.evaluate(() => localStorage.getItem('grill-room-device:videoinput'))).toBe(cams[0]);
});

// 030-AC16: what the browser holds back: cameras without ids (no camera permission yet) give a
// disabled select saying what to do, and a single output (Firefox before selectAudioOutput) no
// Speaker select.
test('device lists the browser holds back', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  await a.addInitScript(() => {
    const list = MediaDevices.prototype.enumerateDevices;
    MediaDevices.prototype.enumerateDevices = async function () {
      return (await list.call(this)).filter((d) => d.kind !== 'audiooutput' || d.deviceId === 'default')
        .map((d) => (d.kind === 'videoinput' ? { deviceId: '', groupId: '', kind: d.kind, label: '' } : d));
    };
  });
  await a.reload();
  await joinAudio(a);
  await devices(a);
  await expect(devSelect(a, 'Microphone')).toBeEnabled();
  await expect(devSelect(a, 'Camera')).toBeDisabled();
  await expect(devSelect(a, 'Camera').locator('option')).toHaveText(['Turn your camera on to choose']);
  await expect(a.locator('#dev-speaker')).toBeHidden();
  await expect(devSelect(a, 'Speaker')).toHaveCount(0);
});

// tabTo presses Tab until the focus is on target, at most 80 times: the keyboard path is real.
async function tabTo(page, target) {
  for (let i = 0; i < 80 && !(await target.evaluate((el) => el === document.activeElement)); i++) await page.keyboard.press('Tab');
  await expect(target).toBeFocused();
}
const bigScreen = (page) => page.locator('#stage-main .tile figcaption');

// 030-AC9, 030-AC11: three screens, the latest large and the others tiles that a click makes large;
// the pick outlives a re-render, and gone, gives way to the latest; full screen on the large one,
// where Escape does not close the stage.
test('several screen shares', async ({ browser, request }) => {
  const room = await newRoom(request);
  const [a, b, c] = [await person(browser, request, room, 'Alice'), await person(browser, request, room, 'Bob'), await person(browser, request, room, 'Carol')];
  for (const p of [a, b, c]) await joinAudio(p);
  for (const p of [a, b, c]) await press(p, 'Share my screen', 'Stop sharing my screen');
  await expect(videoButton(a)).toHaveText('Video 3');
  await videoButton(a).click();
  await expect(bigScreen(a)).toHaveText("Carol's screen");
  await expect(a.locator('#stage-tiles .tile')).toHaveCount(2);
  const aliceTile = tile(a, '#stage-tiles', "Alice's screen");
  await expect(aliceTile).toHaveAttribute('role', 'button');
  await expect(aliceTile).toHaveAccessibleName("Show Alice's screen large");
  await aliceTile.click();
  await expect(bigScreen(a)).toHaveText("Alice's screen");
  await expect(tile(a, '#stage-tiles', "Carol's screen")).toHaveAttribute('role', 'button');
  // The large one is no button.
  await expect(a.locator('#stage-main .tile')).not.toHaveAttribute('role', 'button');
  // A camera coming redraws the stage: the pick stays.
  await press(b, 'Turn my camera on', 'Turn my camera off');
  await expect(tile(a, '#stage-tiles', 'Bob')).toBeVisible();
  await expect(tile(a, '#stage-tiles', 'Bob')).not.toHaveAttribute('role', 'button');
  await holds(a, () => bigScreen(a).textContent(), "Alice's screen");

  // Full screen holds the large screen's box; Escape there leaves the stage open.
  await a.getByRole('button', { name: 'Full screen', exact: true }).click();
  await expect.poll(() => a.evaluate(() => document.fullscreenElement?.id)).toBe('stage-main');
  await a.keyboard.press('Escape');
  await expect(a.locator('#stage')).toBeVisible();
  await a.evaluate(() => document.exitFullscreen().catch(() => {}));
  await expect.poll(() => a.evaluate(() => document.fullscreenElement)).toBeNull();
  // An Escape a browser delivers right after leaving full screen is that exit's, not the stage's.
  await a.getByRole('button', { name: 'Full screen', exact: true }).click();
  await expect.poll(() => a.evaluate(() => document.fullscreenElement?.id)).toBe('stage-main');
  await a.evaluate(async () => {
    await document.exitFullscreen();
    $('stage-full').dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  });
  await expect(a.locator('#stage')).toBeVisible();
  // A browser without element full screen (iPhone Safari) shows no button.
  await a.evaluate(() => { Object.defineProperty(document, 'fullscreenEnabled', { value: false, configurable: true }); renderStage(); });
  await expect(a.locator('#stage-full')).toBeHidden();
  await a.evaluate(() => { delete document.fullscreenEnabled; renderStage(); });
  await expect(a.locator('#stage-full')).toBeVisible();

  // The pick stops: the latest left is large, not the oldest.
  await press(a, 'Stop sharing my screen', 'Share my screen');
  await expect(bigScreen(a)).toHaveText("Carol's screen");
  await expect(tile(a, '#stage-tiles', "Bob's screen")).toBeVisible();
  // A screen started later is the latest: it goes large over a pick.
  await tile(a, '#stage-tiles', "Bob's screen").click();
  await expect(bigScreen(a)).toHaveText("Bob's screen");
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(bigScreen(a)).toHaveText("Alice's screen");
  // No screen left large: full screen ends, and its button goes.
  await a.getByRole('button', { name: 'Full screen', exact: true }).click();
  await expect.poll(() => a.evaluate(() => document.fullscreenElement?.id)).toBe('stage-main');
  for (const p of [b, c]) await press(p, 'Stop sharing my screen', 'Share my screen');
  // A's own bar is under the full screen: stopped as its button would.
  await a.evaluate(() => toggleVideo(false));
  await expect(tile(a, '#stage-tiles', 'Bob')).toBeVisible();
  await expect(a.locator('#stage-full')).toBeHidden();
  await expect.poll(() => a.evaluate(() => document.fullscreenElement)).toBeNull();
});

// 030-AC9, 030-AC11: by keyboard alone, the Video button opens the stage, Tab reaches a screen
// tile, Enter makes it large and keeps the focus in the row, Escape closes the stage and gives the
// focus back to the Video button.
test('stage by keyboard', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await press(b, 'Share my screen', 'Stop sharing my screen');
  await tabTo(a, videoButton(a));
  await a.keyboard.press('Enter');
  await expect(a.locator('#stage')).toBeVisible();
  await expect(bigScreen(a)).toHaveText("Bob's screen");
  await tabTo(a, tile(a, '#stage-tiles', "Alice's screen"));
  await a.keyboard.press('Enter');
  await expect(bigScreen(a)).toHaveText("Alice's screen");
  await expect(tile(a, '#stage-tiles', "Bob's screen")).toBeFocused();
  await a.keyboard.press(' ');
  await expect(bigScreen(a)).toHaveText("Bob's screen");
  await a.keyboard.press('Escape');
  await expect(a.locator('#stage')).toBeHidden();
  await expect(videoButton(a)).toBeFocused();
  // The ✕ gives it back too.
  await a.keyboard.press('Enter');
  await a.locator('#stage-close').click();
  await expect(a.locator('#stage')).toBeHidden();
  await expect(videoButton(a)).toBeFocused();
  // A focused control the stage drops leaves the focus in the stage, where Escape works: a row
  // screen whose share stops, then Full screen once the last screen stops.
  await a.keyboard.press('Enter');
  await tile(a, '#stage-tiles', "Alice's screen").focus();
  await a.evaluate(() => toggleVideo(false));
  await expect(a.locator('#stage-close')).toBeFocused();
  await a.locator('#stage-full').focus();
  await press(b, 'Stop sharing my screen', 'Share my screen');
  await expect(a.locator('#stage-full')).toBeHidden();
  await expect(a.locator('#stage-close')).toBeFocused();
  await a.keyboard.press('Escape');
  await expect(a.locator('#stage')).toBeHidden();
});

// 030-AC10: someone else's screen starting is offered by a strip to a closed stage, which never
// opens by itself; Watch opens it, and the strip goes then, or when that screen stops.
test('sharing strip', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  const strip = b.locator('#share-strip');
  // A toast in the same tick as the share's announcement: both are read, neither replaces the other.
  await b.evaluate(() => voice.once(LivekitClient.RoomEvent.TrackPublished, () => toast('Copied')));
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(strip).toBeVisible();
  await expect(b.locator('#share-strip-text')).toHaveText('Alice is sharing their screen');
  // Read out from the page's always-present live region.
  await expect(b.locator('#toast-live p')).toHaveText(['Alice is sharing their screen', 'Copied']);
  await holds(b, () => b.locator('#stage').isVisible(), false);
  await unwatched(b);
  // The sharer is not told about their own screen.
  await expect(a.locator('#share-strip')).toBeHidden();
  await b.getByRole('button', { name: 'Watch', exact: true }).click();
  await expect(b.locator('#stage')).toBeVisible();
  // The Watch button goes with the strip: the focus moves into the stage.
  await expect(b.locator('#stage-close')).toBeFocused();
  await expect(tile(b, '#stage-main', "Alice's screen")).toBeVisible();
  await expect(strip).toBeHidden();
  // Gone once the stage opened: closing it does not bring it back.
  await b.locator('#stage-close').click();
  await holds(b, () => strip.isVisible(), false);
  // The share stopping takes it away, and the focus on Watch moves to the audio options.
  await press(a, 'Stop sharing my screen', 'Share my screen');
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(strip).toBeVisible();
  await b.locator('#share-watch').focus();
  await press(a, 'Stop sharing my screen', 'Share my screen');
  await expect(strip).toBeHidden();
  await expect(b.locator('#voice-more')).toBeFocused();
  // A stage open when the screen starts needs no strip.
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await videoButton(b).click();
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(tile(b, '#stage-main', "Alice's screen")).toBeVisible();
  await holds(b, () => strip.isVisible(), false);
  // Out of the audio with the focus on Watch: it goes to Join audio, not the message box.
  await b.locator('#stage-close').click();
  await press(a, 'Stop sharing my screen', 'Share my screen');
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await b.locator('#share-watch').focus();
  await b.evaluate(() => leaveVoice());
  await expect(strip).toBeHidden();
  await expect(b.locator('#voice-join')).toBeFocused();
});

// playing counts the audio elements page plays something in: a voice or a screen's sound.
const playing = (page) => page.locator('#voice-audio audio').evaluateAll((els) => els.filter((e) => e.srcObject?.getAudioTracks().length).length);
// screenAudio lists, for each remote screen's sound page knows of, whether it receives it.
const screenAudio = (page) => page.evaluate(() => [...voice.remoteParticipants.values()]
  .flatMap((p) => [...p.audioTrackPublications.values()]).filter((p) => p.source === 'screen_share_audio').map((p) => p.isSubscribed));

// 030-AC12: muting the room stops the screen's sound with the voices; its video keeps coming.
test('muting the room mutes a screen', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  await a.evaluate(() => { window.screenAudio = true; });
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await videoButton(b).click();
  await expect(tile(b, '#stage-main', "Alice's screen")).toBeVisible();
  await expect.poll(() => screenAudio(b)).toEqual([true]);
  // A's mic and A's screen each play in an element of their own.
  await expect.poll(() => playing(b)).toBe(2);
  await b.getByRole('button', { name: 'Mute the room' }).click();
  await expect(b.locator('#voice-deaf')).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(() => screenAudio(b)).toEqual([false]);
  await expect.poll(() => playing(b)).toBe(0);
  await flowing(b, 'video');
  await b.getByRole('button', { name: 'Mute the room' }).click();
  await expect(b.locator('#voice-deaf')).toHaveAttribute('aria-pressed', 'false');
  await expect.poll(() => screenAudio(b)).toEqual([true]);
  await expect.poll(() => playing(b)).toBe(2);
  // A second round leaves one element per track received, no empty one from the first.
  await b.getByRole('button', { name: 'Mute the room' }).click();
  await expect.poll(() => playing(b)).toBe(0);
  await b.getByRole('button', { name: 'Mute the room' }).click();
  await expect.poll(() => playing(b)).toBe(2);
  const received = await b.evaluate(() => [...voice.remoteParticipants.values()].flatMap((p) => [...p.audioTrackPublications.values()]).filter((p) => p.isSubscribed).length);
  expect(received).toBe(2);
  await expect(b.locator('#voice-audio audio')).toHaveCount(received);
});

// 030-AC14: at 390 px, a Video tab while a video is live, with its count; no screen button; the
// stage is the tab, closed when another is picked; the open tab outlives the last video, a closed
// one goes with it; the room name keeps 6rem in the audio and every control 44 px. 030-AC10: the
// strip's Watch opens the tab.
test('Video tab on a phone', async ({ browser, request }) => {
  const room = await newRoom(request, 'Quarterly planning with the whole team');
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await b.setViewportSize({ width: 390, height: 844 });
  await joinAudio(a);
  await joinAudio(b);
  await expect(b.locator('#voice-screen')).toBeHidden();
  expect((await b.locator('#room-name').boundingBox()).width).toBeGreaterThanOrEqual(96);
  for (const el of await b.locator('.room-header button:visible, #tabbar button:visible').all()) {
    const box = await el.boundingBox();
    expect(box.width, await el.evaluate((e) => e.id || e.outerHTML)).toBeGreaterThanOrEqual(44);
    expect(box.height, await el.evaluate((e) => e.id || e.outerHTML)).toBeGreaterThanOrEqual(44);
  }
  // Transcription made way for the name: its toggle is in ⋯, and works there.
  await expect(b.locator('#voice-stt')).toBeHidden();
  await b.locator('#voice-more').click();
  const stt = b.locator('#voice-menu').getByRole('checkbox', { name: 'Transcribe my voice', exact: true });
  // ⋯ carries the toggle's state, by shape and in words: a dot while the voice is written down;
  // off, a stroke across it besides the danger colour.
  const more = b.locator('#voice-more');
  const mark = () => more.evaluate((el) => {
    const probe = document.body.appendChild(Object.assign(document.createElement('span'), { style: 'color: var(--danger)' }));
    const danger = getComputedStyle(probe).color;
    probe.remove();
    const before = getComputedStyle(el, '::before');
    const shape = before.content === 'none' ? 'none' : before.transform !== 'none' ? 'stroke' : before.borderRadius === '50%' ? 'dot' : 'other';
    return { shape, off: getComputedStyle(el).borderTopColor === danger };
  });
  expect(await mark()).toEqual({ shape: 'none', off: false });
  await b.evaluate(() => showSTT({ available: true, listening: true, transcribed: [Number(myID)] }));
  await expect.poll(mark).toEqual({ shape: 'dot', off: false });
  await expect(more).toHaveAccessibleDescription('Your voice is being written down');
  await stt.click();
  await expect(b.locator('#voice-stt')).toHaveAttribute('aria-pressed', 'false');
  await expect(stt).not.toBeChecked();
  await expect.poll(mark).toEqual({ shape: 'stroke', off: true });
  await expect(more).toHaveAccessibleDescription('Your voice is not written down');
  await stt.click();
  await expect(b.locator('#voice-stt')).toHaveAttribute('aria-pressed', 'true');
  await expect(stt).toBeChecked();
  await expect.poll(mark).toEqual({ shape: 'dot', off: false });
  await expect(more).toHaveAccessibleDescription('Your voice is being written down');
  // A change the server refused leaves the box as it was.
  await b.route('**/voice/transcribe', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"down"}' }));
  await stt.click();
  await expect(b.locator('#voice-error')).toContainText('Could not change the transcription of your voice');
  await expect(stt).toBeChecked();
  await b.unroute('**/voice/transcribe');
  await b.keyboard.press('Escape');
  await b.locator('#voice-error-close').click();
  const tab = b.locator('#tab-video');
  await expect(tab).toBeHidden();

  // The camera works from the phone.
  await press(b, 'Turn my camera on', 'Turn my camera off');
  await expect(videoButton(a)).toHaveText('Video 1');
  await expect(tab).toHaveText('Video 1');
  await press(b, 'Turn my camera off', 'Turn my camera on');
  await expect(tab).toBeHidden();

  await press(a, 'Turn my camera on', 'Turn my camera off');
  await expect(tab).toHaveText('Video 1');
  await expect(tab).toHaveAttribute('aria-pressed', 'false');
  await unwatched(b);
  // The strip's Watch opens the tab.
  await press(a, 'Share my screen', 'Stop sharing my screen');
  await expect(tab).toHaveText('Video 2');
  await b.getByRole('button', { name: 'Watch', exact: true }).click();
  await expect(tab).toHaveAttribute('aria-pressed', 'true');
  await expect(b.locator('#stage')).toBeVisible();
  await expect(b.locator('.chat')).toBeHidden();
  await expect(tile(b, '#stage-main', "Alice's screen")).toBeVisible();
  await expect.poll(() => subscribed(b)).toEqual([true, true]);
  await b.screenshot({ path: `${shots}/tab-phone.png` });
  // Another tab closes the stage: no video comes.
  await b.locator('#tabbar [data-tab=chat]').click();
  await expect(b.locator('#stage')).toBeHidden();
  await holds(b, () => subscribed(b), [false, false]);
  // The last video stops with the tab closed: the tab goes, its focus to the open tab's.
  await press(a, 'Stop sharing my screen', 'Share my screen');
  await tab.focus();
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await expect(tab).toBeHidden();
  await expect(b.locator('#tabbar [data-tab=chat]')).toBeFocused();

  // The tab open, it stays, with the empty stage, until another is picked.
  await press(a, 'Turn my camera on', 'Turn my camera off');
  await tab.click();
  await expect(tile(b, '#stage-tiles', 'Alice')).toBeVisible();
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await expect(b.locator('#stage-empty')).toHaveText('Nobody is sharing video right now.');
  await holds(b, () => tab.isVisible(), true);
  await expect(tab).toHaveText('Video');
  await b.locator('#tabbar [data-tab=docs]').click();
  await expect(tab).toBeHidden();
  await expect(b.locator('#stage')).toBeHidden();

  // A wide window sharing its screen, then made narrow: the stage becomes the tab, and the button
  // stays to stop the share, then goes.
  await b.setViewportSize({ width: 1440, height: 900 });
  // Wider, the bar's own toggle tells the transcription: ⋯ says nothing of it, until narrow again.
  await expect(more).toHaveAccessibleDescription('');
  await b.setViewportSize({ width: 390, height: 844 });
  await expect(more).toHaveAccessibleDescription('Your voice is being written down');
  await b.setViewportSize({ width: 1440, height: 900 });
  await press(b, 'Share my screen', 'Stop sharing my screen');
  await videoButton(b).click();
  await b.setViewportSize({ width: 390, height: 844 });
  await expect(tab).toHaveAttribute('aria-pressed', 'true');
  await expect(b.getByRole('button', { name: 'Stop sharing my screen', exact: true })).toBeVisible();
  await b.getByRole('button', { name: 'Stop sharing my screen', exact: true }).focus();
  await b.keyboard.press('Enter');
  await expect(b.locator('#voice-screen')).toHaveAttribute('aria-pressed', 'false');
  await expect(b.locator('#voice-screen')).toBeHidden();
  // Its focus moves to the audio options, not the page.
  await expect(b.locator('#voice-more')).toBeFocused();
  // Leaving the audio closes the open tab.
  await b.locator('#voice-more').click();
  await b.locator('#voice-leave-menu').click();
  await expect(tab).toBeHidden();
  await expect(b.locator('#tabbar [data-tab=chat]')).toHaveAttribute('aria-pressed', 'true');
});

// lkParticipants counts the LiveKit room's participants from the server's own API (RoomService), on
// a token signed here with the run's keys.
async function lkParticipants(request, room) {
  const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url');
  const now = Math.floor(Date.now() / 1000);
  const body = `${b64({ alg: 'HS256', typ: 'JWT' })}.${b64({ iss: process.env.LIVEKIT_API_KEY, nbf: now - 5, exp: now + 60, video: { roomAdmin: true, room } })}`;
  const jwt = `${body}.${createHmac('sha256', process.env.LIVEKIT_API_SECRET).update(body).digest('base64url')}`;
  const r = await request.post(`http://127.0.0.1:${process.env.VIDEO_LIVEKIT_PORT}/twirp/livekit.RoomService/ListParticipants`, {
    headers: { Authorization: `Bearer ${jwt}` }, data: { room },
  });
  expect(r.ok()).toBe(true);
  return ((await r.json()).participants || []).length;
}

// popOut presses Pop out, a real click, and returns the Document PiP window.
async function popOut(page) {
  const [w] = await Promise.all([page.context().waitForEvent('page'), page.locator('#stage-pop').click()]);
  await expect(w.locator('#stage-body')).toBeVisible();
  return w;
}
const unwatchedAll = (page, n) => holds(page, () => subscribed(page), Array(n).fill(false));
const videoPlays = (w, box) => w.locator(`${box} .tile video`).first().evaluate((v) => !v.paused && v.videoWidth > 0);

// 031-AC12, 031-AC13, 031-AC14: the stage pops out into a Document PiP window, B's screen large over
// B's camera, the room's stylesheets with it; its controls act on the call; the page says where the
// video went, keeps its count and its one LiveKit connection; closing the window or Bring it back
// returns the stage, open or closed as it was; Leave audio there ends every local track.
test('pop-out window', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await joinAudio(a);
  await joinAudio(b);
  await press(b, 'Turn my camera on', 'Turn my camera off');
  await press(b, 'Share my screen', 'Stop sharing my screen');
  await expect(videoButton(a)).toHaveText('Video 2');
  await videoButton(a).click();
  await expect(a.locator('#stage-pop')).toBeVisible();

  let w = await popOut(a);
  await expect(tile(w, '#stage-main', "Bob's screen")).toBeVisible();
  await expect(tile(w, '#stage-tiles', 'Bob')).toBeVisible();
  await expect.poll(() => videoPlays(w, '#stage-main')).toBe(true);
  await expect.poll(() => videoPlays(w, '#stage-tiles')).toBe(true);
  // The room's stylesheets came along: the tiles keep the camera row's size.
  expect(await tile(w, '#stage-tiles', 'Bob').evaluate((t) => t.getBoundingClientRect().width)).toBe(160);
  expect(await w.evaluate(() => getComputedStyle(document.body).backgroundColor))
    .toBe(await a.locator('.stage-body').first().evaluate((el) => getComputedStyle(el).backgroundColor));
  await expect(a.locator('#stage-away')).toHaveText(/Video is in the pop-out window/);
  await expect(a.getByRole('button', { name: 'Bring it back' })).toBeVisible();
  await expect(a.locator('#stage-pop')).toBeHidden();
  await expect(videoButton(a)).toHaveText('Video 2');
  expect(await lkParticipants(request, room.id)).toBe(2);
  // The page's stage closed, the window keeps its videos (031-AC13).
  await a.locator('#stage-close').click();
  await holds(a, () => subscribed(a), [true, true]);
  await expect.poll(() => videoPlays(w, '#stage-main')).toBe(true);

  // Its controls are the call's: B hears A muted, sees A's camera, the server has A's hand.
  const aID = await a.evaluate(() => myID);
  const aMuted = () => b.evaluate((id) => voice.remoteParticipants.get(id)?.getTrackPublication('microphone')?.isMuted, aID);
  await w.locator('#voice-mute').click();
  await expect(w.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');
  await expect(a.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(aMuted).toBe(true);
  await w.locator('#voice-cam').click();
  await expect(w.locator('#voice-cam')).toHaveAttribute('aria-label', 'Turn my camera off');
  await expect(videoButton(b)).toHaveText('Video 3');
  await expect(tile(w, '#stage-tiles', 'Alice')).toHaveClass(/mirror/);
  await expect(w.locator('#voice-screen')).toBeVisible();
  await w.locator('#hand-raise').click();
  await expect(w.locator('#hand-raise')).toHaveAttribute('aria-label', 'Lower my hand');
  await expect(b.locator('#people-hands')).toHaveText('1');
  await w.screenshot({ path: `${shots}/pop-out.png` });
  // The page's tip cannot reach the window: its controls show theirs in CSS, on hover and on focus.
  const tipOf = (id) => w.locator(id).evaluate((el) => {
    const st = getComputedStyle(el, '::after');
    return st.visibility === 'visible' ? st.content : '';
  });
  const tipText = async (id) => JSON.stringify(await a.locator(id).getAttribute('data-tip'));
  expect(await tipOf('#voice-leave')).toBe('');
  await w.locator('#voice-leave').hover();
  await expect.poll(() => tipOf('#voice-leave')).toBe(await tipText('#voice-leave'));
  await w.mouse.move(0, 0);
  await expect.poll(() => tipOf('#voice-leave')).toBe('');
  // Keyboard focus shows it at once.
  await w.locator('#voice-cam').focus();
  await w.keyboard.press('Tab');
  await expect(w.locator('#voice-screen')).toBeFocused();
  expect(await tipOf('#voice-screen')).toBe(await tipText('#voice-screen'));
  await w.locator('#voice-screen').blur();

  // A narrow page hides its idle screen share button (030-AC14): the window's follows, Leave stays.
  await a.setViewportSize({ width: 390, height: 844 });
  await expect(a.locator('#voice-screen')).toBeHidden();
  await expect(w.locator('#voice-screen')).toBeHidden();
  await expect(w.locator('#voice-leave')).toBeVisible();
  await a.setViewportSize({ width: 1440, height: 900 });
  await expect(w.locator('#voice-screen')).toBeVisible();

  // A failure from the window's controls shows there too, and goes when the next try works.
  await w.locator('#voice-cam').click();
  await expect(w.locator('#voice-cam')).toHaveAttribute('aria-label', 'Turn my camera on');
  await a.evaluate(() => { window.cameraRefusal = 'NotReadableError'; window.refusalMessage = 'busy'; });
  await w.locator('#voice-cam').click();
  await expect(a.locator('#voice-error')).toBeVisible();
  await expect(w.locator('.pip-error')).toBeVisible();
  await expect(w.locator('.pip-error')).toHaveText(await a.locator('#voice-error-text').textContent());
  await expect(w.locator('.pip-error')).toHaveAttribute('role', 'status');
  await a.evaluate(() => { window.cameraRefusal = ''; });
  await w.locator('#voice-cam').click();
  await expect(w.locator('#voice-cam')).toHaveAttribute('aria-label', 'Turn my camera off');
  await expect(w.locator('.pip-error')).toBeHidden();

  // The page in the background, behind another tab: a share started then still plays in the window.
  // Headless Chrome keeps every page visible, bringToFront included: the page is told it is hidden,
  // and LiveKit, which reads visibilityState, pauses a video it does not see after 5 s. A hidden page
  // may run no IntersectionObserver callback either: none is observed, so LiveKit decides from where
  // the video sits when attached.
  await press(b, 'Stop sharing my screen', 'Share my screen');
  await expect(w.locator('#stage-main .tile')).toHaveCount(0);
  const other = await a.context().newPage();
  await other.bringToFront();
  await a.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
    document.dispatchEvent(new Event('visibilitychange'));
    window.ioObserve = IntersectionObserver.prototype.observe;
    IntersectionObserver.prototype.observe = () => {};
  });
  await a.waitForTimeout(6000);
  await press(b, 'Share my screen', 'Stop sharing my screen');
  await expect(tile(w, '#stage-main', "Bob's screen")).toBeVisible();
  await expect.poll(() => videoPlays(w, '#stage-main')).toBe(true);
  await expect.poll(async () => (await remoteVideo(a)).find((v) => v.source === 'screen_share')?.width || 0).toBeGreaterThan(0);
  // Still received seconds later: a paused video's width would stay at its last value, its frames not.
  const frames = () => w.locator('#stage-main video').evaluate((v) => v.getVideoPlaybackQuality().totalVideoFrames);
  const before = await frames();
  await a.waitForTimeout(6000);
  expect(await frames()).toBeGreaterThan(before + 30);
  // A camera turned off and on again comes back with its track already there: drawn and attached at once.
  const bID = await b.evaluate(() => myID);
  const inPiP = (source) => a.evaluate(([id, src]) => voice.remoteParticipants.get(id).getTrackPublication(src)?.track?.elementInfos.map((i) => i.isPiP), [bID, source]);
  // Both of B's videos, the camera and the live screen share.
  const bothInPiP = async () => [await inPiP('camera'), await inPiP('screen_share')];
  expect(await inPiP('screen_share')).toEqual([true]);
  await press(b, 'Turn my camera off', 'Turn my camera on');
  await expect(tile(w, '#stage-tiles', 'Bob')).toHaveCount(0);
  await press(b, 'Turn my camera on', 'Turn my camera off');
  await expect(tile(w, '#stage-tiles', 'Bob')).toBeVisible();
  await expect.poll(() => inPiP('camera')).toEqual([true]);
  await expect.poll(() => tile(w, '#stage-tiles', 'Bob').locator('video').evaluate((v) => !v.paused && v.videoWidth > 0)).toBe(true);
  await a.evaluate(() => {
    IntersectionObserver.prototype.observe = window.ioObserve;
    delete document.visibilityState;
    delete document.hidden;
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await other.close();

  // Closed from its own ✕ (pagehide), the stage goes back to the page closed, as it was, and stops receiving.
  await w.close();
  await expect(a.locator('#stage-away')).toBeHidden();
  await expect(a.locator('#stage')).toBeHidden();
  await expect(a.locator('#stage #stage-body')).toHaveCount(1);
  await unwatchedAll(a, 2);

  // Bring it back, the page's stage open: the stage is back there, open, its videos playing.
  await videoButton(a).click();
  w = await popOut(a);
  await expect(a.getByRole('button', { name: 'Bring it back' })).toBeFocused();
  await a.getByRole('button', { name: 'Bring it back' }).click();
  await expect.poll(() => w.isClosed()).toBe(true);
  await expect(a.locator('#stage')).toBeVisible();
  await expect(tile(a, '#stage-main', "Bob's screen")).toBeVisible();
  await expect.poll(() => videoPlays(a, '#stage-main')).toBe(true);
  await expect(a.locator('#stage-close')).toBeFocused();
  // Back in the page, LiveKit measures the videos against the page again, not the window.
  await expect.poll(bothInPiP).toEqual([[false], [false]]);
  // Closed from its ✕ with the stage open: LiveKit measures on the window's pagehide, the video still
  // in it. A hidden page runs no visibility callback after: none is delivered here.
  w = await popOut(a);
  await expect.poll(bothInPiP).toEqual([[true], [true]]);
  await a.evaluate(() => {
    for (const v of pip.document.querySelectorAll('video')) {
      v.handleVisibilityChanged = () => {};
      v.srcObject.kept = true;
    }
  });
  await w.close();
  await expect(a.locator('#stage #stage-body')).toHaveCount(1);
  await expect(a.locator('#stage')).toBeVisible();
  await expect.poll(bothInPiP).toEqual([[false], [false]]);
  // Measured again without restarting a stream.
  expect(await a.locator('#stage .tile video').evaluateAll((vs) => vs.map((v) => !!v.srcObject?.kept))).toEqual([true, true, true]);

  // Leave audio from the window ends the call as the page's button does (030-AC5).
  w = await popOut(a);
  await w.locator('#voice-leave').click();
  await expect.poll(() => w.isClosed()).toBe(true);
  await expect.poll(async () => [...new Set(await a.evaluate(() => window.grillDiag.localVideo()))]).toEqual(['ended']);
  expect([...new Set(await gotVideo(a))]).toEqual(['ended']);
  await expect(a.locator('#voice-join')).toBeVisible();
  await expect(a.locator('#stage')).toBeHidden();
  await expect(a.locator('#stage #stage-body')).toHaveCount(1);
  await expect.poll(() => lkParticipants(request, room.id)).toBe(1);
});

// 031-AC15: without Document PiP, Pop out puts the large video in the browser's own PiP: the screen
// share, or with cameras only, the first camera; with neither API, no button.
test('pop-out fallbacks', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  const b = await person(browser, request, room, 'Bob');
  await a.context().addInitScript(() => {
    delete window.documentPictureInPicture;
    window.pipAsked = [];
    window.pipEl = null;
    Object.defineProperty(Document.prototype, 'pictureInPictureElement', { get: () => window.pipEl });
    HTMLVideoElement.prototype.requestPictureInPicture = async function () {
      window.pipAsked.push(this.closest('.tile').querySelector('figcaption').textContent);
      window.pipEl = this;
      return {};
    };
  });
  await a.reload();
  await joinAudio(a);
  await joinAudio(b);
  await press(b, 'Turn my camera on', 'Turn my camera off');
  await expect(videoButton(a)).toHaveText('Video 1');
  await videoButton(a).click();
  await expect(tile(a, '#stage-tiles', 'Bob')).toBeVisible();
  await a.locator('#stage-pop').click();
  await expect.poll(() => a.evaluate(() => window.pipAsked)).toEqual(['Bob']);
  // In the browser's PiP, the camera stays received and its video in the page with the stage closed,
  // until that window closes.
  await a.locator('#stage-close').click();
  await holds(a, () => subscribed(a), [true]);
  expect(await a.evaluate(() => window.pipEl.isConnected)).toBe(true);
  await a.evaluate(() => { const v = window.pipEl; window.pipEl = null; v.dispatchEvent(new Event('leavepictureinpicture')); });
  await unwatched(a);
  await videoButton(a).click();
  await expect(tile(a, '#stage-tiles', 'Bob')).toBeVisible();
  await press(b, 'Share my screen', 'Stop sharing my screen');
  await expect(tile(a, '#stage-main', "Bob's screen")).toBeVisible();
  await a.locator('#stage-pop').click();
  await expect.poll(() => a.evaluate(() => window.pipAsked)).toEqual(['Bob', "Bob's screen"]);
  await expect(a.locator('#stage-away')).toBeHidden();

  const c = await person(browser, request, room, 'Carol');
  await c.context().addInitScript(() => {
    delete window.documentPictureInPicture;
    Object.defineProperty(Document.prototype, 'pictureInPictureEnabled', { get: () => false });
  });
  await c.reload();
  await joinAudio(c);
  await videoButton(c).click();
  await expect(tile(c, '#stage-main', "Bob's screen")).toBeVisible();
  await holds(c, () => c.locator('#stage-pop').isHidden(), true);
});
