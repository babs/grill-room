import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { signIn, noOverflow, rowAction, unfoldTabs } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// 004-AC1: a wrong token is refused, and past 10 tries from one IP even the right one is, for 15
// minutes. The browser gets an IP of its own: every other test signs in from 127.0.0.1, and each
// repeat or retry of this one from another, as the server and its limiter outlive them.
test('admin login refuses a wrong token, then rate-limits the IP', async ({ browser }, info) => {
  const ip = `203.0.113.${50 + info.repeatEachIndex * 10 + info.retry}`;
  const ctx = await browser.newContext({ extraHTTPHeaders: { 'X-Forwarded-For': ip } });
  const page = await ctx.newPage();
  for (let i = 0; i < 10; i++) {
    expect(await signIn(page, 'wrong-' + i)).toBe(401);
    await expect(page.locator('#login-status')).toHaveText('wrong admin token');
  }
  expect(await signIn(page)).toBe(429);
  await expect(page.locator('#login-status')).toHaveText('too many failed logins, retry in 15 minutes');
  await expect(page.getByRole('heading', { name: 'Sessions' })).toHaveCount(0);
  await ctx.close();
});

// 004-AC1: the page lists the open sessions and revokes any; the revoked browser gets a 401 on
// its next request and is back at the login form. Signing out ends one's own.
test('admin sessions list and revoke', async ({ browser }) => {
  const a = await (await browser.newContext()).newPage();
  const bCtx = await browser.newContext({ userAgent: 'e2e-second-browser' });
  const b = await bCtx.newPage();
  expect(await signIn(a)).toBe(201);
  expect(await signIn(b)).toBe(201);
  await expect(b.getByRole('heading', { name: 'Sessions' })).toBeVisible();

  await a.reload();
  await expect(a.locator('#sessions .list-row', { hasText: 'this browser' })).toHaveCount(1);
  const row = a.locator('#sessions .list-row', { hasText: 'e2e-second-browser' });
  await expect(row).toHaveCount(1);
  await rowAction(row, /^Revoke/);
  await expect(row).toHaveCount(0);

  expect((await bCtx.request.get('/api/admin/sessions')).status()).toBe(401);
  await b.getByRole('link', { name: 'Audit', exact: true }).click();
  await expect(b.getByLabel('Admin token')).toBeVisible();
  await expect(b.getByRole('alert')).toHaveText('Your session ended: sign in again.');

  await a.locator('#logout').click();
  await expect(a.getByLabel('Admin token')).toBeVisible();
  expect((await a.context().request.get('/api/admin/sessions')).status()).toBe(401);
});

// 004-AC6 004-AC19: real admin actions show in the audit view, the room's name kept after its
// delete; the view filters by target and action, and the page fits a phone.
test('audit view shows real admin actions, filters, and fits a phone', async ({ browser, request }) => {
  const name = `Audit ${Date.now()}`;
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
  expect((await request.patch(`/api/rooms/${room.id}`, { headers: admin, data: { name: name + ' renamed' } })).status()).toBe(200);
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  expect((await request.delete(`/api/rooms/${room.id}/invites/${inv.id}`, { headers: admin })).status()).toBe(200);
  expect((await request.delete(`/api/rooms/${room.id}`, { headers: admin })).status()).toBe(204);

  const page = await (await browser.newContext()).newPage();
  expect(await signIn(page)).toBe(201);
  await page.getByRole('link', { name: 'Audit', exact: true }).click();
  await page.locator('#audit-target').fill(room.id);
  await page.locator('#audit-target').press('Enter');
  const rows = page.locator('#audit .list-row:not(.head)');
  await expect(rows).toHaveCount(5);
  // Actions read in words (004b-AC7), their code in the tip.
  const want = [['Room deleted', name + ' renamed'], ['Invite revoked', name + ' renamed'], ['Invite created', name + ' renamed'],
    ['Room renamed', name + ' renamed'], ['Room created', name]];
  for (const [i, [action, target]] of want.entries()) {
    await expect(rows.nth(i)).toContainText(action);
    await expect(rows.nth(i)).toContainText(target);
    await expect(rows.nth(i)).toContainText('super-admin');
  }
  await expect(rows.nth(3)).toContainText(`from: ${name}`);
  await page.locator('#audit-action').selectOption('room.delete');
  await expect(rows).toHaveCount(1);
  await expect(rows.first().locator(`[data-tip="${room.id}"]`)).toHaveCount(1);
  // The login just made is there too, under this session.
  await page.locator('#audit-target').fill('');
  await page.locator('#audit-target').press('Enter');
  await page.locator('#audit-action').selectOption('admin.login');
  await expect(rows.first()).toContainText('via: session');

  await page.setViewportSize({ width: 390, height: 844 });
  // The tab bar folds into a menu on a phone: every tab is in its list.
  await unfoldTabs(page.locator('nav.tabs[aria-label=Views]'));
  await expect(page.getByRole('link', { name: 'Sessions' })).toBeInViewport();
  await expect(page.locator('#logout')).toBeInViewport();
  expect(await noOverflow(page)).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  await page.getByRole('link', { name: 'Sessions' }).click();
  await expect(page.locator('#sessions')).toBeVisible();
  expect(await noOverflow(page)).toEqual([]);
  await page.locator('#logout').click();
  await expect(page.getByLabel('Admin token')).toBeInViewport();
  expect(await noOverflow(page)).toEqual([]);
});
