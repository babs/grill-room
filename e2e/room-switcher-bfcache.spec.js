import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';

// Playwright launches Chrome with the back-forward cache off: this file turns it back on.
test.use({ launchOptions: { ignoreDefaultArgs: ['--disable-back-forward-cache'] } });

const admin = { Authorization: `Bearer ${adminToken}` };

// 041-AC5: Back from a room made through /profile#rooms/new, restored from the back-forward cache,
// shows the Rooms tab with the sheet shut, not the sheet left open on a room already created.
test('Back from a new room restored from the cache shuts the sheet', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Start' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Alice');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Alice');
  await page.goto('/profile#rooms/new');
  await page.evaluate(() => { window.kept = 1; });
  await page.getByRole('button', { name: 'Use this starter' }).click();
  await page.getByLabel('Room name').fill('Made here');
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-name')).toHaveText('Made here');
  // A cache restore fires no load event: wait for the commit only.
  await page.goBack({ waitUntil: 'commit' });
  await expect(page).toHaveURL(/\/profile#rooms$/);
  // The same page came back, so the case under test is the restored one.
  await expect.poll(() => page.evaluate(() => window.kept)).toBe(1);
  await expect(page.locator('#room-create-sheet')).toBeHidden();
});

// 041-AC12: /profile restored from the cache by Back reads the counts again: a room read since
// shows none.
test('Back to /profile from the cache shows fresh counts', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Counted' } })).json();
  const join = async (name) => {
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const page = await (await browser.newContext()).newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    return page;
  };
  const alice = await join('Alice');
  await alice.goto('/profile#rooms');
  const bob = await join('Bob');
  await bob.evaluate((id) => fetch(`/api/rooms/${id}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"body":"news"}' }), room.id);
  await alice.reload();
  const badge = alice.locator('#rooms .card .badge');
  await expect(badge).toHaveText('1');
  await alice.evaluate(() => { window.kept = 1; });
  const read = alice.waitForResponse((r) => r.request().method() === 'PUT' && r.url().endsWith('/read'));
  await alice.locator('#rooms .card a').click();
  expect((await read).status()).toBe(204);
  await alice.goBack({ waitUntil: 'commit' });
  await expect.poll(() => alice.evaluate(() => window.kept)).toBe(1);
  await expect(badge).toHaveCount(0);
});
