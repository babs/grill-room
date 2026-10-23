import { copyFileSync } from 'node:fs';
import { test, expect } from './fixtures.js';
import { roomWith, menuItem, noOverflow } from './helpers.js';

// fakes stands in for the browser APIs the sheet reads: the tab's memory (counted, or missing as
// outside Chrome) and the clipboard. The sheet refreshes every 173 ms instead of 5 s; the timers
// of that period are counted while pending, the measures while running.
async function fakes(page, { memory = true } = {}) {
  await page.addInitScript((memory) => {
    const period = 173;
    globalThis.grillDiagRefreshMs = period;
    window.rounds = { pending: new Set(), maxPending: 0, inFlight: 0, maxInFlight: 0 };
    const { setTimeout: set, clearTimeout: clear } = window;
    window.setTimeout = (fn, ms, ...args) => {
      if (ms !== period) return set(fn, ms, ...args);
      const id = set(() => {
        window.rounds.pending.delete(id);
        fn(...args);
      }, ms);
      window.rounds.pending.add(id);
      window.rounds.maxPending = Math.max(window.rounds.maxPending, window.rounds.pending.size);
      return id;
    };
    window.clearTimeout = (id) => {
      window.rounds.pending.delete(id);
      clear(id);
    };
    window.measures = 0;
    // release() lets every held measure end.
    window.held = [];
    window.release = () => window.held.splice(0).forEach((done) => done());
    Object.defineProperty(Performance.prototype, 'measureUserAgentSpecificMemory', {
      configurable: true,
      // A held measure waits for window.release(), as a browser takes its time to measure.
      value: memory ? async () => {
        window.measures++;
        const r = window.rounds;
        r.maxInFlight = Math.max(r.maxInFlight, ++r.inFlight);
        try {
          if (window.holdMeasure) await new Promise((done) => window.held.push(done));
          return { bytes: 123e6, breakdown: [] };
        } finally {
          r.inFlight--;
        }
      } : undefined,
    });
    window.copied = null;
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async (t) => { window.copied = t; } } });
  }, memory);
  await page.reload();
}

const sheet = (page) => page.locator('#diag');
// facts reads a section's rows as {label: value}.
const facts = (page, id) => page.locator(id).evaluate((dl) => Object.fromEntries([...dl.querySelectorAll('dt')].map((dt) => [dt.textContent, dt.nextElementSibling.textContent])));

// 010-AC22: Diagnostics in the user menu shows where the voice is transcribed and why, the engine,
// the model, the memory, the latency, the hand-overs and the connection; it measures the tab's
// memory only while open, and its copy holds no room id, token or participant name.
for (const [label, size] of [['desktop', { width: 1440, height: 900 }], ['phone', { width: 390, height: 844 }]]) {
  test(`diagnostics, ${label}`, async ({ browser, request }, testInfo) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await alice.setViewportSize(size);
    await fakes(alice);
    expect(await alice.evaluate(() => window.measures)).toBe(0);
    await menuItem(alice, 'Diagnostics', 'me');
    await expect(sheet(alice)).toBeVisible();
    await expect(sheet(alice).getByRole('heading')).toHaveText(['Diagnostics', 'Transcription', 'Connection', 'AI host app']);
    await expect.poll(() => facts(alice, '#diag-stt')).toMatchObject({ 'Tab memory': '123 MB' });
    const stt = await facts(alice, '#diag-stt');
    expect(Object.keys(stt)).toEqual(['Mode', 'Engine', 'Model', 'Device memory', 'Tab memory', 'End of speech to line', 'Hand-overs']);
    expect(stt.Mode).toMatch(/^(On this device|By the server: .+)$/);
    expect(stt['Device memory']).toMatch(/\(Chrome and Edge only, rounded and capped by the browser\)$/);
    await expect.poll(() => facts(alice, '#diag-net')).toMatchObject({ 'Event stream': 'open', Audio: 'not in the audio', 'Audio path': 'none' });
    // The room server's own version: the e2e build's.
    expect((await facts(alice, '#diag-net'))['App version']).toMatch(/^v\d+\.\d+\.\d+/);
    // Measured again while open.
    await expect.poll(() => alice.evaluate(() => window.measures)).toBeGreaterThanOrEqual(3);
    expect(await noOverflow(alice)).toEqual([]);
    // Both sections' values start at one x.
    expect(await alice.locator('#diag dd').evaluateAll((dds) => new Set(dds.map((d) => Math.round(d.getBoundingClientRect().left))).size)).toBe(1);
    const shot = testInfo.outputPath(`diagnostics-${label}.png`);
    await alice.screenshot({ path: shot });
    if (process.env.DIAG_SHOTS_DIR) copyFileSync(shot, `${process.env.DIAG_SHOTS_DIR}/diagnostics-${label}.png`);

    // The copy is in view without scrolling, the sheet's foot staying put.
    await expect(sheet(alice).getByRole('button', { name: 'Copy diagnostics' })).toBeInViewport({ ratio: 1 });
    await sheet(alice).getByRole('button', { name: 'Copy diagnostics' }).click({ trial: true });
    await sheet(alice).getByRole('button', { name: 'Copy diagnostics' }).click();
    await expect(alice.locator('#diag-said')).toHaveText('Copied');
    const text = await alice.evaluate(() => window.copied);
    expect(text).toContain('Room: Docs room');
    expect(text).toContain('Tab memory: 123 MB');
    for (const secret of [room.id, 'Alice', 'Bob']) expect(text).not.toContain(secret);
    // No token-like run: ids and secrets are long, the model revision is 12 hex digits.
    expect(text).not.toMatch(/[A-Za-z0-9_-]{20,}/);

    // Closed while a measure runs: it ends, and no refresh is left pending.
    await alice.evaluate(() => { window.holdMeasure = true; });
    await expect.poll(() => alice.evaluate(() => window.rounds.inFlight)).toBe(1);
    await alice.keyboard.press('Escape');
    await expect(sheet(alice)).toBeHidden();
    await alice.evaluate(() => { window.holdMeasure = false; window.release(); });
    await expect.poll(() => alice.evaluate(() => window.rounds.inFlight)).toBe(0);
    expect(await alice.evaluate(() => window.rounds.pending.size)).toBe(0);
  });
}

// 010-AC22: a browser without the memory measure says so.
test('diagnostics without the memory measure', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await fakes(alice, { memory: false });
  await menuItem(alice, 'Diagnostics', 'me');
  await expect.poll(() => facts(alice, '#diag-stt')).toMatchObject({ 'Tab memory': 'not available in this browser' });
});

// 010-AC22: the audio path from the selected candidate pair, and the latency percentiles.
test('diagnostics reads the audio path and the percentiles', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const path = (stats) => alice.evaluate((s) => import('/static/diagnostics.js').then((m) => m.icePath(s)), stats);
  const pair = { id: 'p', type: 'candidate-pair', localCandidateId: 'l' };
  const viaTransport = (local) => [{ id: 't', type: 'transport', selectedCandidatePairId: 'p' }, pair, { id: 'l', ...local }];
  expect(await path(viaTransport({ candidateType: 'host', protocol: 'udp' }))).toBe('direct, UDP');
  expect(await path(viaTransport({ candidateType: 'srflx', protocol: 'tcp' }))).toBe('direct, TCP');
  expect(await path(viaTransport({ candidateType: 'relay', protocol: 'tcp', relayProtocol: 'tls' }))).toBe('TURN/TLS');
  // Without a transport entry (Firefox), the nominated pair that succeeded.
  expect(await path([{ ...pair, nominated: true, state: 'succeeded' }, { id: 'l', candidateType: 'relay', relayProtocol: 'udp' }])).toBe('TURN/UDP');
  expect(await path([{ ...pair, nominated: true, state: 'in-progress' }, { id: 'l', candidateType: 'host', protocol: 'udp' }])).toBe('unknown');
  expect(await path([])).toBe('unknown');
  const pct = (xs, q) => alice.evaluate(([xs, q]) => import('/static/diagnostics.js').then((m) => m.pct(xs, q)), [xs, q]);
  const twenty = Array.from({ length: 20 }, (_, i) => i + 1);
  expect(await pct(twenty, 0.5)).toBe(10);
  expect(await pct(twenty, 0.95)).toBe(19);
  expect(await pct([7], 0.95)).toBe(7);
});

// 010-AC22: closed and opened again while a measure runs, the sheet runs one refresh round, not two.
test('diagnostics reopened during a measure runs one round', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await fakes(alice);
  await menuItem(alice, 'Diagnostics', 'me');
  await alice.evaluate(() => { window.holdMeasure = true; });
  await expect.poll(() => alice.evaluate(() => window.rounds.inFlight)).toBe(1);
  await alice.keyboard.press('Escape');
  await menuItem(alice, 'Diagnostics', 'me');
  // The reopened sheet's own first measure is held too: two measures wait, one per opening.
  await expect.poll(() => alice.evaluate(() => window.rounds.inFlight)).toBe(2);
  await alice.evaluate(() => {
    window.holdMeasure = false;
    window.rounds.maxPending = 0;
    window.rounds.maxInFlight = 0;
    window.release();
  });
  const from = await alice.evaluate(() => window.measures);
  await expect.poll(() => alice.evaluate(() => window.measures)).toBeGreaterThanOrEqual(from + 5);
  expect(await alice.evaluate(() => [window.rounds.maxPending, window.rounds.maxInFlight])).toEqual([1, 1]);
});

// 010-AC22: why the server has the voice, in each case the sheet tells apart.
test('diagnostics says why the server has the voice', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const base = { offer: { revision: 'r', bytes: 1, files: [] }, offered: true, refused: false, on: true, inAudio: true, delayed: false,
    failed: false, untapped: false, sttOff: false, live: true, handovers: [] };
  const cases = [
    [{}, 'device', 'On this device'],
    [{ offer: null, offered: false }, undefined, 'By the server: this server offers no device transcription'],
    [{ offered: false, refused: true }, undefined, "By the server: this device can't use it"],
    [{ offered: false }, undefined, 'By the server: not available on this device'],
    [{ failed: true, live: false }, 'failed', 'By the server: the engine failed on this device, until the page reloads'],
    [{ untapped: true, live: false }, undefined, "By the server: this browser can't pass your microphone to the engine"],
    [{ on: false, live: false }, undefined, 'By the server: transcription on this device is off'],
    [{ inAudio: false, live: false }, undefined, 'By the server: you are not in the audio'],
    [{ delayed: true, live: false }, undefined, 'By the server: waiting a few seconds after the page reload'],
    [{}, 'slow', 'By the server: this device is too slow right now'],
    [{}, 'loading', 'By the server: starting on this device'],
    [{ live: false }, 'slow', 'By the server: starting on this device'],
    [{ untapped: true, on: false, live: false }, undefined, 'By the server: transcription on this device is off'],
    // 010-AC23: turned off, the voice goes nowhere, whatever the device's state.
    [{ sttOff: true, live: false }, undefined, 'Nowhere: you turned the transcription of your voice off'],
    [{ sttOff: true }, 'device', 'Nowhere: you turned the transcription of your voice off'],
  ];
  for (const [over, mode, want] of cases) {
    expect(await alice.evaluate(([i, mode]) => import('/static/diagnostics.js').then((m) => m.where(i, mode)), [{ ...base, ...over }, mode]), want).toBe(want);
  }
});
