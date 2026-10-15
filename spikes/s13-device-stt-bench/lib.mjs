// Shared by ac18.mjs and ac19.mjs: the scripted mic, the in-page recorder, the room steps, the corpus.
import { readFileSync, readdirSync } from 'node:fs';

export const base = `http://127.0.0.1:${process.env.PORT}`;
const admin = { Authorization: `Bearer ${process.env.ADMIN_TOKEN}`, 'Content-Type': 'application/json' };
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// fakeMic (init script) replaces the microphone with a WebAudio graph. micPlay schedules 16 kHz PCM16
// on the mic's own AudioContext clock, so the audio keeps its timing with no driver attached and
// whatever the page's timers do. micClock maps that clock to wall time.
export function fakeMic() {
  let g;
  const bus = () => {
    if (!g) {
      const ctx = new AudioContext();
      g = { ctx, gain: ctx.createGain() };
    }
    return g;
  };
  const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (c) => {
    if (!c?.audio || c.video) return real(c);
    const { ctx, gain } = bus();
    const dst = ctx.createMediaStreamDestination();
    gain.connect(dst);
    return dst.stream;
  };
  window.micClock = async () => {
    const { ctx } = bus();
    await ctx.resume();
    const o = ctx.getOutputTimestamp();
    return { ctxTime: o.contextTime, wall: performance.timeOrigin + o.performanceTime, rate: ctx.sampleRate };
  };
  window.micPlay = (b64, at) => {
    const { ctx, gain } = bus();
    const bin = atob(b64);
    const n = bin.length >> 1;
    const buf = ctx.createBuffer(1, n, 16000);
    const ch = buf.getChannelData(0);
    for (let i = 0; i < n; i++) ch[i] = (((bin.charCodeAt(2 * i) | (bin.charCodeAt(2 * i + 1) << 8)) << 16) >> 16) / 32768;
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(gain);
    src.start(at);
    return n / 16000;
  };
}

// recorder (init script) keeps, in window.__rec: visibility changes, a 1 s main-thread tick (it shows
// timer throttling), the device states, every end-of-speech-to-line latency the pipeline reports, its
// mode changes and the probe records a pipeline hook sends.
export function recorder() {
  const now = () => performance.timeOrigin + performance.now();
  const rec = (window.__rec = { vis: [[now(), document.visibilityState]], ticks: [], states: [], lat: [], modes: [], probes: [], last: null });
  document.addEventListener('visibilitychange', () => rec.vis.push([now(), document.visibilityState]));
  setInterval(() => rec.ticks.push(now()), 1000);
  addEventListener('grill:device-stt', (e) => rec.states.push([now(), e.detail.state]));
  let prev = [];
  addEventListener('grill:device-pipeline', (e) => {
    const m = e.detail;
    const cur = m.latencies || [];
    // cur is the last 50 latencies: the new ones are what follows the longest overlap with prev.
    let n = 0;
    for (; n <= cur.length; n++) {
      const keep = cur.length - n;
      if (keep <= prev.length && cur.slice(0, keep).every((x, i) => x === prev[prev.length - keep + i])) break;
    }
    for (const ms of cur.slice(cur.length - n)) rec.lat.push([now(), ms, m.mode, m.queue]);
    prev = cur;
    rec.last = { ...m, latencies: undefined, t: now() };
  });
  const W = window.Worker;
  window.Worker = class extends W {
    constructor(u, o) {
      super(u, o);
      if (!String(u).includes('pipeline')) return;
      rec.lat.push([now(), null, 'new-pipeline', 0]);
      prev = [];
      this.addEventListener('message', (e) => {
        if (e.data.ev === 'mode') rec.modes.push([now(), e.data.mode, e.data.error || '']);
        else if (e.data.ev === 'probe') rec.probes.push(e.data.r);
      });
    }
  };
}

// probe (evaluated in the pipeline worker) reports each utterance sent to the engine and its length,
// which tells a pause cut from a 15 s cut.
export function probe() {
  if (self.__probed) return;
  self.__probed = true;
  const orig = MessagePort.prototype.postMessage;
  MessagePort.prototype.postMessage = function (m, t) {
    if (m && m.cmd === 'transcribe') self.postMessage({ ev: 'probe', r: { id: m.id, n: m.pcm.length, t: performance.timeOrigin + performance.now() } });
    return orig.call(this, m, t);
  };
}

export async function api(path, body) {
  const r = await fetch(base + path, { method: 'POST', headers: admin, body: JSON.stringify(body || {}) });
  if (!r.ok) throw new Error(`${path}: HTTP ${r.status}`);
  return r.json();
}

export async function newRoom(name) {
  const room = await api('/api/rooms', { name });
  return room;
}

export async function invite(room) {
  return (await api(`/api/rooms/${room.id}/invites`)).url;
}

// join enters the room under name and joins the audio.
export async function join(page, url, name) {
  const u = new URL(url);
  await page.goto(base + u.pathname + u.hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await page.locator('#me', { hasText: name }).waitFor();
  await joinAudio(page);
}

export async function joinAudio(page) {
  await page.getByRole('button', { name: 'Join audio' }).click();
  await page.getByRole('button', { name: 'Audio options' }).waitFor();
}

export async function menu(page) {
  await page.mouse.move(0, 0);
  await page.locator('#voice-menu').evaluate((m) => { if (!m.matches(':popover-open')) m.showPopover(); });
}

// deviceSettled waits until the voice is on the device, or the device refused: the status text.
export async function deviceSettled(page, ms) {
  const h = await page.waitForFunction(() => {
    const s = document.getElementById('device-stt-status')?.textContent || '';
    return (window.grillDeviceSTT?.pipeline?.mode === 'device' || /can't|too slow|stopped|did not match|wrong/.test(s)) && s;
  }, null, { timeout: ms, polling: 500 });
  return h.jsonValue();
}

// Corpus: all.wav is 16 kHz mono PCM16; manifest.json has the spans and pauses (prep.py).
export const man = JSON.parse(readFileSync('/d/manifest.json', 'utf-8'));
const wav = readFileSync('/d/all.wav');
const data = wav.indexOf('data') + 8;
// pcm returns the samples from s0 to s1 seconds as base64 PCM16.
export const pcm = (s0, s1) => wav.subarray(data + 2 * Math.round(s0 * 16000), Math.min(wav.length, data + 2 * Math.round(s1 * 16000))).toString('base64');

// sampler records, every 2 s, the RSS and CPU of this driver's Chrome processes by type (workers run
// in the renderer); the tab is the largest renderer.
export function sampler() {
  const HZ = 100;
  let prev = new Map();
  let prevT = Date.now();
  const rows = [];
  const tick = () => {
    const t = Date.now();
    const parent = new Map();
    const info = new Map();
    for (const pid of readdirSync('/proc').filter((p) => /^\d+$/.test(p))) {
      try {
        const st = readFileSync(`/proc/${pid}/stat`, 'utf-8').split(') ')[1].split(' ');
        parent.set(pid, st[1]);
        info.set(pid, { ticks: Number(st[11]) + Number(st[12]) });
      } catch { /* gone */ }
    }
    const mine = (pid) => { for (let p = pid; p && p !== '0' && p !== '1'; p = parent.get(p)) if (p === String(process.pid)) return true; return false; };
    const row = { t, renderer_max: 0, total: 0, cpu: 0, byType: {} };
    const cur = new Map();
    for (const [pid, { ticks }] of info) {
      if (pid === String(process.pid) || !mine(pid)) continue;
      try {
        const cmd = readFileSync(`/proc/${pid}/cmdline`, 'utf-8');
        if (!/chrome/.test(cmd)) continue;
        const type = (cmd.match(/--type=([a-z-]+)/) || [, 'browser'])[1];
        const rss = Number((readFileSync(`/proc/${pid}/status`, 'utf-8').match(/VmRSS:\s+(\d+)/) || [, 0])[1]) / 1024;
        row.total += rss;
        row.byType[type] = (row.byType[type] || 0) + rss;
        if (type === 'renderer') row.renderer_max = Math.max(row.renderer_max, rss);
        cur.set(pid, ticks);
        if (prev.has(pid)) row.cpu += (ticks - prev.get(pid)) / HZ / ((t - prevT) / 1000);
      } catch { /* gone */ }
    }
    prev = cur;
    prevT = t;
    rows.push(row);
  };
  const timer = setInterval(tick, Number(process.env.SAMPLE_MS || 2000));
  return { rows, stop: () => clearInterval(timer) };
}
