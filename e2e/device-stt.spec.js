import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { chromium } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, playSTT, playEvents } from './helpers.js';
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

// realBar lowers the self-test's speed bar for the real-model specs: a CI runner's speed must not
// decide them. The measured RTFx is still logged; the bar itself is covered by the verdict test.
const realBar = (page) => page.addInitScript(() => { globalThis.grillSelfTestBar = { wasm: 0.5, webgpu: 0.5 }; });

// stub overrides navigator properties before the page's scripts run, as a phone or a small
// machine reports them.
async function stub(page, nav, isolated = true) {
  await page.addInitScript(([nav, isolated]) => {
    // Every reload of these specs would otherwise wait 12 s for the engine (010-AC21).
    globalThis.grillReloadDelayMs ??= 0;
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
    await realBar(page);
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
    // Ready, then the pipeline takes the voice (010-AC17), Silero loaded from the engine's bytes.
    await expect(said(page)).toHaveText('Transcribed on this device', { timeout: 240_000 });
    expect(await page.evaluate(() => window.saidLines)).toContain('Ready: this device can write your voice down.');
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
    await realBar(page);
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

// 010-AC2: the self-test's bars, on fixed measures: the words first, then the speed, each at its
// boundary.
test('the self-test verdict', async ({ browser, request }) => {
  const { pages: [page] } = await roomWith(browser, request, ['Alice']);
  const verdict = (backend, rtfx, w) => page.evaluate((a) => import('/static/stt/device.js').then((m) => m.verdict(...a)), [backend, rtfx, w]);
  expect(await verdict('wasm', 3, 0.2)).toBe('');
  expect(await verdict('wasm', 2.99, 0)).toBe('This device is too slow to write voices down: 3.0 times real time, it needs 3.');
  expect(await verdict('webgpu-hybrid', 8, 0)).toBe('');
  expect(await verdict('webgpu-hybrid', 7.99, 0)).toMatch(/it needs 8\.$/);
  expect(await verdict('wasm', 10, 0.21)).toBe("This device got the test sentence wrong (21 % of words), so it can't write voices down.");
  // A clip decoded to nothing, slowly: the words decide.
  expect(await verdict('wasm', 0.1, 1)).toMatch(/^This device got the test sentence wrong \(100 %/);
});

// The cuts of stt/grill_stt/vad.py's Segmenter on the same probabilities, fed in 1000-sample
// chunks: generated by running it with a fake probs function (uv run --with numpy), each window
// holding its own index so a cut names the windows it kept.
const S = 0.9;
const N = 0.1;
const fill = (...runs) => runs.flatMap(([p, n]) => Array(n).fill(p));
const pause = (start, windows, gap = null) => ({ start, windows, silent: 0.8, gap, forced: false });
const segmenterCases = [
  ['pause cut', fill([N, 20], [S, 30], [N, 40]), [pause(15, 41)], []],
  // 400 ms is 12.5 windows: Python rounds half to even, 12, where Math.round gives 13.
  ['12 voiced windows, the minimum', fill([N, 10], [S, 12], [N, 30]), [pause(5, 23)], []],
  ['11 voiced windows, a cough', fill([N, 10], [S, 11], [N, 30]), [], []],
  ['between thresholds holds the cut', fill([N, 5], [S, 20], [0.4, 30], [N, 30]), [pause(0, 61)], []],
  ['a 24-window pause does not cut', fill([N, 3], [S, 15], [N, 24], [S, 15], [N, 26]), [pause(0, 63)], []],
  // 018-AC2: the utterance after a forced cut follows it with no gap.
  ['15 s forced cut', fill([N, 2], [S, 500], [N, 30]), [{ start: 0, windows: 469, silent: 0, gap: null, forced: true }, pause(469, 39, 0)], []],
  ['speech at the end flushes', fill([N, 8], [S, 40]), [], [{ start: 3, windows: 45, silent: 0, gap: null, forced: false }]],
  ['threshold edges', fill([N, 4], [0.5, 14], [0.35, 30], [0.3499, 25]), [pause(0, 54)], []],
  // 018-AC2: 30 windows from the last voiced window of one utterance to the first of the next.
  ['gap after a pause', fill([N, 3], [S, 20], [N, 30], [S, 20], [N, 30]), [pause(0, 29), pause(48, 31, 0.96)], []],
  // 018-AC2: a cap reached in a pause, then a long pause: the gap is real, not 0; also when the quiet
  // cut leaves a tail too short to post.
  ['a long pause after a forced cut', fill([S, 460], [N, 200], [S, 20], [N, 30]),
    [{ start: 0, windows: 469, silent: 0, gap: null, forced: true }, pause(655, 31, 6.4)], []],
  ['a dropped tail after a forced cut', fill([S, 440], [0, 1], [S, 5], [N, 123], [S, 20], [N, 30]),
    [{ start: 0, windows: 441, silent: 0.896, gap: null, forced: true }, pause(564, 31, 4.128)], []],
  // 018-AC3: the cap cuts on the 0.2 dip of its last 3 s, not on the deeper one before them, and the
  // rest of the search opens the next utterance.
  ['quiet cut', fill([S, 300], [N, 1], [S, 100], [0.2, 1], [S, 20], [0.3, 1], [S, 100], [N, 30]),
    [{ start: 0, windows: 402, silent: 2.144, gap: null, forced: true }, pause(402, 127, 0)], []],
];

// 010-AC9: the device cuts utterances where the server's worker does.
test('the segmenter cuts as the worker does', async ({ browser, request }) => {
  const { pages: [page] } = await roomWith(browser, request, ['Alice']);
  for (const [name, probs, cuts, flushed] of segmenterCases) {
    const got = await page.evaluate(async (probs) => {
      const { Segmenter, WINDOW } = await import('/static/stt/segmenter.js');
      const seg = new Segmenter();
      const cut = (u) => ({ start: u.pcm[0], windows: u.pcm.length / WINDOW, silent: u.silentS, gap: u.gapS, forced: u.forced });
      const out = [];
      probs.forEach((p, i) => {
        const u = seg.push(new Float32Array(WINDOW).fill(i), p, i);
        if (u) out.push(cut(u));
      });
      const tail = seg.flush();
      return { cuts: out, flushed: tail ? [cut(tail)] : [] };
    }, probs);
    expect(got, name).toEqual({ cuts, flushed });
  }
  // The start and the end of speech are the times of the first and the last voiced windows, not
  // the pad's or the cut's.
  const voiced = await page.evaluate(async () => {
    const { Segmenter, WINDOW } = await import('/static/stt/segmenter.js');
    const seg = new Segmenter();
    let u = null;
    for (let i = 0; !u && i < 100; i++) u = seg.push(new Float32Array(WINDOW), i >= 5 && i < 25 ? 0.9 : 0.1, 1000 + i);
    return [u.firstVoiced, u.lastVoiced];
  });
  expect(voiced).toEqual([1005, 1024]);
  // 018-AC2: a flush ends the audio, as a track's end on the server: the next utterance has no gap.
  const gaps = await page.evaluate(async () => {
    const { Segmenter, WINDOW } = await import('/static/stt/segmenter.js');
    const seg = new Segmenter();
    const run = (probs) => probs.map((p) => seg.push(new Float32Array(WINDOW), p)).filter(Boolean).map((u) => u.gapS);
    const talk = [...Array(20).fill(0.9), ...Array(30).fill(0.1)];
    return [run(talk), run(talk), run(Array(20).fill(0.9)), seg.flush()?.gapS, run(talk)];
  });
  expect(gaps).toEqual([[null], [0.96], [], 0.96, [null]]);
  // 018-AC2: nor does the pre-roll from before the flush lead the next utterance.
  const first = await page.evaluate(async () => {
    const { Segmenter, WINDOW } = await import('/static/stt/segmenter.js');
    const seg = new Segmenter();
    const push = (from, probs) => probs.map((p, i) => seg.push(new Float32Array(WINDOW).fill(from + i), p)).filter(Boolean);
    push(0, Array(10).fill(0.1));
    seg.flush();
    return push(10, [...Array(20).fill(0.9), ...Array(30).fill(0.1)]).map((u) => u.pcm[0]);
  });
  expect(first).toEqual([10]);
});

// 010-AC9: the worker's plausibility filter on device lines. The ratios are Python's
// len(raw) / len(zlib.compress(raw)) on the same texts.
test('the plausibility filter drops what the worker drops', async ({ browser, request }) => {
  const { pages: [page] } = await roomWith(browser, request, ['Alice']);
  const cases = [
    ['Bonjour à tous, on commence la réunion.', 0.8367, ''],
    ['on y va on y va on y va on y va on y va', 2.0526, ''],
    ['on y va on y va on y va on y va on y va on y va', 2.4737, 'repetition'],
    ['merci merci merci merci merci merci merci merci merci merci merci merci', 4.1765, 'repetition'],
    ['Привет всем, как дела сегодня', 0.9138, 'script'],
    ['Le café Привет', 0.7, 'script'],
    ['Ça va, Ægir ?', 0.6522, ''],
    // No letters at all is no script fault, as on the server.
    ['12 34 !!', 0.5, ''],
    ['', 0, 'empty'],
    ['   ', null, 'empty'],
  ];
  const got = await page.evaluate(async (texts) => {
    const f = await import('/static/stt/filter.js');
    const out = [];
    for (const t of texts) out.push([Math.round((await f.compressionRatio(t)) * 1e4) / 1e4, await f.implausible(t)]);
    return out;
  }, cases.map(([t]) => t));
  cases.forEach(([text, ratio, why], i) => {
    if (ratio !== null) expect(got[i][0], text).toBe(ratio);
    expect(got[i][1], text).toBe(why);
  });
  // 9 Latin letters of 10 pass, 8 of 10 do not.
  expect(await page.evaluate(() => import('/static/stt/filter.js').then((f) => [f.latinShare('abcdefghiж'), f.latinShare('abcdefghжж')]))).toEqual([0.9, 0.8]);
});

// 010-AC17: the fallback and the return, on injected latencies and an injected clock.
test('the fallback policy', async ({ browser, request }) => {
  const { pages: [page] } = await roomWith(browser, request, ['Alice']);
  const run = (script) => page.evaluate(async (script) => {
    const { Policy } = await import('/static/stt/policy.js');
    let t = 0;
    const p = new Policy(() => t);
    const out = [];
    for (const [op, v] of script) {
      if (op === 'at') t = v * 1000;
      else if (op === 'lat') out.push(p.record(v * 1000));
      else if (op === 'queue') out.push(p.waiting(v));
      else if (op === 'fail') out.push(p.fail());
      else if (op === 'posts') out.push(p.posts(v * 1000));
    }
    return out;
  }, script);
  // Two slow lines are not enough to judge; the third puts p95 above 4 s.
  expect(await run([['lat', 5], ['lat', 5], ['lat', 1]])).toEqual(['device', 'device', 'slow']);
  expect(await run([['lat', 1], ['lat', 1], ['lat', 4]])).toEqual(['device', 'device', 'device']);
  // More than 2 utterances waiting.
  expect(await run([['queue', 2], ['queue', 3]])).toEqual(['device', 'slow']);
  // An engine error: good latencies never bring it back.
  expect(await run([['fail'], ['lat', 1], ['lat', 1], ['lat', 1], ['at', 600], ['lat', 1], ['queue', 0]])).toEqual(Array(6).fill('failed'));
  // Return: the slow lines age out of the last 60 s, three fast ones with an empty queue.
  expect(await run([['lat', 5], ['lat', 5], ['lat', 5], ['at', 61], ['queue', 1], ['lat', 1], ['lat', 1], ['lat', 1], ['queue', 0]]))
    .toEqual(['device', 'device', 'slow', 'slow', 'slow', 'slow', 'slow', 'device']);
  // Between 3 s and 4 s it stays where it is: no flapping.
  expect(await run([['lat', 5], ['lat', 5], ['lat', 5], ['at', 61], ['lat', 3.5], ['lat', 3.5], ['lat', 3.5], ['lat', 3]]))
    .toEqual(['device', 'device', 'slow', 'slow', 'slow', 'slow', 'slow']);
  // Handed back at 10 s, the worker may skip the voice until 15 s: speech that started before then
  // still posts from the device, later speech is only measured.
  expect(await run([['posts', 9], ['at', 10], ['queue', 3], ['posts', 5], ['posts', 11], ['posts', 14.9], ['posts', 15], ['posts', 16],
    ['at', 30], ['posts', 11], ['posts', 31]])).toEqual([true, 'slow', true, true, true, false, false, true, false]);
  // A return posts everything again; the next fall back opens a window of its own.
  expect(await run([['lat', 5], ['lat', 5], ['lat', 5], ['at', 61], ['lat', 1], ['lat', 1], ['lat', 1], ['posts', 61], ['at', 70], ['queue', 3], ['posts', 74.9], ['posts', 75]]))
    .toEqual(['device', 'device', 'slow', 'slow', 'slow', 'device', true, 'slow', true, false]);
  // An engine failure posts nothing more.
  expect(await run([['fail'], ['posts', 0]])).toEqual(['failed', false]);
  // A hung engine fails after 4 times the utterance's length, never under 10 s.
  expect(await page.evaluate(() => import('/static/stt/policy.js').then((m) => [0.5, 2.5, 3, 15].map(m.deadline)))).toEqual([10_000, 10_000, 12_000, 60_000]);
  // In fallback, utterances decoded only to measure the device are kept to 3, the newest; a line to
  // post is never dropped.
  const shed = (q) => page.evaluate((q) => import('/static/stt/policy.js').then((m) => m.shed(q.map(([id, post]) => ({ id, post }))).map((u) => u.id)), q);
  expect(await shed([[1, false], [2, false], [3, false]])).toEqual([1, 2, 3]);
  expect(await shed([[1, false], [2, true], [3, false], [4, false], [5, false], [6, false]])).toEqual([2, 4, 5, 6]);
  expect(await shed([[1, true], [2, true], [3, true], [4, true]])).toEqual([1, 2, 3, 4]);
  // At most one return every 2 minutes: back at 61 s, slow again, fast again at 140 s, still
  // slow at 180 s, back at 181 s.
  expect(await run([
    ['lat', 5], ['lat', 5], ['lat', 5],
    ['at', 61], ['lat', 1], ['lat', 1], ['lat', 1],
    ['at', 62], ['queue', 3], ['queue', 0],
    ['at', 140], ['lat', 1], ['lat', 1], ['lat', 1],
    ['at', 180], ['lat', 1],
    ['at', 181], ['lat', 1],
  ])).toEqual(['device', 'device', 'slow', 'slow', 'slow', 'device', 'slow', 'slow', 'slow', 'slow', 'slow', 'slow', 'device']);
});

// 010-AC12: a voice line's tip says who wrote it down, the People badge has a device variant with
// its own glyph and words, and the details card says where the voice goes. Nothing else differs.
test('device and server marks', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const ids = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.participants.map((p) => p.id)), room.id);
  const [a, b, c] = [ids[0], ids[1], 999999];
  const ts = new Date().toISOString();
  const line = (id, pid, sender, source) => ['', { id, kind: 'voice', participant_id: pid, sender, body: 'Line ' + id, html: '<p>Line ' + id + '</p>', ts, ...(source && { source }) }];
  // The chat shows Written by default (020-AC5): these lines are read in All.
  await alice.evaluate(() => localStorage.setItem('grill-room-chat-filter', 'all'));
  await playEvents(alice, room.id, [
    ['stt', { available: true, listening: true, transcribed: [b], engines: [] }],
    ['presence', { people: [
      { id: a, here: true, audio: true, mic: true, muted: false, deaf: false, device_stt: true },
      { id: b, here: true, audio: true, mic: true, muted: false, deaf: false, device_stt: false },
    ] }],
    line(1000001, a, 'Alice', 'device'), line(1000002, b, 'Bob', 'server'), line(1000003, c, 'Carol'),
  ]);
  await bob.close();
  const msg = (n) => alice.locator(`.msg.voice[data-id="${n}"]`);
  await expect(msg(1000001).locator('.voice-src')).toHaveAttribute('data-tip', "Transcribed on Alice's device");
  await expect(msg(1000002).locator('.voice-src')).toHaveAttribute('data-tip', 'Transcribed by the server');
  // A line stored before 010 has no source: the server wrote it.
  await expect(msg(1000003).locator('.voice-src')).toHaveAttribute('data-tip', 'Transcribed by the server');
  await msg(1000001).locator('.voice-src').hover();
  await expect(alice.locator('#person-tip')).toHaveText("Transcribed on Alice's device");
  // To the screen reader and the keyboard too, and on a tap.
  await expect(msg(1000001).locator('.voice-src')).toHaveAccessibleName("Voice line. Transcribed on Alice's device");
  await expect(msg(1000002).locator('.voice-src')).toHaveAccessibleName('Voice line. Transcribed by the server');
  await expect(msg(1000002).locator('.voice-src')).toHaveAttribute('tabindex', '0');
  await alice.mouse.move(0, 0);
  await expect(alice.locator('#person-tip')).toBeHidden();
  await msg(1000002).locator('.voice-src').evaluate((el) => el.dispatchEvent(new PointerEvent('click', { pointerType: 'touch', bubbles: true })));
  await expect(alice.locator('#person-tip')).toHaveText('Transcribed by the server');
  expect(await msg(1000001).getAttribute('class')).toBe(await msg(1000002).getAttribute('class'));
  await expect(msg(1000001).locator('.meta')).toHaveText(/^Alice \(voice\) /);
  await expect(alice.locator('#messages [title]')).toHaveCount(0);

  const badge = (name) => alice.locator('#participants li').filter({ has: alice.locator('.name', { hasText: name }) }).locator('.tag-transcribed');
  await expect(badge('Alice')).toHaveAttribute('data-icon', 'transcribed-device');
  await expect(badge('Alice')).toHaveAccessibleName('Their device transcribes this voice');
  await expect(badge('Alice')).toHaveAttribute('data-tip', 'Their device transcribes this voice');
  await expect(badge('Bob')).toHaveAttribute('data-icon', 'transcribed');
  await expect(badge('Bob')).toHaveAccessibleName('The transcription hears this voice');
  // Its own glyph, not a colour change.
  const glyph = (name) => badge(name).locator('svg').innerHTML();
  expect(await glyph('Alice')).not.toBe(await glyph('Bob'));

  const card = async (name) => {
    await alice.locator('#participants li .name', { hasText: name }).hover();
    await expect(alice.locator('#person-card strong')).toHaveText(name);
    return alice.locator('#person-card li').allTextContents();
  };
  expect(await card('Alice')).toContain('Transcribed on their device');
  await alice.keyboard.press('Escape');
  expect(await card('Bob')).toContain('Transcribed by the server');
});

// A stand-in for stt/engine.js: ready at once, the real Silero and runtime bytes for the pipeline,
// and a fixed text for every utterance, so the pipeline runs without the 400 MB model. A test sets
// how long each decode takes over the "stub-engine" BroadcastChannel.
const stubEngine = `let rev = '';
let delay = 0;
new BroadcastChannel('stub-engine').onmessage = (e) => { delay = e.data.delay; };
onmessage = async (e) => {
  const d = e.data;
  if (d.cmd === 'load') {
    rev = d.revision;
    postMessage({ ev: 'ready', backend: 'wasm', isolated: crossOriginIsolated, ms: {} });
  } else if (d.cmd === 'pipe') {
    const get = (n) => fetch('/models/' + rev + '/' + n).then((r) => r.arrayBuffer()).then((b) => new Uint8Array(b));
    d.port.onmessage = (m) => setTimeout(() => d.port.postMessage({ ev: 'text', id: m.data.id, text: 'Bonjour à tous.', ms: 1 }), delay);
    d.port.postMessage({ ev: 'vad', silero: await get('silero_vad_v6.onnx'), wasm: await get('ort-wasm-simd-threaded.jsep.wasm') });
  }
};
`;

// livePipeline joins a room with the device pipeline running on the stub engine and the real
// Silero. The server side is played by routes: lines and device beats are recorded, and hang(kind)
// makes the next line or beat hang until released. log lists the device beats and the hung ones'
// failures and the engine's loads, in order.
async function livePipeline(browser, request) {
  const offer = await realOffer(request);
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await stub(page, desktop);
  // Every audio context the page makes, so a test can stop the tap's as a dead tap would.
  await page.addInitScript(() => {
    const Base = window.AudioContext;
    window.audioContexts = [];
    window.AudioContext = class extends Base {
      constructor(...args) {
        super(...args);
        window.audioContexts.push(this);
      }
    };
  });
  const t = { page, room, lines: [], beats: [], log: [], hung: {}, released: [] };
  await page.context().route('**/static/stt/engine.js', async (r) => {
    t.log.push('engine');
    return r.fulfill({ response: await r.fetch(), body: stubEngine });
  });
  const hangNext = (kind, r) => {
    if (!t.hung[kind]?.armed || !t.hung[kind].which(r.request().postDataJSON())) return null;
    t.hung[kind] = { url: r.request().url() };
    return new Promise((release) => t.released.push(release));
  };
  t.hang = (kind, which = () => true) => { t.hung[kind] = { armed: true, which }; };
  await page.context().route(`**/api/rooms/${room.id}/voice/device-lines`, async (r) => {
    t.lines.push(r.request().postDataJSON());
    const wait = hangNext('line', r);
    if (wait) return wait;
    return r.fulfill({ status: 201, json: {} });
  });
  await page.context().route(`**/api/rooms/${room.id}/presence`, async (r) => {
    const body = r.request().postDataJSON();
    if (!('device_stt' in body)) return r.continue();
    const wait = hangNext('beat', r);
    t.log.push(wait ? 'hung' : 'beat:' + body.device_stt);
    if (wait) return wait;
    t.beats.push(body);
    return r.fulfill({ status: 204 });
  });
  page.context().on('requestfailed', (r) => { if (r.url().endsWith('/presence')) t.log.push('failed'); });
  // A device that passed earlier skips the self-test.
  await page.evaluate((rev) => localStorage.setItem('grill-room-device-stt-passed:' + rev, 'wasm'), offer.revision);
  await offerTo(page, room.id, offer);
  await switchOn(page);
  await expect(said(page)).toHaveText('Transcribed on this device');
  t.tab = await page.evaluate(() => window.grillTab);
  t.stats = () => page.evaluate(() => window.grillDeviceSTT.pipeline);
  // ticks waits for n of the pipeline's 1 s ticks: the test's clock.
  t.ticks = async (n) => {
    const at = (await t.stats()).ticks;
    await expect.poll(async () => (await t.stats()).ticks).toBeGreaterThanOrEqual(at + n);
    return t.stats();
  };
  // mic plays the self-test clip, at the page's default rate, as the live mic; null mutes it. A
  // looped one talks on; a chopped one says 1.5 s, then pauses 1 s, an utterance every 2.5 s.
  t.mic = (which, loop = false, chop = false) => page.evaluate(async ([which, loop, chop]) => {
    if (which && !window.fakeMics?.[which]) {
      const ctx = new AudioContext();
      let buf = await ctx.decodeAudioData(await (await fetch('/static/stt/selftest-fr.wav')).arrayBuffer());
      if (chop) {
        const [say, gap] = [1.5 * buf.sampleRate, buf.sampleRate];
        const from = buf.getChannelData(0);
        const n = Math.floor(from.length / say);
        const out = ctx.createBuffer(1, n * (say + gap), buf.sampleRate);
        for (let k = 0; k < n; k++) out.getChannelData(0).set(from.subarray(k * say, (k + 1) * say), k * (say + gap));
        buf = out;
      }
      const src = ctx.createBufferSource();
      src.buffer = buf;
      src.loop = loop;
      const dst = ctx.createMediaStreamDestination();
      src.connect(dst);
      src.start();
      window.fakeMics = { ...window.fakeMics, [which]: dst.stream.getAudioTracks()[0] };
    }
    window.grillMic = which ? window.fakeMics[which] : null;
    dispatchEvent(new CustomEvent('grill:mic', { detail: { track: window.grillMic } }));
  }, [which, loop, chop]);
  return t;
}

// 010-AC9, 010-AC13, 010-AC17: the pipeline taps the live mic only, follows a new mic track, cuts
// the speech with the real Silero, posts the line, and beats its device flag from its worker, one
// beat at a time, while audio comes in or the mic is muted. Turning it off drops the flag at once
// and still posts the speech in progress; the next engine waits for the last one to end.
test('the pipeline hears the live mic only and beats its flag', async ({ browser, request }) => {
  test.setTimeout(120_000);
  const t = await livePipeline(browser, request);
  try {
    // No mic yet, as muted: no audio, and the beats hold the voice for when it comes.
    const none = await t.ticks(2);
    expect(none.windows).toBe(0);
    await expect.poll(() => t.beats.length).toBeGreaterThanOrEqual(1);
    // The clip once, then silence: one utterance after the 800 ms pause.
    await t.mic('first');
    await expect.poll(async () => (await t.stats()).posted, { timeout: 30_000 }).toBeGreaterThanOrEqual(1);
    expect(t.lines[0]).toEqual({ body: 'Bonjour à tous.' });
    const live = await t.stats();
    expect(live.utterances).toBeGreaterThanOrEqual(1);
    expect(live.latencies.length).toBeGreaterThanOrEqual(1);
    expect(live.mode).toBe('device');
    expect(t.beats.length).toBeGreaterThanOrEqual(1);
    expect(t.beats.every((b) => b.tab === t.tab && b.device_stt === true)).toBe(true);

    // Muted: once the windows already sent are through, no window comes, and the beats go on, so an
    // unmute does not find the worker transcribing.
    await t.mic(null);
    const muted = await t.ticks(2);
    const later = await t.ticks(2);
    expect(later.windows).toBe(muted.windows);
    expect(later.beats).toBeGreaterThan(muted.beats);
    // Back live: windows again.
    await t.mic('first');
    await expect.poll(async () => (await t.stats()).windows).toBeGreaterThan(muted.windows);

    // A new mic track, the old one ended, as LiveKit restarts it on a device switch: the tap follows.
    const before = (await t.stats()).windows;
    await t.page.evaluate(() => window.fakeMics.first.stop());
    await t.mic('second', true);
    await expect.poll(async () => (await t.stats()).windows).toBeGreaterThan(before + 100);

    // A beat that hangs: the next one leaves only once it failed, 2 s on, never beside it.
    t.log.length = 0;
    t.hang('beat');
    await expect.poll(() => t.log.slice(0, 3)).toEqual(['hung', 'failed', 'beat:true']);

    // Off in the middle of speech: the flag drops at once and the speech in progress still posts.
    await expect.poll(async () => (await t.stats()).speaking).toBe(true);
    const posted = t.lines.length;
    await toggle(t.page).getByRole('checkbox').uncheck();
    await expect.poll(() => t.beats.at(-1)).toEqual({ tab: t.tab, device_stt: false });
    await expect.poll(() => t.lines.length).toBeGreaterThan(posted);

    // On again, then a dead tap (its audio context stops, the track still tapped): no window, no beat.
    const contexts = await t.page.evaluate(() => window.audioContexts.length);
    await toggle(t.page).getByRole('checkbox').check();
    await expect(said(t.page)).toHaveText('Transcribed on this device');
    await expect.poll(async () => (await t.stats()).windows).toBeGreaterThan(0);
    await t.page.evaluate((n) => window.audioContexts[n].suspend(), contexts);
    const dead = await t.ticks(2);
    expect(await t.ticks(2)).toMatchObject({ windows: dead.windows, beats: dead.beats });
    // Off, its false beat hung, and on at once: the new engine loads only once the old pipeline is
    // through, its engine with it.
    t.log.length = 0;
    t.hang('beat', (b) => b.device_stt === false);
    await toggle(t.page).getByRole('checkbox').uncheck();
    await toggle(t.page).getByRole('checkbox').check();
    await expect.poll(() => t.log.slice(0, 3)).toEqual(['hung', 'failed', 'engine']);
  } finally {
    for (const release of t.released) release();
  }
});

// 010-AC17: a device that lags hands the voice back, its flag down at once, and takes it back, its
// flag up at once, once it keeps up over the last 60 s.
test('a lagging device falls back and returns', async ({ browser, request }) => {
  test.setTimeout(240_000);
  const t = await livePipeline(browser, request);
  const engine = (delay) => t.page.evaluate((delay) => new BroadcastChannel('stub-engine').postMessage({ delay }), delay);
  try {
    // 8 s a decode, under the 10 s deadline, and an utterance every 2.5 s: the queue grows.
    await engine(8000);
    await t.mic('chopped', true, true);
    await expect.poll(async () => (await t.stats()).mode, { timeout: 60_000 }).toBe('slow');
    await expect(said(t.page)).toHaveText('Transcribed by the server: this device is too slow right now');
    await expect.poll(() => t.beats.at(-1)?.device_stt).toBe(false);
    // The next chopped utterance starts within 5 s of the fall back: the device still posts it.
    const posted = t.lines.length;
    await expect.poll(async () => (await t.stats()).graced, { timeout: 30_000 }).toBeGreaterThanOrEqual(1);
    await t.page.evaluate(() => {
      window.seen = [];
      addEventListener('grill:device-pipeline', (e) => window.seen.push({ mode: e.detail.mode, beats: e.detail.beats }));
    });
    // Fast again: once the slow lines left the last 60 s, the voice comes back.
    await engine(0);
    await expect.poll(() => t.lines.length).toBeGreaterThan(posted);
    await expect.poll(async () => (await t.stats()).mode, { timeout: 150_000, intervals: [1000] }).toBe('device');
    await expect(said(t.page)).toHaveText('Transcribed on this device');
    // The counters sent with the return already count its beat: it left with the mode change.
    const seen = await t.page.evaluate(() => window.seen);
    const back = seen.findIndex((s) => s.mode === 'device');
    expect(back).toBeGreaterThan(0);
    expect(seen[back].beats).toBeGreaterThan(seen[back - 1].beats);
    await expect.poll(() => t.beats.at(-1)?.device_stt).toBe(true);
  } finally {
    for (const release of t.released) release();
  }
});

// 010-AC17: a post that hangs counts as lost after 10 s and still gives the policy its latency; a
// mic the browser cannot tap hands the voice back.
test('a hung line post still measures the device', async ({ browser, request }) => {
  test.setTimeout(90_000);
  const t = await livePipeline(browser, request);
  try {
    t.hang('line');
    await t.mic('first');
    await expect.poll(async () => (await t.stats()).lost, { timeout: 40_000 }).toBe(1);
    expect(t.lines[0]).toEqual({ body: 'Bonjour à tous.' });
    // The first line's latency, measured when its post gave up; the lines after it went through.
    const s = await t.stats();
    expect(s.latencies[0]).toBeGreaterThanOrEqual(10_000);
    await expect.poll(async () => (await t.stats()).posted).toBe(t.lines.length - 1);

    // A track the audio graph refuses (here a video one): the voice goes back to the server, the
    // engine stops and the choice stays.
    await t.page.evaluate(() => {
      const track = document.createElement('canvas').captureStream().getVideoTracks()[0];
      window.grillMic = track;
      dispatchEvent(new CustomEvent('grill:mic', { detail: { track } }));
    });
    await expect(said(t.page)).toHaveText("Transcribed by the server: this browser can't pass your microphone to the voice-to-text engine.");
    expect(await t.page.evaluate(() => [window.grillDeviceSTT.state, localStorage.getItem('grill-room-device-stt')])).toEqual(['off', '1']);
    await expect.poll(() => t.beats.at(-1)?.device_stt).toBe(false);
  } finally {
    for (const release of t.released) release();
  }
});

// 010-AC21: on a reload the engine waits, so the old page's memory goes first; the server keeps the
// voice meanwhile. A fresh load starts at once.
test('a reload starts the engine late, a fresh load at once', async ({ browser, request }) => {
  test.setTimeout(90_000);
  const offer = await realOffer(request);
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  expect(await page.evaluate(() => import('/static/stt/device.js').then((m) => m.RELOAD_DELAY_MS))).toBe(12_000);
  const delay = 4000;
  await page.addInitScript((delay) => { globalThis.grillReloadDelayMs = delay; }, delay);
  await stub(page, desktop);
  await page.addInitScript(() => {
    window.saidLines = [];
    addEventListener('DOMContentLoaded', () => {
      const el = document.getElementById('device-stt-status');
      new MutationObserver(() => window.saidLines.push(el.textContent)).observe(el, { childList: true, characterData: true, subtree: true });
    });
  });
  // The stub engine stamps its ready with the time, to compare with the page's load.
  const engine = stubEngine.replace('ms: {}', 'ms: { at: performance.timeOrigin + performance.now() }');
  const log = [];
  await page.context().route('**/static/stt/engine.js', async (r) => {
    log.push('engine');
    return r.fulfill({ response: await r.fetch(), body: engine });
  });
  await page.context().route(`**/api/rooms/${room.id}/presence`, (r) => {
    const body = r.request().postDataJSON();
    if (!('device_stt' in body)) return r.continue();
    log.push('beat:' + body.device_stt);
    return r.fulfill({ status: 204 });
  });
  // Chosen earlier, passed earlier; offerTo reloads the page.
  await page.evaluate((rev) => {
    localStorage.setItem('grill-room-device-stt', '1');
    localStorage.setItem('grill-room-device-stt-passed:' + rev, 'wasm');
  }, offer.revision);
  await offerTo(page, room.id, offer);
  expect(await page.evaluate(() => performance.getEntriesByType('navigation')[0].type)).toBe('reload');
  await audio(page, true);
  await expect(said(page)).toHaveText('Transcribed by the server: starting on this device in a few seconds');
  expect(log).toEqual([]);
  await expect(said(page)).toHaveText('Transcribed on this device', { timeout: 30_000 });
  const startedAfter = await page.evaluate(() => window.grillDeviceSTT.load.at - performance.timeOrigin);
  expect(startedAfter).toBeGreaterThanOrEqual(delay);
  // The flag comes up only with the device's own pipeline, after the engine.
  await expect.poll(() => log.includes('beat:true')).toBe(true);
  expect(log[0]).toBe('engine');

  // A fresh load of the same room: no wait.
  await page.goto(page.url());
  expect(await page.evaluate(() => performance.getEntriesByType('navigation')[0].type)).toBe('navigate');
  await expect.poll(() => page.evaluate(() => window.grillDeviceSTT?.checked)).toBe(offer.revision);
  await audio(page, true);
  await expect(said(page)).toHaveText('Transcribed on this device', { timeout: 30_000 });
  expect(await page.evaluate(() => window.saidLines)).not.toContain('Transcribed by the server: starting on this device in a few seconds');
});

// 010-AC17: an engine that dies while it transcribes leaves the voice on the server until the page
// reloads, the choice kept for that reload.
test('an engine crash keeps the voice on the server until a reload', async ({ browser, request }) => {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await stub(page, desktop);
  const crash = `onmessage = (e) => {
  if (e.data.cmd !== 'load') return;
  postMessage({ ev: 'ready', backend: 'wasm', isolated: crossOriginIsolated, ms: {} });
  setTimeout(() => { throw new Error('the engine crashed'); }, 0);
};
`;
  await page.context().route('**/static/stt/engine.js', async (r) => r.fulfill({ response: await r.fetch(), body: crash }));
  await page.evaluate(() => localStorage.setItem('grill-room-device-stt-passed:rev-crash', 'wasm'));
  await offerTo(page, room.id, fakeOffer('rev-crash'));
  await switchOn(page);
  const reason = 'Transcribed by the server: the voice-to-text engine failed on this device. Reload the page to try again.';
  await expect(said(page)).toHaveText(reason);
  await expect(toggle(page).getByRole('checkbox')).not.toBeChecked();
  expect(await page.evaluate(() => [window.grillDeviceSTT.state, localStorage.getItem('grill-room-device-stt')])).toEqual(['off', '1']);
  // Back in the audio, or ticked again: still the server.
  await audio(page, false);
  await audio(page, true);
  await toggle(page).getByRole('checkbox').click();
  await expect(toggle(page).getByRole('checkbox')).not.toBeChecked();
  expect(await page.evaluate(() => window.grillDeviceSTT.state)).toBe('off');
  await expect(said(page)).toHaveText(reason);
});

// 010-AC9: the mic comes at the context's own rate; the resampler keeps a tone's level and pitch,
// filters what would fold back above 8 kHz, and hands the segmenter 512-sample windows.
test('the resampler', async ({ browser, request }) => {
  const { pages: [page] } = await roomWith(browser, request, ['Alice']);
  const run = (rate, hz) => page.evaluate(async ([rate, hz]) => {
    const { Resampler } = await import('/static/stt/resample.js');
    const r = new Resampler(rate);
    const all = [];
    let sizes = new Set();
    // 2 s in uneven chunks.
    for (let i = 0, k = 0; i < 2 * rate; k++) {
      const n = Math.min(k % 2 ? 2048 : 1000, 2 * rate - i);
      const chunk = Float32Array.from({ length: n }, (_, j) => Math.sin((2 * Math.PI * hz * (i + j)) / rate));
      for (const w of r.push(chunk)) {
        sizes.add(w.length);
        all.push(...w);
      }
      i += n;
    }
    // One second, past the filter's start-up.
    const s = all.slice(8000, 24000);
    const rms = Math.sqrt(s.reduce((a, x) => a + x * x, 0) / s.length);
    let crossings = 0;
    for (let i = 1; i < s.length; i++) if ((s[i - 1] < 0) !== (s[i] < 0)) crossings++;
    return { windows: all.length / 512, sizes: [...sizes], rms, crossings };
  }, [rate, hz]);
  for (const rate of [96000, 48000, 44100]) {
    const tone = await run(rate, 1000);
    expect(tone.sizes, String(rate)).toEqual([512]);
    // 2 s at 16 kHz, less the kernel's lookahead and the last partial window.
    expect(tone.windows, String(rate)).toBe(62);
    expect(tone.rms, String(rate)).toBeCloseTo(Math.SQRT1_2, 2);
    // 1 kHz over 1 s: 2000 sign changes.
    expect(Math.abs(tone.crossings - 2000), String(rate)).toBeLessThanOrEqual(2);
    // A 12 kHz tone has no place under 8 kHz: dropping samples would fold it to 4 kHz at full level.
    expect((await run(rate, 12000)).rms, String(rate)).toBeLessThan(0.01);
    // Just past the cutoff: the kernel widens with the input rate to keep the band edge in Hz.
    expect((await run(rate, 9500)).rms, String(rate)).toBeLessThan(0.01);
  }
});
