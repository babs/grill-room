import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, menuItem } from './helpers.js';
import { snoozeKey } from './quiet-push.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const day = 24 * 3600e3;
const denied = 'The browser did not allow notifications: allow them in its site settings, then try again.';

// spy fixes what the browser says of notifications and counts the permission prompts: the
// prompt itself never shows under Playwright. perm is Notification.permission, answer what the
// prompt would answer.
async function spy(page, perm, answer = 'denied') {
  await page.evaluate((k) => localStorage.removeItem(k), snoozeKey);
  await page.context().addInitScript(([perm, answer]) => {
    window.permissionCalls = 0;
    Object.defineProperty(Notification, 'permission', { get: () => perm, configurable: true });
    Notification.requestPermission = () => { window.permissionCalls++; return Promise.resolve(answer); };
  }, [perm, answer]);
  await page.reload();
  await expect(page.locator('#me')).not.toBeEmpty();
}
const calls = (page) => page.evaluate(() => window.permissionCalls);

// say sends a line; once the box is empty, the page has made its offer or not (008-AC11).
async function say(page, text) {
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
  await expect(page.locator('#body')).toHaveValue('');
  await expect(page.locator('#messages')).toContainText(text);
}

// 008-AC10: only the explainer's primary button calls requestPermission; every way out of it
// calls nothing.
test('the explainer gates the permission prompt', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await spy(alice, 'default');
  const sheet = alice.locator('#push-explain');
  const open = async () => {
    if (!(await alice.locator('#notify').evaluate((d) => d.open))) await menuItem(alice, 'Notifications', 'me');
    await alice.locator('#push-toggle').click();
    await expect(sheet).toBeVisible();
  };
  await open();
  await expect(sheet.getByRole('heading')).toHaveText('Turn on notifications?');
  await expect(sheet).toContainText('Get notified when someone mentions you directly, even when this tab is closed.');
  await expect(sheet).toContainText('Your browser will ask for permission next.');
  await expect(sheet.getByRole('button', { name: 'Not now' })).toHaveClass(/btn-secondary/);
  await expect(sheet.getByRole('button', { name: 'Turn on notifications' })).toHaveClass(/btn-primary/);
  expect(await calls(alice)).toBe(0);

  const ways = {
    'Not now': () => sheet.getByRole('button', { name: 'Not now' }).click(),
    Escape: () => alice.keyboard.press('Escape'),
    backdrop: () => alice.mouse.click(4, 4),
    '✕': () => sheet.getByRole('button', { name: 'Close' }).click(),
  };
  const snoozed = () => alice.evaluate((k) => localStorage.getItem(k), snoozeKey);
  for (const [name, close] of Object.entries(ways)) {
    await close();
    await expect(sheet, name).toBeHidden();
    expect(await calls(alice), name).toBe(0);
    await expect.poll(snoozed, name).not.toBeNull();
    await alice.evaluate((k) => localStorage.removeItem(k), snoozeKey);
    await open();
  }
  await sheet.getByRole('button', { name: 'Turn on notifications' }).click();
  await expect(sheet).toBeHidden();
  expect(await calls(alice)).toBe(1);
  await expect(alice.locator('#push-status')).toHaveText(denied);
});

// 008-AC11: the browser's own request dismissed, neither allowed nor refused, snoozes the offer
// too; the offer opens on the sheet itself, not on a button a keystroke would press.
test('a dismissed browser request snoozes the offer', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await spy(alice, 'default', 'default');
  await say(alice, 'first line');
  await expect(alice.locator('#push-explain')).toBeVisible();
  await expect(alice.locator('#push-explain-title')).toBeFocused();
  await alice.locator('#push-yes').click();
  await expect.poll(() => alice.evaluate((k) => Number(localStorage.getItem(k)) - Date.now(), snoozeKey)).toBeGreaterThan(14 * day - 60e3);
  expect(await calls(alice)).toBe(1);
  await alice.reload();
  await say(alice, 'after the reload');
  await expect(alice.locator('#push-explain')).toBeHidden();
});

// 008-AC10: allowed already, turning on subscribes at once; refused, the sheet says how to allow.
test('granted and denied skip the explainer', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await spy(alice, 'granted');
  await menuItem(alice, 'Notifications', 'me');
  const [key] = await Promise.all([
    alice.waitForRequest((r) => r.method() === 'GET' && r.url().endsWith(`/api/rooms/${room.id}/push`)),
    alice.locator('#push-toggle').click(),
  ]);
  expect(key.method()).toBe('GET');
  // No push service under Playwright: the subscription fails, and the sheet says so.
  await expect(alice.locator('#push-status')).toHaveText(/^Notifications/);
  await expect(alice.locator('#push-explain')).toBeHidden();
  expect(await calls(alice)).toBe(0);

  await spy(bob, 'denied');
  await menuItem(bob, 'Notifications', 'me');
  await bob.locator('#push-toggle').click();
  await expect(bob.locator('#push-status')).toHaveText(denied);
  await expect(bob.locator('#push-explain')).toBeHidden();
  expect(await calls(bob)).toBe(0);
});

// 008-AC11: offered once after the first message of a page load, snoozed 14 days by a dismissal,
// for every room of the browser; the Notifications sheet still reaches it; turning on clears the
// snooze.
test('the unprompted offer and its snooze', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const t0 = new Date('2026-10-13T10:00:00Z').getTime();
  await alice.clock.install({ time: t0 });
  await spy(alice, 'default', 'granted');
  const sheet = alice.locator('#push-explain');
  await say(alice, 'first line');
  await expect(sheet).toBeVisible();
  await sheet.getByRole('button', { name: 'Not now' }).click();
  // The installed clock runs on: the snooze ends 14 days after the dismissal, give or take the run.
  await expect.poll(() => alice.evaluate((k) => Number(localStorage.getItem(k)) - Date.now(), snoozeKey)).toBeGreaterThan(14 * day - 60e3);
  expect(await alice.evaluate((k) => Number(localStorage.getItem(k)) - Date.now(), snoozeKey)).toBeLessThanOrEqual(14 * day);
  await say(alice, 'second line');
  await expect(sheet).toBeHidden();

  await alice.clock.setSystemTime(t0 + 14 * day - 3600e3);
  await alice.reload();
  await say(alice, 'a day short');
  await expect(sheet).toBeHidden();
  // Another room of the same browser is snoozed too.
  const other = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Other room' } })).json();
  const inv = await (await request.post(`/api/rooms/${other.id}/invites`, { headers: admin })).json();
  await alice.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await alice.getByLabel('Your name').fill('Alice');
  await alice.getByRole('button', { name: 'Join the room' }).click();
  await expect(alice.locator('#room-name')).toHaveText('Other room');
  await say(alice, 'elsewhere');
  await expect(sheet).toBeHidden();
  // The sheet's button reaches it through the snooze.
  await menuItem(alice, 'Notifications', 'me');
  await alice.locator('#push-toggle').click();
  await expect(sheet).toBeVisible();
  await alice.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  await alice.locator('#notify').getByRole('button', { name: 'Close' }).click();

  // The Escape above started the snooze over: 14 days and a minute later, the offer is back.
  const now = await alice.evaluate(() => Date.now());
  await alice.clock.setSystemTime(now + 14 * day + 60e3);
  await alice.reload();
  await say(alice, 'two weeks on');
  await expect(sheet).toBeVisible();
  await sheet.getByRole('button', { name: 'Turn on notifications' }).click();
  expect(await alice.evaluate((k) => localStorage.getItem(k), snoozeKey)).toBeNull();
  expect(await calls(alice)).toBe(1);
});

// 008-AC11: no unprompted offer on an iPhone outside the home screen, once allowed or refused,
// with push on for this room already, or without the Push API. The docs window sends no line.
test('no unprompted offer where it cannot work', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Docs room' } })).json();
  const ua = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';
  const cases = [
    ['iPhone', { userAgent: ua, viewport: { width: 390, height: 844 } }, 'default'],
    ['granted', {}, 'granted'], ['denied', {}, 'denied'], ['already on', {}, 'default'], ['no push', {}, 'default'],
  ];
  for (const [name, opts, perm] of cases) {
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const ctx = await browser.newContext(opts);
    if (name === 'no push') await ctx.addInitScript(() => { delete window.PushManager; });
    if (name === 'already on') await ctx.addInitScript((id) => localStorage.setItem('grill-room-push:' + id, '1'), room.id);
    const page = await ctx.newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    await spy(page, perm);
    await say(page, 'hello from ' + name);
    await expect(page.locator('#push-explain'), name).toBeHidden();
    expect(await calls(page), name).toBe(0);
    await page.context().close();
  }
});
