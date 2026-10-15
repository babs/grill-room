// 010-AC18 on plain Google Chrome, attached over CDP only to set up and to collect, so no client holds
// the tab while it is hidden. setup: Alice joins, mutes the room keeping her mic, turns device mode on,
// and the scripted mic gets the whole sentence schedule; then the process exits, which drops CDP.
// collect: closes the tab that hid the room and reads the page's record. Writes /out/ac18-<phase>.json.
// usage: node ac18.mjs setup|collect
import { readFileSync, writeFileSync } from 'node:fs';
import { chromium } from '/nm/@playwright/test/index.mjs';
import { base, deviceSettled, fakeMic, invite, join, man, menu, newRoom, pcm, recorder, sleep } from './lib.mjs';

const phase = process.argv[2];
const every = Number(process.env.EVERY_S || 18.5);
const log = (s) => console.log(`[ac18 ${phase}] ${new Date().toISOString()} ${s}`);
const browser = await chromium.connectOverCDP(`http://127.0.0.1:${process.env.CDP_PORT}`);
const ctx = browser.contexts()[0];
const res = { phase };
try {
  if (phase === 'setup') {
    await ctx.addInitScript(fakeMic);
    await ctx.addInitScript(recorder);
    const page = ctx.pages()[0] || await ctx.newPage();
    const room = await newRoom('AC18 hidden tab');
    res.room = room.id;
    await join(page, await invite(room), 'Alice');
    // Mute the room, keep the mic: the tab plays nothing, the voice still goes out.
    await menu(page);
    await page.locator('#voice-keepmic input').check();
    await page.locator('#voice-deaf').click();
    await menu(page);
    await page.locator('#voice-device input').check();
    res.status = await deviceSettled(page, 900_000);
    res.selfTest = await page.evaluate(() => window.grillDeviceSTT.selfTest);
    res.load = await page.evaluate(() => window.grillDeviceSTT.load);
    res.deaf = await page.locator('#voice-deaf').getAttribute('aria-pressed');
    res.micMuted = await page.locator('#voice-mute').getAttribute('aria-pressed');
    log(JSON.stringify(res));
    if (!/Transcribed on this device/.test(res.status)) throw new Error(`device mode refused: ${res.status}`);
    // The Piper lines, one every `every` seconds, all scheduled now on the mic's clock.
    const clk = await page.evaluate(() => window.micClock());
    const lines = man.spans.filter((s) => s.set === 'piper');
    res.schedule = [];
    for (const [i, s] of lines.entries()) {
      const at = clk.ctxTime + 3 + i * every;
      const dur = await page.evaluate(([b, a]) => window.micPlay(b, a), [pcm(s.start, s.end), at]);
      const start = clk.wall + (at - clk.ctxTime) * 1000;
      res.schedule.push({ item: s.item, lang: s.lang, ref: s.ref, start, end: start + dur * 1000 });
    }
    res.visibility = await page.evaluate(() => document.visibilityState);
  } else {
    const pages = ctx.pages();
    const page = pages.find((p) => !p.url().endsWith('/healthz'));
    for (const p of pages) if (p !== page) await p.close();
    const setup = JSON.parse(readFileSync('/out/ac18-setup.json', 'utf-8'));
    const end = setup.schedule.at(-1).end + 20_000;
    if (end > Date.now()) await sleep(end - Date.now());
    res.url = page.url().replace(base, '');
    res.rec = await page.evaluate(() => window.__rec);
    res.pipeline = await page.evaluate(() => window.grillDeviceSTT.pipeline);
  }
} catch (e) {
  res.error = String(e?.stack || e);
  log(`ERROR ${res.error}`);
} finally {
  writeFileSync(`/out/ac18-${phase}.json`, JSON.stringify(res));
  // No browser.close(): it would end the browser; exiting drops the CDP connection only.
  process.exit(res.error ? 1 : 0);
}
