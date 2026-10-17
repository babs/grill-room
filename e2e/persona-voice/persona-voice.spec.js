import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { test, expect } from '../fixtures.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { adminToken, sttMetrics, ttsMetrics, sttContainer, engine } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// silentMic replaces the microphone with a silent WebAudio stream: the listener says nothing, so
// any voice line in the room would be a persona's words written down.
function silentMic() {
  navigator.mediaDevices.getUserMedia = async () => {
    const ctx = new AudioContext();
    const dst = ctx.createMediaStreamDestination();
    ctx.createGain().connect(dst);
    return dst.stream;
  };
}

const escape = (s) => s.replace(/[{}"]/g, (c) => '\\' + c);
async function metric(url, series) {
  const text = await (await fetch(url)).text();
  return Number(text.match(new RegExp(`^${escape(series)} (\\S+)$`, 'm'))?.[1] ?? 0);
}

function workerLog() {
  const r = spawnSync('docker', ['logs', sttContainer], { encoding: 'utf8', timeout: 10_000 });
  return `${r.stdout}\n${r.stderr}`.split('\n').filter((l) => l.startsWith('{')).map((l) => JSON.parse(l));
}

// level measures the loudest 20 ms of the persona's audio element over ms, in dBFS.
const level = (page, ms) => page.evaluate(async (ms) => {
  const el = document.querySelector('#voice-audio audio[data-pid^="persona-"]');
  if (!el?.srcObject) return -Infinity;
  const ctx = new AudioContext();
  await ctx.resume();
  const an = ctx.createAnalyser();
  an.fftSize = 1024;
  ctx.createMediaStreamSource(el.srcObject).connect(an);
  const buf = new Float32Array(an.fftSize);
  let peak = -Infinity;
  const end = performance.now() + ms;
  while (performance.now() < end) {
    an.getFloatTimeDomainData(buf);
    const rms = Math.sqrt(buf.reduce((a, x) => a + x * x, 0) / buf.length);
    peak = Math.max(peak, 20 * Math.log10(rms || 1e-9));
    await new Promise((r) => setTimeout(r, 20));
  }
  await ctx.close();
  return peak;
}, ms);

// 013-AC13, 013-AC14, 013-AC15, 013-AC19: a persona's line plays in the call through its own bot;
// the STT worker never subscribes to the bot, no voice line carries the persona's words, the bot is
// not in People, and the device tap holds the local microphone only.
test('a persona speaks in the call and nobody writes it down', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Persona voice' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await browser.newContext({
    permissions: ['microphone'],
  });
  ctx.setDefaultTimeout(15_000);
  await ctx.addInitScript(snoozePush, snoozeKey);
  await ctx.addInitScript(silentMic);
  const page = await ctx.newPage();
  const url = new URL(inv.url);
  await page.goto(url.pathname + url.hash);
  await page.getByLabel('Your name').fill('Alice');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Alice');
  await page.getByRole('button', { name: 'Join audio' }).click();
  await expect(page.getByRole('button', { name: 'Audio options' })).toBeVisible();
  // The worker joins on its own poll: wait until it listens to Alice before judging what it hears.
  await expect.poll(() => metric(sttMetrics, 'grill_stt_tracks'), { timeout: 30_000 }).toBe(1);

  // The bot joins once Alice listens, before any line (013-AC15).
  const bot = page.locator('#voice-audio audio[data-pid^="persona-"]');
  await expect(bot).toHaveCount(1, { timeout: 30_000 });
  const spoken = await metric(ttsMetrics, 'grill_tts_lines_spoken_total');
  const r = await request.post('/api/admin/test/speak', { headers: admin, data: { room: room.id, persona: 'Grill', spoken: '**I changed** the payment spec. Alice, which date do we keep?' } });
  expect(r.status()).toBe(204);
  // The persona's track carries sound, well above the silence the fake engine leaves between words.
  await expect.poll(() => level(page, 500), { timeout: 30_000 }).toBeGreaterThan(-35);
  await expect.poll(() => metric(ttsMetrics, 'grill_tts_lines_spoken_total'), { timeout: 60_000 }).toBe(spoken + 1);
  // The fake engine runs far ahead of real time, so a gap is the pipeline's; the real engine's
  // timing on a shared runner says nothing, the lab measures it (013-AC17).
  if (engine === 'fake') expect(await metric(ttsMetrics, 'grill_tts_underruns_total')).toBe(0);
  mkdirSync('test-results/persona-voice', { recursive: true });
  await page.screenshot({ path: `test-results/persona-voice/room-${engine}.png`, fullPage: true });

  // 013-AC19: the worker listens to Alice alone and never touches the bot.
  const me = await page.evaluate(() => document.querySelector('#participants li')?.dataset.pid);
  const log = workerLog().filter((l) => l.room === room.id);
  expect(log.filter((l) => l.event === 'track_listening').map((l) => String(l.participant))).toEqual([me]);
  expect(log.filter((l) => l.event === 'track_ignored')).toEqual([]);
  expect(await metric(sttMetrics, 'grill_stt_tracks')).toBe(1);
  // Nothing the persona said became a voice line; the voice count and People name humans only.
  await expect(page.locator('.msg.voice')).toHaveCount(0);
  await expect(page.locator('#participants li')).toHaveCount(1);
  await expect(page.locator('#participants')).not.toContainText('Grill');
  // The device tap would hear the local microphone, never the bot's track (010-AC9).
  expect(await page.evaluate(() => {
    const remote = document.querySelector('#voice-audio audio[data-pid^="persona-"]').srcObject.getAudioTracks()[0];
    return Boolean(window.grillMic) && window.grillMic !== remote && window.grillMic.id !== remote.id;
  })).toBe(true);

  // The bot leaves once nobody listens (013-AC15, 5 s in this stack).
  await page.getByRole('button', { name: 'Audio options' }).click();
  await page.getByRole('button', { name: 'Leave audio' }).click();
  await expect.poll(() => metric(ttsMetrics, 'grill_tts_bots'), { timeout: 30_000 }).toBe(0);
  await ctx.close();
});
