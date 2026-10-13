import { test, expect } from '@playwright/test';
import { roomWith } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const phone = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };

async function phonePage(browser, room, request) {
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext(phone)).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Carol');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Carol');
  return page;
}

const row = (page, name) => page.locator('#participants li, #participants-away li', { has: page.locator('.name', { hasText: name }) });

// 005-AC6, 005-AC7, 005-AC14 at phone width: the list opens over the chat, shows who is in the room
// with their role badges and audio state from the server, and moves someone who left to "Not here"
// once the grace runs out.
test('participant list on a phone follows the server presence', async ({ browser, request }) => {
  test.setTimeout(90_000);
  const { room, pages: [, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const carol = await phonePage(browser, room, request);
  const carolID = await carol.evaluate(() => myID);
  expect((await request.put(`/api/rooms/${room.id}/admins/${carolID}`, { headers: admin })).status()).toBe(204);

  // 005-AC36: the phone's People tab.
  await expect(carol.locator('.layout > aside')).toBeHidden();
  await carol.locator('#tabbar').getByRole('button', { name: /People/ }).click();
  await expect(carol.locator('.layout > aside')).toBeVisible();
  await expect(carol.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol']);
  await expect(carol.locator('#tab-people-count')).toHaveText('3');
  const me = row(carol, 'Carol');
  await expect(me.locator('.badge')).toHaveText(['you', 'admin']);
  await expect(me.locator('.badge.role-admin')).toHaveAttribute('title', /Room admin/);
  await expect(row(carol, 'Bob').locator('.badge')).toHaveCount(0);
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-state', 'none');

  // A second tab of Bob's in the audio, muted: everyone sees it, in the audio or not.
  const beat = (body) => bob.evaluate(([id, b]) => fetch(`/api/rooms/${id}/presence`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b),
  }).then((r) => r.status), [room.id, body]);
  expect(await beat({ tab: 'bob-second', audio: true, muted: true })).toBe(204);
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-state', 'muted');
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('title', 'Muted');
  // That tab closes: its audio goes at once, Bob stays, his first tab still open.
  expect(await beat({ tab: 'bob-second', gone: true })).toBe(204);
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-state', 'none');

  // Bob closes his page: within the grace he stays, then he is not here, collapsed, seen just now.
  await bob.context().close();
  await carol.waitForTimeout(5000);
  await expect(carol.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol']);
  await expect(carol.locator('#participants li .name')).toHaveText(['Alice', 'Carol'], { timeout: 30_000 });
  await expect(carol.locator('#away summary')).toHaveText('Not here (1)');
  await expect(row(carol, 'Bob')).toBeHidden();
  await carol.locator('#away summary').click();
  await expect(row(carol, 'Bob').locator('.seen')).toHaveText('seen just now');

  await carol.locator('#tabbar').getByRole('button', { name: /Chat/ }).click();
  await expect(carol.locator('.layout > aside')).toBeHidden();
});

// 005-AC7, 005-AC22: each audio state, the speaking pulse's neighbours, the deafened icon and the
// "transcribed" badge, from presence and stt events played to the page as the server sends them.
test('participant list shows audio states and transcription', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol', 'Dan', 'Eve']);
  const ids = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.participants.map((p) => p.id)), room.id);
  const [a, b, c, d, e] = ids;
  const away = new Date(Date.now() - 5 * 60_000).toISOString();
  const people = [
    { id: a, here: true, audio: true, mic: true, muted: false, deaf: false },
    { id: b, here: true, audio: true, mic: false, muted: true, deaf: false },
    { id: c, here: true, audio: true, mic: false, muted: false, deaf: true },
    { id: d, here: true, audio: false, mic: false, muted: false, deaf: false },
    { id: e, here: false, audio: false, mic: false, muted: false, deaf: false, seen_at: away },
  ];
  await alice.route(`**/api/rooms/${room.id}/events*`, (r) => r.fulfill({
    status: 200, headers: { 'Content-Type': 'text/event-stream' },
    body: `event: presence\ndata: ${JSON.stringify({ people })}\n\n`
      + `event: stt\ndata: ${JSON.stringify({ available: true, listening: true, transcribed: [a, b] })}\n\n`,
  }));
  await alice.reload();
  const state = (name) => row(alice, name).locator('.audio');
  await expect(state('Alice')).toHaveAttribute('data-state', 'live');
  await expect(state('Alice')).toHaveAttribute('title', 'Mic live');
  await expect(state('Bob')).toHaveAttribute('data-state', 'muted');
  await expect(state('Carol')).toHaveAttribute('data-state', 'listening');
  await expect(state('Carol')).toHaveAttribute('title', 'Listening only, deafened: hears nobody');
  await expect(state('Carol')).toContainText('🔕');
  await expect(state('Dan')).toHaveAttribute('data-state', 'none');
  for (const [name, n] of [['Alice', 1], ['Bob', 1], ['Carol', 0], ['Dan', 0]]) {
    await expect(row(alice, name).locator('.badge.transcribed')).toHaveCount(n);
  }
  await expect(alice.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol', 'Dan']);
  await expect(alice.locator('#away summary')).toHaveText('Not here (1)');
  await alice.locator('#away summary').click();
  await expect(row(alice, 'Eve').locator('.seen')).toHaveText('seen 5 min ago');
});

// 005-AC19: event lines show their time, as messages do.
test('event lines show their time', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const line = alice.locator('.msg.system', { hasText: 'Alice joined' });
  await expect(line.locator('time')).toHaveText(/^\d{2}:\d{2}/);
  await expect(line.locator('time')).toHaveAttribute('datetime', /^\d{4}-\d{2}-\d{2}T/);
});
