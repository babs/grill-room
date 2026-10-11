import { expect } from '@playwright/test';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, openSync, readFileSync } from 'node:fs';
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
// claude image: no Claude request. env adds to the daemon's environment.
export function startDaemon(roomURL, env = {}) {
  const dir = mkdtempSync(join('test-results', 'daemon-'));
  const stateDir = join(dir, 'state');
  writeFileSync(join(dir, '.credentials.json'),
    JSON.stringify({ claudeAiOauth: { accessToken: 'e2e-fake', expiresAt: Date.now() + 3600e3 } }), { mode: 0o600 });
  const proc = spawn('../bin/grill-room-daemon', [roomURL], {
    env: {
      ...process.env, CLAUDE_CONFIG_DIR: dir, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none',
      GRILL_ROOM_SANDBOX_IMAGE: process.env.GRILL_ROOM_SANDBOX_FAKE_IMAGE || 'grill-room-sandbox:fake', LOG_LEVEL: 'info', ...env,
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
  return { proc, link, exited, sandbox: sandboxName(stateDir, roomURL), cleanup: () => { proc.kill(); rmSync(dir, { recursive: true, force: true }); } };
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

// select puts the page selection on the first occurrence of needle inside one text node.
function select(frame, needle) {
  return frame.evaluate((n) => {
    const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = w.nextNode(); node; node = w.nextNode()) {
      const i = node.data.indexOf(n);
      if (i < 0) continue;
      const r = document.createRange();
      r.setStart(node, i);
      r.setEnd(node, i + n.length);
      getSelection().removeAllRanges();
      getSelection().addRange(r);
      return;
    }
    throw new Error('not in the document: ' + n);
  }, needle);
}

// annotateSelection selects needle in the open document and saves comment on it.
export async function annotateSelection(page, needle, comment) {
  const frame = await docFrame(page);
  await select(frame, needle);
  await page.frameLocator('#doc-frame iframe').locator('.gr-annot-bubble').click();
  await expect(page.locator('#annot-target')).toHaveText(`"${needle}"`);
  await page.getByRole('textbox', { name: 'Annotation', exact: true }).fill(comment);
  await page.locator('#annot-new').getByRole('button', { name: 'Save' }).click();
  await expect(page.locator('#annot-new')).toBeHidden();
}

// liveDaemon runs the real daemon with this machine's Claude Code login (read only), the real
// sandbox image and haiku. Its log goes to a file: the proxy logs one upstream_response per request.
export function liveDaemon(dir, roomURL) {
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const stateDir = join(dir, 'state');
  const log = join(dir, 'daemon.log');
  const proc = spawn('../bin/grill-room-daemon', [roomURL], {
    env: { ...process.env, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none', GRILL_ROOM_AGENT_MODEL: 'haiku', LOG_LEVEL: 'info' },
    stdio: ['ignore', 'ignore', openSync(log, 'w')],
  });
  const exited = new Promise((resolve) => proc.on('exit', resolve));
  const link = (async () => {
    for (let i = 0; i < 100; i++) {
      const m = readFileSync(log, 'utf8').match(/Admin page: (http:\/\/127\.0\.0\.1:\d+\/s\/\S+)/);
      if (m) return m[1];
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error('no admin link in ' + log);
  })();
  return { proc, exited, link, log, sandbox: sandboxName(stateDir, roomURL) };
}

// sandboxName names a daemon's sandbox as cmd/grill-room-daemon does: a killed daemon leaves its
// sandbox behind, and a test may reach into it.
const sandboxName = (stateDir, roomURL) => createHash('sha256').update(stateDir + '\0' + roomURL).digest('hex').slice(0, 12);

export const logLines = (d) => readFileSync(d.log, 'utf8').split('\n').filter((l) => l.startsWith('{')).map((l) => JSON.parse(l));
export const messagesRequests = (d) => logLines(d).filter((l) => l.msg === 'upstream_response' && l.path.startsWith('/v1/messages')).length;

export function removeSandbox(name) {
  const docker = (...args) => { try { return execFileSync('docker', args, { encoding: 'utf8' }); } catch { return ''; } };
  const label = `label=grill-room-daemon=${name}`;
  for (const id of docker('ps', '-aq', '--filter', label).split('\n').filter(Boolean)) docker('rm', '-f', '-v', id);
  for (const id of docker('network', 'ls', '-q', '--filter', label).split('\n').filter(Boolean)) docker('network', 'rm', id);
  for (const id of docker('volume', 'ls', '-q', '--filter', label).split('\n').filter(Boolean)) docker('volume', 'rm', '-f', id);
}

export const agentState = (page, roomID) => page.evaluate((id) => fetch(`/api/rooms/${id}/agent`).then((r) => r.json()), roomID);

// ask says text in the room, presses the persona's button and returns its synthesis.
export async function ask(page, roomID, text, persona = 'Grill') {
  const before = await page.locator('#messages li.msg.agent').count();
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
  await expect(page.locator('#messages')).toContainText(text.slice(0, 40));
  await page.getByRole('button', { name: 'Ask ' + persona }).click();
  await expect(page.locator('#messages li.msg.agent')).toHaveCount(before + 1, { timeout: 240_000 });
  const st = await agentState(page, roomID);
  expect(st.turn.status).toBe('done');
  return page.locator('#messages li.msg.agent').last().locator('.body').textContent();
}
