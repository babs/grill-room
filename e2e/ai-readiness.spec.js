import { devices } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, fakeDaemon, agentState, patchPersona, appointAdmin, noOverflow, smallTargets } from './helpers.js';

const phone = { width: 390, height: 844 };
const desktop = { width: 1440, height: 900 };
const handOff = (link) => `Can you run the AI for our grill-room room? Open ${link}, then click the room's name › Run the AI on my computer.`;

// openAI opens the sheet from the room menu.
async function openAI(page) {
  await page.locator('#room-open').click();
  await page.locator('#room-menu').getByRole('link', { name: 'AI in this room' }).click();
  const sheet = page.locator('#ai');
  await expect(sheet).toBeVisible();
  return sheet;
}

const row = (page, id) => page.locator(`#ai-rows .ai-row[data-row="${id}"]`);

// status posts the fake AI host app's heartbeat, as the app does when its readiness changes.
async function status(baseURL, roomID, host, body) {
  const r = await fetch(`${baseURL}/api/rooms/${roomID}/daemon/status`, { method: 'POST',
    headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  expect(r.status).toBe(204);
}

async function copied(page, text) {
  await expect(page.locator('.toast', { hasText: 'Message copied.' }).last()).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(text);
  await page.evaluate(() => navigator.clipboard.writeText(''));
}

// shots captures the open sheet at both widths, under the test's output directory; on a phone
// nothing sticks out and every target is 44 px.
async function shots(page, who, testInfo) {
  for (const vp of [phone, desktop]) {
    await page.setViewportSize(vp);
    if (vp === phone) {
      expect(await noOverflow(page), who).toEqual([]);
      expect(await smallTargets(page), who).toEqual([]);
    }
    await page.screenshot({ path: testInfo.outputPath(`ai-${who}-${vp.width}.png`) });
  }
}

// 039-AC1 039-AC2 039-AC4 039-AC5 039-AC8: no one runs the AI; a member opens the sheet from the
// room menu, reads the rows in order and gets both ways to fix it; #ai opens it on load and its
// close clears the hash.
test('AI in this room: no host', async ({ browser, request, baseURL }) => {
  const { room, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await bob.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  const sheet = await openAI(bob);
  await expect(bob).toHaveURL(/#ai$/);
  await expect(sheet.getByRole('heading', { name: 'AI in this room' })).toBeVisible();
  await expect(sheet.locator('#ai-rows .ai-row')).toHaveCount(3);
  expect(await sheet.locator('#ai-rows .ai-row').evaluateAll((els) => els.map((e) => e.dataset.row))).toEqual(['host', 'ask', 'waiting']);
  const host = row(bob, 'host');
  await expect(host).toHaveAttribute('data-state', 'blocked');
  await expect(host.locator('.name')).toHaveText('No one runs the AI for this room.');
  await expect(host.locator('.ai-who')).toHaveText('Who: anyone in the room');
  await host.getByRole('button', { name: 'Ask someone to run it' }).click();
  await copied(bob, handOff(`${baseURL}/r/${room.id}`));
  await host.getByRole('button', { name: 'Run the AI on my computer' }).click();
  await expect(bob.locator('#host-agent')).toBeVisible();
  await bob.keyboard.press('Escape');
  await expect(row(bob, 'waiting').locator('.name')).toHaveText('Nothing waits for the AI host.');
  // 039-AC4: with no one running the AI, no host to name.
  await expect(row(bob, 'ask').locator('.name')).toHaveText('Whoever runs the AI decides who can ask the personas.');
  await expect(row(bob, 'ask').locator('.ai-who')).toHaveText('Who: anyone in the room');
  await expect(row(bob, 'ask').locator('a, button')).toHaveCount(0);
  await expect(sheet.locator('#ai-choices')).toHaveText('No AI host yet: these choices appear once someone runs the AI.');
  await sheet.getByRole('button', { name: 'Close' }).click();
  await expect(sheet).toBeHidden();
  await expect(bob).toHaveURL(`${baseURL}/r/${room.id}`);

  // A same-document goto only changes the hash: the reload is the load with #ai.
  await bob.goto(`/r/${room.id}#ai`);
  await bob.reload();
  await expect(bob.locator('#ai')).toBeVisible();
  await expect(row(bob, 'host').locator('.name')).toHaveText('No one runs the AI for this room.');
});

// 039-AC1 039-AC2 039-AC3 039-AC4 039-AC7: the host's app connected but without a usable Claude
// account, then gone, then the room removed from it. The member reads the reason and who acts; the
// host gets "Open my AI host app".
test('AI in this room: host cannot host, then app not connected', async ({ browser, request, baseURL }) => {
  test.setTimeout(60_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await bob.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    await status(baseURL, room.id, host, { can_host: false, reason: 'No Claude credentials found.', reason_code: 'account' });
    await openAI(bob);
    const can = row(bob, 'can-host');
    await expect(can.locator('.name')).toHaveText('Alice\'s AI host app has no Claude account it can use.');
    await expect(can).toHaveAttribute('data-state', 'blocked');
    await expect(can.locator('.ai-who')).toHaveText('Who: Alice (AI host)');
    await expect(can.locator('a, button')).toHaveCount(0);
    // 039-AC1: the state icon is hidden from screen readers; its word is read before the line.
    await expect(can).toHaveText(/^Blocked: Alice's AI host app has no Claude account/);
    // 039-AC7: an app that cannot host vouches for no connector list.
    await expect(bob.locator('#ai-choices li').nth(2)).toHaveText('Connectors: shown once Alice\'s AI host app can host');
    await expect(row(bob, 'host').locator('.name')).toHaveText('Alice runs the AI on their computer.');
    await expect(row(bob, 'ask').locator('.name')).toHaveText('Alice decides who can ask the personas. You can still mention them in a message.');
    await row(bob, 'ask').getByRole('button', { name: 'Copy a message to Alice' }).click();
    await copied(bob, 'Can you tick "Can ask the AI" next to my name in Docs room?');

    await openAI(alice);
    const mine = row(alice, 'can-host');
    await expect(mine.locator('.name')).toHaveText('Your AI host app has no Claude account it can use.');
    await expect(mine.locator('.ai-who')).toHaveText('Who: you');
    await expect(mine.getByRole('link', { name: 'Open my AI host app' })).toHaveAttribute('href', await alice.locator('#host-app-open').getAttribute('href'));
    await expect(row(alice, 'ask').locator('.name')).toHaveText('You can ask the personas.');

    // An unknown code shows the reason as the app sent it.
    await status(baseURL, room.id, host, { can_host: false, reason: 'Disk full on this computer.', reason_code: 'disk' });
    await expect(row(bob, 'can-host').locator('.name')).toHaveText('Disk full on this computer.');

    host();
    await expect(row(bob, 'host').locator('.name')).toHaveText('Alice\'s AI host app is not connected.', { timeout: 30_000 });
    await expect(row(bob, 'can-host')).toHaveCount(0);
    await expect(row(bob, 'host').locator('.ai-who')).toHaveText('Who: Alice (AI host)');
    await row(bob, 'host').getByRole('button', { name: 'Copy a message to Alice' }).click();
    await copied(bob, 'Your AI host app is not connected to Docs room. Can you open it so the personas can answer?');
    await expect(row(alice, 'host').locator('.name')).toHaveText('Your AI host app is not connected.');
    await expect(row(alice, 'host').getByRole('link', { name: 'Open my AI host app' })).toBeVisible();
    // 039-AC7: the host still has a paired app; its connectors are unknown until it connects.
    await expect(bob.locator('#ai-choices li').nth(2)).toHaveText('Connectors: shown once Alice\'s AI host app connects');
    await expect(alice.locator('#ai-choices li').nth(2)).toHaveText('Connectors: shown once your AI host app connects');

    // 039-AC2: the host removes the room from their app; anyone may take over.
    const gone = await fetch(`${baseURL}/api/rooms/${room.id}/daemon`, { method: 'DELETE', headers: { Authorization: 'Bearer ' + host.token } });
    expect(gone.status).toBe(204);
    const removed = row(bob, 'host');
    await expect(removed.locator('.name')).toHaveText('Alice removed this room from their AI host app.');
    await expect(removed.locator('.ai-who')).toHaveText('Who: Alice (AI host), or anyone to take over');
    await expect(removed.getByRole('button')).toHaveText(['Copy a message to Alice', 'Run the AI on my computer']);
    await removed.getByRole('button', { name: 'Copy a message to Alice' }).click();
    await copied(bob, `Can you add Docs room back to your AI host app? Paste ${baseURL}/r/${room.id} under Rooms › Add a room.`);
    const mine2 = row(alice, 'host');
    await expect(mine2.locator('.name')).toHaveText('You removed this room from your AI host app.');
    await expect(mine2.locator('a, button')).toHaveText(['Open my AI host app', 'Run the AI on my computer']);
  } finally {
    host();
  }
});

// 039-AC5 039-AC7: a held change waits for the host. The member reads the count and copies it;
// the host's action closes the sheet on the approval strip. Both read the host's choices.
test('AI in this room: changes waiting and the host\'s choices', async ({ browser, request, baseURL }, testInfo) => {
  test.setTimeout(60_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await bob.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop', { approve_all: true, max_model: 'sonnet', connectors: [{ name: 'Linear' }] });
  try {
    const grill = (await agentState(bob, room.id)).personas[0].id;
    expect(await patchPersona(bob, room.id, grill, { name: 'Griller' })).toBe(202);

    await openAI(bob);
    const waiting = row(bob, 'waiting');
    await expect(waiting).toHaveAttribute('data-state', 'waiting');
    await expect(waiting.locator('.name')).toHaveText('Waiting for Alice:');
    await expect(waiting.locator('.meta').first()).toHaveText('Persona changes and new personas: 1');
    await expect(waiting.locator('.ai-who')).toHaveText('Who: Alice (AI host)');
    await waiting.getByRole('button', { name: 'Copy a message to Alice' }).click();
    await copied(bob, 'Can you look at what waits for your approval in Docs room? Persona changes and new personas: 1.');
    const choices = bob.locator('#ai-choices li');
    await expect(choices).toHaveText(['Models up to sonnet', 'New personas and changes: wait for Alice\'s approval', 'Connectors: Linear',
      'Websites: see Room settings › Websites and connectors', 'Only Alice changes these, in their AI host app.']);
    await expect(bob.locator('#ai-choices').getByRole('link', { name: 'Room settings › Websites and connectors' })).toHaveAttribute('href', '#manage/network');
    await shots(bob, 'member', testInfo);

    await openAI(alice);
    await expect(alice.locator('#ai-choices li').nth(1)).toHaveText('New personas and changes: wait for your approval');
    await expect(row(alice, 'waiting').locator('.name')).toHaveText('Waiting for you:');
    await shots(alice, 'host', testInfo);
    await row(alice, 'waiting').getByRole('button', { name: 'Open the approvals' }).click();
    await expect(alice.locator('#ai')).toBeHidden();
    await expect(alice.locator('#approvals')).toBeFocused();
    await expect(alice.locator('#approvals')).toBeVisible();

    // No cap on either: one line says so. Bob's sheet is still open and follows.
    await status(baseURL, room.id, host, { can_host: true });
    await expect(bob.locator('#ai-choices li').first()).toHaveText('No cap on models');
    await expect(bob.locator('#ai-choices li').nth(1)).toHaveText('New personas and changes: apply at once');
    await expect(bob.locator('#ai-choices li').nth(2)).toHaveText('No connectors');
  } finally {
    host();
  }
});

// 039-AC6: a plugin that needs host setup or waits for the host's approval names the host; one the
// host refused names a room admin, who gets "Open Plugins". The plugin views are served as the server words them.
test('AI in this room: plugins that do not load', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await appointAdmin(bob, request, room.id);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  const view = (id, name, extra) => ({ id, name, url: 'https://example.com/' + name + '.git', ref: 'main', ref_name: 'main', entry: '', personas: [],
    versions: [{ id, n: 1, state: 'ok', short_commit: 'abcdef1', commit: 'abcdef1234' }], ...extra });
  const plugins = { plugins: [view(1, 'opt', { needs_setup: true, running: 1, why: 'needs host setup on the AI host\'s computer' }),
    view(2, 'lint', { versions: [{ id: 2, n: 1, state: 'refused', short_commit: 'abcdef2', commit: 'abcdef2234' }], why: 'refused by the AI host' }),
    view(3, 'fine', { running: 1 }),
    view(4, 'pend', { versions: [{ id: 4, n: 1, state: 'pending', short_commit: 'abcdef4', commit: 'abcdef4234', added_by: 'Bob' }], why: 'waiting for the AI host\'s approval' })] };
  try {
    for (const p of [alice, bob]) {
      await p.route((u) => u.pathname === `/api/rooms/${room.id}/plugins`, (r) => r.fulfill({ json: plugins }));
      await p.goto(`/r/${room.id}#ai`);
      await p.reload();
    }
    const rows = bob.locator('#ai-rows .ai-row[data-row^="plugin-"]');
    await expect(rows).toHaveCount(3);
    await expect(rows.nth(0).locator('.name')).toHaveText('opt: needs host setup on the AI host\'s computer.');
    await expect(rows.nth(0).locator('.ai-who')).toHaveText('Who: Alice (AI host)');
    await expect(rows.nth(0).getByRole('button')).toHaveText(['Copy a message to Alice']);
    await expect(alice.locator('#ai-rows .ai-row[data-row^="plugin-"]').nth(0).locator('a, button')).toHaveCount(0);
    await expect(rows.nth(1).locator('.name')).toHaveText('lint: refused by the AI host.');
    await expect(rows.nth(1).locator('.ai-who')).toHaveText('Who: a room admin');
    await expect(alice.locator('#ai-rows .ai-row[data-row^="plugin-"]').nth(0).locator('.ai-who')).toHaveText('Who: you');
    // 039-AC6: waiting for the host's approval is the host's to act on, even for a room admin.
    await expect(rows.nth(2).locator('.name')).toHaveText('pend: waiting for the AI host\'s approval.');
    await expect(rows.nth(2)).toHaveAttribute('data-state', 'waiting');
    await expect(rows.nth(2).locator('.ai-who')).toHaveText('Who: Alice (AI host)');
    await expect(rows.nth(2).getByRole('link', { name: 'Open Plugins' })).toHaveCount(0);
    await expect(rows.nth(2).getByRole('button', { name: 'Copy a message to Alice' })).toBeVisible();
    await expect(alice.locator('#ai-rows .ai-row[data-row^="plugin-"]').nth(1).getByRole('link')).toHaveCount(0);
    await rows.nth(1).getByRole('link', { name: 'Open Plugins' }).click();
    await expect(bob.locator('#ai')).toBeHidden();
    await expect(bob.locator('#manage')).toBeVisible();
    await expect(bob).toHaveURL(/#manage\/plugins$/);
  } finally {
    host();
  }
});

// 039-AC11 039-AC3: the open sheet follows the agent event: the host connects, its app reports the
// sandbox getting ready, then ready, with no reload.
test('AI in this room: rows follow the AI host live', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await openAI(bob);
  await expect(row(bob, 'host').locator('.name')).toHaveText('No one runs the AI for this room.');
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    await expect(row(bob, 'host').locator('.name')).toHaveText('Alice runs the AI on their computer.');
    await expect(row(bob, 'host')).toHaveAttribute('data-state', 'ready');
    await expect(bob.locator('#ai-choices li').first()).toHaveText('No cap on models');
    await status(baseURL, room.id, host, { can_host: false, reason: 'Pulling the sandbox image.', reason_code: 'image_pending' });
    await expect(row(bob, 'can-host').locator('.name')).toHaveText('The sandbox is still getting ready on Alice\'s computer.');
    await expect(row(bob, 'can-host')).toHaveAttribute('data-state', 'waiting');
    await status(baseURL, room.id, host, { can_host: false, reason: 'The sandbox image failed.', reason_code: 'image_failed' });
    await expect(row(bob, 'can-host').locator('.name')).toHaveText('The sandbox could not be prepared on Alice\'s computer.');
    await status(baseURL, room.id, host, { can_host: false, reason: 'Cannot reach Docker.', reason_code: 'docker' });
    await expect(row(bob, 'can-host').locator('.name')).toHaveText('Docker is not running on Alice\'s computer.');
    await status(baseURL, room.id, host, { can_host: true });
    await expect(row(bob, 'can-host')).toHaveCount(0);
    await expect(bob).toHaveURL(/#ai$/);
  } finally {
    host();
  }
});

// 039-AC2 039-AC3: on a phone the AI host app's page is not on this device: the host's row says
// where to open it instead of linking to it, as the room menu hides "Open my AI host app".
test('AI in this room: no app link on a phone', async ({ browser, request, baseURL }) => {
  const admin = { Authorization: `Bearer ${adminToken}` };
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Phone room' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await browser.newContext({ viewport: phone, userAgent: devices['Pixel 7'].userAgent, hasTouch: true });
  const page = await ctx.newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Alice');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Alice');
  const host = await fakeDaemon(page, baseURL, room.id, 'Alice laptop');
  try {
    await status(baseURL, room.id, host, { can_host: false, reason: 'No Claude credentials found.', reason_code: 'account' });
    await openAI(page);
    const can = row(page, 'can-host');
    await expect(can.locator('.name')).toHaveText('Your AI host app has no Claude account it can use.');
    await expect(can.locator('.meta').first()).toHaveText('Open your AI host app on the computer that runs it.');
    await expect(can.locator('a, button')).toHaveCount(0);
    host();
    await expect(row(page, 'host').locator('.name')).toHaveText('Your AI host app is not connected.', { timeout: 30_000 });
    await expect(row(page, 'host').locator('.meta').first()).toHaveText('Open your AI host app on the computer that runs it.');
    await expect(row(page, 'host').locator('a, button')).toHaveCount(0);
  } finally {
    host();
  }
});
