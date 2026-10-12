import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { roomWith, startDaemon, pairDaemon, removeSandbox } from './helpers.js';

const sections = ['Room', 'Instances', 'Rooms', 'Claude accounts', 'Settings', 'Monitoring', 'Logs'];

// 003-AC26 003-AC27 003-AC28 003-AC29: the supervisor's page shows every section; a turn shows up
// in the history with its persona, account, cause, cost and context, in the daily totals and in
// the CSV export; the filters narrow it; a login near expiry and a context above 80% raise
// alerts. The same page fits a phone's width. Fake claude: no Claude request.
test('admin page sections, monitoring, CSV export and alerts, at desktop and phone width', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const roomURL = `${baseURL}/r/${room.id}`;
  // A login 10 minutes from expiry; an unreachable GitHub keeps the update check on this machine.
  const daemon = startDaemon(roomURL, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' }, { expiresIn: 10 * 60e3 });
  try {
    const admin = await pairDaemon(alice, daemon);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    for (const name of sections) await expect(admin.getByRole('heading', { name, exact: true })).toBeVisible();

    const card = admin.locator('#instances li.instance');
    await expect(card).toContainText(`${baseURL} · running, v`);
    await expect(card.locator('.badge')).toHaveText("the supervisor's binary");
    await expect(card).toContainText(/Worker pid \d+, protocol 3/);
    await expect(admin.locator('#rooms-section')).toContainText(`${roomURL} connected, hosts the agent, parallel turns off`);
    await expect(admin.locator('#accounts li').first()).toContainText('default');
    await expect(admin.locator('#alerts')).toContainText('Claude account default: the access token expires in');

    await admin.getByLabel('Days the turn history keeps').fill('7');
    await admin.getByRole('button', { name: 'Keep', exact: true }).click();
    await expect(admin.locator('#retention-status')).toHaveText('Saved.');
    await expect(admin.locator('#settings-instance')).toHaveValue(/^[0-9a-f]{12}$/);
    await expect(admin.getByLabel('Sandbox CPUs')).toHaveValue('2');

    await alice.locator('#body').fill('FAKE_CONTEXT_FULL');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(alice.locator('#messages li.msg.agent')).toHaveCount(1, { timeout: 60_000 });

    const rows = admin.locator('#turns tbody tr');
    await expect(rows).toHaveCount(1, { timeout: 15_000 });
    for (const text of [roomURL, 'Grill', 'default', 'pressed by Alice', 'ok', '$0.0125', '85%']) await expect(rows.first()).toContainText(text);
    await expect(admin.locator('#totals tbody tr').first()).toContainText(/\d{4}-\d{2}-\d{2}default1/);
    await expect(admin.locator('#alerts')).toContainText(`Grill in ${roomURL} uses 85% of its context`);

    await admin.locator('#filter-status').selectOption('ok');
    await expect(rows).toHaveCount(1);
    await admin.locator('#filter-from').fill('2999-01-01');
    await expect(admin.locator('#monitoring-section')).toContainText('No turn matches.');
    await admin.getByRole('button', { name: 'Clear' }).click();
    await expect(rows).toHaveCount(1);

    const [file] = await Promise.all([admin.waitForEvent('download'), admin.getByRole('button', { name: 'Export CSV' }).click()]);
    expect(file.suggestedFilename()).toBe('grill-room-turns.csv');
    const [header, row, ...rest] = readFileSync(await file.path(), 'utf8').trimEnd().split('\n');
    expect(rest).toHaveLength(0);
    const cols = header.split(',');
    const cell = (name) => row.split(',')[cols.indexOf(name)];
    expect([cell('room'), cell('persona'), cell('account'), cell('cause'), cell('status'), cell('cost_usd'), cell('context_pct')])
      .toEqual([roomURL, 'Grill', 'default', 'pressed by Alice', 'ok', '0.0125', '85']);

    // A phone: every section reachable, nothing wider than the screen.
    await admin.setViewportSize({ width: 390, height: 844 });
    for (const name of sections) {
      const h = admin.getByRole('heading', { name, exact: true });
      await h.scrollIntoViewIfNeeded();
      await expect(h).toBeInViewport();
    }
    // Tables scroll inside their own box; anything else past the edge is named.
    const wide = await admin.evaluate(() => [...document.querySelectorAll('body *')]
      .filter((e) => !e.parentElement?.closest('.scroll') && !['pre', 'select'].includes(e.tagName.toLowerCase()) &&
        (e.getBoundingClientRect().right > innerWidth + 1 || (getComputedStyle(e).overflowX === 'visible' && e.scrollWidth > e.clientWidth + 1)))
      .map((e) => `${e.tagName.toLowerCase()}#${e.id}.${e.className}`));
    expect(wide).toEqual([]);
    expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
    await admin.locator('#alerts').scrollIntoViewIfNeeded();
    await expect(admin.locator('#alerts')).toBeInViewport();

    // Limits apply by restarting the worker, which comes back.
    await admin.getByLabel('Sandbox memory').fill('2g');
    await admin.getByRole('button', { name: 'Apply the limits' }).click();
    await expect(admin.locator('#limits-status')).toHaveText('Applied: the worker restarts.');
    await expect(card).toContainText(`${baseURL} · running`, { timeout: 30_000 });
    await expect(admin.getByLabel('Sandbox memory')).toHaveValue('2g');
  } finally {
    daemon.cleanup();
    await daemon.exited;
    removeSandbox(daemon.sandbox);
  }
});
