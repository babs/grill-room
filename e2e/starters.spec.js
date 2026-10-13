import { test, expect } from '@playwright/test';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, confirmSheet } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// starterAction picks name in the ⋯ menu of the starter row titled title.
async function starterAction(page, list, title, name) {
  const row = page.locator(`#starters-${list} li`, { hasText: title });
  await row.getByRole('button', { name: 'More actions for ' + title }).click();
  await page.getByRole('menuitem', { name }).click();
}

async function audited(request, action, target) {
  const log = await (await request.get(`/api/admin/audit?action=${action}&target=${encodeURIComponent(target)}`, { headers: admin })).json();
  return log.entries;
}

// 006-AC1 006-AC2 006-AC3 006-AC5: a profile duplicates a built-in into Yours, which another
// profile does not see, shares it with everyone, and the super-admin removes it from /admin
// Libraries; publishing and removing are in the audit log.
test('starter library: duplicate, share, delete, /admin', async ({ browser, request }) => {
  // Unique names: repeated runs share the server and its library.
  const me = 'Alice' + Date.now().toString(36);
  const { pages: [alice, bob] } = await roomWith(browser, request, [me, 'Bob']);
  await alice.goto('/profile');
  await expect(alice.locator('#starters-builtin li')).toHaveCount(3);
  await expect(alice.locator('#starters-builtin li').first()).toContainText('Spec grill');
  await expect(alice.locator('#starters-builtin li').first()).toContainText('by grill-room');

  await starterAction(alice, 'builtin', 'Retro', 'Duplicate to yours');
  const mine = alice.locator('#starters-personal li', { hasText: 'Retro' });
  await expect(mine).toContainText('v1 · by ' + me);
  // Its row menu's keyboard: Escape closes it and gives the focus back to ⋯.
  const more = mine.getByRole('button', { name: 'More actions for Retro' });
  await more.click();
  await expect(alice.getByRole('menuitem', { name: 'Duplicate to yours' })).toBeFocused();
  await alice.keyboard.press('ArrowDown');
  await expect(alice.getByRole('menuitem', { name: 'Share with everyone' })).toBeFocused();
  await alice.keyboard.press('Escape');
  await expect(more).toBeFocused();

  await bob.goto('/profile');
  await expect(bob.locator('#starters-builtin li')).toHaveCount(3);
  await expect(bob.locator('#starters-personal')).toHaveCount(0);
  await expect(bob.locator('#starters-instance li', { hasText: me })).toHaveCount(0);

  await starterAction(alice, 'personal', 'Retro', 'Share with everyone');
  await confirmSheet(alice, 'Share');
  await expect(alice.locator('#starters-instance li', { hasText: me })).toContainText('Retro');
  await bob.reload();
  const shared = bob.locator('#starters-instance li', { hasText: me });
  await expect(shared).toContainText('Retro');
  // Not his to share or delete: his menu only duplicates.
  await shared.getByRole('button', { name: 'More actions for Retro' }).click();
  await expect(bob.getByRole('menuitem')).toHaveText(['Duplicate to yours']);
  await bob.keyboard.press('Escape');

  const page = await (await browser.newContext()).newPage();
  expect(await signIn(page, adminToken, 'libraries')).toBe(201);
  const row = page.locator('#starters tbody tr', { hasText: 'Retro' }).filter({ hasText: me });
  await expect(row).toHaveCount(1);
  await expect(row.locator('td').nth(2)).toHaveText('1');
  await row.getByRole('button', { name: 'Remove the starter Retro' }).click();
  await confirmSheet(page, 'Remove');
  await expect(row).toHaveCount(0);
  const removed = await audited(request, 'starter.delete', 'Retro');
  expect(removed[0].actor).toBe('super-admin');
  expect(removed[0].details.version).toBe(1);
  const published = await audited(request, 'starter.publish', 'Retro');
  expect(published[0].actor).toMatch(/^profile:/);

  // A personal one its owner deletes from the profile page.
  await starterAction(alice, 'builtin', 'Doc review', 'Duplicate to yours');
  await starterAction(alice, 'personal', 'Doc review', 'Delete');
  await confirmSheet(alice, 'Delete');
  await expect(alice.locator('#starters-personal')).toHaveCount(0);
  await expect(alice.locator('#starters-instance li', { hasText: me })).toHaveCount(0);
});
