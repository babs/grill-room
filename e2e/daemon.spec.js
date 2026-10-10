import { test, expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { roomWith } from './helpers.js';

// startDaemon runs the real daemon binary against this room, with a fake credentials file and a
// throwaway state dir, and resolves with the admin page link it prints.
function startDaemon(roomURL) {
  const dir = mkdtempSync(join('test-results', 'daemon-'));
  writeFileSync(join(dir, '.credentials.json'),
    JSON.stringify({ claudeAiOauth: { accessToken: 'e2e-fake', expiresAt: Date.now() + 3600e3 } }), { mode: 0o600 });
  const proc = spawn('../bin/grill-room-daemon', [roomURL], {
    env: { ...process.env, CLAUDE_CONFIG_DIR: dir, GRILL_ROOM_DAEMON_STATE_DIR: join(dir, 'state'), GRILL_ROOM_DAEMON_BROWSER: 'none', LOG_LEVEL: 'info' },
    stdio: ['ignore', 'ignore', 'pipe'],
  });
  const exited = new Promise((resolve) => proc.on('exit', resolve));
  const link = new Promise((resolve, reject) => {
    let out = '';
    proc.stderr.on('data', (b) => {
      out += b;
      const m = out.match(/Admin page: (http:\/\/127\.0\.0\.1:\d+\/s\/\S+)/);
      if (m) resolve(m[1]);
    });
    proc.on('exit', (code) => reject(new Error('daemon exited ' + code + ': ' + out)));
  });
  return { proc, link, exited, cleanup: () => { proc.kill(); rmSync(dir, { recursive: true, force: true }); } };
}

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
