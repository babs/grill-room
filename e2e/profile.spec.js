import { test, expect } from '@playwright/test';
import { menuItem } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

const invitePath = async (request, roomID) => {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  return new URL(inv.url).pathname + new URL(inv.url).hash;
};

async function join(page, path, name) {
  await page.goto(path);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
}

const newRoom = async (request, name) => (await request.post('/api/rooms', { headers: admin, data: { name } })).json();

// 002-AC1 002-AC2 002-AC3: join makes a profile and shows its recovery link once; a second room
// joins the same profile; the link signs a new browser into it, rooms included.
test('profile across rooms and devices', async ({ browser, request }) => {
  const a = await newRoom(request, 'Room A');
  const b = await newRoom(request, 'Room B');
  const ctx = await browser.newContext({ permissions: ['clipboard-read', 'clipboard-write'] });
  const page = await ctx.newPage();
  await join(page, await invitePath(request, a.id), 'Alice');
  // 005-AC32: no panel on the first join; the user menu holds the link, and one notice offers it
  // after the first message.
  await expect(page.locator('#recovery')).toBeHidden();
  await page.locator('#body').fill('hello');
  await page.locator('#body').press('Enter');
  await expect(page.locator('#notice-text')).toContainText('Use on another device');
  await menuItem(page, 'Use on another device', 'me');
  await expect(page.locator('#recovery')).toBeVisible();
  await expect(page.locator('#recovery-link')).toHaveValue(/\/profile#recover=[A-Z0-9]+$/);
  const link = await page.locator('#recovery-link').inputValue();
  await page.getByRole('button', { name: 'Copy link' }).click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(link);
  await page.getByRole('button', { name: 'I saved it' }).click();
  await expect(page.locator('#recovery')).toBeHidden();
  await page.reload();
  await expect(page.locator('#me')).toHaveText('Alice');
  await expect(page.locator('#recovery')).toBeHidden();
  // Saved, the link is gone from this tab: the menu points to the email sign-in.
  await menuItem(page, 'Use on another device', 'me');
  await expect(page.locator('#recovery-none')).toBeVisible();
  await expect(page.locator('#recovery-has')).toBeHidden();
  await page.keyboard.press('Escape');

  await join(page, await invitePath(request, b.id), 'Ali');
  await expect(page.locator('#recovery')).toBeHidden();
  await menuItem(page, 'My rooms and profile', 'me');
  await expect(page).toHaveURL('/profile');
  await expect(page.locator('#rooms li .room-line')).toHaveText(['Room B as Ali', 'Room A as Alice']);

  // A browser holding its own profile is warned, and Cancel leaves it as it was.
  const bob = await (await browser.newContext()).newPage();
  await join(bob, await invitePath(request, b.id), 'Bob');
  await bob.goto(new URL(link).pathname + new URL(link).hash);
  await expect(bob.locator('#confirm-text')).toContainText('This browser already holds a profile.');
  await bob.getByRole('button', { name: 'Cancel' }).click();
  await expect(bob.locator('#status')).toHaveText('Link not used.');
  await expect(bob.locator('#rooms li .room-line')).toHaveText(['Room B as Bob']);
  expect(new URL(bob.url()).hash).toBe('');

  const other = await (await browser.newContext()).newPage();
  await other.goto(new URL(link).pathname + new URL(link).hash);
  await expect(other.locator('#confirm-text')).toHaveText('Only continue if you asked for this link yourself.');
  await other.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(other.locator('#status')).toHaveText('Signed in on this device.');
  await expect(other.locator('#rooms li .room-line')).toHaveText(['Room B as Ali', 'Room A as Alice']);
  await other.getByRole('link', { name: 'Room A' }).click();
  await expect(other.locator('#me')).toHaveText('Alice');
  await other.getByLabel('Message').fill('from the other device');
  await other.getByLabel('Message').press('Enter');
  await expect(other.locator('.msg.text').last()).toContainText('from the other device');

  // Without SMTP, no email option anywhere.
  await other.goto('/profile');
  await expect(other.locator('#email')).toBeHidden();
  const stranger = await (await browser.newContext()).newPage();
  await stranger.goto('/profile');
  await expect(stranger.locator('#signed-out')).toBeVisible();
  await expect(stranger.locator('#signin')).toBeHidden();
});

// 002-AC6 moved to room-admins.spec.js: renaming is a room admin's since 004-AC8.

// 002-AC7: a member creates an invite, a guest joins through it, the member revokes it.
test('member invites and revokes', async ({ browser, request }) => {
  const room = await newRoom(request, 'Invites');
  const alice = await (await browser.newContext()).newPage();
  const bob = await (await browser.newContext()).newPage();
  await join(alice, await invitePath(request, room.id), 'Alice');
  await join(bob, await invitePath(request, room.id), 'Bob');
  await menuItem(alice, 'Invite people');
  await alice.getByRole('button', { name: 'Create an invite link' }).click();
  await expect(alice.locator('#invite-link')).toHaveValue(/\/join#[A-Z0-9]+$/);
  const url = await alice.locator('#invite-link').inputValue();
  await expect(alice.locator('#invite-list li')).toHaveCount(1);

  const carol = await (await browser.newContext()).newPage();
  await join(carol, new URL(url).pathname + new URL(url).hash, 'Carol');
  await expect(alice.locator('#participants')).toContainText('Carol');

  // Bob sees only his own invites: none.
  await menuItem(bob, 'Invite people');
  await expect(bob.locator('#invite-list li')).toHaveCount(0);

  await alice.locator('#invite-list').getByRole('button', { name: /Revoke/ }).click();
  await expect(alice.locator('#invite-list li')).toHaveCount(0);
  await expect(carol.locator('#banner')).toHaveText('Your access to this room has ended.');
});
