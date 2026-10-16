import { test, expect } from './fixtures.js';
import { execFileSync } from 'node:child_process';
import { roomWith, startDaemon, pairDaemon, removeSandbox, menuItem } from './helpers.js';
import { adminToken } from './playwright.config.js';

// curlInSandbox asks the egress proxy for host from inside the sandbox and returns its CONNECT answer.
function curlInSandbox(sandbox, host) {
  try {
    return execFileSync('docker', ['exec', 'grill-room-' + sandbox, 'curl', '-s', '-o', '/dev/null', '-m', '10', '-w', '%{http_connect}', `https://${host}/`], { encoding: 'utf8' });
  } catch (err) {
    return err.stdout;
  }
}

// 002-AC36 002-AC34 002-AC35: on the daemon page the host sees the default allowlist, is told why a
// wildcard, an Anthropic host or a non-domain is refused, adds and removes domains, and the list
// survives a reload. After a turn starts the sandbox, requests the proxy refuses show up with host
// and time, newest first. Real daemon and Docker sandbox, fake claude; no Internet needed.
test('egress allowlist and denied requests on the daemon page', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    await admin.locator('#tab-settings').click();
    const list = admin.locator('#allowlist');
    const status = admin.locator('#allowlist-status');
    for (const d of ['pypi.org', 'files.pythonhosted.org', 'github.com', 'registry.npmjs.org']) await expect(list).toContainText(d);
    await expect(admin.locator('#denied-empty')).toHaveText('None yet.');

    const add = async (domain) => {
      await admin.getByLabel('Domain').fill(domain);
      await admin.locator('#allowlist-form').getByRole('button', { name: 'Add' }).click();
    };
    for (const [bad, why] of [['*.example.com', 'no wildcards'], ['api.anthropic.com', 'Anthropic host'], ['https://example.com/x', 'not a domain name']]) {
      await add(bad);
      await expect(status).toContainText(why);
      await expect(list).not.toContainText(bad);
    }
    await add('Example.COM');
    await expect(status).toHaveText('Saved.');
    await expect(list.locator('tbody tr', { hasText: 'example.com' })).toHaveCount(1);
    await expect(admin.getByLabel('Domain')).toHaveValue('');
    await admin.getByRole('button', { name: 'Remove github.com' }).click();
    await expect(list.locator('code', { hasText: /^github\.com$/ })).toHaveCount(0);
    await admin.reload();
    await expect(list.locator('code', { hasText: /^example\.com$/ })).toHaveCount(1);
    await expect(list.locator('code', { hasText: /^github\.com$/ })).toHaveCount(0);

    // A turn starts the sandbox and its proxy, with the list just saved.
    await alice.locator('#body').fill('FAKE_WHOAMI');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(alice.locator('#messages li.msg.agent')).toHaveCount(1, { timeout: 60_000 });
    await expect.poll(() => curlInSandbox(daemon.sandbox, 'github.com'), { timeout: 20_000 }).toBe('403');
    expect(curlInSandbox(daemon.sandbox, 'api.anthropic.com')).toBe('403');

    const rows = admin.locator('#denied tbody tr');
    await expect(rows.first()).toContainText('api.anthropic.com', { timeout: 15_000 });
    await expect(rows.nth(1)).toContainText('github.com');
    await expect(admin.locator('#denied-empty')).toBeHidden();
    await expect(rows.first().locator('td').first()).toContainText(String(new Date().getFullYear()));

    // 003b-AC16: a request the proxy lets through counts as a hit on its domain's row.
    expect(curlInSandbox(daemon.sandbox, 'pypi.org')).toBe('200');
    await expect(list.locator('tbody tr', { hasText: /^pypi\.org/ }).locator('td.num')).not.toHaveText(/^[0–]$/, { timeout: 15_000 });
  } finally {
    daemon.cleanup();
    await daemon.exited;
    removeSandbox(daemon.sandbox);
  }
});

// 014-AC1 014-AC2 014-AC3 014-AC26 014-AC27: the docs hosts come back in one click; "Allow every
// website" asks first behind a red warning, lets any public name through and counts it as reached
// without an entry, still refusing the metadata address; the local-network switch needs it, asks
// with its own warning, and both go off with one click. Real daemon and Docker sandbox, fake
// claude; the public hosts need Internet access.
test('allow every website and its local-network extension', async ({ browser, request, baseURL }) => {
  test.setTimeout(150_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    await admin.locator('#tab-settings').click();
    const list = admin.locator('#allowlist');
    const status = admin.locator('#allowlist-status');

    await expect(admin.locator('#add-docs')).toHaveCount(0);
    await admin.getByRole('button', { name: 'Remove code.claude.com' }).click();
    await expect(admin.locator('#add-docs')).toBeVisible();
    await admin.locator('#add-docs').click();
    await expect(status).toHaveText('Anthropic docs sites added.');
    await expect(list.locator('code', { hasText: /^code\.claude\.com$/ })).toHaveCount(1);
    await expect(admin.locator('#add-docs')).toHaveCount(0);

    const all = admin.locator('#all-websites');
    const local = admin.locator('#local-network');
    await expect(all).not.toBeChecked();
    await expect(local).toBeDisabled();
    const sheet = admin.locator('dialog.confirm');
    await all.click();
    await expect(sheet.locator('.warning')).toContainText('anything they hold can leave');
    await sheet.getByRole('button', { name: 'Cancel' }).click();
    await expect(all).not.toBeChecked();
    await all.click();
    await sheet.getByRole('button', { name: 'Allow every website' }).click();
    await expect(all).toBeChecked();
    await expect(local).toBeEnabled();
    await local.click();
    await expect(sheet.locator('.warning')).toContainText('your own network');
    await sheet.getByRole('button', { name: 'Allow the local network' }).click();
    await expect(local).toBeChecked();
    await admin.reload();
    await expect(all).toBeChecked();
    await expect(local).toBeChecked();

    // A turn starts the sandbox and its proxy under both switches.
    await alice.locator('#body').fill('FAKE_WHOAMI');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(alice.locator('#messages li.msg.agent')).toHaveCount(1, { timeout: 60_000 });
    await expect.poll(() => curlInSandbox(daemon.sandbox, 'example.com'), { timeout: 20_000 }).toBe('200');
    expect(curlInSandbox(daemon.sandbox, '169.254.169.254')).toBe('403');
    expect(curlInSandbox(daemon.sandbox, 'api.anthropic.com')).toBe('403');
    await expect(list.locator('tr.reached', { hasText: 'example.com' })).toContainText('reached via Allow every website', { timeout: 15_000 });

    // Off needs no confirmation and takes the extension with it.
    await all.click();
    await expect(all).not.toBeChecked();
    await expect(local).not.toBeChecked();
    await expect(local).toBeDisabled();
    await expect.poll(() => curlInSandbox(daemon.sandbox, 'example.com'), { timeout: 20_000 }).toBe('403');
  } finally {
    daemon.cleanup();
    await daemon.exited;
    removeSandbox(daemon.sandbox);
  }
});

// ask posts text as page and presses the Grill, then waits for its n-th answer.
async function ask(page, text, n) {
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
  await page.getByRole('button', { name: 'Ask Grill' }).click();
  await expect(page.locator('#messages li.msg.agent')).toHaveCount(n, { timeout: 60_000 });
}

// 014-AC8 to 014-AC15: a website the agent was refused shows in the chat; the AI host allows it for
// the room and the next answer reaches it; with the host's delegation a room admin allows a second
// one for the room only, while a plain member never gets a button; the daemon page lists both
// with the room as scope and removes one, which the chat hears of. Real daemon and sandbox, fake
// claude; example.com and example.org need Internet access.
test('blocked websites in the chat, allowed by the host and a delegated admin', async ({ browser, request, baseURL }) => {
  test.setTimeout(240_000);
  const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
  const me = await bob.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: { Authorization: `Bearer ${adminToken}` } })).status()).toBe(204);
  await bob.reload();
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    const line = (page, domain) => page.locator('#messages li.msg.blocked', { hasText: domain });

    await ask(alice, 'FAKE_FETCH example.com', 1);
    await expect(line(alice, 'example.com')).toContainText('Grill tried to reach example.com (blocked)', { timeout: 15_000 });
    await expect(line(alice, 'example.com').getByRole('button')).toHaveText(['Allow for this room', 'Allow everywhere']);
    await expect(line(bob, 'example.com')).toBeVisible();
    await expect(line(bob, 'example.com').getByRole('button')).toHaveCount(0);
    await expect(line(carol, 'example.com').getByRole('button')).toHaveCount(0);
    // Whoever cannot allow it is told who can: the line never looks like a dead end.
    await expect(line(carol, 'example.com')).toContainText('Pending: only the AI host, Alice, can allow it.');
    await expect(line(bob, 'example.com')).toContainText('Pending: only the AI host, Alice, can allow it.');
    // The banner holds it for the host until decided, across a reload; a plain member gets none.
    const banner = (page) => page.locator('#approval-list li.blocked-request');
    await expect(banner(alice)).toHaveCount(1);
    await expect(banner(alice)).toContainText('Grill tried to reach example.com.');
    await expect(banner(alice).getByRole('button')).toHaveText(['Allow for this room', 'Allow everywhere', 'Dismiss']);
    await expect(banner(carol)).toHaveCount(0);
    await expect(banner(bob)).toHaveCount(0);
    await alice.reload();
    await expect(banner(alice)).toHaveCount(1);
    // 390 and 1440 wide, the banner stays on screen.
    for (const width of [390, 1440]) {
      await alice.setViewportSize({ width, height: 900 });
      await expect(banner(alice).getByRole('button', { name: 'Dismiss: example.com' })).toBeInViewport();
      expect(await alice.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    }
    await banner(alice).getByRole('button', { name: 'Allow for this room: example.com' }).click();
    await expect(line(alice, 'example.com')).toContainText('Alice allowed example.com for this room');
    await expect(banner(alice)).toHaveCount(0);
    await expect(line(carol, 'example.com')).toContainText('Alice allowed example.com for this room');
    await ask(alice, 'FAKE_FETCH example.com', 2);
    await expect(alice.locator('#messages li.msg.agent').last()).toContainText('Fetched example.com: 200');

    await ask(alice, 'FAKE_FETCH example.org', 3);
    await expect(line(alice, 'example.org')).toContainText('(blocked)', { timeout: 15_000 });
    await menuItem(alice, 'Room settings');
    await alice.locator('#manage').getByRole('link', { name: 'Websites and connectors' }).click();
    await alice.locator('#egress-admins').check();
    await expect(alice.locator('#messages li.msg.system', { hasText: 'Alice let the room admins allow websites for this room.' })).toHaveCount(1);
    await alice.locator('#manage-close').click();
    await expect(line(bob, 'example.org').getByRole('button')).toHaveText(['Allow for this room']);
    await expect(banner(bob).getByRole('button')).toHaveText(['Allow for this room', 'Dismiss']);
    await expect(banner(carol)).toHaveCount(0);
    await expect(line(carol, 'example.org').getByRole('button')).toHaveCount(0);
    await expect(line(carol, 'example.org')).toContainText('Pending: only the AI host, Alice, or a room admin can allow it.');
    await line(bob, 'example.org').getByRole('button', { name: 'Allow for this room' }).click();
    await expect(line(alice, 'example.org')).toContainText('Bob allowed example.org for this room');

    // The daemon page: both entries scoped to the room, one removed.
    await admin.locator('#tab-settings').click();
    const rows = admin.locator('#allowlist tr.room-entry');
    await expect(rows).toHaveCount(2);
    await expect(rows.filter({ hasText: 'example.org' })).toContainText('Docs room');
    await admin.getByRole('button', { name: 'Remove example.org from Docs room' }).click();
    await expect(rows).toHaveCount(1);
    await expect(alice.locator('#messages li.msg.system', { hasText: 'Alice removed example.org from this room\'s websites' })).toHaveCount(1);
  } finally {
    daemon.cleanup();
    await daemon.exited;
    removeSandbox(daemon.sandbox);
  }
});
