import { expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

export async function roomWith(browser, request, names) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Docs room' } })).json();
  const pages = [];
  for (const name of names) {
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const page = await (await browser.newContext()).newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    pages.push(page);
  }
  return { room, pages };
}

export function upload(page, name, mimeType, content) {
  return page.locator('#upload-input').setInputFiles({ name, mimeType, buffer: Buffer.from(content) });
}

// docFrame returns the document frame on show, once its page has loaded.
export async function docFrame(page) {
  const handle = await page.locator('#doc-frame iframe').elementHandle();
  const frame = await handle.contentFrame();
  await frame.waitForLoadState();
  return frame;
}

// startDaemon runs the real daemon binary against this room, with a fake credentials file and a
// throwaway state dir, and resolves with the admin page link it prints. Its sandbox uses the fake
// claude image: no Claude request.
export function startDaemon(roomURL) {
  const dir = mkdtempSync(join('test-results', 'daemon-'));
  writeFileSync(join(dir, '.credentials.json'),
    JSON.stringify({ claudeAiOauth: { accessToken: 'e2e-fake', expiresAt: Date.now() + 3600e3 } }), { mode: 0o600 });
  const proc = spawn('../bin/grill-room-daemon', [roomURL], {
    env: {
      ...process.env, CLAUDE_CONFIG_DIR: dir, GRILL_ROOM_DAEMON_STATE_DIR: join(dir, 'state'), GRILL_ROOM_DAEMON_BROWSER: 'none',
      GRILL_ROOM_SANDBOX_IMAGE: process.env.GRILL_ROOM_SANDBOX_FAKE_IMAGE || 'grill-room-sandbox:fake', LOG_LEVEL: 'info',
    },
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

// pairDaemon approves the daemon's pairing from the participant's room page, as a person would.
export async function pairDaemon(page, daemon) {
  const admin = await page.context().newPage();
  await admin.goto(await daemon.link);
  await expect(admin.locator('#pairing')).toHaveText('Waiting for your approval in the room.');
  const [approval] = await Promise.all([page.context().waitForEvent('page'), admin.getByRole('link', { name: 'approve in the room' }).click()]);
  await approval.getByRole('button', { name: 'Approve' }).click();
  await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.');
  await approval.close();
  return admin;
}
