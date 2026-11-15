import { devices } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, fakeDaemon, agentState, patchPersona, appointAdmin, noOverflow, smallTargets, line, live, personaSheet, playEvents, openStrip } from './helpers.js';

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
  // A click that leaves the focus where it was (as WebKit does): only the sheet's listener puts it on Run.
  await host.getByRole('button', { name: 'Run the AI on my computer' }).dispatchEvent('click');
  await expect(bob.locator('#host-agent')).toBeVisible();
  await bob.keyboard.press('Escape');
  await expect(row(bob, 'host').getByRole('button', { name: 'Run the AI on my computer' })).toBeFocused();
  await row(bob, 'host').getByRole('button', { name: 'Run the AI on my computer' }).click();
  await expect(bob.locator('#host-agent')).toBeVisible();
  // An agent event redraws the rows under the open sheet: Escape still lands on the same action.
  await bob.evaluate(() => renderAI());
  await bob.keyboard.press('Escape');
  await expect(row(bob, 'host').getByRole('button', { name: 'Run the AI on my computer' })).toBeFocused();
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
      'Websites: see AI in this room › Websites and connectors', 'Only Alice changes these, in their AI host app.']);
    // 040-AC9: the link names the tab's new place.
    await expect(bob.locator('#ai-choices').getByRole('link', { name: 'AI in this room › Websites and connectors' })).toHaveAttribute('href', '#ai/network');
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
    // 040-AC9: Open Plugins switches the sheet to its Plugins tab.
    await rows.nth(1).getByRole('link', { name: 'Open Plugins' }).click();
    await expect(bob.locator('#ai')).toBeVisible();
    await expect(bob.locator('#ai #tab-plugins')).toHaveAttribute('aria-current', 'page');
    await expect(bob.locator('#ai #panel-plugins')).toBeVisible();
    await expect(bob.locator('#ai #panel-status')).toBeHidden();
    await expect(bob).toHaveURL(/#ai\/plugins$/);
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

// missing clicks a "What's missing?" link, checks it lands on the AI sheet alone, runs during while
// the sheet is open, then closes it and checks the focus came back to expected.
async function missing(page, scope, expected, during) {
  await scope.getByRole('link', { name: 'What\'s missing?' }).click();
  await expect(page.locator('#ai')).toBeVisible();
  await expect(page).toHaveURL(/#ai$/);
  await expect(page.locator('dialog[open]')).toHaveCount(1);
  if (during) await during();
  await page.keyboard.press('Escape');
  await expect(page.locator('#ai')).toBeHidden();
  await expect(page).not.toHaveURL(/#ai$/);
  await expect(expected).toBeFocused();
}
const runButton = (page) => page.locator('.persona-pill .persona-run').first();
const pillButton = (page) => page.locator('button.persona').first();
// pressRun presses ▶ from the keyboard: a forced click lands on the pill when the bar re-lays out
// under it (a resize, a state word), and ▶ is aria-disabled while it cannot ask.
const pressRun = (page) => runButton(page).press('Enter');

// 039-AC9: wherever a persona cannot answer, "What's missing?" opens the AI sheet: the reason sheet
// of an offline pill's ▶, its card, the reason once a host runs it but has not let this member ask,
// a blocked website's line, and a plugin that does not load on the persona's Capabilities tab.
test('AI in this room: "What\'s missing?" links', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await pressRun(bob);
  await expect(bob.locator('#why-text')).toContainText('No one runs the AI for this room yet');
  await missing(bob, bob.locator('#why'), runButton(bob));
  await expect(bob.locator('#why')).toBeHidden();

  // On a phone the card's link stands alone in its foot: a 44 px target of its own.
  await bob.setViewportSize(phone);
  await bob.locator('button.persona').first().click();
  await expect(bob.locator('#persona-card').getByRole('link', { name: 'What\'s missing?' })).toBeVisible();
  expect(await smallTargets(bob)).toEqual([]);

  // The card redraws as the AI host connects; Bob still may not ask, and the link keeps the focus.
  await bob.locator('#persona-card-missing').focus();
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    await expect(bob.locator('#persona-card dd').first()).toHaveText('ready');
    await expect(bob.locator('#persona-card-missing')).toBeFocused();
    await missing(bob, bob.locator('#persona-card'), pillButton(bob));
    await bob.setViewportSize(desktop);
    await expect(bob.locator('#persona-card')).toBeHidden();

    await expect(bob.locator('button.persona').first()).toHaveAttribute('aria-label', /ready/);
    await pressRun(bob);
    await expect(bob.locator('#why-text')).toContainText('Only Alice can let you ask');
    await missing(bob, bob.locator('#why'), runButton(bob));

    const blocked = { ...line(1e9 + 2, 'system', 'Grill tried to reach example.com (blocked)'), sender: 'Grill',
      blocked: { domain: 'example.com', persona: 'Grill', count: 1, decision: '' } };
    // A system line reloads the room 200 ms later, which redraws the line: let it land first, or it
    // brings back the link the last step removes. loadRoom sends grill:room after the redraw.
    const reloaded = bob.evaluate(() => new Promise((done) => addEventListener('grill:room', () => done(), { once: true })));
    await live(bob, blocked);
    await reloaded;
    // The line redraws while the sheet is open: the focus goes to its new link.
    const blockedLine = bob.locator(`#messages > li[data-id="${blocked.id}"]`);
    await missing(bob, blockedLine, blockedLine.locator('a[href="#ai"]'), () => bob.evaluate(() => renderBlocked()));
    // It may redraw just after the sheet closed too: the focus still follows; once moved, it stays.
    await bob.evaluate(() => renderBlocked());
    await expect(blockedLine.locator('a[href="#ai"]')).toBeFocused();
    await bob.locator('#body').focus();
    await bob.evaluate(() => renderBlocked());
    await expect(bob.locator('#body')).toBeFocused();
    // A redraw that drops the link while the sheet is open leaves nothing to follow: the composer.
    await missing(bob, blockedLine, bob.locator('#body'), () => blockedLine.locator('a[href="#ai"]').evaluate((a) => a.remove()));

    const plugins = { plugins: [{ id: 1, name: 'opt', url: 'https://example.com/opt.git', ref: 'main', ref_name: 'main', entry: '', personas: [], running: 1,
      needs_setup: true, why: 'needs host setup on the AI host\'s computer', versions: [{ id: 1, n: 1, state: 'ok', short_commit: 'abcdef1', commit: 'abcdef1234' }] }] };
    await bob.route((u) => u.pathname === `/api/rooms/${room.id}/plugins`, (r) => r.fulfill({ json: plugins }));
    await bob.reload();
    const caps = await personaSheet(bob, 'Grill', 'Capabilities');
    // Inside a sheet the sheet's opener gets the focus back: the room menu's button.
    await missing(bob, caps.locator('li[data-plugin="opt"]'), bob.locator('#room-open'));
    await expect(bob.locator('#persona-sheet')).toBeHidden();
    await expect(bob.locator('#personas')).toBeHidden();
  } finally {
    host();
  }
});

const ready = (page) => page.locator('#chat-ready');
// layout measures the chat column, the checklist and the composer.
const layout = (page) => page.evaluate(() => {
  const r = document.getElementById('chat-ready');
  const box = (id) => document.getElementById(id).getBoundingClientRect();
  return { view: innerHeight, chat: document.querySelector('.chat').getBoundingClientRect().height, messages: box('messages').height,
    ready: box('chat-ready').height, readyBottom: box('chat-ready').bottom, scroll: r.scrollHeight, client: r.clientHeight,
    send: box('send').bottom, sendTop: box('send').top };
});
const readyRows = (page) => ready(page).locator('.ai-row').evaluateAll((els) => els.map((e) => e.dataset.row));

// 039-AC10: a new room with a persona and no host lists, in the chat's empty state, what the
// personas still need, with its actions; the host connecting takes its row away, the member's
// ask row stays until the host lets them ask, then the checklist goes.
test('AI in this room: the empty room\'s checklist', async ({ browser, request, baseURL }, testInfo) => {
  test.setTimeout(60_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await bob.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await expect(ready(bob)).toBeVisible();
  await expect(ready(bob).getByRole('heading')).toHaveText('Before the personas can answer');
  expect(await readyRows(bob)).toEqual(['host', 'ask']);
  const none = ready(bob).locator('[data-row="host"]');
  await expect(none.locator('.name')).toHaveText('No one runs the AI for this room.');
  await expect(none.getByRole('button')).toHaveText(['Run the AI on my computer', 'Ask someone to run it']);
  await none.getByRole('button', { name: 'Ask someone to run it' }).click();
  await copied(bob, handOff(`${baseURL}/r/${room.id}`));
  await shots(bob, 'checklist-member', testInfo);
  const fit = () => layout(bob);
  // At 1440 px the chat is a grid beside the Transcription pane: its checklist row is capped the same way.
  await expect(bob.locator('body')).toHaveClass(/transcript-room/);
  const wide = await fit();
  expect(wide.ready).toBeLessThanOrEqual(0.4 * wide.chat + 1);
  expect(wide.send).toBeLessThanOrEqual(wide.view);
  // Whole when it fits, and flush on the composer: no clipped rows, no empty band between them.
  expect(wide.scroll).toBeLessThanOrEqual(wide.client + 1);
  expect(Math.abs(wide.sendTop - wide.readyBottom)).toBeLessThan(1);
  // 037-AC7 039-AC10: the checklist shares the empty chat's area; with it, the reading area keeps its floor.
  await bob.setViewportSize({ width: 390, height: 844 });
  // The page drops the desktop's Transcription pane on the resize: measure the phone layout.
  await expect(bob.locator('body')).not.toHaveClass(/transcript-room/);
  expect(await bob.evaluate(() => ['messages', 'chat-ready'].reduce((h, id) => h + document.getElementById(id).getBoundingClientRect().height, 0))).toBeGreaterThanOrEqual(633);
  // The page fits its body to the viewport on the resize event: poll until it has.
  await expect.poll(async () => { const f = await fit(); return f.send <= f.view; }).toBe(true);
  const f = await fit();
  // The checklist is capped at 40% of the chat column: of the 633 px reading area, the rest is the messages'.
  expect(f.ready).toBeLessThanOrEqual(0.4 * f.chat + 1);
  expect(f.messages).toBeGreaterThanOrEqual(633 - 0.4 * f.chat);
  await bob.setViewportSize({ width: 390, height: 667 });
  await expect.poll(async () => { const g = await fit(); return g.view === 667 && g.send <= g.view && g.messages > 0; }).toBe(true);
  await bob.setViewportSize({ width: 390, height: 844 });
  await expect(ready(alice)).toBeVisible();

  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    await expect(ready(bob).locator('[data-row="host"]')).toHaveCount(0);
    expect(await readyRows(bob)).toEqual(['ask']);
    await expect(ready(bob).locator('[data-row="ask"] .name')).toHaveText('Alice decides who can ask the personas. You can still mention them in a message.');
    await expect(ready(alice)).toBeHidden();
    // A focused action whose row goes hands the focus to the composer, not to the page.
    await ready(bob).locator('[data-row="ask"] button').focus();
    const bobID = await bob.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.me.id), room.id);
    expect(await alice.evaluate(([r, p]) => fetch(`/api/rooms/${r}/participants/${p}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ can_trigger: true }) }).then((res) => res.status), [room.id, bobID])).toBe(204);
    await expect(ready(bob)).toBeHidden();
    await expect(bob.locator('#body')).toBeFocused();
  } finally {
    host();
  }
});

// 039-AC10: the first message ends the checklist, rows not ready or not, and it stays gone.
test('AI in this room: the first message ends the checklist', async ({ browser, request }) => {
  const { pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await expect(ready(bob)).toBeVisible();
  await bob.locator('#body').fill('Hello');
  await bob.locator('#body').press('Enter');
  await expect(bob.locator('#messages')).toContainText('Hello');
  await expect(ready(bob)).toBeHidden();
  await bob.reload();
  await expect(bob.locator('#messages')).toContainText('Hello');
  await expect(ready(bob)).toBeHidden();
});

// 039-AC9: the approver's view of a blocked website: the approval strip's item and the chat line
// keep their Allow buttons and end with the link. The stream is played as the AI host's seat gets it.
test('AI in this room: "What\'s missing?" for the AI host', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const a = { ...line(1e9 + 2, 'system', 'Grill tried to reach example.com (blocked)', '2099-01-01T00:00:00Z'), sender: 'Grill',
    blocked: { domain: 'example.com', persona: 'Grill', count: 1, decision: '' } };
  const state = { ...(await agentState(alice, room.id)), you_host: true, egress_host: '',
    blocked_requests: [{ message_id: a.id, domain: 'example.com', persona: 'Grill', count: 1 }] };
  await playEvents(alice, room.id, [['agent', state], ['', a]]);
  const chat = alice.locator(`#messages > li[data-id="${a.id}"]`);
  await expect(chat.getByRole('button', { name: 'Allow for this room' })).toBeVisible();
  await missing(alice, chat, chat.locator('a[href="#ai"]'));
  await openStrip(alice);
  const item = alice.locator('#approval-list li.blocked-request');
  await expect(item.getByRole('button', { name: 'Allow for this room: example.com' })).toBeVisible();
  // The strip redraws while the sheet is open: its item is gone, the strip keeps the focus.
  await missing(alice, item, alice.locator('#approvals'), () => alice.evaluate(() => renderApprovals()));
  // Opened from the chat line, "Open the approvals" hands the focus to the strip, and a redraw of
  // the line leaves it there.
  await chat.getByRole('link', { name: 'What\'s missing?' }).click();
  await expect(alice.locator('#ai')).toBeVisible();
  await row(alice, 'waiting').getByRole('button', { name: 'Open the approvals' }).click();
  await expect(alice.locator('#ai')).toBeHidden();
  await expect(alice.locator('#approvals')).toBeFocused();
  await alice.evaluate(() => renderBlocked());
  await expect(alice.locator('#approvals')).toBeFocused();
});

// 039-AC4 039-AC10: a seat of the AI host's profile that is not the host's seat decides, but only
// the host's seat ticks "Can ask the AI". Played through the stream: the agent state carries you_host.
test('AI in this room: another seat of the AI host', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    const state = { ...(await agentState(bob, room.id)), you_host: true, can_trigger: [] };
    await playEvents(bob, room.id, [['agent', state]]);
    const text = 'You run the AI as Alice: open the room as Alice and tick "Can ask the AI" next to your name in People.';
    await expect(ready(bob).locator('[data-row="ask"] .name')).toHaveText(text);
    await expect(ready(bob).locator('[data-row="ask"] :is(a, button)')).toHaveCount(0);
    await openAI(bob);
    await expect(row(bob, 'ask').locator('.name')).toHaveText(text);
    await expect(row(bob, 'ask').locator('.ai-who')).toHaveText('Who: you');
  } finally {
    host();
  }
});

// 039-AC10: a room without personas has no checklist; the AI sheet still opens.
test('AI in this room: no checklist without personas', async ({ browser, request }) => {
  const { room, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await expect(ready(bob)).toBeVisible();
  for (const p of (await agentState(bob, room.id)).personas) {
    expect(await bob.evaluate(([r, id]) => fetch(`/api/rooms/${r}/personas/${id}`, { method: 'DELETE' }).then((x) => x.status), [room.id, p.id])).toBe(204);
  }
  await expect(bob.locator('.persona-pill')).toHaveCount(0);
  await expect(ready(bob)).toBeHidden();
  await bob.reload();
  await expect(bob.locator('#messages')).toContainText('Bob joined');
  await expect(ready(bob)).toBeHidden();
  await openAI(bob);
});

// 039-AC9: a member who may not ask gets "Only Alice can let you ask…" with the link even while the
// persona waits in the queue; the queue is played through the stream.
test('AI in this room: "What\'s missing?" on a queued persona', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    const st = await agentState(bob, room.id);
    const grill = st.personas[0];
    const queue = [{ id: 1, persona_id: grill.id, participant_id: st.host_participant_id, source: 'button' }];
    await playEvents(bob, room.id, [['agent', { ...st, can_trigger: [], queue }]]);
    await expect(bob.locator('button.persona').first()).toHaveAttribute('aria-label', /waiting, position 1/);
    await pressRun(bob);
    await expect(bob.locator('#why-text')).toContainText('Only Alice can let you ask');
    await missing(bob, bob.locator('#why'), runButton(bob));
    await bob.locator('button.persona').first().click();
    await missing(bob, bob.locator('#persona-card'), pillButton(bob));
  } finally {
    host();
  }
});

// 039-AC9: the other reasons a persona cannot answer end with the link too: the AI host's app gone,
// a persona waiting for the AI host's approval (played through the stream), a plugin with no version
// that loads.
test('AI in this room: "What\'s missing?" for the other reasons', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  await expect(pillButton(bob)).toHaveAttribute('aria-label', /ready/);
  host();
  await expect(pillButton(bob)).toHaveAttribute('aria-label', /offline/, { timeout: 30_000 });
  await pressRun(bob);
  await expect(bob.locator('#why-text')).toContainText('Alice\'s AI host app is not connected');
  await missing(bob, bob.locator('#why'), runButton(bob));

  const st = await agentState(bob, room.id);
  const bobID = await bob.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.me.id), room.id);
  const pending = { ...st, online: true, can_trigger: [bobID], personas: st.personas.map((p, i) => (i ? p : { ...p, pending: true })) };
  const plugins = { plugins: [{ id: 2, name: 'lint', url: 'https://example.com/lint.git', ref: 'main', ref_name: 'main', entry: '', personas: [],
    why: 'refused by the AI host', versions: [{ id: 2, n: 1, state: 'refused', short_commit: 'abcdef2', commit: 'abcdef2234' }] }] };
  await bob.route((u) => u.pathname === `/api/rooms/${room.id}/plugins`, (r) => r.fulfill({ json: plugins }));
  await playEvents(bob, room.id, [['agent', pending]]);
  await pressRun(bob);
  await expect(bob.locator('#why-text')).toContainText('waits for the AI host\'s approval');
  await missing(bob, bob.locator('#why'), runButton(bob));

  const caps = await personaSheet(bob, st.personas[0].name, 'Capabilities');
  await expect(caps.locator('li[data-plugin="lint"]')).toContainText('refused by the AI host');
  await missing(bob, caps.locator('li[data-plugin="lint"]'), bob.locator('#room-open'));
});

// 039-AC10: a checklist taller than its cap scrolls in its box, on a short phone and in the desktop
// grid beside the Transcription pane; the messages and the composer keep the rest.
test('AI in this room: the checklist past its cap', async ({ browser, request }) => {
  const { pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await expect(ready(bob)).toBeVisible();
  for (const [vp, grid] of [[{ width: 390, height: 560 }, false], [{ width: 1440, height: 420 }, true]]) {
    await bob.setViewportSize(vp);
    await expect(bob.locator('body')).toHaveClass(grid ? /transcript-room/ : /^(?!.*transcript-room)/);
    await expect.poll(async () => { const f = await layout(bob); return f.view === vp.height && f.send <= f.view; }, { message: `${vp.width}: composer on screen` }).toBe(true);
    const f = await layout(bob);
    expect(f.ready, `${vp.width}: capped`).toBeLessThanOrEqual(0.4 * f.chat + 1);
    expect(f.scroll, `${vp.width}: scrolls in its box`).toBeGreaterThan(f.client);
    expect(f.messages, `${vp.width}: messages keep room`).toBeGreaterThan(0);
  }
});

// The sheet's opener was redrawn away while it was open: closing it hands the focus to the composer.
test('AI in this room: focus after the checklist\'s row goes', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const runIt = ready(bob).locator('[data-row="host"]').getByRole('button', { name: 'Run the AI on my computer' });
  // A redraw that keeps the row: back to the same action, not the composer.
  await runIt.click();
  await expect(bob.locator('#host-agent')).toBeVisible();
  await bob.evaluate(() => renderReady());
  await bob.keyboard.press('Escape');
  await expect(runIt).toBeFocused();
  await runIt.click();
  await expect(bob.locator('#host-agent')).toBeVisible();
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    await expect(ready(bob).locator('[data-row="host"]')).toHaveCount(0);
    await bob.keyboard.press('Escape');
    await expect(bob.locator('#host-agent')).toBeHidden();
    await expect(bob.locator('#body')).toBeFocused();
  } finally {
    host();
  }
});
