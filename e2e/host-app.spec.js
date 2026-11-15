import { devices } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, startDaemon, openDaemonPage, stopDaemon, menuItem, connectedRow, fakeDaemon } from './helpers.js';

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
    await openDaemonPage(page, await daemon.link);
    await expect(page.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
    const approval = await approve(page, alice.context());
    await expect(approval.locator('#pairing-text')).toContainText('AI host app paired');
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
    // The row carries the approval too: after a restart the status line above is gone.
    const code2 = await page.locator('#code').textContent();
    await expect(page.locator(`.room-row[data-room$="/r/${second.id}"] .approve-link`)).toHaveText(`Approve, code ${code2}`);
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

const handOff = (url) => `Can you run the AI for our grill-room room? Open ${url}, then click the room's name › Run the AI on my computer.`;

// 038-AC2 038-AC3: on a phone or tablet, by its OS, the host sheet says what running the AI needs,
// offers no download and no app page, and gives a message to send instead; the room menu hides
// "Open my AI host app" even with this participant's app connected. A touch laptop keeps the download.
test('phone host sheet: no download, a message to send', async ({ browser, request, baseURL }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Phone host' } })).json();
  const mac = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15';
  const linux = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36';
  const cases = [['Android', devices['Pixel 7'].userAgent, true], ['iPhone', devices['iPhone 13'].userAgent, true], ['iPad', mac, true, 5],
    ['touch laptop', linux, false]];
  for (const [name, userAgent, mobile, touchPoints] of cases) {
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, userAgent, hasTouch: true, permissions: ['clipboard-read', 'clipboard-write'] });
    // iPadOS reports a Mac: only its touch points tell it apart.
    if (touchPoints) await ctx.addInitScript((n) => {
      Object.defineProperty(Navigator.prototype, 'platform', { get: () => 'MacIntel' });
      Object.defineProperty(Navigator.prototype, 'maxTouchPoints', { get: () => n });
    }, touchPoints);
    const page = await ctx.newPage();
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    const stop = await fakeDaemon(page, baseURL, room.id, name + ' app');
    try {
      await page.locator('#room-open').click();
      await expect(page.locator('#host-app-open'), name).toBeVisible({ visible: !mobile });
      await page.keyboard.press('Escape');
      // Diagnostics keeps no AI host app link either.
      const diag = page.locator('section', { has: page.locator('#diag-host-app') });
      if (mobile) await expect(diag, name).toHaveAttribute('hidden', '');
      else await expect(diag, name).not.toHaveAttribute('hidden', '');
      await menuItem(page, 'Run the AI on my computer');
      const sheet = page.locator('#host-agent');
      await expect(sheet.getByText('Running the AI needs a computer with Docker.'), name).toBeVisible({ visible: mobile });
      await expect(sheet.locator('#host-download'), name).toBeVisible({ visible: !mobile });
      await expect(sheet.getByRole('link', { name: 'the AI host app', exact: true }), name).toBeVisible({ visible: !mobile });
      if (mobile) {
        await sheet.getByRole('button', { name: 'Ask someone to run it' }).click();
        await expect(page.locator('.toast', { hasText: 'Message copied.' })).toBeVisible();
        expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(handOff(`${baseURL}/r/${room.id}`));
      }
    } finally {
      stop();
      await ctx.close();
    }
  }
});

// 038-AC3: with no one running the AI, the ▶ of an offline pill and the pill's card offer the same
// message; a room with an AI host whose app is away does not.
test('offline pill: ask someone to run it', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  const copied = handOff(`${baseURL}/r/${room.id}`);
  await alice.locator('.persona-pill .persona-run').first().click({ force: true });
  await expect(alice.locator('#why-text')).toContainText('No one runs the AI for this room yet');
  await alice.locator('#why').getByRole('button', { name: 'Ask someone to run it' }).click();
  await expect(alice.locator('.toast', { hasText: 'Message copied.' })).toBeVisible();
  expect(await alice.evaluate(() => navigator.clipboard.readText())).toBe(copied);
  await alice.locator('#why').getByRole('button', { name: 'OK' }).click();

  await alice.evaluate(() => navigator.clipboard.writeText(''));
  await alice.locator('button.persona').first().click();
  await alice.locator('#persona-card').getByRole('button', { name: 'Ask someone to run it' }).click();
  await expect(alice.locator('.toast', { hasText: 'Message copied.' }).last()).toBeVisible();
  expect(await alice.evaluate(() => navigator.clipboard.readText())).toBe(copied);
  await alice.keyboard.press('Escape');

  // No clipboard: the message shows, to copy by hand.
  await alice.evaluate(() => Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true }));
  await alice.locator('.persona-pill .persona-run').first().click({ force: true });
  await alice.locator('#why').getByRole('button', { name: 'Ask someone to run it' }).click();
  await expect(alice.locator('#why-title')).toHaveText('Ask someone to run it');
  await expect(alice.locator('#why-text')).toHaveText(copied);
  await expect(alice.locator('#why-ask')).toBeHidden();
  await expect(alice.locator('#why').getByRole('button', { name: 'OK' })).toBeFocused();
  await alice.locator('#why').getByRole('button', { name: 'OK' }).click();

  // An AI host whose app went away: the room names them, and the hand-off goes.
  const stop = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  await expect(alice.locator('.persona-pill .persona-run').first()).toHaveAttribute('aria-disabled', 'false');
  stop();
  await expect(alice.locator('.persona-pill .persona-run').first()).toHaveAttribute('aria-disabled', 'true', { timeout: 30_000 });
  await alice.locator('.persona-pill .persona-run').first().click({ force: true });
  await expect(alice.locator('#why-text')).toContainText("AI host app is not connected");
  await expect(alice.locator('#why-ask')).toBeHidden();
  await alice.locator('#why').getByRole('button', { name: 'OK' }).click();
  await alice.locator('button.persona').first().click();
  await expect(alice.locator('#persona-card-ask')).toBeVisible();
  await expect(alice.locator('#persona-card-handoff')).toHaveCount(0);
});
