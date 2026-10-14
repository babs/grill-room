import { test, expect } from '@playwright/test';
import { readFileSync, mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { join, resolve } from 'node:path';
import { roomWith, startDaemon, pairDaemon, ask, skillZip, appointAdmin, agentState } from './helpers.js';
import { gitPort } from './playwright.config.js';

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
    await admin.locator('#tab-settings').click();
    await admin.getByLabel('Approve every new persona and persona change').check();
    await admin.locator('#settings-form').getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#settings-status')).toHaveText('Saved.');

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
