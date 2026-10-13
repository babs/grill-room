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
    const ctx = await browser.newContext();
    // The annotations panel starts closed below 100rem (005-AC37); the specs read it open.
    await ctx.addInitScript(() => { if (!localStorage.getItem('grill-room-annotations')) localStorage.setItem('grill-room-annotations', '1'); });
    const page = await ctx.newPage();
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
// claude image: no Claude request. env adds to the daemon's environment; expiresIn is the fake
// login's lifetime in milliseconds.
export function startDaemon(roomURL, env = {}, { expiresIn = 3600e3 } = {}) {
  const dir = mkdtempSync(join('test-results', 'daemon-'));
  const stateDir = join(dir, 'state');
  writeFileSync(join(dir, '.credentials.json'),
    JSON.stringify({ claudeAiOauth: { accessToken: 'e2e-fake', expiresAt: Date.now() + expiresIn } }), { mode: 0o600 });
  const daemonEnv = {
    ...process.env, CLAUDE_CONFIG_DIR: dir, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none',
    GRILL_ROOM_SANDBOX_IMAGE: process.env.GRILL_ROOM_SANDBOX_FAKE_IMAGE || 'grill-room-sandbox:fake', LOG_LEVEL: 'info', ...env,
  };
  const proc = spawn('../bin/grill-room-daemon', [roomURL], { env: daemonEnv, stdio: ['ignore', 'ignore', 'pipe'] });
  // handOver launches the daemon again with another room: it hands the room to the running
  // supervisor and exits 0 (003-AC6).
  const handOver = (url) => new Promise((resolve) => {
    spawn('../bin/grill-room-daemon', [url], { env: daemonEnv, stdio: 'ignore' }).on('exit', resolve);
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
  return { proc, link, exited, handOver, sandbox: sandboxName(stateDir, roomURL), cleanup: () => { proc.kill(); rmSync(dir, { recursive: true, force: true }); } };
}

// pairDaemon approves the daemon's pairing from the participant's room page, as a person would.
export async function pairDaemon(page, daemon) {
  const admin = await page.context().newPage();
  await admin.goto(await daemon.link);
  // The worker asks to pair once the Docker orphan cleanup is done, seconds on a loaded runner.
  await expect(admin.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
  const [approval] = await Promise.all([page.context().waitForEvent('page'), admin.getByRole('link', { name: 'approve in the room' }).click()]);
  await approval.getByRole('button', { name: 'Approve' }).click();
  await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.', { timeout: 30_000 });
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
// sandbox image and haiku, env added. Its log goes to a file: the proxy logs one upstream_response
// per request.
export function liveDaemon(dir, roomURL, env = {}) {
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const stateDir = join(dir, 'state');
  const log = join(dir, 'daemon.log');
  const proc = spawn('../bin/grill-room-daemon', [roomURL], {
    env: { ...process.env, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none', GRILL_ROOM_MAX_MODEL: 'haiku', LOG_LEVEL: 'info', ...env },
    stdio: ['ignore', 'ignore', openSync(log, 'w')],
  });
  const exited = new Promise((resolve) => proc.on('exit', resolve));
  const link = (async () => {
    for (let i = 0; i < 100; i++) {
      const m = readFileSync(log, 'utf8').match(/Admin page: (http:\/\/127\.0\.0\.1:\d+\/s\/\S+)/);
      if (m) return m[1];
      await new Promise((r) => setTimeout(r, 100)); // polls the log file, bounded by 100 rounds
    }
    throw new Error('no admin link in ' + log);
  })();
  return { proc, exited, link, log, sandbox: sandboxName(stateDir, roomURL) };
}

// latestLink is the newest admin page link a supervisor printed to log: each one opens one session.
export const latestLink = async (log) => {
  for (let i = 0; i < 100; i++) {
    const all = [...readFileSync(log, 'utf8').matchAll(/Admin page: (http:\/\/127\.0\.0\.1:\d+\/s\/\S+)/g)];
    if (all.length) return all.at(-1)[1];
    await new Promise((r) => setTimeout(r, 100)); // polls the log file, bounded by 100 rounds
  }
  throw new Error('no admin link in ' + log);
};

// adminAPI opens a session on a supervisor's page API, as the page does with its link.
export async function adminAPI(request, log) {
  const link = await latestLink(log);
  const origin = new URL(link).origin;
  const res = await request.post(`${origin}/api/session`, { headers: { Origin: origin }, data: { code: link.split('/s/')[1] } });
  expect(res.status()).toBe(200);
  const headers = { Origin: origin, Authorization: `Bearer ${(await res.json()).token}` };
  const call = async (method, path, data) => {
    const r = await request.fetch(origin + path, { method, headers, data });
    expect(r.ok(), `${method} ${path}: ${await r.text()}`).toBeTruthy();
    return r.status() === 204 ? null : r.json();
  };
  return { get: (p) => call('GET', p), post: (p, d) => call('POST', p, d) };
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

// skillZip writes a zip of a skill folder whose script prints out; without frontmatter it is one
// the room must refuse.
export function skillZip(name, out, frontmatter = true) {
  const zip = join(mkdtempSync(join('test-results', 'skill-')), name + '.zip');
  const md = frontmatter ? `---\nname: ${name}\ndescription: Prints ${out}.\n---\nRun scripts/stamp.sh.\n` : '# No frontmatter\n';
  execFileSync('python3', ['-I', '-c', `import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr(sys.argv[2] + "/SKILL.md", sys.argv[3])
    z.writestr(sys.argv[2] + "/scripts/stamp.sh", "echo " + sys.argv[4] + "\\n")`, zip, name, md, out]);
  return zip;
}

// signIn signs page in to /admin at view, and returns the login's status.
export async function signIn(page, token = adminToken, view = 'sessions') {
  await page.goto('/admin#' + view);
  await page.getByLabel('Admin token').fill(token);
  const [res] = await Promise.all([
    page.waitForResponse((r) => r.url().endsWith('/api/admin/login')),
    page.getByRole('button', { name: 'Sign in' }).click(),
  ]);
  return res.status();
}

// noOverflow lists what sticks out past the viewport, tables and tab bars scrolling in their own
// box aside (005-AC35).
export const noOverflow = (page) => page.evaluate(() => [...document.querySelectorAll('body *')]
  .filter((e) => !e.parentElement?.closest('.scroll, .tabs') && e.getBoundingClientRect().right > innerWidth + 1)
  .map((e) => `${e.tagName.toLowerCase()}#${e.id}.${e.className}`));

// menuItem picks an item of a header menu (005-AC32): '⋯' (more), the user's initials (me) or the
// audio options (voice).
export async function menuItem(page, name, which = 'more') {
  await page.locator({ more: '#more-open', me: '#me', voice: '#voice-more' }[which]).click();
  const menu = page.locator({ more: '#more-menu', me: '#me-menu', voice: '#voice-menu' }[which]);
  await menu.getByRole(name === 'My rooms and profile' || name === 'Open documents in a new window' ? 'link' : 'button', { name, exact: true }).click();
}

// messageAction runs Edit or Delete on a line through its actions button (005-AC35).
export async function messageAction(line, name) {
  await line.getByRole('button', { name: 'Line actions' }).click();
  await line.page().locator('#msg-menu').getByRole('button', { name, exact: true }).click();
}

// smallTargets lists the visible controls under min px wide or high (005-AC30). Inline targets in
// a message's text are exempt, as WCAG 2.5.8 exempts them; a checkbox counts by its label.
export const smallTargets = (page, min = 44) => page.evaluate((min) => {
  const sel = 'button, select, textarea, summary, .tabs a, a.btn, .menu a, input:not([type=checkbox]):not([type=radio]):not([type=hidden]):not([type=file]), label:has(> input[type=checkbox])';
  return [...document.querySelectorAll(sel)].filter((e) => {
    if (e.closest('.msg .body, .msg .meta, #doc-frame')) return false;
    const r = e.getBoundingClientRect();
    const s = getComputedStyle(e);
    if (!r.width || !r.height || s.visibility === 'hidden' || e.closest('[hidden], [inert]')) return false;
    // A small control may take its target from an absolute ::after around it (008-AC14).
    const hit = getComputedStyle(e, '::after');
    const grow = hit.content !== 'none' && hit.position === 'absolute' ? [-parseFloat(hit.left) - parseFloat(hit.right), -parseFloat(hit.top) - parseFloat(hit.bottom)] : [0, 0];
    return r.width + Math.max(0, grow[0]) < min - 0.5 || r.height + Math.max(0, grow[1]) < min - 0.5;
  }).map((e) => `${e.tagName.toLowerCase()}${e.id ? '#' + e.id : ''} "${(e.getAttribute('aria-label') || e.textContent).trim().slice(0, 30)}" ${Math.round(e.getBoundingClientRect().width)}x${Math.round(e.getBoundingClientRect().height)}`);
}, min);

// stopDaemon stops the supervisor from its page: Stop, then the confirm sheet's Stop (005-AC39).
export async function stopDaemon(admin) {
  await admin.locator('#stop').click();
  await admin.locator('dialog.confirm').getByRole('button', { name: 'Stop', exact: true }).click();
}

// confirmSheet answers the shared confirm sheet with its danger button (005-AC39).
export async function confirmSheet(page, name) {
  await page.locator('dialog.confirm').getByRole('button', { name, exact: true }).click();
}

// unfold opens a folded section of a persona's form: 'advanced' or 'more' (005-AC40).
export async function unfold(form, which) {
  const d = form.locator('details.' + which);
  if (!(await d.evaluate((e) => e.open))) await d.locator('summary').click();
}

// openStrip shows every item of the status strip (005-AC38): only the most urgent shows folded.
export async function openStrip(page) {
  const more = page.locator('#strip-more');
  if (await more.isVisible() && (await more.getAttribute('aria-expanded')) === 'false') await more.click();
}

// sandboxHas says whether the daemon's sandbox holds text in file, a path under /workspace: what
// a turn under way has written there, before the room sees any of it.
export function sandboxHas(daemon, file, text) {
  try {
    execFileSync('docker', ['exec', 'grill-room-' + daemon.sandbox, 'grep', '-q', text, '/workspace/' + file]);
    return true;
  } catch {
    return false;
  }
}

// docAction runs an item of the document's ⋯ menu (008-AC8).
export async function docAction(page, name) {
  await page.locator('#doc-more').click();
  await page.locator('#doc-menu').getByRole(name === 'Download' || name === 'Open in a new window' ? 'link' : 'button', { name, exact: true }).click();
}

// personMenu opens the ⋯ of name's row in People (008-AC13) and returns the menu.
export async function personMenu(page, name) {
  await page.locator('#participants li, #participants-away li').filter({ has: page.locator('.name', { hasText: name }) }).locator('.person-more').click();
  return page.locator('#person-menu');
}
