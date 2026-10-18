import { randomUUID } from 'node:crypto';
import { test, expect } from '../fixtures.js';
import { fakeDaemon } from '../helpers.js';
import { adminToken } from '../playwright.config.js';
import { podA, podB } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// join opens an invite of room on the server at origin: the invite comes from pod A either way.
async function join(browser, request, roomID, origin, name) {
  const inv = new URL((await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json()).url);
  const page = await (await browser.newContext()).newPage();
  await page.goto(origin + inv.pathname + inv.hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

async function roomOnBothPods(browser, request) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Two pods' } })).json();
  return { room, alice: await join(browser, request, room.id, podA, 'Alice'), bob: await join(browser, request, room.id, podB, 'Bob') };
}

// watchLines records, in page, when each chat line's text first shows: the page's clock, which the
// sending page shares.
const watchLines = (page) => page.evaluate(() => {
  window.seenAt = {};
  const list = document.querySelector('#messages');
  new MutationObserver(() => {
    for (const b of list.querySelectorAll('li.msg .body')) {
      const t = b.textContent.trim();
      if (!(t in window.seenAt)) window.seenAt[t] = Date.now();
    }
  }).observe(list, { childList: true, subtree: true, characterData: true });
});

const seenAt = (page, text) => page.evaluate((t) => Object.entries(window.seenAt).find(([k]) => k.includes(t))?.[1], text);

// postResult posts a turn's result as the daemon does, from page, and returns when it left.
const postResult = (page, roomID, d, turnID, synthesis) => page.evaluate(async ([room, token, id, body]) => {
  const at = Date.now();
  const r = await fetch(`/api/rooms/${room}/daemon/turns/${id}`, { method: 'POST',
    headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  return { at, status: r.status };
}, [roomID, d.token, turnID, { session_id: randomUUID(), session_offset: 0, synthesis }]);

const p95 = (xs) => [...xs].sort((a, b) => a - b)[Math.ceil(xs.length * 0.95) - 1];

// 017-AC10: lines posted through pod A show in a browser streaming from pod B within 1 s, p95 over
// 20 lines; so does a turn result posted to A.
test('lines and a turn result posted to pod A reach a browser on pod B', async ({ browser, request }) => {
  const { room, alice, bob } = await roomOnBothPods(browser, request);
  await watchLines(bob);
  const tag = randomUUID().slice(0, 8);
  const sent = await alice.evaluate(async ([room, tag]) => {
    const out = {};
    for (let i = 0; i < 20; i++) {
      const body = `line ${i} ${tag}`;
      out[body] = Date.now();
      const r = await fetch(`/api/rooms/${room}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body }) });
      if (r.status !== 201) throw new Error(`post ${i}: ${r.status}`);
      await new Promise((ok) => setTimeout(ok, 100)); // paces the lines, as people type
    }
    return out;
  }, [room.id, tag]);
  await expect.poll(() => bob.evaluate((tag) => Object.keys(window.seenAt).filter((k) => k.endsWith(tag)).length, tag), { timeout: 10_000 }).toBe(20);
  const seen = await bob.evaluate(() => window.seenAt);
  const deltas = Object.entries(sent).map(([body, at]) => seen[body] - at);
  test.info().annotations.push({ type: 'lines p95 ms', description: String(p95(deltas)) });
  expect(p95(deltas)).toBeLessThanOrEqual(1000);

  const d = await fakeDaemon(alice, podA, room.id, 'alice-laptop');
  try {
    const ask = alice.getByRole('button', { name: 'Ask Grill' });
    await expect(ask).toBeEnabled();
    await ask.click();
    await expect.poll(() => d.turns.length).toBe(1);
    const synthesis = `result ${tag}`;
    const res = await postResult(alice, room.id, d, d.turns[0], synthesis);
    expect(res.status).toBe(200);
    await expect.poll(() => seenAt(bob, synthesis), { timeout: 5_000 }).toBeTruthy();
    const delta = (await seenAt(bob, synthesis)) - res.at;
    test.info().annotations.push({ type: 'turn result ms', description: String(delta) });
    expect(delta).toBeLessThanOrEqual(1000);
  } finally {
    d();
  }
});

// 017-AC11: Bob's app streams from pod A and reports its status to pod B; Bob presses "run" on B and
// the result is posted to B: the turn reaches the app on A and its result shows in both browsers.
// No request answers "agent is offline".
test('a turn runs whatever pod each request lands on', async ({ browser, request }) => {
  const { room, alice, bob } = await roomOnBothPods(browser, request);
  const offline = [];
  for (const page of [alice, bob]) {
    page.on('response', async (r) => {
      if (r.status() === 409 && (await r.text().catch(() => '')).includes('offline')) offline.push(r.url());
    });
  }
  const d = await fakeDaemon(bob, podB, room.id, 'bob-laptop', {}, podA);
  try {
    const ask = bob.getByRole('button', { name: 'Ask Grill' });
    await expect(ask).toBeEnabled();
    const [press] = await Promise.all([bob.waitForResponse((r) => r.url().endsWith(`/api/rooms/${room.id}/turns`)), ask.click()]);
    expect(press.status()).toBe(201);
    await expect.poll(() => d.turns.length).toBe(1);
    const synthesis = `answer ${randomUUID().slice(0, 8)}`;
    expect((await postResult(bob, room.id, d, d.turns[0], synthesis)).status).toBe(200);
    for (const page of [alice, bob]) await expect(page.locator('#messages li.msg.agent').last()).toContainText(synthesis);
    expect(offline).toEqual([]);
  } finally {
    d();
  }
});
