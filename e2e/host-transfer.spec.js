import { test, expect } from './fixtures.js';
import { roomWith, fakeDaemon, appointAdmin, personMenu, confirmSheet, signIn } from './helpers.js';

const hostBadge = (page, name) => page.locator('#participants li', { hasText: name }).locator('.host-badge');

// 009-AC33 009-AC34: a room admin who does not host makes another participant the AI host from
// People › ⋯; the chat names who did it, and only the previous host gets the standby notice. The
// super-admin does the same from /admin's room view.
test('a room admin and the super-admin move the AI host', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
  await appointAdmin(alice, request, room.id);
  const stops = [];
  try {
    stops.push(await fakeDaemon(bob, baseURL, room.id, 'bob-laptop'));
    await expect(hostBadge(alice, 'Bob')).toHaveAttribute('aria-label', 'host');
    stops.push(await fakeDaemon(carol, baseURL, room.id, 'carol-laptop'));

    await (await personMenu(alice, 'Carol')).getByRole('button', { name: 'Make AI host' }).click();
    await confirmSheet(alice, 'Make AI host');
    for (const p of [alice, bob, carol]) {
      await expect(hostBadge(p, 'Carol')).toHaveAttribute('aria-label', 'host');
      await expect(p.locator('#messages li.msg.system').last()).toContainText('Alice made Carol the AI host');
    }
    await expect(bob.locator('#notice-text')).toHaveText('Alice made Carol the AI host. Your AI host app stays connected as a standby.');
    await expect(alice.locator('#notice')).toBeHidden();
    await expect(carol.locator('#notice')).toBeHidden();

    const admin = await (await browser.newContext()).newPage();
    expect(await signIn(admin, undefined, `rooms/${room.id}/admins`)).toBe(201);
    const list = admin.locator('#room-panel #room-admins');
    await expect(list.locator('li', { hasText: 'Carol' })).toContainText('AI host');
    await list.getByRole('button', { name: 'Make Bob the AI host' }).click();
    await confirmSheet(admin, 'Make AI host');
    await expect(list.locator('li', { hasText: 'Bob' })).toContainText('AI host');
    await expect(hostBadge(alice, 'Bob')).toHaveAttribute('aria-label', 'host');
    await expect(carol.locator('#notice-text')).toHaveText('The admin made Bob the AI host. Your AI host app stays connected as a standby.');

    // The room admin takes the role for her own app.
    stops.push(await fakeDaemon(alice, baseURL, room.id, 'alice-laptop'));
    await (await personMenu(alice, 'Alice')).getByRole('button', { name: 'Make AI host' }).click();
    await confirmSheet(alice, 'Make AI host');
    await expect(hostBadge(bob, 'Alice')).toHaveAttribute('aria-label', 'host');
    await expect(bob.locator('#messages li.msg.system').last()).toContainText('Alice took the AI host role');
    await expect(bob.locator('#notice-text')).toHaveText('Alice took the AI host role. Your AI host app stays connected as a standby.');
  } finally {
    for (const stop of stops) stop();
  }
});
