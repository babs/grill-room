import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

async function invite(request, roomID) {
  const res = await request.post(`/api/rooms/${roomID}/invites`, { headers: admin });
  expect(res.status()).toBe(201);
  const inv = await res.json();
  return { ...inv, path: new URL(inv.url).pathname + new URL(inv.url).hash };
}

async function join(page, path, name) {
  await page.goto(path);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
}

// 001-AC1 001-AC3 001-AC4: a guest joins from the link in a browser, chats live, then loses access.
test('join by invite, chat, revoke, delete', async ({ browser, request }) => {
  const res = await request.post('/api/rooms', { headers: admin, data: { name: 'E2E room' } });
  expect(res.status()).toBe(201);
  const room = await res.json();
  const aliceInv = await invite(request, room.id);
  const bobInv = await invite(request, room.id);

  const alice = await (await browser.newContext()).newPage();
  await join(alice, aliceInv.path, 'Alice');
  await expect(alice).toHaveURL(`/r/${room.id}`);
  await expect(alice.locator('#room-name')).toHaveText('E2E room');
  await expect(alice.locator('#me')).toHaveText('Alice');
  await expect(alice.locator('#messages')).toContainText('Alice joined');

  await alice.getByLabel('Message').fill('hello from alice');
  await alice.getByLabel('Message').press('Enter');
  await expect(alice.locator('.msg.text').last()).toContainText('Alice');
  await expect(alice.locator('.msg.text').last()).toContainText('hello from alice');
  await expect(alice.getByLabel('Message')).toHaveValue('');

  const bob = await (await browser.newContext()).newPage();
  await join(bob, bobInv.path, 'Bob');
  await expect(bob.locator('#messages')).toContainText('hello from alice');
  await expect(alice.locator('#messages')).toContainText('Bob joined');
  await expect(alice.locator('#participants li .name, #participants-away li .name')).toHaveText(['Alice', 'Bob']);

  // Revoking Alice's invite ends her session live, without a reload.
  expect((await request.delete(`/api/rooms/${room.id}/invites/${aliceInv.id}`, { headers: admin })).status()).toBe(204);
  await expect(alice.locator('#banner')).toHaveText('Your access to this room has ended.');
  await expect(alice.locator('#send')).toBeHidden();

  // Deleting the room ends Bob's too.
  expect((await request.delete(`/api/rooms/${room.id}`, { headers: admin })).status()).toBe(204);
  await expect(bob.locator('#banner')).toHaveText('Your access to this room has ended.');

  // A reload after the end shows the rejoin hint.
  await alice.reload();
  await expect(alice.locator('#banner')).toContainText('Open your invite link again');
});

test('join page without an invite explains what is missing', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('#status')).toContainText('You need an invite link');
  await expect(page.locator('#join')).toBeHidden();
});

test('revoked invite link is refused on the join page', async ({ page, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'R' } })).json();
  const inv = await invite(request, room.id);
  await request.delete(`/api/rooms/${room.id}/invites/${inv.id}`, { headers: admin });
  await join(page, inv.path, 'Mallory');
  await expect(page.locator('#status')).toHaveText('This invite link is invalid or was revoked.');
  await expect(page).toHaveURL(/\/join/);
});
