import { test, expect } from './fixtures.js';
import { roomWith, fakeDaemon, appointAdmin, personMenu, confirmSheet, signIn, rowAction } from './helpers.js';

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
    await rowAction(list.locator('> li', { hasText: 'Bob' }), 'Make Bob the AI host');
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

// 016-AC9: a room admin removes the AI host; the first other eligible app takes the role, with its
// line, and the removed host's page ends. 016-AC14, Q2: the super-admin removes from /admin's room
// view, blocked, and lifts the block from its Invites tab.
test('removing the AI host hands the role to the standby app', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
  await appointAdmin(alice, request, room.id);
  const stops = [];
  try {
    stops.push(await fakeDaemon(bob, baseURL, room.id, 'bob-laptop'));
    await expect(hostBadge(alice, 'Bob')).toHaveAttribute('aria-label', 'host');
    stops.push(await fakeDaemon(carol, baseURL, room.id, 'carol-laptop'));
    await (await personMenu(alice, 'Bob')).getByRole('button', { name: 'Remove from room…' }).click();
    await confirmSheet(alice, 'Remove');
    await expect(bob.locator('#banner')).toHaveText('Your access to this room has ended.');
    await expect(hostBadge(alice, 'Carol')).toHaveAttribute('aria-label', 'host');
    await expect(alice.locator('#messages')).toContainText('Carol holds the agent(s).');

    const admin = await (await browser.newContext()).newPage();
    expect(await signIn(admin, undefined, `rooms/${room.id}/admins`)).toBe(201);
    const list = admin.locator('#room-panel #room-admins');
    await rowAction(list.locator('> li', { hasText: 'Carol' }), 'Remove Carol from the room');
    await admin.locator('dialog.confirm').getByLabel('Block from rejoining').check();
    await confirmSheet(admin, 'Remove');
    await expect(list.locator('> li', { hasText: 'Carol' })).toHaveCount(0);
    await expect(alice.locator('#messages')).toContainText('The admin removed Carol from the room and blocked them from rejoining');
    await admin.goto(`/admin#rooms/${room.id}/invites`);
    const blocked = admin.locator('#room-panel #blocked');
    await expect(blocked).toContainText('Carol');
    await blocked.getByRole('button', { name: 'Unblock Carol' }).click();
    await expect(admin.locator('#room-panel #blocked')).toHaveCount(0);
    await expect(alice.locator('#messages')).toContainText('The admin unblocked Carol');
  } finally {
    for (const stop of stops) stop();
  }
});
