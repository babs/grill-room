import { test, expect } from '../fixtures.js';
import { menuItem } from '../helpers.js';

// 019-AC8: no invite and no profile shows the form; it lands in the room as its admin; past the
// per-IP limit the reason shows under the form.
test('create a room without an invite, then the per-IP limit', async ({ browser }, testInfo) => {
  // One IP per repeat: the hourly limit outlives a run.
  const ip = { 'X-Forwarded-For': `198.51.100.${7 + testInfo.repeatEachIndex}` };
  const ctx = await browser.newContext({ extraHTTPHeaders: ip });
  const page = await ctx.newPage();
  await page.goto('/');
  const create = page.getByRole('button', { name: 'Create a room' });
  await expect(create).toBeVisible();
  await expect(page.locator('#status')).toHaveText('');
  await create.click();
  await expect(page.locator('#room-name-error')).toContainText('Type a name for the room');
  await expect(page.locator('#display-name-error')).toContainText('Type the name the others will see');
  await page.getByLabel('Room name').fill('Open room');
  await page.getByLabel('Display name').fill('Ann');
  await create.click();
  await expect(page).toHaveURL(/\/r\/[a-z0-9]+$/);
  await expect(page.locator('#me')).toHaveText('Ann');
  // The recovery link reaches the room page, as after a join.
  await menuItem(page, 'Use on another device', 'me');
  await expect(page.locator('#recovery-link')).toHaveValue(/\/profile#recover=[A-Z0-9]+$/);
  await page.keyboard.press('Escape');
  // General is a room admin's tab only: a member's sheet opens without it.
  await menuItem(page, 'Room settings');
  await expect(page.locator('#manage #room-panel #panel-general')).toBeVisible();
  // With a profile now: no form, the profile link and the invite line.
  await page.goto('/');
  await expect(page.locator('#your-profile')).toBeVisible();
  await expect(create).toBeHidden();
  await expect(page.locator('#status')).toContainText('You need an invite link');

  const other = await (await browser.newContext({ extraHTTPHeaders: ip })).newPage();
  await other.goto('/');
  await other.getByLabel('Room name').fill('Second');
  await other.getByLabel('Display name').fill('Bob');
  await other.getByRole('button', { name: 'Create a room' }).click();
  await expect(other.locator('#create-error')).toContainText('too many rooms created from your network');
  await expect(other).toHaveURL(/\/$/);
});

// 019-AC8: a reply that is not JSON, as the plain-text 404 once the flag is off, still shows a
// reason under the form, not a network error.
test('a non-JSON refusal shows a reason under the form', async ({ page }) => {
  await page.route('**/api/rooms/anonymous', (route) => route.fulfill({ status: 404, contentType: 'text/plain', body: '404 page not found\n' }));
  await page.goto('/');
  await page.getByLabel('Room name').fill('Gone');
  await page.getByLabel('Display name').fill('Cy');
  await page.getByRole('button', { name: 'Create a room' }).click();
  await expect(page.locator('#create-error')).toHaveText('Room creation is unavailable right now.');
});

// 019-AC8: when the profile check fails, the page falls back to the invite line, with no form.
test('a failed profile check shows the invite line', async ({ page }) => {
  await page.route('**/api/profile', (route) => route.fulfill({ status: 500, contentType: 'application/json', body: '{}' }));
  await page.goto('/');
  await expect(page.locator('#status')).toContainText('You need an invite link');
  await expect(page.locator('#create')).toBeHidden();
});
