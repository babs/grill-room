import { test, expect } from './fixtures.js';
import { execFileSync } from 'node:child_process';
import { roomWith, startDaemon, pairDaemon, removeSandbox } from './helpers.js';

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
