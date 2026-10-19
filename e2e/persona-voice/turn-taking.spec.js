import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { test, expect } from '../fixtures.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { adminToken, ttsMetrics, engine } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// FR/EN read speech from FLEURS (CC-BY-4.0): source, licence and timings in voice/fixtures/fr-en.json.
const fixture = JSON.parse(readFileSync(new URL('../voice/fixtures/fr-en.json', import.meta.url)));
const wav = readFileSync(new URL('../voice/fixtures/fr-en.wav', import.meta.url));

// firstLoud is the offset in ms of a 16-bit PCM buffer's first 10 ms window above -40 dBFS: the
// speech onset in the file, which the fixture's word timings place only roughly.
function firstLoud(pcm) {
  const n = 160;
  for (let i = 0; i + 2 * n <= pcm.length; i += 2 * n) {
    let sum = 0;
    for (let j = 0; j < n; j++) sum += (pcm.readInt16LE(i + 2 * j) / 32768) ** 2;
    if (20 * Math.log10(Math.sqrt(sum / n) || 1e-9) > -40) return i / 32;
  }
  return 0;
}

// clip is sentence i of the 16 kHz mono fixture, from its first word, as a WAV.
function clip(i) {
  const u = fixture.utterances[i];
  const data = wav.indexOf('data') + 8;
  const at = (x) => data + 2 * Math.round(Math.max(0, x) * 16000);
  const pcm = wav.subarray(at(u.start), Math.min(wav.length, at(u.end)));
  const head = Buffer.alloc(44);
  head.write('RIFF', 0);
  head.writeUInt32LE(36 + pcm.length, 4);
  head.write('WAVEfmt ', 8);
  head.writeUInt32LE(16, 16);
  head.writeUInt16LE(1, 20);
  head.writeUInt16LE(1, 22);
  head.writeUInt32LE(16000, 24);
  head.writeUInt32LE(32000, 28);
  head.writeUInt16LE(2, 32);
  head.writeUInt16LE(16, 34);
  head.write('data', 36);
  head.writeUInt32LE(pcm.length, 40);
  return Buffer.concat([head, pcm]);
}

// speechMic: the microphone is a WebAudio graph the test drives. window.talk(i) plays sentence i
// into it and resolves when it ends; silence otherwise.
function speechMic() {
  let ctx;
  let gain;
  navigator.mediaDevices.getUserMedia = async () => {
    ctx = new AudioContext();
    gain = ctx.createGain();
    const dst = ctx.createMediaStreamDestination();
    gain.connect(dst);
    return dst.stream;
  };
  window.talk = async (i) => {
    await ctx.resume();
    const src = ctx.createBufferSource();
    src.buffer = await ctx.decodeAudioData(await (await fetch('/__clips/' + i + '.wav')).arrayBuffer());
    src.connect(gain);
    const ended = new Promise((r) => { src.onended = r; });
    src.start();
    // The onset in the file, on the wall clock both pages share: the clip starts at its first word.
    window.talkedAt = performance.timeOrigin + performance.now();
    await ended;
  };
}

// silentMic: the listener says nothing, though the bots hear her track too.
function silentMic() {
  navigator.mediaDevices.getUserMedia = async () => {
    const ctx = new AudioContext();
    const dst = ctx.createMediaStreamDestination();
    ctx.createGain().connect(dst);
    return dst.stream;
  };
}

// meter: the listener's page samples every remote track's level each 10 ms, on its own clock,
// into window.levels[identity] = [[t, dBFS], ...].
function meter() {
  window.levels = {};
  const ctx = new AudioContext();
  const seen = new Set();
  setInterval(() => {
    for (const el of document.querySelectorAll('#voice-audio audio')) {
      const id = el.dataset.pid;
      if (seen.has(el) || !el.srcObject) continue;
      seen.add(el);
      const an = ctx.createAnalyser();
      an.fftSize = 512;
      ctx.createMediaStreamSource(el.srcObject).connect(an);
      const buf = new Float32Array(an.fftSize);
      const rows = (window.levels[id] = window.levels[id] || []);
      setInterval(() => {
        an.getFloatTimeDomainData(buf);
        const rms = Math.sqrt(buf.reduce((a, x) => a + x * x, 0) / buf.length);
        rows.push([performance.now(), 20 * Math.log10(rms || 1e-9)]);
      }, 10);
    }
    ctx.resume();
  }, 50);
}

async function join(browser, inv, name, mic) {
  const ctx = await browser.newContext({ permissions: ['microphone'] });
  ctx.setDefaultTimeout(15_000);
  await ctx.route('**/__clips/*.wav', (r) => r.fulfill({ contentType: 'audio/wav', body: clip(Number(/(\d+)\.wav$/.exec(r.request().url())[1])) }));
  await ctx.addInitScript(snoozePush, snoozeKey);
  for (const script of [mic].flat()) await ctx.addInitScript(script);
  const page = await ctx.newPage();
  const url = new URL(inv.url);
  await page.goto(url.pathname + url.hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  const inAudio = page.waitForResponse((r) => r.url().endsWith('/presence') && r.request().postDataJSON().audio === true);
  await page.getByRole('button', { name: 'Join audio' }).click();
  await inAudio;
  return { ctx, page };
}

const escape = (x) => x.replace(/[{}"]/g, (c) => '\\' + c);
async function metric(url, series) {
  const text = await (await fetch(url)).text();
  return Number(text.match(new RegExp(`^${escape(series)} (\\S+)$`, 'm'))?.[1] ?? 0);
}

const LOUD = -40;
// AC22's sample: an interruption whose own audio reached the listener later than STALL_MS after
// its file onset met a stalled media path, which no persona can beat, and is left out. Bob cuts in
// until 20 are kept, so the p95 is not their maximum.
const MAX_INTERRUPTIONS = 30;
const KEPT = 20;
const STALL_MS = 250;
const FROM_HEARD = Boolean(process.env.CI);
// Where speech starts in each clip, from its samples.
const onsetIn = Object.fromEntries([0, 1, 2, 3].map((i) => [i, firstLoud(clip(i).subarray(44))]));
const QUIET = -55;

// onsetAfter is the first time at or after t a series is louder than LOUD.
const onsetAfter = (rows, t) => rows.find(([at, db]) => at >= t && db > LOUD)?.[0];
// starts are the times a series turns loud after at least ms of quiet: a persona starting to talk.
function starts(rows, ms) {
  const out = [];
  let quietSince = rows[0]?.[0] ?? 0;
  let quiet = true;
  for (const [at, db] of rows) {
    if (db > LOUD) {
      if (quiet && at - quietSince >= ms) out.push(at);
      quiet = false;
    } else if (db < QUIET && !quiet) {
      quiet = true;
      quietSince = at;
    }
  }
  return out;
}

// words lists the loud runs of a series within [from, to] as [start, ms]; a run ends after 20 ms
// under QUIET. A word has no dip under QUIET, while the listener hears only 45-80 ms of a 100 ms
// gap: the meter's window and the codec's tail take some, and a talk spurt's first gaps are
// squeezed by the jitter buffer catching up.
function words(rows, from, to) {
  const out = [];
  let run = null;
  let quietSince = null;
  for (const [at, db] of rows) {
    if (at < from || at > to) continue;
    if (db > LOUD) {
      if (!run) out.push((run = [at, 0]));
      run[1] = at - run[0];
      quietSince = null;
    } else if (run && db < QUIET) {
      quietSince ??= at;
      if (at - quietSince >= 20) run = null;
    }
  }
  return out;
}

// loudAround reports a series loud within [t - before, t].
const loudAround = (rows, t, before) => rows.some(([at, db]) => at >= t - before && at <= t && db > LOUD);

// silentFrom is the start of the first run after t that stays under QUIET for at least ms.
function silentFrom(rows, t, ms) {
  let start = null;
  for (const [at, db] of rows) {
    if (at < t) continue;
    if (db < QUIET) {
      start ??= at;
      if (at - start >= ms) return start;
    } else start = null;
  }
  return null;
}

// 013-AC21, AC22, AC23, AC24: a persona never starts while a human talks, stops within 300 ms of the
// onset in the human's file (p95 over the interruptions kept; in CI, from when Alice hears Bob),
// says the interrupted sentence again once the room is quiet, and a second persona waits for the
// first to finish.
test('turn-taking in the call', async ({ browser, request }) => {
  test.setTimeout(420_000);
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Turns' } })).json();
  // A short quiet time keeps every interruption inside one line.
  expect((await request.patch(`/api/rooms/${room.id}/speech`, { headers: admin, data: { quiet_ms: 500, resume_s: 20, queue_s: 300 } })).status()).toBe(200);
  const inv = async () => (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const alice = await join(browser, await inv(), 'Alice', [meter, silentMic]);
  const bob = await join(browser, await inv(), 'Bob', speechMic);
  try {
    const templates = await (await alice.page.request.get(`/api/rooms/${room.id}/templates`)).json();
    expect((await alice.page.request.post(`/api/rooms/${room.id}/personas`, { data: { template_id: templates.find((t) => t.name === 'Secretary').id } })).status()).toBe(201);
    await expect(alice.page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(2, { timeout: 30_000 });
    const ids = Object.fromEntries(await alice.page.locator('.persona-pill').evaluateAll((els) => els.map((e) => [e.dataset.name, 'persona-' + e.dataset.id])));
    const levels = () => alice.page.evaluate(() => window.levels);
    // Only the rows since t: copying the whole history out stalls the meter's timers and slows each
    // poll as the call goes on, until a cut comes after Grill's line has ended.
    const since = (id, t) => alice.page.evaluate(([id, t]) => (window.levels[id] || []).filter(([at]) => at >= t), [id, t]);
    const now = () => alice.page.evaluate(() => performance.now());
    const origin = await alice.page.evaluate(() => performance.timeOrigin);
    const speak = async (persona, spoken) => {
      const r = await request.post('/api/admin/test/speak', { headers: admin, data: { room: room.id, persona, spoken } });
      expect(r.status()).toBe(204);
    };
    // Each interruption says the cut sentence again from its start, so the line ends only once a
    // sentence plays through: two 30-word sentences (the room's 60-word cap) outlast every cut.
    const sentence = Array(3).fill('one two three four five six seven eight nine ten').join(' ') + '.';

    // AC21: Bob talks; the line queued meanwhile starts only after he stopped and the room was quiet.
    // Both bots hear both humans: LiveKit names a speaker to a bot only once it hears their track.
    await expect.poll(() => metric(ttsMetrics, 'grill_tts_human_tracks'), { timeout: 30_000 }).toBe(4);
    const t0 = await now();
    const talking = bob.page.evaluate(() => window.talk(0));
    // Alice hears Bob before the line is queued: it is queued while a human talks.
    const humanLoud = ([t, loud]) => Object.entries(window.levels)
      .some(([id, rows]) => !id.startsWith('persona-') && rows.some(([at, db]) => at >= t && db > loud));
    await expect.poll(() => alice.page.evaluate(humanLoud, [t0, LOUD]), { timeout: 15_000 }).toBe(true);
    await speak('Grill', `${sentence} ${sentence}`);
    await talking;
    await expect.poll(async () => onsetAfter((await levels())[ids.Grill] || [], 0), { timeout: 20_000 }).toBeTruthy();
    let L = await levels();
    const bobKey = Object.keys(L).find((k) => !k.startsWith('persona-'));

    // AC22, AC23: interruptions while Grill speaks; each stops it, each quiet resumes it.
    const stops = [];
    const paths = [];
    for (let i = 0; i < MAX_INTERRUPTIONS && paths.filter((p) => p <= STALL_MS).length < KEPT; i++) {
      const before = await now();
      await expect.poll(async () => onsetAfter(await since(ids.Grill, before), before), { timeout: 20_000 }).toBeTruthy();
      await bob.page.evaluate((n) => window.talk(n), 1 + (i % 3));
      const fileOnset = (await bob.page.evaluate(() => window.talkedAt)) - origin + onsetIn[1 + (i % 3)];
      // A second before: starts() needs Bob quiet for 300 ms ahead of his onset.
      const bobRows = await since(bobKey, before - 1000);
      const grillRows = await since(ids.Grill, before);
      // Bob's onset as Alice hears it: loud after quiet, not the tail of his previous sentence.
      const heard = starts(bobRows, 300).find((t) => t >= before);
      const silent = silentFrom(grillRows, heard - 20, 350);
      expect(heard, `interruption ${i}: Bob heard`).toBeTruthy();
      // A cut after the line ended leaves nothing to resume: the loop fell behind Grill's sentence.
      // From the file onset: a stalled path to Alice can let the pod stop Grill before she hears Bob.
      expect(loudAround(grillRows, fileOnset, 500), `interruption ${i}: Grill speaking when Bob cut in`).toBe(true);
      expect(silent, `interruption ${i}: Grill went quiet`).toBeTruthy();
      // 013-AC22 from the onset in Bob's file, his own path to the pod included; in CI from when
      // Alice hears him, as a shared runner's media path is no persona's latency.
      stops.push(Math.max(0, silent - (FROM_HEARD ? heard : fileOnset)));
      // Bob's own path, file to Alice's ears: what no persona can do better than.
      paths.push(Math.round(heard - fileOnset));
      // It picks the sentence up again once the room is quiet (AC23).
      const after = await now();
      await expect.poll(async () => onsetAfter(await since(ids.Grill, after), after), { timeout: 10_000 }).toBeTruthy();
    }
    const from = FROM_HEARD ? 'heard' : 'file';
    console.log('stops from', from, 'in order', JSON.stringify(stops.map((x) => Math.round(x))), 'bob path', JSON.stringify(paths));
    const kept = stops.filter((_, i) => paths[i] <= STALL_MS).sort((a, b) => a - b);
    const p95 = kept[Math.ceil(kept.length * 0.95) - 1];
    mkdirSync('test-results/persona-voice', { recursive: true });
    writeFileSync('test-results/persona-voice/stops.json', JSON.stringify({ from, stops, paths, kept, p95 }) + '\n');
    console.log(FROM_HEARD ? 'stop latency from heard ms' : 'stop latency ms', JSON.stringify(kept), 'p95', p95, 'stalled', stops.length - kept.length);
    expect(kept.length, 'interruptions with a media path under STALL_MS').toBeGreaterThanOrEqual(KEPT);
    expect(p95).toBeLessThanOrEqual(300);

    // AC21: across the whole call, no persona started while Bob's level was up.
    L = await levels();
    for (const id of [ids.Grill]) {
      // Bob was quiet for the room's quiet time before each start (500 ms here).
      for (const t of starts(L[id], 300)) expect(loudAround(L[bobKey], t, 500), `${id} started at ${Math.round(t)} within 500 ms of Bob`).toBe(false);
    }

    // AC24: Secretary's line waits until Grill's ends.
    const queued = await now();
    await speak('Secretary', 'Noted, I will add it to the minutes.');
    await expect.poll(async () => onsetAfter(await since(ids.Secretary, queued), queued), { timeout: 120_000 }).toBeTruthy();
    // Over the 300 ms the silence cap leaves between sentences: a 1 s quiet is the line's end.
    await expect.poll(async () => silentFrom(await since(ids.Grill, queued), queued, 1000), { timeout: 10_000 }).not.toBeNull();
    const sec = await since(ids.Secretary, queued);
    const grill = await since(ids.Grill, queued);
    const secStart = onsetAfter(sec, queued);
    expect(silentFrom(grill, queued, 1000)).toBeLessThanOrEqual(secStart);
    expect(grill.filter(([at, db]) => at > secStart && db > LOUD)).toEqual([]);
    // The listener hears the whole line, its first word included: 8 words of 250 ms on the fake
    // engine; real speech has no such shape.
    if (engine === 'fake') {
      await expect.poll(now, { timeout: 10_000 }).toBeGreaterThan(secStart + 3500);
      const said = words(await since(ids.Secretary, queued), secStart, secStart + 3500);
      expect(said.length, JSON.stringify(said)).toBe(8);
      expect(said[0][1], JSON.stringify(said)).toBeGreaterThanOrEqual(200);
    }
  } catch (e) {
    // What the listener heard, for the next failure's diagnosis.
    const L = await Promise.race([alice.page.evaluate(() => window.levels), new Promise((r) => setTimeout(r, 5000, null))]).catch(() => null);
    // A file in the results dir: CI uploads it, while the list reporter drops a body attachment.
    const path = test.info().outputPath('levels.json');
    try {
      writeFileSync(path, JSON.stringify(L));
    } catch {}
    await test.info().attach('levels', { path, contentType: 'application/json' }).catch(() => {});
    throw e;
  } finally {
    await alice.ctx.close();
    await bob.ctx.close();
  }
});

// A human who leaves mid-speech does not hold the room: the persona they cut off resumes once the
// room is quiet, not at the end of its resume window.
test('a speaker who leaves mid-speech does not hold the persona', async ({ browser, request }) => {
  test.setTimeout(180_000);
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Leaver' } })).json();
  const quietMs = 500;
  expect((await request.patch(`/api/rooms/${room.id}/speech`, { headers: admin, data: { quiet_ms: quietMs, resume_s: 20, queue_s: 300 } })).status()).toBe(200);
  const inv = async () => (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const alice = await join(browser, await inv(), 'Alice', [meter, silentMic]);
  const bob = await join(browser, await inv(), 'Bob', speechMic);
  try {
    await expect(alice.page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(1, { timeout: 30_000 });
    const grill = await alice.page.locator('.persona-pill').evaluate((e) => 'persona-' + e.dataset.id);
    const since = (id, t) => alice.page.evaluate(([id, t]) => (window.levels[id] || []).filter(([at]) => at >= t), [id, t]);
    const now = () => alice.page.evaluate(() => performance.now());
    const sentence = Array(3).fill('one two three four five six seven eight nine ten').join(' ') + '.';
    const t0 = await now();
    expect((await request.post('/api/admin/test/speak', { headers: admin, data: { room: room.id, persona: 'Grill', spoken: `${sentence} ${sentence}` } })).status()).toBe(204);
    await expect.poll(async () => onsetAfter(await since(grill, t0), t0), { timeout: 20_000 }).toBeTruthy();
    // Bob cuts in with his 6 s sentence; Grill stops while he still talks.
    const cut = await now();
    bob.page.evaluate(() => window.talk(1)).catch(() => {});
    await expect.poll(async () => silentFrom(await since(grill, cut), cut, 350), { timeout: 10_000 }).not.toBeNull();
    const bobKey = Object.keys(await alice.page.evaluate(() => window.levels)).find((k) => !k.startsWith('persona-'));
    const left = await now();
    expect(loudAround(await since(bobKey, cut), left, 300), 'Bob still speaking when his page closes').toBe(true);
    await bob.ctx.close();
    await expect.poll(async () => onsetAfter(await since(grill, left), left), { timeout: 25_000 }).toBeTruthy();
    const resumed = onsetAfter(await since(grill, left), left) - left;
    console.log('resumed after the leaver ms', Math.round(resumed));
    // The quiet time, plus Bob's leave reaching the pod and Grill's audio reaching Alice.
    expect(resumed).toBeLessThanOrEqual(quietMs + 2500);
  } finally {
    await alice.ctx.close();
    await bob.ctx.close().catch(() => {});
  }
});
