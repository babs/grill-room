import { mkdirSync, writeFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';

// 013-AC17 on the lab deployment: with each persona's bot in the call and nobody speaking, the first
// audio frame leaves the pod within 0.3 s of the line (p95 over 20 lines), and two rooms speaking
// at once play with no gap over 100 ms. A fake host daemon answers each turn with a canned
// spoken.md through the real result route: no Claude request. The tts pod's metrics come through
// `kubectl -n grill-room port-forward deploy/tts <port>:9100`, its URL in LAB_TTS_METRICS.
const base = `https://grill-room.${process.env.K8S_DOMAIN}`;
const admin = { Authorization: `Bearer ${process.env.GRILL_ROOM_ADMIN_TOKEN}` };
const json = { 'Content-Type': 'application/json' };
const metricsURL = process.env.LAB_TTS_METRICS;
const LINES = 10;

const spoken = [
  'I changed the payment spec. Retries now stop after three tries, with a longer pause each time.',
  "J'ai relu la spec de paiement. Le webhook GitHub renvoie une 502 quand le pod redémarre.",
];

function silentMic() {
  navigator.mediaDevices.getUserMedia = async () => {
    const ctx = new AudioContext();
    const dst = ctx.createMediaStreamDestination();
    ctx.createGain().connect(dst);
    return dst.stream;
  };
}

async function listener(browser, roomID) {
  const inv = await (await fetch(`${base}/api/rooms/${roomID}/invites`, { method: 'POST', headers: admin })).json();
  const ctx = await browser.newContext({ permissions: ['microphone'] });
  await ctx.addInitScript(silentMic);
  const page = await ctx.newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Listener');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Listener');
  return page;
}

// fakeDaemon pairs a host daemon for room that answers each turn with a canned spoken.md.
async function fakeDaemon(page, roomID, line) {
  const room = `${base}/api/rooms/${roomID}`;
  const p = await (await fetch(`${room}/pairings`, { method: 'POST', headers: json, body: JSON.stringify({ name: 'lab-voice' }) })).json();
  expect(await page.evaluate(([r, id]) => fetch(`/api/rooms/${r}/pairings/${id}`, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ approve: true }) }).then((x) => x.status), [roomID, p.id])).toBe(200);
  const { token } = await (await fetch(`${base}/api/pairings/${p.id}`, { headers: { Authorization: 'Bearer ' + p.secret } })).json();
  const auth = { Authorization: 'Bearer ' + token };
  const stop = new AbortController();
  let done = 0;
  const events = await fetch(`${room}/daemon/events`, { headers: auth, signal: stop.signal });
  (async () => {
    const dec = new TextDecoder();
    let buf = '';
    const session = crypto.randomUUID();
    let offset = 0;
    for await (const chunk of events.body) {
      buf += dec.decode(chunk);
      for (let i; (i = buf.indexOf('\n\n')) >= 0; buf = buf.slice(i + 2)) {
        const ev = buf.slice(0, i);
        if (!ev.includes('event: turn')) continue;
        const id = JSON.parse(ev.split('data: ')[1]).id;
        const job = await (await fetch(`${room}/daemon/turns/${id}`, { headers: auth })).json();
        const res = await fetch(`${room}/daemon/turns/${id}`, { method: 'POST', headers: { ...auth, ...json }, body: JSON.stringify({
          synthesis: `${job.persona}: see the call.`, session_id: session, session_offset: offset, session: btoa('lab\n'),
          changes: [{ path: 'spoken.md', content: Buffer.from(line).toString('base64') }],
        }) });
        offset += 4;
        if (res.status === 200) done++;
      }
    }
  })().catch(() => {});
  expect((await fetch(`${room}/daemon/status`, { method: 'POST', headers: { ...auth, ...json }, body: JSON.stringify({ can_host: true }) })).status).toBe(204);
  return { stop: () => stop.abort(), done: () => done };
}

async function metrics() {
  const text = await (await fetch(metricsURL)).text();
  const get = (re) => Number(text.match(re)?.[1] ?? 0);
  const buckets = Object.fromEntries([...text.matchAll(/^grill_tts_first_audio_seconds_bucket\{le="([^"]+)"\} (\S+)$/gm)].map((m) => [m[1], Number(m[2])]));
  return { buckets, underruns: get(/^grill_tts_underruns_total (\S+)$/m), spoken: get(/^grill_tts_lines_spoken_total (\S+)$/m) };
}

test('two rooms speak at once on the lab', async ({ browser }) => {
  test.skip(!metricsURL, 'LAB_TTS_METRICS is not set: port-forward the tts pod first');
  test.setTimeout(900_000);
  const before = await metrics();
  const rooms = [];
  for (const [i, line] of spoken.entries()) {
    const room = await (await fetch(`${base}/api/rooms`, { method: 'POST', headers: { ...admin, ...json }, body: JSON.stringify({ name: `Lab voice ${i}` }) })).json();
    const page = await listener(browser, room.id);
    const daemon = await fakeDaemon(page, room.id, line);
    const inAudio = page.waitForResponse((r) => r.url().endsWith('/presence') && r.request().postDataJSON().audio === true);
    await page.getByRole('button', { name: 'Join audio' }).click();
    await inAudio;
    await expect(page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(1, { timeout: 60_000 });
    rooms.push({ room, page, daemon });
  }
  const agent = async (r) => (await r.page.request.get(`/api/rooms/${r.room.id}/agent`)).json();
  for (let n = 0; n < LINES; n++) {
    await Promise.all(rooms.map(async (r) => {
      const grill = (await agent(r)).personas[0];
      const res = await r.page.request.post(`/api/rooms/${r.room.id}/turns`, { data: { persona_id: grill.id } });
      expect(res.status()).toBe(201);
      await expect.poll(() => r.daemon.done(), { timeout: 60_000 }).toBe(n + 1);
    }));
    // Each line ends before the next turn: first audio is measured with the room quiet.
    await expect.poll(async () => (await metrics()).spoken - before.spoken, { timeout: 120_000 }).toBe(rooms.length * (n + 1));
    if (n === 1) {
      mkdirSync('test-results/lab', { recursive: true });
      await rooms[0].page.screenshot({ path: 'test-results/lab/voice-room.png' });
    }
  }
  const after = await metrics();
  const total = after.buckets['+Inf'] - (before.buckets['+Inf'] ?? 0);
  const fast = after.buckets['0.3'] - (before.buckets['0.3'] ?? 0);
  const report = { lines: total, within_0_3s: fast, share: fast / total, underruns: after.underruns - before.underruns, buckets: after.buckets };
  mkdirSync('test-results/lab', { recursive: true });
  writeFileSync('test-results/lab/voice-metrics.json', JSON.stringify(report, null, 1) + '\n');
  console.log('lab voice', JSON.stringify(report));
  for (const r of rooms) {
    r.daemon.stop();
    await fetch(`${base}/api/rooms/${r.room.id}`, { method: 'DELETE', headers: admin });
  }
  expect(total).toBe(rooms.length * LINES);
  expect(fast / total).toBeGreaterThanOrEqual(0.95);
  expect(report.underruns).toBe(0);
});
