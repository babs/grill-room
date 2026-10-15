import { test, expect } from './fixtures.js';
import { roomWith } from './helpers.js';

const notice = (page) => page.getByRole('alert').filter({ hasText: "Couldn't load, reload the page" });

// 009-AC35: a module that fails to load says so instead of leaving its part of the page dead: on the room
// page Room settings (a dependency of room-admin.js, to cover the whole graph), and /admin.
test('a module that fails to load says so', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(notice(alice)).toHaveCount(0);
  await alice.route('**/static/admin/room-panel.js', (r) => r.abort('internetdisconnected'));
  await alice.reload();
  await expect(notice(alice)).toBeVisible();
  await expect(notice(alice).getByRole('button', { name: 'Reload' })).toBeVisible();

  const admin = await (await browser.newContext()).newPage();
  await admin.route('**/static/admin/admin.js', (r) => r.abort('internetdisconnected'));
  await admin.goto('/admin');
  await expect(notice(admin)).toBeVisible();
  // Reload, once the connection is back, gives the page its module.
  await admin.unrouteAll();
  await notice(admin).getByRole('button', { name: 'Reload' }).click();
  await expect(admin.getByLabel('Admin token')).toBeVisible();
  await expect(notice(admin)).toHaveCount(0);
});
