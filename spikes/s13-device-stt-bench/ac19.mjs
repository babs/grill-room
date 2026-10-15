// 010-AC19 bench, one participant per process: `alice` turns device mode on (cold load, self-test,
// then warm reloads), `bob` keeps the server worker; both play the first BENCH_SECS of the S11 corpus
// into their scripted mic, in a visible headed tab. Writes /out/<role>.json.
// usage: node ac19.mjs alice|bob
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { chromium } from '/nm/@playwright/test/index.mjs';
import { deviceSettled, fakeMic, invite, join, joinAudio, man, menu, newRoom, pcm, probe, recorder, sampler, sleep } from './lib.mjs';

const role = process.argv[2];
const secs = Number(process.env.BENCH_SECS || 1800);
const WARM = 3;
const CHUNK = 60;
// Whole items only: the audio stops at the last pause that ends inside the budget.
let liveEnd = 0;
for (const [, e] of man.pauses) if (e <= secs) liveEnd = e;
const res = { role, liveEnd, warm: [], marks: [] };
// marks time the phases, so memory peaks can be told apart: [name, wall ms].
const mark = (n) => res.marks.push([n, Date.now()]);
const log = (s) => console.log(`[${role}] ${new Date().toISOString()} ${s}`);

const waitFile = async (f) => { while (!existsSync(f)) await sleep(500); return JSON.parse(readFileSync(f, 'utf-8')); };

let room;
if (role === 'alice') {
  room = await newRoom('AC19 bench');
  writeFileSync('/out/room.json', JSON.stringify({ room, alice: await invite(room), bob: await invite(room) }));
}
const inv = await waitFile('/out/room.json');
const mem = role === 'alice' ? sampler() : null;
// Playwright's anti-throttling flags stay off: the tab must run as a user's would.
const ctx = await chromium.launchPersistentContext(`/prof/${role}`, {
  channel: 'chrome', headless: false,
  ignoreDefaultArgs: ['--disable-background-timer-throttling', '--disable-renderer-backgrounding', '--disable-backgrounding-occluded-windows'],
  args: ['--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
});
ctx.setDefaultTimeout(30_000);
try {
  await ctx.addInitScript(fakeMic);
  await ctx.addInitScript(recorder);
  const watch = (p) => {
    p.on('worker', (w) => { if (w.url().includes('pipeline')) w.evaluate(probe).catch((e) => log(`probe: ${e}`)); });
    p.on('crash', () => log('PAGE CRASHED'));
    return p;
  };
  let page = watch(ctx.pages()[0] || await ctx.newPage());
  await join(page, inv[role], role === 'alice' ? 'Alice' : 'Bob');
  log('in the audio');

  if (role === 'alice') {
    await menu(page);
    mark('cold');
    await page.locator('#voice-device input').check();
    const t = Date.now();
    res.cold = { status: await deviceSettled(page, 900_000), sec: (Date.now() - t) / 1000 };
    res.cold.selfTest = await page.evaluate(() => window.grillDeviceSTT.selfTest);
    res.cold.load = await page.evaluate(() => window.grillDeviceSTT.load);
    log(`cold ${JSON.stringify(res.cold)}`);
    if (!/Transcribed on this device/.test(res.cold.status)) throw new Error(`device mode refused: ${res.cold.status}`);
    // Warm loads: the choice is remembered, so joining the audio loads the engine from Cache Storage.
    for (let i = 0; i < WARM; i++) {
      mark(`warm${i}`);
      // WARM_NEW_TAB: a new tab, so a new renderer, as a user coming back; else a reload, which keeps
      // the renderer and the old page's heap while the new one loads.
      if (process.env.WARM_NEW_TAB) {
        const url = page.url();
        await page.close();
        page = watch(await ctx.newPage());
        await page.goto(url);
      } else await page.reload();
      await joinAudio(page);
      await deviceSettled(page, 120_000);
      const w = await page.evaluate(() => {
        const s = window.__rec.states;
        const a = s.find((x) => x[1] === 'loading');
        const b = s.find((x) => x[1] === 'ready');
        // engine: when the reloaded page started its engine worker, from the page's time origin (010-AC21).
        const e = window.__rec.workers.find((x) => x[1].includes('engine'));
        return { sec: a && b ? (b[0] - a[0]) / 1000 : null, load: window.grillDeviceSTT.load, origin: window.__rec.origin, engine: e ? e[0] : null };
      });
      res.warm.push(w);
      log(`warm ${JSON.stringify(w)}`);
    }
  } else {
    while (!existsSync('/out/go.json')) {
      if (existsSync('/out/alice.json')) throw new Error('alice stopped before playing');
      await sleep(500);
    }
  }

  // Play: chunks scheduled back to back on the mic's clock, each pushed one chunk ahead.
  mark('idle');
  await sleep(role === 'alice' ? 20_000 : 0);
  const clk = await page.evaluate(() => window.micClock());
  const at0 = clk.ctxTime + 2;
  res.t0 = clk.wall + 2000;
  res.clock = clk;
  if (role === 'alice') writeFileSync('/out/go.json', JSON.stringify({ t0: res.t0 }));
  log(`playing ${liveEnd.toFixed(0)} s`);
  for (let k = 0; k * CHUNK < liveEnd; k++) {
    const wait = res.t0 + (k - 1) * CHUNK * 1000 - Date.now();
    if (wait > 0) await sleep(wait);
    await page.evaluate(([b, at]) => window.micPlay(b, at), [pcm(k * CHUNK, Math.min(liveEnd, (k + 1) * CHUNK)), at0 + k * CHUNK]);
  }
  await sleep(res.t0 + liveEnd * 1000 + 20_000 - Date.now());
  mark('end');
  res.rec = await page.evaluate(() => window.__rec);
  res.pipeline = await page.evaluate(() => window.grillDeviceSTT?.pipeline);
} catch (e) {
  res.error = String(e?.stack || e);
  log(`ERROR ${res.error}`);
} finally {
  mem?.stop();
  res.mem = mem?.rows;
  writeFileSync(`/out/${role}.json`, JSON.stringify(res));
  await ctx.close().catch(() => {});
}
