import { test, expect } from './fixtures.js';
import { signIn } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const path = (url) => new URL(url).pathname + new URL(url).hash;

async function join(browser, request, roomID, name) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(path(inv.url));
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

// 011-AC1 011-AC2: /admin › Profiles makes a one-time recovery link, shown once with Copy and its
// expiry; another one replaces it and voids the first; the link signs a fresh browser in once.
test('admin recovery link', async ({ browser, request }) => {
  const name = `Rita ${Date.now()}`;
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Recovery ' + name } })).json();
  const rita = await join(browser, request, room.id, name);
  const ctx = await browser.newContext({ permissions: ['clipboard-read', 'clipboard-write'] });
  const page = await ctx.newPage();
  expect(await signIn(page, adminToken, 'profiles')).toBe(201);
  const row = page.locator('#profiles tbody tr', { hasText: name });
  const generate = row.getByRole('button', { name: `Generate a recovery link for ${name}` });
  const field = row.getByLabel(`Recovery link for ${name}`, { exact: true });
  await generate.click();
  await expect(field).toHaveValue(/\/profile#recover=\S+$/);
  await expect(row.locator('.recovery-link .hint')).toContainText('expires ');
  const first = await field.inputValue();
  await row.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect(row.getByRole('button', { name: 'Copied', exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(first);
  await generate.click();
  await expect(field).not.toHaveValue(first);
  const second = await field.inputValue();

  // The voided one signs nobody in.
  const voided = await (await browser.newContext()).newPage();
  await voided.goto(path(first));
  await voided.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(voided.locator('#status')).toHaveText('this recovery link is invalid');
  await expect(voided.locator('#signed-out')).toBeVisible();

  const fresh = await (await browser.newContext()).newPage();
  await fresh.goto(path(second));
  await expect(fresh.locator('#confirm-text')).toHaveText('Only continue if you asked for this link yourself.');
  await fresh.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(fresh.locator('#status')).toHaveText('Signed in on this device.');
  await expect(fresh.locator('#rooms li .room-line')).toHaveText([`${room.name} as ${name}`]);
  expect((await (await fresh.request.get('/api/profile')).json()).profile.id)
    .toBe((await (await rita.request.get('/api/profile')).json()).profile.id);

  const again = await (await browser.newContext()).newPage();
  await again.goto(path(second));
  await again.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(again.locator('#status')).toHaveText('this recovery link is invalid');
  await expect(again.locator('#signed-out')).toBeVisible();
});

// 011-AC25 011-AC4 011-AC8: the join page links a signed-in browser to its profile; without SMTP a
// signed-out one gets no email recovery, and the room shows no marker, no email banner, and keeps
// the recovery link notice after the first message.
test('join page profile link, and no email features without SMTP', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Landing' } })).json();
  const stranger = await (await browser.newContext()).newPage();
  await stranger.goto('/');
  await expect(stranger.locator('#status')).toContainText('You need an invite link');
  await expect(stranger.locator('#your-profile')).toBeHidden();
  await expect(stranger.locator('#recover')).toBeHidden();

  const lea = await join(browser, request, room.id, 'Lea');
  await expect.poll(() => lea.evaluate(() => own.profile && own.profile.id)).toBeTruthy();
  await expect(lea.locator('#me-anon')).toBeHidden();
  await lea.locator('#body').fill('hello');
  await lea.locator('#body').press('Enter');
  await expect(lea.locator('#notice-text')).toContainText('Use on another device');
  await expect(lea.locator('#email-nudge')).toBeHidden();
  await expect(lea.locator('#participants .anon-mark')).toHaveCount(0);

  for (const at of ['/', '/join']) {
    await lea.goto(at);
    await expect(lea.getByRole('link', { name: 'Your profile Lea' })).toBeVisible();
    await expect(lea.locator('#recover')).toBeHidden();
  }
  await lea.getByRole('link', { name: 'Your profile Lea' }).click();
  await expect(lea).toHaveURL(/\/profile$/);
  await expect(lea.locator('#profile-anon')).toBeHidden();
});
