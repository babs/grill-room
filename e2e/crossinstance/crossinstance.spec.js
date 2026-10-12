import { test, expect } from '@playwright/test';
import { join } from 'node:path';
import { ask, liveDaemon, logLines, messagesRequests, pairDaemon, removeSandbox } from '../helpers.js';

const local = `https://grill-room.${process.env.DEV_DOMAIN}`;
const lab = `https://grill-room.${process.env.K8S_DOMAIN}`;
const admin = { Authorization: `Bearer ${process.env.GRILL_ROOM_ADMIN_TOKEN}` };
// One answer, no tool: one request per turn.
const persona = 'You answer in one short sentence, from the conversation alone. Never use a tool, never write a file.';

// enter joins base's room as name in a fresh browser context.
async function enter(browser, request, base, room, name) {
  const inv = await (await request.post(`${base}/api/rooms/${room}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(inv.url);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

// move exports room from one instance and imports it on another, returning the import's answer.
async function move(request, from, room, to) {
  const res = await request.get(`${from}/api/rooms/${room}/export`, { headers: admin });
  expect(res.status()).toBe(200);
  const archive = await res.body();
  const preview = await request.post(`${to}/api/rooms/import?preview=1`, { headers: { ...admin, 'Content-Type': 'application/gzip' }, data: archive });
  expect(preview.status(), await preview.text()).toBe(200);
  const imported = await request.post(`${to}/api/rooms/import`, { headers: { ...admin, 'Content-Type': 'application/gzip' }, data: archive });
  expect(imported.status(), await imported.text()).toBe(201);
  const got = await imported.json();
  console.log(`${from === local ? 'compose' : 'lab'} -> ${to === local ? 'compose' : 'lab'}:`,
    JSON.stringify({ preview: (await preview.json()).counts, counts: got.counts, relinked: got.relinked, unlinked: got.unlinked, warnings: got.warnings }));
  expect(got.same_instance).toBe(false);
  return got;
}

// 004-AC13 004-AC12: a room moves from the compose stack to the lab, where a haiku turn resumes the
// session the compose turn wrote; then the lab room moves back to compose, where its first
// participant is relinked to the profile it started with, and the lab one keeps a uuid hint.
test('compose to lab and back, the lab turn resuming the imported session', async ({ browser, request }) => {
  const dir = join('test-results', 'crossinstance');
  const word = 'KIWI-' + Math.floor(Math.random() * 1e6);
  const daemons = [];
  try {
    const room = await (await request.post(`${local}/api/rooms`, { headers: admin, data: { name: 'Cross-instance ' + word } })).json();
    const alice = await enter(browser, request, local, room.id, 'Alice');
    const grill = (await (await alice.request.get(`${local}/api/rooms/${room.id}/agent`)).json()).personas[0];
    expect((await alice.request.patch(`${local}/api/rooms/${room.id}/personas/${grill.id}`, { data: { text: persona } })).status()).toBe(200);
    const d1 = liveDaemon(join(dir, 'compose'), `${local}/r/${room.id}`);
    daemons.push(d1);
    await pairDaemon(alice, d1);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 180_000 });
    console.log('compose answer:', await ask(alice, room.id, `Remember this code word: ${word}. Just say you will.`));

    const there = await move(request, local, room.id, lab);
    expect(there.unlinked).toBe(1);
    const bob = await enter(browser, request, lab, there.room.id, 'Bob');
    await expect(bob.locator('#messages')).toContainText(`Remember this code word: ${word}`);
    const d2 = liveDaemon(join(dir, 'lab'), `${lab}/r/${there.room.id}`);
    daemons.push(d2);
    await pairDaemon(bob, d2);
    await expect(bob.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 180_000 });
    const answer = await ask(bob, there.room.id, 'What is the code word I gave you? Answer with the word alone.');
    console.log('lab answer:', answer);
    expect(answer).toContain(word);
    expect(logLines(d2).some((l) => l.msg === 'turn_started' && l.resume === true), 'the lab turn resumed a session').toBe(true);

    const back = await move(request, lab, there.room.id, local);
    expect(back.relinked).toBe(1);
    expect(back.unlinked).toBe(1);
    await alice.goto(`${local}/r/${back.room.id}`);
    await expect(alice.locator('#messages')).toContainText(answer.trim().slice(0, 40));

    const requests = daemons.reduce((n, d) => n + messagesRequests(d), 0);
    console.log(`real Claude requests: ${requests}`);
    expect(requests).toBeLessThanOrEqual(2);
  } finally {
    for (const d of daemons) {
      d.proc.kill('SIGTERM');
      await d.exited;
      removeSandbox(d.sandbox);
    }
  }
});
