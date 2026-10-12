import { test, expect } from '@playwright/test';
import { roomWith, startDaemon } from './helpers.js';

// 001-AC22: run the daemon with the room URL, open its local page, approve in the room session
// with one click: the daemon is paired and connected, and no token was shown or typed.
test('daemon pairs through the room approval', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await alice.context().newPage();
    await admin.goto(await daemon.link);
    await expect(admin).toHaveURL(/^http:\/\/127\.0\.0\.1:\d+\/$/);
    await expect(admin.locator('#pairing')).toHaveText('Waiting for your approval in the room.');
    const code = await admin.locator('#code').textContent();
    expect(code).toMatch(/^\d{3}-\d{3}$/);

    const [approval] = await Promise.all([alice.context().waitForEvent('page'), admin.getByRole('link', { name: 'approve in the room' }).click()]);
    await expect(approval.locator('#pairing-text')).toContainText(code);
    await approval.getByRole('button', { name: 'Approve' }).click();
    await expect(approval.locator('#pairing-text')).toContainText('Daemon paired');

    await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.');
    await expect(admin.locator('#creds')).toContainText('found in');
    await expect(admin.locator('#can-host')).toBeVisible();
    // The page reloads into the same session; the one-time link is spent.
    await admin.reload();
    await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.');
    const spent = await alice.context().newPage();
    await spent.goto(await daemon.link);
    await expect(spent.locator('#banner')).toContainText('already used');
    for (const p of [admin, approval, alice]) await expect(p.locator('body')).not.toContainText('e2e-fake');

    await admin.getByRole('button', { name: 'Stop the daemon' }).click();
    await expect(admin.locator('#banner')).toHaveText('The daemon stopped. You can close this tab.');
    expect(await daemon.exited).toBe(0);
  } finally {
    daemon.cleanup();
  }
});

// 003-AC4, 003-AC5: the page says the supervisor is a dev build. Accepting unsigned binaries from
// an instance needs its host typed exactly; while on, a red banner and a badge show.
test('daemon page shows its signature and guards the unsigned toggle', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  // An unreachable GitHub: no update check leaves this machine.
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' });
  try {
    const admin = await alice.context().newPage();
    await admin.goto(await daemon.link);
    await expect(admin.locator('#self-sig')).toHaveText('dev build');
    await expect(admin.locator('#instances')).toContainText("Worker binary: the supervisor's own binary");
    await expect(admin.locator('#relax-banner')).toBeHidden();
    await expect(admin.locator('#checked')).toContainText('127.0.0.1:9');

    const accept = admin.getByRole('button', { name: 'Accept unsigned binaries' });
    await admin.locator('#relax-host').fill('127.0.0.1');
    await accept.click();
    await expect(admin.locator('#relax-status')).toContainText("type the instance's host exactly");
    await expect(admin.locator('#relax-banner')).toBeHidden();
    await admin.locator('#relax-host').fill(new URL(baseURL).host);
    await accept.click();
    await expect(admin.locator('#relax-banner')).toBeVisible();
    await expect(admin.locator('#relax-banner')).toContainText(baseURL);
    await expect(admin.locator('#instances .badge.danger')).toHaveText('accepts unsigned binaries');
    await admin.getByRole('button', { name: 'Stop accepting unsigned binaries' }).click();
    await expect(admin.locator('#relax-banner')).toBeHidden();
    await expect(admin.locator('#instances .badge.danger')).toHaveCount(0);

    await admin.getByRole('button', { name: 'Stop the daemon' }).click();
    expect(await daemon.exited).toBe(0);
  } finally {
    daemon.cleanup();
  }
});
