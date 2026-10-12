import { test, expect } from '@playwright/test';
import { roomWith, startDaemon, pairDaemon } from './helpers.js';

// 003-AC18: one supervisor serves two rooms. With the global cap at one, a press in the second
// room while the first one's turn runs shows "queued (host busy)" there, and runs once the first
// turn ends.
test('a turn waiting for the host shows queued (host busy)', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const { room: first, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const { room: second, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  const daemon = startDaemon(`${baseURL}/r/${first.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    expect(await daemon.handOver(`${baseURL}/r/${second.id}`)).toBe(0);
    await expect(admin.locator('#pairing')).toHaveText('Waiting for your approval in the room.');
    const [approval] = await Promise.all([bob.context().waitForEvent('page'), (async () => {
      const link = await admin.getByRole('link', { name: 'approve in the room' }).getAttribute('href');
      await bob.evaluate((href) => window.open(href), link);
    })()]);
    await approval.getByRole('button', { name: 'Approve' }).click();
    await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.');
    await approval.close();
    await admin.getByLabel('Turns at once, across every instance and room').fill('1');
    await admin.getByRole('button', { name: 'Set', exact: true }).click();
    await expect(admin.locator('#max-turns-status')).toHaveText('Saved.');

    for (const page of [alice, bob]) {
      await expect(page.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    }
    await alice.locator('#body').fill('FAKE_SLOW');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(alice.locator('#agent-status')).toHaveText('Grill working…', { timeout: 30_000 });
    await bob.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(bob.locator('#agent-status')).toHaveText('Grill queued (host busy)…', { timeout: 30_000 });
    await expect(bob.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill queued (host busy)');
    await expect(admin.locator('#concurrency')).toContainText('Now 1 running, 1 waiting.');
    await expect(bob.locator('#messages li.msg.agent')).toHaveCount(1, { timeout: 60_000 });
    await expect(alice.locator('#messages li.msg.agent')).toHaveCount(1);
    await expect(bob.locator('#agent-status')).toHaveText('');
    await expect(admin.locator('#concurrency')).toContainText('1 waited for a slot');
  } finally {
    daemon.cleanup();
  }
});
