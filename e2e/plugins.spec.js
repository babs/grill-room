import { test, expect } from './fixtures.js';
import { readFileSync, mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { gunzipSync } from 'node:zlib';
import { join, resolve } from 'node:path';
import { roomWith, startDaemon, pairDaemon, ask, skillZip, appointAdmin, agentState, personaSheet, closePersonas, approveEverything } from './helpers.js';
import { gitPort, adminToken } from './playwright.config.js';

// The plugin the Phase 2 DoD names: its plugin.json and its spec-feature skill.
const files = {
  '.claude-plugin/plugin.json': '{"name":"babs","version":"1.0.0","description":"e2e fixture","author":{"name":"e2e"}}\n',
  'skills/spec-feature/SKILL.md': '---\nname: spec-feature\ndescription: Write a spec.\n---\nWrite specs/NNN-slug.md.\n',
};
const sha = (s) => createHash('sha256').update(s).digest('hex');
const skillsDir = '/home/agent/.grill-room/plugin';
const pluginDir = '/home/agent/.grill-room/plugins/babs';

// publishRepo commits files to a new repo and publishes it bare where the e2e git server
// (tools/e2egit) serves it, and returns its link: the e2e server build reaches example.com there.
function publishRepo(name) {
  const work = mkdtempSync(join('test-results', 'repo-'));
  for (const [p, c] of Object.entries(files)) {
    mkdirSync(join(work, p, '..'), { recursive: true });
    writeFileSync(join(work, p), c);
  }
  const env = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_AUTHOR_NAME: 'e2e',
    GIT_AUTHOR_EMAIL: 'e2e@example.invalid', GIT_COMMITTER_NAME: 'e2e', GIT_COMMITTER_EMAIL: 'e2e@example.invalid' };
  const git = (...args) => execFileSync('git', args, { cwd: work, env, stdio: 'pipe' });
  git('init', '-q', '-b', 'main');
  git('add', '-A');
  git('commit', '-q', '-m', 'fixture');
  execFileSync('git', ['clone', '-q', '--bare', work, resolve('test-results/git', name + '.git')], { env, stdio: 'pipe' });
  return `https://example.com:${gitPort}/${name}.git`;
}

// call sends a JSON request with page's session and returns its status and body.
const call = (page, method, path, body) => page.evaluate(async ([m, p, b]) => {
  const r = await fetch(p, { method: m, headers: { 'Content-Type': 'application/json' }, body: b === null ? undefined : JSON.stringify(b) });
  return { status: r.status, body: await r.json().catch(() => null) };
}, [method, path, body ?? null]);

// dirs asks Grill for its --plugin-dir flags (fake claude, FAKE_PLUGINS) and returns them, each
// with its file list.
async function dirs(page, roomID, tag) {
  const out = (await ask(page, roomID, `FAKE_PLUGINS ${tag}`)).trim();
  expect(out).toMatch(/^Plugin dirs: .*\.$/);
  return out.slice('Plugin dirs: '.length, -1).split(' | ');
}

// 009-AC13 009-AC17 009-AC18: a room admin adds the fixture plugin for Grill through the API (the
// add form is slice C's), from a repo on the local git server. The host's daemon inspects it in a
// throwaway container from the sandbox image; with "approve everything" on it waits for the host,
// and Grill's turns get the skills' --plugin-dir only. Once the host approves it, a turn gets two,
// grill-room's then the plugin's, which holds the repo's plugin.json and spec-feature skill at
// their hashes. Turned off, the next turn gets one again and the sandbox keeps no plugin folder.
// Real daemon and Docker sandbox, fake claude.
test('plugins: inspected, approved, loaded after the skills', async ({ browser, request, baseURL }) => {
  test.setTimeout(240_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const grill = (await agentState(alice, room.id)).personas[0].id;
  // A skill on Grill gives the turn grill-room's --plugin-dir, the one the plugins come after.
  const zip = [...readFileSync(skillZip('stamp-' + Date.now().toString(36), 'STAMP'))];
  const skill = await alice.evaluate(async (bytes) => {
    const r = await fetch('/api/profile/skills', { method: 'POST', headers: { 'Content-Type': 'application/zip' }, body: new Uint8Array(bytes) });
    return r.json();
  }, zip);
  expect((await call(alice, 'PUT', `/api/rooms/${room.id}/personas/${grill}/skills/${skill.id}`, { accept_disclaimer: true })).status).toBe(200);

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    await approveEverything(admin);

    expect(await dirs(alice, room.id, 'before')).toEqual([expect.stringMatching(new RegExp(`^${skillsDir} \\[`))]);

    const added = await call(alice, 'POST', `/api/rooms/${room.id}/plugins`, { url: publishRepo('skills-' + Date.now().toString(36)), ref: 'main', personas: [grill] });
    expect(added.status, JSON.stringify(added.body)).toBe(201);
    const babs = async () => (await call(alice, 'GET', `/api/rooms/${room.id}/plugins`)).body.plugins.find((p) => p.name === 'babs');
    await expect.poll(async () => (await babs()).versions[0].state, { timeout: 90_000 }).toBe('pending');
    const pending = await dirs(alice, room.id, 'pending');
    expect(pending).toHaveLength(1);
    expect(pending[0]).toMatch(new RegExp(`^${skillsDir} \\[`));

    const plugin = await babs();
    expect((await call(alice, 'POST', `/api/rooms/${room.id}/plugins/${plugin.id}/versions/1/decision`, { approve: true })).status).toBe(200);
    const loaded = await dirs(alice, room.id, 'approved');
    expect(loaded).toHaveLength(2);
    expect(loaded[0]).toMatch(new RegExp(`^${skillsDir} \\[`));
    expect(loaded[1]).toMatch(new RegExp(`^${pluginDir} \\[`));
    for (const [p, c] of Object.entries(files)) expect(loaded[1]).toContain(`${p}=${sha(c)}`);

    expect((await call(alice, 'DELETE', `/api/rooms/${room.id}/personas/${grill}/plugins/${plugin.id}`)).status).toBe(204);
    expect(await dirs(alice, room.id, 'off')).toEqual([expect.stringMatching(new RegExp(`^${skillsDir} \\[`))]);
    const left = execFileSync('docker', ['exec', 'grill-room-' + daemon.sandbox, 'sh', '-c', 'ls -A /home/agent/.grill-room/plugins 2>&1 || true'], { encoding: 'utf8' });
    expect(left).toContain('No such file or directory');
  } finally {
    daemon.cleanup();
  }
});

// SHOTS=<dir> keeps a screenshot of the daemon page's plugin screens, as plugins-ui.spec.js does.
const shot = (page, name) => (process.env.SHOTS ? page.screenshot({ path: `${process.env.SHOTS}/${name}.png`, fullPage: true }) : null);

// bareRepo commits files to a new repo and returns the absolute path of its bare clone, which the
// daemon's git reaches through a fake ssh: no server, no key.
function bareRepo(files) {
  const work = mkdtempSync(join('test-results', 'private-'));
  for (const [p, c] of Object.entries(files)) {
    mkdirSync(join(work, p, '..'), { recursive: true });
    writeFileSync(join(work, p), c);
  }
  const env = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_AUTHOR_NAME: 'e2e',
    GIT_AUTHOR_EMAIL: 'e2e@example.invalid', GIT_COMMITTER_NAME: 'e2e', GIT_COMMITTER_EMAIL: 'e2e@example.invalid' };
  const git = (...args) => execFileSync('git', args, { cwd: work, env, stdio: 'pipe' });
  git('init', '-q', '-b', 'main');
  git('add', '-A');
  git('commit', '-q', '-m', 'private');
  const bare = resolve(work + '.git');
  execFileSync('git', ['clone', '-q', '--bare', work, bare], { env, stdio: 'pipe' });
  return bare;
}

// 009-AC19 009-AC20 009-AC22 009-AC23 009-AC24 009-AC25, Phase 3 DoD: a room admin asks the AI
// host to fetch a private ssh repo; the host's daemon page lists the request and the host's git
// (a fake ssh running git-upload-pack on this machine) fetches it only on Fetch; the room lists the
// plugin private and the daemon inspects it. Its required sensitive option unset, a turn runs
// without it and the persona's Capabilities tab and the daemon page say it needs host setup. Set
// on the daemon page, behind the warning, a SessionStart hook gets CLAUDE_PLUGIN_OPTION_API_KEY
// and the value it printed reaches the room as [redacted]. The page adds one MCP website to the
// allowlist and refuses an IP. Real daemon and Docker sandbox, fake claude.
test('plugins: private repo through the host, options, allowlist', async ({ browser, request, baseURL }) => {
  test.setTimeout(300_000);
  const secret = 'e2e-s3cret-value';
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const hook = 'echo len=${#CLAUDE_PLUGIN_OPTION_API_KEY} key=$CLAUDE_PLUGIN_OPTION_API_KEY';
  const bare = bareRepo({
    '.claude-plugin/plugin.json': JSON.stringify({ name: 'opt', userConfig: { api_key: { type: 'string', title: 'API key', description: 'The service key.', sensitive: true, required: true } } }),
    'hooks/hooks.json': JSON.stringify({ hooks: { SessionStart: [{ hooks: [{ type: 'command', command: hook }] }] } }),
    '.mcp.json': JSON.stringify({ mcpServers: { docs: { type: 'http', url: 'https://mcp.example.com/mcp' }, lan: { type: 'http', url: 'http://10.1.2.3:8080/mcp' } } }),
  });
  const ssh = resolve(mkdtempSync(join('test-results', 'ssh-')), 'ssh.sh');
  writeFileSync(ssh, '#!/bin/sh\nfor a; do last=$a; done\nexec sh -c "$last"\n');
  const url = 'git@example.invalid:' + bare;
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GIT_SSH_COMMAND: 'sh ' + ssh, GIT_SSH_VARIANT: 'simple', GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' });
  try {
    const admin = await pairDaemon(alice, daemon);

    await alice.goto(`/r/${room.id}#manage/plugins`);
    const tab = alice.locator('#manage');
    await tab.locator('#plugin-add-open').click();
    const add = alice.locator('#plugin-add');
    await add.locator('#plugin-url').fill(url);
    await add.getByRole('button', { name: 'Check', exact: true }).click();
    await add.locator('#plugin-host-personas').getByLabel(/Grill$/).check();
    await add.getByRole('button', { name: 'Ask the AI host to fetch it' }).click();
    await expect(tab.locator('#plugin-fetches li .plugin-state')).toHaveText('waiting for the AI host to fetch it');

    await admin.locator('#tab-rooms').click();
    const req = admin.locator('.fetch-request');
    await expect(req).toContainText('Alice asks for a plugin');
    await expect(req.locator('.fetch-url')).toHaveText(url);
    await shot(admin, 'daemon-fetch-request');
    await req.getByRole('button', { name: 'Fetch ' + url }).click();
    const row = tab.locator('#plugin-list li[data-plugin="opt"]');
    await expect(row.locator('.badge')).toHaveText('private', { timeout: 60_000 });
    await expect(row.locator('.plugin-state')).toHaveText('v1 in use', { timeout: 90_000 });
    await expect(tab.locator('#plugin-fetches')).toHaveCount(0);
    await expect(admin.locator('.fetch-request')).toHaveCount(0);

    await alice.locator('#manage-close').click();
    expect((await ask(alice, room.id, 'FAKE_HOOKS unset')).trim()).toBe('Hooks: none.');
    let caps = (await personaSheet(alice, 'Grill', 'Capabilities')).locator('.persona-plugins');
    await expect(caps.locator('li[data-plugin="opt"]')).toContainText("needs host setup on the AI host's computer");
    await expect(caps.locator('.plugin-total')).toHaveText('None of them loads for Grill.');
    await closePersonas(alice);

    const setup = admin.locator('.plugin-setup[data-plugin="opt"]');
    await expect(setup.locator('.needs-setup')).toHaveText('Needs host setup: it does not load until its required options are set.');
    await expect(setup.locator('.warn-line')).toHaveText('The AI can read these values during a turn. Anyone in the room can ask it to show them.');
    await expect(setup).toContainText('The service key.');
    const key = setup.getByLabel('API key (required)');
    await expect(key).toHaveAttribute('type', 'password');
    await expect(key).toHaveAttribute('placeholder', 'not set');
    await shot(admin, 'daemon-plugin-setup');
    await key.fill(secret);
    await setup.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(setup.locator('.needs-setup')).toHaveCount(0);
    await expect(key).toHaveValue('');
    await expect(key).toHaveAttribute('placeholder', 'set: type to replace it');

    expect((await ask(alice, room.id, 'FAKE_HOOKS set')).trim()).toBe(`Hooks: len=${secret.length} key=[redacted].`);
    caps = (await personaSheet(alice, 'Grill', 'Capabilities')).locator('.persona-plugins');
    await expect(caps.locator('li[data-plugin="opt"]')).not.toContainText('needs host setup');
    await closePersonas(alice);

    const domains = setup.locator('.plugin-domains li');
    await expect(domains).toHaveText(['10.1.2.3 not on the allowlist Add to allowlist', 'mcp.example.com not on the allowlist Add to allowlist']);
    await setup.getByRole('button', { name: 'Add mcp.example.com to the allowlist' }).click();
    await expect(admin.locator('.toast', { hasText: 'mcp.example.com added to the allowlist' })).toBeVisible();
    await expect(domains.nth(1)).toHaveText('mcp.example.com on the allowlist');
    await setup.getByRole('button', { name: 'Add 10.1.2.3 to the allowlist' }).click();
    await expect(setup).toContainText('"10.1.2.3" is not a domain name');
    await shot(admin, 'daemon-plugin-allowlist');
    await admin.locator('#tab-network').click();
    await expect(admin.locator('#allowlist')).toContainText('mcp.example.com');
    await expect(admin.locator('#allowlist')).not.toContainText('10.1.2.3');

    // 009-AC26 009-AC27, Phase 4 DoD: the Export tab names the private plugin; the archive holds
    // its snapshot and the session of the turn whose hook printed the option, but not the value set
    // on the daemon page; another import lands the plugin waiting for a host.
    await alice.goto(`/r/${room.id}#manage/export`);
    await expect(alice.locator('#export-private')).toHaveText('This plugin comes from a private repo: opt. The archive holds its files, which whoever holds it can read. Options and secrets stay on the AI host.');
    await shot(alice, 'export-private-plugin');
    const auth = { Authorization: `Bearer ${adminToken}` };
    const res = await request.get(`/api/rooms/${room.id}/export`, { headers: auth });
    expect(res.status()).toBe(200);
    const archive = await res.body();
    const raw = gunzipSync(archive);
    expect(raw.includes('"name":"opt"')).toBe(true);
    expect(raw.includes('[redacted]')).toBe(true);
    expect(raw.includes(secret)).toBe(false);
    const imp = await request.post('/api/rooms/import', { headers: { ...auth, 'Content-Type': 'application/gzip' }, data: archive });
    expect(imp.status()).toBe(201);
    const rep = await imp.json();
    expect(rep.counts.plugins).toBe(1);
    expect(rep.warnings).toContain("Plugins: 1. None loads until this instance's AI host checks and approves it.");
  } finally {
    daemon.cleanup();
  }
});
