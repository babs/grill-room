import { test, expect } from '@playwright/test';
import { signIn, noOverflow } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const path = (url) => new URL(url).pathname + new URL(url).hash;

// join seats page's browser in roomID as name, through a fresh admin invite; it returns the invite.
async function join(page, request, roomID, name) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  await page.goto(path(inv.url));
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return inv;
}

const profileOf = async (page) => (await (await page.request.get('/api/profile')).json()).profile;

// 011-AC10 011-AC11 011-AC12 011-AC14: /admin merges B into A after a preview that marks the room both sit
// in and names the final email; swap exchanges them and back. B's browser then is A, with the union
// of both room lists and no duplicate. The sheet is full screen on a phone.
test('merge two profiles from /admin', async ({ browser, request }) => {
  const stamp = Date.now();
  const anna = `Anna ${stamp}`, bea = `Bea ${stamp}`;
  const shared = await (await request.post('/api/rooms', { headers: admin, data: { name: `Shared ${stamp}` } })).json();
  const own = await (await request.post('/api/rooms', { headers: admin, data: { name: `Bea only ${stamp}` } })).json();
  const a = await (await browser.newContext()).newPage();
  await join(a, request, shared.id, anna);
  const b = await (await browser.newContext()).newPage();
  await join(b, request, shared.id, bea);
  await join(b, request, own.id, bea);
  const [idA, idB] = [(await profileOf(a)).id, (await profileOf(b)).id];
  expect(idA).not.toBe(idB);

  const page = await (await browser.newContext({ viewport: { width: 1280, height: 800 } })).newPage();
  expect(await signIn(page, adminToken, 'profiles')).toBe(201);
  await page.locator('#profile-' + idB).getByRole('button', { name: `Merge ${bea} into another profile` }).click();
  const sheet = page.locator('#merge-sheet');
  await expect(sheet).toBeVisible();
  await expect(sheet.locator('#merge-from')).toHaveValue(String(idB));
  await expect(sheet.locator('#merge-confirm')).toBeDisabled();
  // One profile into itself is refused before anything is asked.
  await sheet.locator('#merge-keep').selectOption(String(idB));
  await expect(sheet.locator('#merge-same')).toHaveText('A profile cannot be merged into itself.');
  await expect(sheet.locator('#merge-confirm')).toBeDisabled();

  await sheet.locator('#merge-keep').selectOption(String(idA));
  await expect(sheet.locator('#merge-keep-who')).toContainText(anna);
  await expect(sheet.locator('#merge-from-who')).toContainText(bea);
  await expect(sheet.locator('#merge-email')).toHaveText('none');
  await expect(sheet.locator('#merge-counts')).toContainText('Seats1 + 2');
  const rooms = sheet.locator('#merge-rooms li');
  await expect(rooms).toHaveCount(2);
  await expect(rooms.filter({ hasText: shared.name })).toContainText('both');
  await expect(rooms.filter({ hasText: own.name })).not.toContainText('both');

  await page.setViewportSize({ width: 390, height: 844 });
  const box = await sheet.boundingBox();
  expect(box).toEqual({ x: 0, y: 0, width: 390, height: 844 });
  expect(await noOverflow(page)).toEqual([]);
  await page.setViewportSize({ width: 1280, height: 800 });

  await sheet.locator('#merge-swap').click();
  await expect(sheet.locator('#merge-keep')).toHaveValue(String(idB));
  await expect(sheet.locator('#merge-keep-who')).toContainText(bea);
  await expect(sheet.locator('#merge-from-who')).toContainText(anna);
  await sheet.locator('#merge-swap').click();
  await expect(sheet.locator('#merge-keep')).toHaveValue(String(idA));
  await expect(sheet.locator('#merge-keep-who')).toContainText(anna);

  await sheet.locator('#merge-confirm').click();
  await expect(sheet).toBeHidden();
  await expect(page.locator('.toasts .toast')).toContainText(`Merged into ${anna}. Moved 2 seats`);
  await expect(page.locator('#profile-' + idB)).toHaveCount(0);
  await expect(page.locator('#profile-' + idA)).toContainText(own.name);

  // A had no chosen name: it keeps its join name, not one of B's seats' (011-AC12).
  await b.goto('/profile');
  await expect(b.locator('#profile-name')).toHaveText(anna);
  // The list's order is the server's: compare as a set.
  await expect.poll(async () => (await b.locator('#rooms li .room-line a').allTextContents()).sort()).toEqual([shared.name, own.name].sort());
  expect((await profileOf(b)).id).toBe(idA);
});

// 011-AC15 011-AC16 011-AC17 011-AC18: a profile whose only seat lost its invite is empty. Each
// purge pass is limited to it: parallel specs keep their own empty profiles. With the
// Instance switch off a purge pass deletes nothing; on (the default) it deletes it, audited as
// system. Its browser is then signed out and its next join makes a new profile.
test('purge of an empty profile and the Instance switch', async ({ browser, request }) => {
  const name = `Ghost ${Date.now()}`;
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Purge ' + name } })).json();
  const ghost = await (await browser.newContext()).newPage();
  const inv = await join(ghost, request, room.id, name);
  const before = await profileOf(ghost);
  const uuid = (await (await request.get('/api/admin/profiles', { headers: admin })).json()).find((p) => p.id === before.id).uuid;
  expect((await request.delete(`/api/rooms/${room.id}/invites/${inv.id}`, { headers: admin })).status()).toBe(200);
  const purge = async () => {
    const res = await request.post('/api/admin/test/purge', { headers: admin, data: { after_s: 0, profile_ids: [before.id] } });
    expect(res.status()).toBe(200);
    return (await res.json()).deleted || [];
  };

  const page = await (await browser.newContext()).newPage();
  expect(await signIn(page, adminToken, 'instance')).toBe(201);
  const box = page.getByRole('checkbox', { name: 'Delete empty profiles after 24 h of inactivity' });
  await expect(box).toBeChecked();
  try {
    await box.uncheck();
    await expect(page.locator('.toasts .toast')).toHaveText('Saved');
    await expect(box).toBeEnabled();
    expect((await (await request.get('/api/admin/settings', { headers: admin })).json()).purge_empty_profiles).toBe(false);
    const log = await (await request.get('/api/admin/audit?action=instance.settings', { headers: admin })).json();
    expect(log.entries[0].details.purge_empty_profiles).toBe(false);
    expect(await purge()).toEqual([]);
    expect((await profileOf(ghost))?.id).toBe(before.id);
  } finally {
    await box.check();
    await expect(box).toBeEnabled();
  }
  expect((await (await request.get('/api/admin/settings', { headers: admin })).json()).purge_empty_profiles).toBe(true);
  expect(await purge()).toContain(uuid);
  const audit = await (await request.get('/api/admin/audit?action=profile.purge', { headers: admin })).json();
  expect(audit.entries.find((e) => e.details.uuids.includes(uuid))?.actor).toBe('system');

  await ghost.goto('/');
  await expect(ghost.locator('#your-profile')).toBeHidden();
  // No SMTP on this server: no email recovery to offer either (011-AC4).
  await expect(ghost.locator('#recover')).toBeHidden();
  const other = await (await request.post('/api/rooms', { headers: admin, data: { name: 'After ' + name } })).json();
  await join(ghost, request, other.id, name);
  const after = await profileOf(ghost);
  expect(after.id).not.toBe(before.id);
});
