import { mkdirSync, writeFileSync } from 'node:fs';
import { chromium } from '@playwright/test';
import { test, expect } from '../fixtures.js';
import { adminToken, ttsMetrics, sttMetrics, engine } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
// The Chrome of persona-voice/echo/: what it plays is what its microphone hears.
const ws = process.env.ECHO_BROWSER_WS;
const LINES = 10;
const lines = [
  "J'ai relu la spec de paiement, les relances s'arrêtent après trois essais.",
  'The webhook answers a 502 while the pod restarts, so the retry waits a little longer.',
  'On garde le feature flag coupé en staging jusqu’à jeudi.',
  'I would split the migration in two steps, the backfill first and the switch after.',
  'Le tableau de bord montre une latence stable depuis lundi matin.',
];

const gauge = (text, name) => Number(text.match(new RegExp(`^${name} (\\S+)$`, 'm'))?.[1] ?? 0);

// sttIdle holds once the STT worker had no utterance queued or in an engine for 2 s: past its
// 800 ms end of utterance, what it heard is posted.
async function sttIdle() {
  let since = 0;
  await expect.poll(async () => {
    const text = await (await fetch(sttMetrics)).text();
    if (gauge(text, 'grill_stt_queue_depth') || gauge(text, 'grill_stt_engines_busy')) since = 0;
    else since ||= Date.now();
    return since > 0 && Date.now() - since >= 2_000;
  }, { intervals: [250], timeout: 60_000 }).toBe(true);
}

// Lines said to their end or dropped.
async function metrics() {
  const text = await (await fetch(ttsMetrics)).text();
  const sum = (re) => [...text.matchAll(re)].reduce((a, m) => a + Number(m[1]), 0);
  return sum(/^grill_tts_lines_spoken_total (\S+)$/gm) + sum(/^grill_tts_lines_dropped_total\{[^}]*\} (\S+)$/gm);
}

// A human whose microphone is the loop joins the call. Echo on: Grill says LINES lines, the STT
// worker's lines for that human come back. Echo off: Grill says one, the times its echo cut it come back.
async function run(request, room, name, echoCancellation) {
  const browser = await chromium.connect(ws, { headers: { 'x-playwright-launch-options': JSON.stringify({
    headless: false, args: ['--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'] }) } });
  try {
    const ctx = await browser.newContext({ permissions: ['microphone'] });
    ctx.setDefaultTimeout(30_000);
    if (!echoCancellation) {
      await ctx.addInitScript(() => {
        const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
        navigator.mediaDevices.getUserMedia = (c) => gum(c?.audio ? { ...c, audio: { ...(typeof c.audio === 'object' ? c.audio : {}), echoCancellation: false } } : c);
      });
    }
    const inv = await (await request.post(`/api/rooms/${room}/invites`, { headers: admin })).json();
    const page = await ctx.newPage();
    const url = new URL(inv.url);
    // The browser runs on the host network: the server's own address reaches it.
    await page.goto(new URL(url.pathname + url.hash, test.info().project.use.baseURL).href);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    const me = (await (await page.request.get(`/api/rooms/${room}`)).json()).me.id;
    const inAudio = page.waitForResponse((r) => r.url().endsWith('/presence') && r.request().postDataJSON().audio === true);
    await page.getByRole('button', { name: 'Join audio' }).click();
    await inAudio;
    await expect(page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(1, { timeout: 60_000 });
    const heard = async () => {
      const b = await (await page.request.get(`/api/rooms/${room}/messages?limit=500`)).json();
      return (b.messages ?? b).filter((m) => m.kind === 'voice' && m.participant_id === me).map((m) => m.body);
    };
    const tts = async (name) => gauge(await (await fetch(ttsMetrics)).text(), name);
    if (!echoCancellation) {
      // Its own echo cuts the persona off and resumes it, over and over: the first line is enough.
      // Each fragment is shorter than the STT's 400 ms VAD minimum, so the cut, not a line, proves the loop.
      const before = await tts('grill_tts_lines_interrupted_total');
      expect((await request.post('/api/admin/test/speak', { headers: admin, data: { room, persona: 'Grill', spoken: lines[0] } })).status()).toBe(204);
      await expect.poll(() => tts('grill_tts_lines_interrupted_total'), { timeout: 90_000 }).toBeGreaterThan(before);
      return { cut: await tts('grill_tts_lines_interrupted_total') - before, heard: await heard() };
    }
    const cutBefore = await tts('grill_tts_lines_interrupted_total');
    const spokenBefore = await tts('grill_tts_lines_spoken_total');
    for (let i = 0; i < LINES; i++) {
      const before = await metrics();
      const r = await request.post('/api/admin/test/speak', { headers: admin, data: { room, persona: 'Grill', spoken: lines[i % lines.length] } });
      expect(r.status()).toBe(204);
      await expect.poll(metrics, { timeout: 90_000 }).toBeGreaterThan(before);
    }
    await sttIdle();
    // Every line was said to its end, and its own echo never cut the persona off.
    expect(await tts('grill_tts_lines_spoken_total') - spokenBefore).toBe(LINES);
    expect(await tts('grill_tts_lines_interrupted_total')).toBe(cutBefore);
    return await heard();
  } finally {
    await browser.close();
  }
}

// 013-AC35: with echoCancellation on, a silent human whose microphone hears the call's own audio
// posts no line while a persona says 10; with it off the loop carries the persona's voice back and
// cuts it off.
test('no persona echo through a listener with echo cancellation', async ({ request }) => {
  test.skip(!ws, 'ECHO_BROWSER_WS is not set: run with PERSONA_VOICE_ECHO_IMAGE');
  test.skip(engine === 'fake', 'needs speech: run on Pocket');
  test.setTimeout(1_200_000);
  const room = (await (await request.post('/api/rooms', { headers: admin, data: { name: 'Echo' } })).json()).id;
  const on = await run(request, room, 'Echo on', true);
  const off = await run(request, room, 'Echo off', false);
  mkdirSync('test-results/persona-voice', { recursive: true });
  writeFileSync('test-results/persona-voice/echo.json', JSON.stringify({ on, off }, null, 1) + '\n');
  console.log('echo lines', JSON.stringify({ on, off }));
  expect(on).toEqual([]);
  expect(off.cut).toBeGreaterThan(0);
});
