import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { chromium } from '@playwright/test';
import { test, expect } from '../fixtures.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { adminToken, serverMetrics, sttMetrics, sttContainer } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const desktop = { userAgentData: { mobile: false }, hardwareConcurrency: 8, deviceMemory: 8 };

// FR/EN read speech from FLEURS (CC-BY-4.0): source, license and timings are in fr-en.json.
const fixture = JSON.parse(readFileSync(new URL('../voice/fixtures/fr-en.json', import.meta.url)));
const wav = readFileSync(new URL('../voice/fixtures/fr-en.wav', import.meta.url));

// clip cuts sentence i out of the 16 kHz mono 16-bit fixture, with 300 ms around it, as a WAV.
function clip(i) {
  const u = fixture.utterances[i];
  const data = wav.indexOf('data') + 8;
  const at = (s) => data + 2 * Math.round(Math.max(0, s) * 16000);
  const pcm = wav.subarray(at(u.start - 0.3), Math.min(wav.length, at(u.end + 0.3)));
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

// fakeMic replaces the microphone with a WebAudio graph the test drives: window.say(url) plays a
// clip into it and resolves when it ends, so the test decides when a sentence is spoken. Between
// clips the mic sends silence.
function fakeMic() {
  let graph;
  const bus = () => {
    if (!graph) {
      const ctx = new AudioContext();
      graph = { ctx, gain: ctx.createGain() };
    }
    return graph;
  };
  const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (c) => {
    if (!c?.audio || c.video) return real(c);
    const { ctx, gain } = bus();
    const dst = ctx.createMediaStreamDestination();
    gain.connect(dst);
    return dst.stream;
  };
  window.say = async (url) => {
    const { ctx, gain } = bus();
    await ctx.resume();
    const src = ctx.createBufferSource();
    src.buffer = await ctx.decodeAudioData(await (await fetch(url)).arrayBuffer());
    src.connect(gain);
    const ended = new Promise((r) => { src.onended = r; });
    src.start();
    await ended;
  };
}

const words = (s) => new Set(s.toLowerCase().replaceAll('’', "'").match(/[\p{L}\p{N}']+/gu) ?? []);
// heard tells whether most of a line's words come from sentence i.
const heard = (body, i) => {
  const w = words(body);
  const ref = words(fixture.utterances[i].text);
  return w.size > 0 && [...w].filter((x) => ref.has(x)).length / w.size >= 0.5;
};

// voiceLines reads the voice lines on show: sender, text and the tip naming who wrote it down.
const voiceLines = (page) => page.locator('.msg.voice').evaluateAll((els) => els.map((li) => ({
  sender: li.dataset.sender,
  body: li.querySelector('.body').textContent,
  tip: li.querySelector('.voice-src')?.dataset.tip,
})));
const deviceTip = "Transcribed on Alice's device";
const serverTip = 'Transcribed by the server';

const escape = (s) => s.replace(/[{}"]/g, (c) => '\\' + c);
// metric reads one series of a Prometheus endpoint; NaN when the series is not there.
async function metric(url, series) {
  const text = await (await fetch(url)).text();
  return Number(text.match(new RegExp(`^${escape(series)} (\\S+)$`, 'm'))?.[1]);
}
const flags = () => metric(serverMetrics, 'grill_room_device_stt_participants');
const skipped = () => metric(sttMetrics, 'grill_stt_device_tracks');
const listened = () => metric(sttMetrics, 'grill_stt_tracks');
const serverLines = async () => [
  await metric(serverMetrics, 'grill_room_voice_lines_total{source="server"}'),
  await metric(serverMetrics, 'grill_room_voice_lines_deduped_total{source="server"}'),
];

// resumedAt reads the worker's first track_resumed log line for room after since (ms), as a time.
function resumedAt(room, since) {
  const r = spawnSync('docker', ['logs', sttContainer], { encoding: 'utf8', timeout: 10_000 });
  const lines = `${r.stdout}\n${r.stderr}`.split('\n').filter((l) => l.startsWith('{')).map((l) => JSON.parse(l));
  return lines.filter((l) => l.event === 'track_resumed' && l.room === room).map((l) => Date.parse(l.timestamp)).find((t) => t >= since);
}

// 010-AC9, 010-AC10, 010-AC12, 010-AC13, 010-AC14, 010-AC15: Alice's voice goes to her device and
// back to the server worker, announced and silent, through LiveKit, the real worker and the real
// device model.
test('the voice hands over between the device and the server', async ({ request, baseURL }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Device voice' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  // A private context keeps Cache Storage in memory and refuses the 383 MB encoder: a profile on disk,
  // outside test-results/, which a failed CI run uploads.
  const dir = mkdtempSync(join(process.env.RUNNER_TEMP || tmpdir(), 'grill-room-device-voice-'));
  const ctx = await chromium.launchPersistentContext(dir, {
    channel: 'chrome', baseURL,
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
  // An action that cannot act fails within its step, not at the test's timeout.
  ctx.setDefaultTimeout(15_000);
  const timings = {};
  try {
    await ctx.addInitScript(snoozePush, snoozeKey);
    await ctx.addInitScript(fakeMic);
    await ctx.addInitScript((nav) => {
      for (const [k, v] of Object.entries(nav)) Object.defineProperty(Navigator.prototype, k, { get: () => v, configurable: true });
      // A runner's CPU must not decide the test: the bar is covered by device-stt.spec.js.
      globalThis.grillSelfTestBar = { wasm: 0.5, webgpu: 0.5 };
    }, desktop);
    await ctx.route('**/__clips/*.wav', (r) => r.fulfill({ contentType: 'audio/wav', body: clip(Number(/(\d+)\.wav$/.exec(r.request().url())[1])) }));
    // silent, once set, drops the device's beats and lines as a crashed pipeline would stop sending
    // them; lastBeat is when the server last heard a device beat.
    let silent = false;
    let lastBeat = 0;
    await ctx.route(`**/api/rooms/${room.id}/presence`, (r) => {
      const body = r.request().postDataJSON();
      if (body.device_stt === true) {
        if (silent) return r.abort();
        lastBeat = Date.now();
      }
      return r.continue();
    });
    await ctx.route(`**/api/rooms/${room.id}/voice/device-lines`, (r) => (silent ? r.abort() : r.continue()));

    const page = ctx.pages()[0] || await ctx.newPage();
    const url = new URL(inv.url);
    await page.goto(url.pathname + url.hash);
    await page.getByLabel('Your name').fill('Alice');
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText('Alice');
    await page.getByRole('button', { name: 'Join audio' }).click();
    await expect(page.getByRole('button', { name: 'Audio options' })).toBeVisible();
    await expect.poll(listened, { timeout: 30_000 }).toBe(1);

    const box = page.locator('#voice-device').getByRole('checkbox', { name: 'Transcribe on this device' });
    const said = page.locator('#device-stt-status');
    // menu opens the audio menu, the pointer off it: a row's tip under the pointer would cover the
    // row below.
    const menu = async () => {
      await page.mouse.move(0, 0);
      await page.locator('#voice-menu').evaluate((m) => { if (!m.matches(':popover-open')) m.showPopover(); });
    };
    const say = (i) => page.evaluate((u) => window.say(u), `/__clips/${i}.wav`);
    const lineFor = async (i, tip) => (await voiceLines(page)).find((l) => heard(l.body, i) && l.tip === tip);
    // deviceOn ticks the toggle and waits until both the server and the worker hold the flag.
    const deviceOn = async (timeout) => {
      await menu();
      await box.check();
      await expect(said).toHaveText('Transcribed on this device', { timeout });
      await expect.poll(flags, { timeout: 10_000 }).toBe(1);
      await expect.poll(skipped, { timeout: 10_000 }).toBe(1);
    };

    // a. The self-test on the real model, then a sentence: one line from the device, none from the worker.
    await deviceOn(300_000);
    await say(0);
    await expect.poll(() => lineFor(0, deviceTip), { timeout: 60_000 }).toMatchObject({ sender: 'Alice' });
    expect(await skipped()).toBe(1);

    // b. Announced stop: the toggle goes off, the worker takes the voice back.
    await menu();
    let stop = Date.now();
    await box.uncheck();
    await expect.poll(flags, { timeout: 10_000 }).toBe(0);
    await expect.poll(skipped, { timeout: 10_000 }).toBe(0);
    timings.announced = resumedAt(room.id, stop) - stop;
    await say(1);
    await expect.poll(() => lineFor(1, serverTip), { timeout: 60_000 }).toMatchObject({ sender: 'Alice' });
    // The worker posts in order: had it heard sentence 0, that line would be stored or deduped already.
    expect(await serverLines()).toEqual([1, 0]);

    // c. Silent stop: the device's beats and lines stop reaching the server, the flag lapses.
    await deviceOn(120_000);
    silent = true;
    await expect.poll(flags, { timeout: 10_000 }).toBe(0);
    await expect.poll(skipped, { timeout: 10_000 }).toBe(0);
    timings.silent = resumedAt(room.id, lastBeat) - lastBeat;
    await say(2);
    await expect.poll(() => lineFor(2, serverTip), { timeout: 60_000 }).toMatchObject({ sender: 'Alice' });
    expect(await lineFor(2, deviceTip)).toBeUndefined();

    // e. The device beats again, then the page closes and the flag drops. Playwright sees neither
    // the route nor the request of a closing page's keepalive beat, so only the flag is asserted.
    silent = false;
    await expect.poll(flags, { timeout: 10_000 }).toBe(1);
    await expect.poll(skipped, { timeout: 10_000 }).toBe(1);
    stop = Date.now();
    await page.close({ runBeforeUnload: true });
    await expect.poll(skipped, { timeout: 10_000, intervals: [100] }).toBe(0);
    timings.closed = Date.now() - stop;
    await expect.poll(flags, { timeout: 10_000 }).toBe(0);
  } finally {
    // 010-AC15, from the stop to the worker's track_resumed log line: at most 3 s announced, 5 s
    // silent; closed is from page.close() to the worker's gauge at 0. Reported, not asserted: a
    // runner's clock jitter must not decide it.
    console.log('010-AC15 handover, ms:', JSON.stringify(timings));
    for (const [type, ms] of Object.entries(timings)) test.info().annotations.push({ type: `010-AC15 ${type} ms`, description: String(ms) });
    await ctx.close();
    rmSync(dir, { recursive: true, force: true });
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  }
});
