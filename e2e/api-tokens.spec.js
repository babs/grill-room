import { test, expect } from './fixtures.js';
import { mkdirSync } from 'node:fs';
import { roomWith, noOverflow, smallTargets } from './helpers.js';

// Personal API tokens on /profile (specs/012-agent-api.md, Phase 1).
// SHOTS=<dir> saves screenshots for the visual check.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : null);
const sizes = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } };

// 038-AC13: the tab and its heading name Claude Code; each scope says what it lets Claude Code do.
test('API tokens and Claude Code: the scopes say what they allow', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Scopes']);
  const page = await pat.context().newPage();
  await page.setViewportSize(sizes.phone);
  await page.goto('/profile#tokens');
  await expect(page.locator('#tab-tokens')).toHaveText(/^API tokens and Claude Code/);
  await expect(page.locator('#panel-tokens h2')).toHaveText('API tokens and Claude Code');
  await page.locator('#token-list .empty').getByRole('button', { name: 'New token' }).click();
  const scopes = page.locator('#token-sheet .token-scope label');
  await expect(scopes).toHaveText(['Read only: Claude Code can read your rooms, documents and transcripts.',
    'Full: it can also create rooms, add personas, link rooms and change settings.']);
  await expect(page.locator('#token-sheet').getByLabel('Read only: Claude Code can read')).toBeChecked();
  expect(await noOverflow(page)).toEqual([]);
});

// 012-AC1 012-AC2 012-AC8 012-AC9: create a token, see it once with its Copy, watch its first use
// show in its row, revoke it from the row's ⋯ and see the next request refused.
for (const [vp, size] of Object.entries(sizes)) {
  test(`api token create, copy, use and revoke on ${vp}`, async ({ browser, request }) => {
    const name = `Tok ${vp}`;
    const { pages: [pat] } = await roomWith(browser, request, [name]);
    await pat.context().grantPermissions(['clipboard-read', 'clipboard-write']);
    const page = await pat.context().newPage();
    await page.setViewportSize(size);
    await page.goto('/profile#tokens');
    await expect(page.locator('#panel-tokens')).toBeVisible();
    await expect(page.locator('#count-tokens')).toHaveText('0');
    await page.locator('#token-list .empty').getByRole('button', { name: 'New token' }).click();
    const sheet = page.locator('#token-sheet');
    await expect(sheet).toBeVisible();
    await sheet.getByLabel('Name').fill(`ci ${vp}`);
    await sheet.getByLabel('Expires').selectOption('30');
    await sheet.getByLabel('Full: it can also create rooms').check();
    await sheet.getByRole('button', { name: 'Create token' }).click();
    // 038-AC13
    await expect(sheet).toContainText('This command shows once. Lost it? Make a new token and revoke this one.');
    const value = sheet.locator('#token-value .value');
    const token = await value.getAttribute('data-value');
    expect(token).toMatch(/^grt_[A-Z2-7]{26}$/);
    await expect(sheet.locator('#token-curl .value')).toHaveAttribute('data-value', new RegExp(`Bearer ${token}" https?://`));
    // 012-AC22: the Claude Code command, with the public URL and the token, copies whole.
    const command = `claude mcp add --transport http grill-room ${new URL(page.url()).origin}/mcp --header "Authorization: Bearer ${token}"`;
    const mcpValue = sheet.locator('#token-mcp .value');
    await expect(mcpValue).toHaveAttribute('data-value', command);
    await mcpValue.getByRole('button', { name: 'Copy the Claude Code command' }).click();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(command);
    await value.getByRole('button', { name: 'Copy the token' }).click();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(token);
    expect(await noOverflow(page)).toEqual([]);
    if (vp === 'phone') expect(await smallTargets(page)).toEqual([]);
    await shot(page, `tokens-sheet-${vp}`);
    await sheet.getByRole('button', { name: 'Done' }).click();
    await expect(sheet).toBeHidden();
    // The plaintext left with the sheet.
    await expect(page.locator('body')).not.toContainText(token);

    const row = page.locator('#token-list .list-row');
    await expect(row).toHaveCount(1);
    await expect(row).toContainText(`ci ${vp}`);
    await expect(row).toContainText('Full');
    await expect(row).toContainText('expires in 30 days');
    await expect(row).toContainText('never used');
    expect(await noOverflow(page)).toEqual([]);
    if (vp === 'phone') expect(await smallTargets(page)).toEqual([]);

    const auth = { Authorization: `Bearer ${token}` };
    expect((await request.get('/api/profile', { headers: auth })).status()).toBe(200);
    await page.reload();
    await expect(row).toContainText('used just now');
    await shot(page, `tokens-list-${vp}`);

    await row.getByRole('button', { name: `More actions for the token ci ${vp}` }).click();
    await page.getByRole('menuitem', { name: 'Revoke' }).click();
    await expect(page.locator('.toast').filter({ hasText: 'Revoked' })).toBeVisible();
    await expect(row).toHaveCount(0);
    const res = await request.get('/api/profile', { headers: auth });
    expect(res.status()).toBe(401);
  });
}
