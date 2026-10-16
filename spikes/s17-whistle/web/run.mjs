// S17 driver: one headed Google Chrome session (Playwright, channel 'chrome') on the S17 page; samples RAM/CPU per
// Chrome process type every 2 s. Runs inside the Playwright container under Xvfb (see ../run-browser.sh).
// usage: node run.mjs --tag T --mode live|bench|load [--secs N] [--keywords] [--phone] [--reload N]
//   --phone: 390x844 viewport, mobile UA and touch. CPU emulation is the container's quota (run-browser.sh PHONE=1):
//   DevTools CPU throttling refuses worker targets ("Operation is only supported for pages, not workers").
//   --reload N: N warm loads by page.reload() after the first (load mode)
import { spawn } from 'node:child_process';
import { appendFileSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { chromium } from '/w/node_modules/@playwright/test/index.mjs';

const argv = process.argv.slice(2);
const flag = (n, d = null) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const tag = flag('tag');
const mode = flag('mode', 'bench');
const out = `/out/${tag}`;
mkdirSync(out, { recursive: true });
const log = (s) => { console.log(s); appendFileSync(`${out}/run.log`, `${new Date().toISOString()} ${s}\n`); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const events = [];

// Live: whole items only, cut at the last pause that ends inside the budget.
const man = JSON.parse(readFileSync('/data/manifest.json', 'utf-8'));
let liveEnd = man.total;
let wav = '/data/all.wav';
if (mode === 'live' && flag('secs')) {
  liveEnd = 0;
  for (const [, e] of man.pauses) if (e <= Number(flag('secs'))) liveEnd = e;
  const all = readFileSync('/data/all.wav');
  const at = all.indexOf('data') + 8;
  const pcm = all.subarray(at, at + 2 * Math.round(liveEnd * 16000));
  const h = Buffer.from(all.subarray(0, 44));
  h.writeUInt32LE(36 + pcm.length, 4);
  h.writeUInt32LE(pcm.length, 40);
  wav = `${out}/live.wav`;
  writeFileSync(wav, Buffer.concat([h, pcm]));
}

const HZ = 100;
let prev = new Map();
let prevT = Date.now();
function sample() {
  const nowT = Date.now();
  const agg = {};
  const cur = new Map();
  for (const pid of readdirSync('/proc').filter((p) => /^\d+$/.test(p))) {
    try {
      const cmd = readFileSync(`/proc/${pid}/cmdline`, 'utf-8');
      if (!/chrome/.test(cmd) || /node/.test(cmd.split('\0')[0])) continue;
      const type = (cmd.match(/--type=([a-z-]+)/) || [, 'browser'])[1];
      const st = readFileSync(`/proc/${pid}/stat`, 'utf-8').split(') ')[1].split(' ');
      const ticks = Number(st[11]) + Number(st[12]);
      const rss = Number((readFileSync(`/proc/${pid}/status`, 'utf-8').match(/VmRSS:\s+(\d+)/) || [, 0])[1]) / 1024;
      cur.set(pid, ticks);
      const a = (agg[type] ||= { rss: 0, cpu: 0, max: 0 });
      a.rss += rss;
      a.max = Math.max(a.max, rss);
      if (prev.has(pid)) a.cpu += (ticks - prev.get(pid)) / HZ / ((nowT - prevT) / 1000);
    } catch { /* process exited */ }
  }
  prev = cur;
  prevT = nowT;
  appendFileSync(`${out}/proc.jsonl`, `${JSON.stringify({ t: nowT, ...agg })}\n`);
}
const sampler = setInterval(sample, 2000);

const port = 4317;
const server = spawn('node', ['/app/server.mjs', String(port), '/eng', '/ort', '/data'], { stdio: 'inherit' });
await sleep(500);
const args = ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--autoplay-policy=no-user-gesture-required'];
if (mode === 'live') args.push(`--use-file-for-fake-audio-capture=${wav}%noloop`);
const phone = argv.includes('--phone');
const res = { tag, mode, liveEnd, argv };
let ctx;
try {
  ctx = await chromium.launchPersistentContext(flag('profile', `/prof/${tag}`), {
    channel: 'chrome', headless: false, args, permissions: ['microphone'],
    ...(phone ? { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true,
      userAgent: 'Mozilla/5.0 (Linux; Android 14; Pixel 7a) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Mobile Safari/537.36' } : {}),
  });
  res.chrome = ctx.browser()?.version() ?? null;
  const page = ctx.pages()[0] || await ctx.newPage();
  page.on('worker', async (w) => log(`worker ${w.url()}`));
  page.on('console', (m) => {
    const t = m.text();
    if (t.startsWith('[S17] ')) {
      const e = JSON.parse(t.slice(6));
      e.t = Date.now();
      events.push(e);
      appendFileSync(`${out}/events.jsonl`, `${JSON.stringify(e)}\n`);
      if (e.ev !== 'line') log(`${e.ev} ${JSON.stringify(e).slice(0, 300)}`);
    } else if (m.type() === 'error' || m.type() === 'warning') appendFileSync(`${out}/console.log`, `${Date.now()} [${m.type()}] ${t.slice(0, 400)}\n`);
  });
  page.on('crash', () => log('PAGE CRASHED'));
  const qs = new URLSearchParams({ mode, secs: flag('secs', ''), keywords: argv.includes('--keywords') ? '1' : '0' });
  const until = async (pred, ms) => { const end = Date.now() + ms; while (!pred() && Date.now() < end) await sleep(100); if (!pred()) throw new Error('timeout'); };
  const failed = () => events.some((e) => e.ev === 'error');
  const loads = [];
  for (let i = 0; i <= Number(flag('reload', '0')); i++) {
    const n = events.filter((e) => e.ev === 'loaded').length;
    const t0 = Date.now();
    if (i === 0) await page.goto(`http://127.0.0.1:${port}/?${qs}`);
    else await page.reload();
    await until(() => failed() || events.filter((e) => e.ev === 'loaded').length > n, 15 * 60 * 1000);
    if (failed()) throw new Error('page error');
    loads.push((Date.now() - t0) / 1000);
    await sleep(3000);
  }
  res.loadSec = loads;
  if (mode === 'live') {
    await until(() => events.some((e) => e.ev === 'mic'), 60000);
    await sleep(liveEnd * 1000 + 20000);
    await page.evaluate(() => window.S17.stats());
    await sleep(1000);
  } else if (mode === 'bench') {
    await until(() => failed() || events.some((e) => e.ev === 'vad-done'), 60 * 60 * 1000);
    const { utts } = events.find((e) => e.ev === 'vad-done');
    await until(() => failed() || events.filter((e) => e.ev === 'line').length >= utts, 60 * 60 * 1000);
  }
} catch (e) {
  res.error = String(e && e.stack || e);
  log(`ERROR ${res.error}`);
} finally {
  sample();
  writeFileSync(`${out}/result.json`, JSON.stringify(res, null, 1));
  clearInterval(sampler);
  await ctx?.close().catch(() => {});
  server.kill();
}
