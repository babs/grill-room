import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { chromium } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, playSTT } from './helpers.js';
import { snoozePush, snoozeKey } from './quiet-push.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const desktop = { userAgentData: { mobile: false }, hardwareConcurrency: 8, deviceMemory: 8 };
const ipad = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15';
const fakeOffer = (revision) => ({ revision, bytes: 432e6, files: [] });

// realOffer is the e2e server's device offer: null unless GRILL_ROOM_E2E_MODEL_DIR names the model
// files. CI must run the self-test, so there a missing model fails instead of skipping.
async function realOffer(request) {
  const offer = await (await request.get('/api/admin/test/device-stt', { headers: admin })).json();
  if (!offer && process.env.CI) throw new Error('set GRILL_ROOM_E2E_MODEL_DIR: the self-test needs the real model');
  test.skip(!offer, 'GRILL_ROOM_E2E_MODEL_DIR is not set');
  return offer;
}

// profiled joins a new room in a browser profile on disk: a private context keeps Cache Storage in
// memory and refuses a 383 MB entry, which a real profile stores. The profile goes with the test.
async function profiled(request, baseURL, use) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Device room' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const dir = mkdtempSync(join('test-results', 'device-profile-'));
  const ctx = await chromium.launchPersistentContext(dir, { channel: 'chrome', baseURL });
  try {
    await ctx.addInitScript(snoozePush, snoozeKey);
    const page = ctx.pages()[0] || await ctx.newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill('Alice');
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText('Alice');
    await use(room, page);
  } finally {
    await ctx.close();
    rmSync(dir, { recursive: true, force: true });
  }
}

// stub overrides navigator properties before the page's scripts run, as a phone or a small
// machine reports them.
async function stub(page, nav, isolated = true) {
  await page.addInitScript(([nav, isolated]) => {
    for (const [k, v] of Object.entries(nav)) Object.defineProperty(Navigator.prototype, k, { get: () => v, configurable: true });
    if (!isolated) Object.defineProperty(window, 'crossOriginIsolated', { get: () => false });
  }, [nav, isolated]);
}

// offerTo replays an "stt" event carrying the offer, and waits for the page's capability check.
async function offerTo(page, roomID, offer) {
  await playSTT(page, roomID, { engines: ['whisper'], device: offer });
  await expect.poll(() => page.evaluate(() => window.grillSTT && window.grillDeviceSTT?.checked)).toBe(offer?.revision ?? null);
  // The toggle lives in the audio menu, which needs audio: open it as its button would.
  await page.locator('#voice-menu').evaluate((m) => m.showPopover());
}

const toggle = (page) => page.locator('#voice-device');
const said = (page) => page.locator('#device-stt-status');
// audio plays the event room.js sends when the tab joins or leaves the audio: the e2e server has none.
const audio = (page, on) => page.evaluate((on) => dispatchEvent(new CustomEvent('grill:voice', { detail: { on } })), on);

// switchOn ticks the toggle as a participant does: from the audio menu, so in the audio.
async function switchOn(page) {
  await audio(page, true);
  await toggle(page).getByRole('checkbox').check();
}

// 010-AC1: only a capable desktop sees the toggle, off by default with its download size.
test('the toggle shows on a capable desktop only', async ({ browser, request }) => {
  const cases = [
    ['a phone', { ...desktop, userAgentData: { mobile: true } }],
    ['an iPad', { ...desktop, userAgentData: null, maxTouchPoints: 5, userAgent: ipad }],
    ['2 cores', { ...desktop, hardwareConcurrency: 2 }],
    ['4 GB of memory', { ...desktop, deviceMemory: 4 }],
    ['no isolation', desktop, false],
    ['no offer', desktop, true, null],
    ['a desktop', desktop],
  ];
  const { room, pages } = await roomWith(browser, request, cases.map(([name]) => name));
  for (const [i, [name, nav, isolated = true, offer = fakeOffer('rev-ac1')]] of cases.entries()) {
    const page = pages[i];
    await stub(page, nav, isolated);
    if (name === 'a desktop') {
      // An older revision's files, and the choice made earlier.
      await page.evaluate(() => caches.open('grill-room-stt:rev-old').then(() => localStorage.setItem('grill-room-device-stt', '1')));
    }
    await offerTo(page, room.id, offer);
    if (name !== 'a desktop') {
      await expect(toggle(page), name).toBeHidden();
      await expect(said(page), name).toBeHidden();
      continue;
    }
    await expect(toggle(page)).toBeVisible();
    // Chosen but not in the audio: no engine yet.
    expect(await page.evaluate(() => window.grillDeviceSTT.state)).toBe('off');
    await expect(toggle(page).getByRole('checkbox', { name: 'Transcribe on this device' })).not.toBeChecked();
    await expect(said(page)).toHaveText('Downloads 432 MB once');
    await expect(page.locator('#stt-credit')).toBeHidden();
    // Only the offered revision keeps its files.
    await expect.poll(() => page.evaluate(() => caches.keys())).not.toContain('grill-room-stt:rev-old');
    // Ticked outside the audio: stored, and no engine starts (the change runs setState in the click).
    await page.evaluate(() => { window.deviceStates = []; addEventListener('grill:device-stt', (e) => window.deviceStates.push(e.detail.state)); });
    await toggle(page).getByRole('checkbox').check();
    expect(await page.evaluate(() => [window.deviceStates, localStorage.getItem('grill-room-device-stt')])).toEqual([[], '1']);
  }
});

// 010-AC2: the self-test's word error rate normalises case, accents and punctuation.
test('the self-test scores words, not spelling', async ({ browser, request }) => {
  const { pages: [page] } = await roomWith(browser, request, ['Alice']);
  const wer = (ref, hyp) => page.evaluate(([r, h]) => import('/static/stt/device.js').then((m) => m.wer(r, h)), [ref, hyp]);
  expect(await wer("L'eau se déverse sur la digue.", "l'eau  SE DÉVERSE sur la digue")).toBe(0);
  // Decomposed accents (NFD) read as the composed ones.
  expect(await wer('se déverse', 'se déverse')).toBe(0);
  expect(await wer('une section de cent pieds', 'une section de 100 pieds')).toBeCloseTo(0.2);
  expect(await wer('les voies navigables', '')).toBe(1);
});

const good = Buffer.from('first file\n');
const sri = (b) => 'sha256-' + createHash('sha256').update(b).digest('base64');

// twoFiles is an offer of two small files, served by route: vocab.txt as pinned, then badName
// through bad(route).
async function twoFiles(page, revision, bad, { served = [], badName = 'decoder_joint-model.int8.onnx' } = {}) {
  await page.context().route(`**/models/${revision}/*`, (r) => {
    served.push(r.request().url());
    return r.request().url().endsWith('/' + badName) ? bad(r) : r.fulfill({ body: good });
  });
  return { revision, bytes: 2 * good.length, files: [
    { name: 'vocab.txt', size: good.length, integrity: sri(good) },
    { name: badName, size: good.length, integrity: sri(good) },
  ] };
}

// 010-AC2: a file that does not match its sha256, by a flipped byte or a longer body, model or
// runtime module, stops the download with a message and leaves nothing of that revision in Cache
// Storage.
test('a hash mismatch stops and caches nothing', async ({ browser, request }) => {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  const flipped = Buffer.from(good);
  flipped[0] ^= 1;
  await stub(page, desktop);
  const cases = [
    ['rev-flipped', flipped, 'decoder_joint-model.int8.onnx'],
    ['rev-longer', Buffer.concat([good, Buffer.from('x')]), 'decoder_joint-model.int8.onnx'],
    ['rev-runtime', flipped, 'ort.all.min.mjs'],
  ];
  for (const [rev, body, badName] of cases) {
    const offer = await twoFiles(page, rev, (r) => r.fulfill({ body }), { badName });
    await offerTo(page, room.id, offer);
    await switchOn(page);
    await expect(said(page), rev).toHaveText(`A downloaded file (${badName}) did not match its checksum, so nothing was kept. Try again later.`);
    await expect(toggle(page).getByRole('checkbox')).not.toBeChecked();
    expect(await page.evaluate(() => caches.keys())).not.toContain('grill-room-stt:' + rev);
    // A mismatch is the server's fault, not the device's: the option stays.
    await expect(toggle(page)).toBeVisible();
  }
});

// 010-AC2: a download that fails is no verdict on the device: the reason shows for the session,
// and the option is still offered after a reload.
test('a failed download keeps the option', async ({ browser, request }) => {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await stub(page, desktop);
  const offer = await twoFiles(page, 'rev-aborted', (r) => r.abort('connectionreset'));
  await offerTo(page, room.id, offer);
  await switchOn(page);
  const reason = 'The download stopped. Check your connection and try again.';
  await expect(said(page)).toHaveText(reason);
  // An error reads as one without its colour: the alert icon leads it.
  await expect(said(page)).toHaveClass(/\berror\b/);
  await expect(said(page).locator('svg')).toHaveCount(1);
  await expect(toggle(page).getByRole('checkbox')).not.toBeChecked();
  // The file checked before the connection dropped stays for the next try.
  expect(await page.evaluate(() => caches.open('grill-room-stt:rev-aborted').then((c) => c.match('/models/rev-aborted/vocab.txt')).then(Boolean))).toBe(true);
  await offerTo(page, room.id, offer);
  await expect(toggle(page)).toBeVisible();
  await expect(toggle(page).getByRole('checkbox')).not.toBeChecked();
  await expect(said(page)).toHaveText(reason);
});

// 010-AC2: a cached file is checked like a downloaded one: a rotten copy is fetched again.
test('a corrupt cached file is fetched again', async ({ browser, request }) => {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await stub(page, desktop);
  const served = [];
  const flipped = Buffer.from(good);
  flipped[0] ^= 1;
  // The second file fails, so the load stops right after the first.
  const offer = await twoFiles(page, 'rev-rot', (r) => r.fulfill({ body: flipped }), { served });
  await offerTo(page, room.id, offer);
  await page.evaluate(() => caches.open('grill-room-stt:rev-rot').then((c) => c.put('/models/rev-rot/vocab.txt', new Response('rotten'))));
  await switchOn(page);
  await expect(said(page)).toHaveText(/did not match its checksum/);
  expect(served.filter((u) => u.endsWith('/vocab.txt'))).toHaveLength(1);
});

// 010-AC2: an engine that stops answering is no verdict on the device either.
test('a hung engine stops with a reason and keeps the option', async ({ browser, request }) => {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await stub(page, desktop);
  // The real response's headers: a worker script without the page's COEP does not load at all.
  await page.context().route('**/static/stt/engine.js', async (r) => r.fulfill({ response: await r.fetch(), body: 'onmessage = () => {};\n' }));
  await offerTo(page, room.id, fakeOffer('rev-hung'));
  await page.evaluate(() => import('/static/stt/device.js').then((m) => { m.timing.stallMs = 500; }));
  await switchOn(page);
  await expect(said(page)).toHaveText('The voice-to-text engine stopped responding. Try again later.');
  await expect(toggle(page)).toBeVisible();
  await expect(toggle(page).getByRole('checkbox')).not.toBeChecked();
});

// 010-AC2, 010-AC3, 010-AC8: the real model downloads, passes the self-test, and no request
// leaves the origin; the credit shows while device mode is on. The pass is remembered.
test('the self-test passes on the real model', async ({ request, baseURL }) => {
  test.setTimeout(300_000);
  const offer = await realOffer(request);
  await profiled(request, baseURL, async (room, page) => {
    const away = [];
    page.context().on('request', (r) => { if (!r.url().startsWith(baseURL + '/')) away.push(r.url()); });
    await stub(page, desktop);
    // Every status line, so the test sees the progress move within a file.
    await page.addInitScript(() => {
      window.saidLines = [];
      addEventListener('DOMContentLoaded', () => {
        const el = document.getElementById('device-stt-status');
        new MutationObserver(() => window.saidLines.push(el.textContent)).observe(el, { childList: true, characterData: true, subtree: true });
      });
    });
    await offerTo(page, room.id, offer);
    // The size offered is the size downloaded: every file, the runtime's modules too.
    const size = `${Math.round(offer.bytes / 1e6)} MB`;
    await expect(said(page)).toHaveText(`Downloads ${size} once`);
    await switchOn(page);
    await expect(page.locator('#stt-credit')).toBeVisible();
    await expect(said(page)).toHaveText('Ready: this device can write your voice down.', { timeout: 240_000 });
    const run = await page.evaluate(() => ({ test: window.grillDeviceSTT.selfTest, load: window.grillDeviceSTT.load }));
    console.log('device self-test', JSON.stringify(run));
    expect(run.test.wer).toBeLessThanOrEqual(0.2);
    const lines = await page.evaluate(() => window.saidLines);
    const steps = lines.filter((t) => t.startsWith('Downloading: '));
    expect(steps.length).toBeGreaterThan(1);
    expect(steps.every((t) => t.endsWith(` of ${size}`))).toBe(true);
    const progress = lines.map((t) => /^Downloading: (\d+) %/.exec(t)?.[1]).filter(Boolean).map(Number);
    // Steps of 10 %, several of them inside the encoder, the first and biggest file.
    expect(progress.every((n) => n % 10 === 0)).toBe(true);
    expect(new Set(progress.filter((n) => n > 0 && n < 88)).size).toBeGreaterThan(1);
    // 010-AC3: no WebGPU adapter in headless Chrome, so WASM, threaded in an isolated worker.
    expect(run.load).toMatchObject({ backend: 'wasm', isolated: true });
    expect(await page.evaluate(() => caches.keys())).toContain('grill-room-stt:' + offer.revision);

    // On the next load the engine waits for the audio, then comes from the cache and skips the
    // test. The e2e server has no audio: the page's audio events are played as room.js sends them.
    await page.reload();
    await expect.poll(() => page.evaluate(() => window.grillDeviceSTT?.checked)).toBe(offer.revision);
    expect(await page.evaluate(() => window.grillDeviceSTT.state)).toBe('off');
    await audio(page, true);
    await expect.poll(() => page.evaluate(() => window.grillDeviceSTT.state), { timeout: 120_000 }).toBe('ready');
    const warm = await page.evaluate(() => ({ test: window.grillDeviceSTT.selfTest, load: window.grillDeviceSTT.load }));
    console.log('device warm load', JSON.stringify(warm));
    expect(warm.test).toBeUndefined();
    // Leaving the audio ends the engine and keeps the choice.
    await audio(page, false);
    expect(await page.evaluate(() => [window.grillDeviceSTT.state, localStorage.getItem('grill-room-device-stt')])).toEqual(['off', '1']);
    expect(away).toEqual([]);
  });
});

// 010-AC2: a device that gets the clip wrong loses the option for this model revision, says why
// for the session, and a new revision offers it again.
test('a failed self-test hides the option for that revision', async ({ request, baseURL }) => {
  test.setTimeout(300_000);
  const offer = await realOffer(request);
  await profiled(request, baseURL, async (room, page) => {
    // Silence in place of the clip: no words, a WER of 1.
    await page.route('**/static/stt/selftest-fr.wav', async (r) => {
      const wav = Buffer.from(await (await r.fetch()).body());
      wav.fill(0, 44);
      await r.fulfill({ body: wav });
    });
    await stub(page, desktop);
    await offerTo(page, room.id, offer);
    await switchOn(page);
    const reason = "This device got the test sentence wrong (100 % of words), so it can't write voices down.";
    await expect(said(page)).toHaveText(reason, { timeout: 240_000 });
    await expect(toggle(page)).toBeHidden();
    expect(await page.evaluate(() => caches.keys())).not.toContain('grill-room-stt:' + offer.revision);

    // Gone for this revision, the reason still there; a new revision offers it again.
    await offerTo(page, room.id, offer);
    await expect(toggle(page)).toBeHidden();
    await expect(said(page)).toHaveText(reason);
    await offerTo(page, room.id, fakeOffer('rev-next'));
    await expect(toggle(page)).toBeVisible();
    await expect(said(page)).toHaveText('Downloads 432 MB once');
  });
});
