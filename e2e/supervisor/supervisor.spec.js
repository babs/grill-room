import { test, expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { mkdirSync, openSync, readFileSync, rmSync } from 'node:fs';
import { hostname } from 'node:os';
import { join } from 'node:path';
import { adminAPI, ask, latestLink, logLines, messagesRequests, connectedRow } from '../helpers.js';

const local = `https://grill-room.${process.env.DEV_DOMAIN}`;
const lab = `https://grill-room.${process.env.K8S_DOMAIN}`;
const admin = { Authorization: `Bearer ${process.env.GRILL_ROOM_ADMIN_TOKEN}` };
const question = 'Without using any tool, reply in one short sentence: what is this room about?';

// joinRoom creates a room on base and enters it as name in a fresh context of browser.
async function joinRoom(browser, request, base, name, roomName) {
  const room = await (await request.post(`${base}/api/rooms`, { headers: admin, data: { name: roomName } })).json();
  const inv = await (await request.post(`${base}/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(inv.url);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return { base, room, page, url: `${base}/r/${room.id}` };
}

// approve pairs the supervisor's room on show with its page from the participant's room page.
async function approve(adminPage, room) {
  await expect(adminPage.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 60_000 });
  const link = await adminPage.getByRole('link', { name: 'approve in the room' }).getAttribute('href');
  expect(link.startsWith(room.base)).toBeTruthy();
  const [approval] = await Promise.all([room.page.context().waitForEvent('page'), room.page.evaluate((href) => window.open(href), link)]);
  await approval.getByRole('button', { name: 'Approve' }).click();
  await expect(connectedRow(adminPage)).toBeAttached({ timeout: 60_000 });
  await approval.close();
}

// 003-AC31: one supervisor serves a room on the compose instance (a release signed with a dev
// key) and a room on the lab instance (a project-signed release) at once, each on its own
// account. Two haiku turns run in parallel across the rooms, the monitoring records both with
// their accounts, and the lab room shows who holds the agent, never the machine.
test('one supervisor, two instances, two accounts, two turns at once', async ({ browser, request }) => {
  const dir = join('test-results', 'supervisor');
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const log = join(dir, 'supervisor.log');
  const rooms = [];
  let proc;
  let exited;
  let api;
  try {
    rooms.push(await joinRoom(browser, request, local, 'Alice', 'AC31 local'));
    rooms.push(await joinRoom(browser, request, lab, 'Alice', 'AC31 lab'));
    const [mine, theirs] = rooms;
    proc = spawn(process.env.GRILL_ROOM_E2E_SUPERVISOR || '../bin/grill-room-daemon', [mine.url], {
      env: { ...process.env, GRILL_ROOM_DAEMON_STATE_DIR: join(dir, 'state'), GRILL_ROOM_DAEMON_BROWSER: 'none', GRILL_ROOM_DAEMON_ADMIN_PORT: '0', GRILL_ROOM_MAX_MODEL: 'haiku', LOG_LEVEL: 'info' },
      stdio: ['ignore', 'ignore', openSync(log, 'w')],
    });
    // A binary that cannot start emits error, not exit.
    exited = new Promise((resolve) => { proc.on('exit', resolve); proc.on('error', resolve); });
    api = await adminAPI(request, log);
    // The compose instance serves a dev build: its worker runs once the dev key is trusted.
    await api.post('/api/dev-keys', { name: 'e2e', pem: readFileSync(process.env.GRILL_ROOM_E2E_DEV_KEY, 'utf8') });
    await api.post('/api/accounts', { name: 'token', kind: 'setup_token', token: readFileSync(process.env.GRILL_ROOM_E2E_SETUP_TOKEN_FILE, 'utf8').trim() });

    const adminPage = await mine.page.context().newPage();
    await adminPage.goto(await latestLink(log));
    await approve(adminPage, mine);
    await api.post('/api/room', { url: theirs.url });
    await approve(adminPage, theirs);

    const status = await api.get('/api/status');
    expect(status.concurrency.max_turns).toBe(2);
    const instance = (base) => status.instances.find((i) => i.url === base);
    expect(instance(local).trust).toContain('dev key');
    expect(instance(lab).version).toMatch(/^v\d+\.\d+\.\d+$/);
    await api.post('/api/instance-account', { instance: instance(lab).id, room: theirs.url, account: 'token' });

    for (const r of rooms) {
      await expect(r.page.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 120_000 });
    }
    await expect(theirs.page.locator('#messages')).toContainText('Alice holds the agent(s).');
    await expect(theirs.page.locator('body')).not.toContainText(hostname());

    // Both turns start together; the supervisor must hold both slots at one moment.
    let both = false;
    let done = false;
    const watch = (async () => {
      while (!both && !done) {
        both = (await api.get('/api/status')).concurrency.running.length === 2;
        await new Promise((r) => setTimeout(r, 200)); // samples the slots until both turns end
      }
    })();
    const answers = await Promise.all(rooms.map((r) => ask(r.page, r.room.id, question))).finally(() => { done = true; });
    await watch;
    console.log('answers:', JSON.stringify(answers));
    expect(both, 'two turns running at once').toBe(true);

    const { turns } = await api.get('/api/monitor/turns');
    const turn = (url) => turns.find((t) => t.room === url);
    expect(turn(mine.url)).toMatchObject({ account: 'default', status: 'ok' });
    expect(turn(theirs.url)).toMatchObject({ account: 'token', status: 'ok' });
    const [a, b] = [turn(mine.url), turn(theirs.url)].map((t) => [Date.parse(t.at), Date.parse(t.at) + t.duration_ms]);
    expect(a[0] < b[1] && b[0] < a[1], 'the recorded turns overlap').toBe(true);
    console.log('monitor:', JSON.stringify([turn(mine.url), turn(theirs.url)].map((t) => ({ room: t.room.replace(/^https:\/\/[^/]+/, ''), account: t.account, model: t.model, at: t.at, duration_ms: t.duration_ms, queue_ms: t.queue_ms, status: t.status }))));
    const requests = messagesRequests({ log });
    console.log(`real Claude requests: ${requests}`);
    expect(requests).toBeLessThanOrEqual(6);
    expect(logLines({ log }).filter((l) => l.msg === 'upstream_response').map((l) => l.account).sort()).toEqual(expect.arrayContaining(['default', 'token']));
  } finally {
    // The token account's secret sits in the OS keyring, outside the state dir.
    if (api) await api.post('/api/accounts/remove', { name: 'token' }).catch((e) => console.log('account removal:', e.message));
    if (proc) {
      proc.kill('SIGTERM');
      await exited;
    }
    for (const r of rooms) {
      const me = await r.page.request.get(`${r.base}/api/profile`).then((x) => x.json()).catch(() => null);
      expect((await request.delete(`${r.base}/api/rooms/${r.room.id}`, { headers: admin })).status()).toBe(204);
      expect(me?.profile?.id, 'a profile to delete').toBeTruthy();
      expect((await request.delete(`${r.base}/api/profiles/${me.profile.id}`, { headers: admin })).status()).toBe(204);
    }
  }
});
