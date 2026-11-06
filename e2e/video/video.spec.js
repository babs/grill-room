import { test, expect } from '../fixtures.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const shots = 'test-results/video';

// Headless Chrome cannot capture a screen, even with --auto-select-desktop-capture-source (029's
// screen-capture.spec.js): getDisplayMedia shares an animated 1280 × 720 canvas instead, and keeps
// the constraints it was asked for in window.displayConstraints. The camera is Chrome's fake one.
// window.cameraRefusal and window.screenRefusal make either reject with that DOMException name and
// the message window.refusalMessage ('refused' by default),
// window.mediaDelay (ms) holds either answer back, window.screenAudio adds the tab's sound to the
// screen. Every video track handed out is in window.gotVideo.
function stubMedia() {
  window.gotVideo = [];
  const wait = () => new Promise((r) => setTimeout(r, window.mediaDelay || 0));
  const gum = MediaDevices.prototype.getUserMedia;
  MediaDevices.prototype.getUserMedia = async function (c) {
    if (!c?.video) return gum.call(this, c);
    await wait();
    if (window.cameraRefusal) throw new DOMException(window.refusalMessage ?? 'refused', window.cameraRefusal);
    const stream = await gum.call(this, c);
    window.gotVideo.push(...stream.getVideoTracks());
    return stream;
  };
  MediaDevices.prototype.getDisplayMedia = async function (c) {
    window.displayConstraints = c;
    await wait();
    if (window.screenRefusal) throw new DOMException(window.refusalMessage ?? 'refused', window.screenRefusal);
    const canvas = Object.assign(document.createElement('canvas'), { width: 1280, height: 720 });
    const g = canvas.getContext('2d');
    let n = 0;
    setInterval(() => { g.fillStyle = `hsl(${(n++ * 7) % 360} 60% 50%)`; g.fillRect(0, 0, 1280, 720); }, 66);
    const stream = canvas.captureStream(15);
    window.gotVideo.push(...stream.getVideoTracks());
    if (window.screenAudio) stream.addTrack(new AudioContext().createMediaStreamDestination().stream.getAudioTracks()[0]);
    return stream;
  };
}

async function newRoom(request) {
  return (await request.post('/api/rooms', { headers: admin, data: { name: 'Video' } })).json();
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

// 030-AC2: the buttons come with the audio, the camera off; no screen button without getDisplayMedia;
// 44 × 44 px on a phone.
test('camera and screen buttons', async ({ browser, request }) => {
  const room = await newRoom(request);
  const a = await person(browser, request, room, 'Alice');
  await expect(a.locator('#voice-cam')).toBeHidden();
  await expect(a.locator('#voice-screen')).toBeHidden();
  await joinAudio(a);
  await expect(a.getByRole('button', { name: 'Turn my camera on', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(a.getByRole('button', { name: 'Share my screen', exact: true })).toHaveAttribute('aria-pressed', 'false');
  expect(await a.evaluate(() => window.grillDiag.localVideo())).toEqual([]);
  await a.setViewportSize({ width: 390, height: 844 });
  for (const id of ['#voice-cam', '#voice-screen']) {
    const box = await a.locator(id).boundingBox();
    expect(box.width).toBeGreaterThanOrEqual(44);
    expect(box.height).toBeGreaterThanOrEqual(44);
  }
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

  // Off releases the camera; on again, it is back for B (030-AC5, 030-AC7, 030-AC8).
  await press(a, 'Turn my camera off', 'Turn my camera on');
  await expect(videoButton(b)).toBeHidden();
  await expect(videoButton(a)).toBeHidden();
  await expect(a.locator('#stage')).toBeHidden();
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['ended']);
  await press(a, 'Turn my camera on', 'Turn my camera off');
  expect(await a.evaluate(() => window.grillDiag.localVideo())).toEqual(['live']);
  await expect(videoButton(b)).toHaveText('Video 1');
  await videoButton(b).click();
  await expect(tile(b, '#stage-tiles', 'Alice')).toBeVisible();
  await expect.poll(async () => (await remoteVideo(b))[0].width).toBeGreaterThan(0);
  await a.getByRole('button', { name: 'Leave audio' }).click();
  await expect.poll(() => a.evaluate(() => window.grillDiag.localVideo())).toEqual(['ended']);
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
});
