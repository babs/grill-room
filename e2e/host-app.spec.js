import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, startDaemon, stopDaemon, menuItem, connectedRow } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// approve follows the daemon page's "approve in the room" link and approves there; it resolves
// with the room page that approved.
async function approve(page, ctx) {
  const [approval] = await Promise.all([ctx.waitForEvent('page'), page.getByRole('link', { name: 'approve in the room' }).click()]);
  await approval.getByRole('button', { name: 'Approve' }).click();
  return approval;
}

// 008-AC15 008-AC16 008-AC17: the room page explains both ways; a daemon started from a link
// inside a room pairs, and the room's pairing notice goes once it is connected; a second room
// added from the daemon's page pairs the same way while the first stays connected; a link that is
// no room link says why.
test('add a room from the AI host app page', async ({ browser, request, baseURL }) => {
  const { room: first, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const second = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Second room' } })).json();
  const inv = await (await request.post(`/api/rooms/${second.id}/invites`, { headers: admin })).json();
  const joined = await alice.context().newPage();
  await joined.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await joined.getByLabel('Your name').fill('Alice');
  await joined.getByRole('button', { name: 'Join the room' }).click();
  await expect(joined.locator('#room-name')).toHaveText('Second room');
  await joined.close();

  await menuItem(alice, 'Run the AI on my computer');
  const sheet = alice.locator('#host-agent');
  await expect(sheet).toContainText('Add a room');
  await expect(sheet.getByLabel("This room's link")).toHaveValue(`${baseURL}/r/${first.id}`);
  await alice.keyboard.press('Escape');

  // A link to the docs window, with a query, starts the daemon on the room itself.
  const daemon = startDaemon(`${baseURL}/r/${first.id}/docs?from=link#top`);
  try {
    const page = await alice.context().newPage();
    await page.goto(await daemon.link);
    await expect(page.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
    const approval = await approve(page, alice.context());
    await expect(approval.locator('#pairing-text')).toContainText('Daemon paired');
    await expect(connectedRow(page, first.id)).toBeAttached({ timeout: 30_000 });
    await expect(approval.locator('#pairing')).toBeHidden();

    // Not a room link: the page says why and nothing changes.
    await page.locator('#room-url').fill(`${baseURL}/admin`);
    await page.getByRole('button', { name: 'Add a room' }).click();
    await expect(page.locator('#room-error')).toContainText('is not a room link');
    await expect(connectedRow(page, first.id)).toBeAttached();

    await page.locator('#room-url').fill(`${baseURL}/r/${second.id}/docs/1?x=1#m2`);
    await page.getByRole('button', { name: 'Add a room' }).click();
    await expect(page.locator('#room-url')).toHaveValue('');
    await expect(page.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
    await expect(page.locator('#code')).toHaveText(/^\d{3}-\d{3}$/);
    const approval2 = await approve(page, alice.context());
    await expect(approval2).toHaveURL(new RegExp(`/r/${second.id}$`));
    await expect(connectedRow(page, second.id)).toBeAttached({ timeout: 30_000 });
    await expect(approval2.locator('#pairing')).toBeHidden();
    await expect(page.locator('.room-row')).toHaveCount(2);
    for (const name of ['Docs room', 'Second room']) await expect(page.locator('.room-row', { hasText: name })).toHaveCount(1);

    await stopDaemon(page);
    expect(await daemon.exited).toBe(0);
  } finally {
    daemon.cleanup();
  }
});
