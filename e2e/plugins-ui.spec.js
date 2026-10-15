import { test, expect } from './fixtures.js';
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join, resolve } from 'node:path';
import { roomWith, appointAdmin, fakeDaemon, agentState, openStrip, personaSheet, closePersonas, confirmSheet, noOverflow, smallTargets } from './helpers.js';
import { gitPort } from './playwright.config.js';

const phone = { width: 390, height: 844 };
const desktop = { width: 1440, height: 900 };
// SHOTS=<dir> keeps a screenshot of each plugin screen, as design.spec.js does.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : null);

// A marketplace with two plugins in the repo and one from elsewhere (009-AC6). rich runs a hook
// and names an MCP server by URL, what the approval sheet must show (009-AC14).
const market = {
  '.claude-plugin/marketplace.json': JSON.stringify({ name: 'team-tools', owner: { name: 'e2e' }, plugins: [
    { name: 'rich', source: './plugins/rich', description: 'Hello and a docs server' },
    { name: 'lint', source: './plugins/lint' },
    { name: 'zero', source: './plugins/zero' },
    { name: 'remote', source: { source: 'github', repo: 'o/r' } }] }),
  'plugins/rich/.claude-plugin/plugin.json': '{"name":"rich","version":"0.1.0","description":"Says hello."}\n',
  'plugins/rich/skills/hello/SKILL.md': '---\nname: hello\ndescription: Say hello.\n---\nSay hello.\n',
  'plugins/rich/hooks/hooks.json': JSON.stringify({ hooks: { SessionStart: [{ hooks: [{ type: 'command', command: 'echo started >> "$HOME/log"' }] }] } }),
  'plugins/rich/.mcp.json': JSON.stringify({ mcpServers: { docs: { type: 'http', url: 'https://docs.example.com/mcp' } } }),
  'plugins/lint/.claude-plugin/plugin.json': '{"name":"lint"}\n',
  'plugins/lint/commands/fix.md': '---\ndescription: Fix.\n---\nFix it.\n',
  'plugins/zero/.claude-plugin/plugin.json': '{"name":"zero"}\n',
  'plugins/zero/commands/z.md': '---\ndescription: Z.\n---\nZ.\n',
};

const gitEnv = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_AUTHOR_NAME: 'e2e',
  GIT_AUTHOR_EMAIL: 'e2e@example.invalid', GIT_COMMITTER_NAME: 'e2e', GIT_COMMITTER_EMAIL: 'e2e@example.invalid' };

// newRepo commits files and publishes the repo bare where the e2e git server (tools/e2egit)
// serves it. commit(more) adds a commit and publishes it; both return the commit.
function newRepo(name, files) {
  const work = mkdtempSync(join('test-results', 'repo-'));
  const bare = resolve('test-results/git', name + '.git');
  const git = (...args) => execFileSync('git', args, { cwd: work, env: gitEnv, encoding: 'utf8' }).trim();
  const commit = (fs) => {
    for (const [p, c] of Object.entries(fs)) {
      mkdirSync(join(work, p, '..'), { recursive: true });
      writeFileSync(join(work, p), c);
    }
    git('add', '-A');
    git('commit', '-q', '-m', 'change');
    return git('rev-parse', 'HEAD');
  };
  git('init', '-q', '-b', 'main');
  const first = commit(files);
  execFileSync('git', ['clone', '-q', '--bare', work, bare], { env: gitEnv });
  return { url: `https://example.com:${gitPort}/${name}.git`, first, commit: (fs) => { const c = commit(fs); git('push', '-q', bare, 'main'); return c; } };
}

// call sends a JSON request with page's session and returns its status and body.
const call = (page, method, path, body) => page.evaluate(async ([m, p, b]) => {
  const r = await fetch(p, { method: m, headers: { 'Content-Type': 'application/json' }, body: b === null ? undefined : JSON.stringify(b) });
  return { status: r.status, body: await r.json().catch(() => null) };
}, [method, path, body ?? null]);

const toast = (page, text) => expect(page.locator('#toasts .toast', { hasText: text })).toBeVisible();

// fits checks the open screen at phone width: nothing past the viewport, every target 44 px.
async function fits(page, vp, what) {
  expect(await noOverflow(page), what).toEqual([]);
  if (vp === phone) expect(await smallTargets(page), what).toEqual([]);
}

for (const vp of [phone, desktop]) {
  // 009-AC5 009-AC6 009-AC7 009-AC10 009-AC11 009-AC12 009-AC14 009-AC15 009-AC16 009-AC18: a room
  // admin adds a plugin picked from a marketplace on the local git server for Grill; the host
  // (a fake AI host app, its inspection posted through the daemon API) reviews it in the approval
  // sheet, then an update with its change; the persona's Capabilities tab shows each plugin's
  // state and cost and their total; a member reads all of it and gets a 403 on every write.
  test(`plugins: add, approve, update, remove at ${vp.width} px`, async ({ browser, request, baseURL }) => {
    test.setTimeout(120_000);
    const tag = vp.width + '-' + Date.now().toString(36);
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await appointAdmin(alice, request, room.id);
    const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop', { approve_all: true });
    const base = `/api/rooms/${room.id}/plugins`;
    const versionOf = async (name, n) => (await call(alice, 'GET', base)).body.plugins.find((p) => p.name === name).versions.find((v) => v.n === n);
    const report = async (name, n, body) => {
      const v = await versionOf(name, n);
      const r = await fetch(`${baseURL}/api/rooms/${room.id}/daemon/plugins/${v.id}/report`, { method: 'POST',
        headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      expect(r.status).toBe(200);
    };
    const inspected = { ok: true, validate: { success: true, manifest: { warnings: [{ path: 'author', message: 'No author information provided.' }] } },
      details: { name: 'rich', always_on_tokens: 1513, cost_known: true } };
    try {
      for (const p of [alice, bob]) await p.setViewportSize(vp);
      const repo = newRepo('team-' + tag, market);
      const short1 = repo.first.slice(0, 7);
      const grill = (await agentState(alice, room.id)).personas[0].id;

      // Room settings › Plugins, empty.
      await alice.goto(`/r/${room.id}#manage/plugins`);
      const tab = alice.locator('#manage');
      await expect(tab.locator('#plugins-empty')).toHaveText('No plugins yet. Add one from a git repository.');

      // 009-AC7: a link with credentials is refused in those words.
      await tab.locator('#plugin-add-open').click();
      const add = alice.locator('#plugin-add');
      await add.locator('#plugin-url').fill(`https://u:secret@example.com:${gitPort}/x.git`);
      await add.getByRole('button', { name: 'Check', exact: true }).click();
      await expect(add.getByRole('alert')).toHaveText('Never put credentials in a link. For a private repo, the AI host fetches it.');

      // 009-AC6 009-AC10: the marketplace's entries, none checked, remote disabled with why; the
      // personas, none checked; Add waits for a pick.
      await add.locator('#plugin-url').fill(repo.url);
      await expect(add.locator('#plugin-ref')).toHaveAttribute('placeholder', 'branch, tag or commit; empty = default branch');
      await add.getByRole('button', { name: 'Check', exact: true }).click();
      await expect(add.locator('#plugin-found')).toHaveText(`Found main @ ${short1}, the marketplace team-tools.`);
      const entries = add.locator('#plugin-entries');
      await expect(entries.getByLabel('rich', { exact: true })).not.toBeChecked();
      await expect(entries.getByLabel('lint', { exact: true })).not.toBeChecked();
      await expect(entries.getByLabel('remote', { exact: true })).toBeDisabled();
      await expect(entries.getByLabel('remote', { exact: true })).toHaveAccessibleDescription('Only plugins stored in this repo can be added.');
      await expect(add.locator('#plugin-personas').getByLabel(/Grill$/)).not.toBeChecked();
      await expect(add.locator('#plugin-add-submit')).toBeDisabled();
      await entries.getByLabel('rich', { exact: true }).check();
      await add.locator('#plugin-personas').getByLabel(/Grill$/).check();
      await fits(alice, vp, 'add sheet');
      await shot(alice, `plugin-add-${vp.width}`);
      await add.locator('#plugin-add-submit').click();
      await toast(alice, 'Added rich. The AI host checks it next.');
      await expect(add).toBeHidden();

      // 009-AC5: "<name>, <ref> @ <short commit>"; inspecting while the host checks it.
      const row = tab.locator('#plugin-list li[data-plugin="rich"]');
      await expect(row.locator('.plugin-name')).toHaveText(`rich, main @ ${short1}`);
      await expect(row.locator('.plugin-state')).toHaveText('v1: the AI host is checking it');
      await expect(row).toContainText('On for Grill');

      // 009-AC14: inspected under "approve everything", it waits for the host: the strip opens
      // the approval sheet, with the hook's command, the MCP URL and domain, the warning, the cost.
      await report('rich', 1, inspected);
      await expect(row.locator('.plugin-state')).toHaveText("v1: waiting for the AI host's approval");
      await alice.locator('#manage-close').click();
      await openStrip(alice);
      await expect(alice.locator('#approval-list li', { hasText: 'plugin rich' })).toContainText(`Alice added the plugin rich, main @ ${short1}.`);
      await openStrip(bob);
      await expect(bob.locator('#approval-list li', { hasText: 'plugin rich' })).toContainText('The AI host decides.');
      await alice.getByRole('button', { name: 'Review the plugin rich v1' }).first().click();
      const review = alice.locator('#plugin-review');
      await expect(review.locator('#plugin-review-title')).toHaveText('Review rich v1');
      await expect(review.locator('#plugin-cost')).toHaveText('~1,513 tokens per answer');
      await expect(review.locator('#plugin-components')).toContainText('Skills (1): hello');
      await expect(review.locator('#plugin-components')).toContainText('Hooks (1): SessionStart');
      await expect(review.locator('#plugin-risky li')).toHaveText(['Hook SessionStartecho started >> "$HOME/log"', 'MCP server docshttps://docs.example.com/mcp']);
      await expect(review.locator('#plugin-risky code.risk')).toHaveCount(2);
      await expect(review.locator('#plugin-domains')).toHaveText('Websites its MCP servers name: docs.example.com.');
      await expect(review.locator('#plugin-warnings')).toHaveText('author: No author information provided.');
      await fits(alice, vp, 'approval sheet');
      await shot(alice, `plugin-review-${vp.width}`);
      await review.getByRole('button', { name: 'Approve', exact: true }).click();
      await toast(alice, 'Approved rich v1');

      // A member reads the list, gets no write action, and a 403 from every write route (009-AC5).
      await bob.goto(`/r/${room.id}#manage/plugins`);
      await expect(bob.locator('#manage #plugin-list li[data-plugin="rich"] .plugin-state')).toHaveText('v1 in use');
      await expect(bob.locator('#plugin-add-open, #manage button[aria-label^="More actions for the plugin"]')).toHaveCount(0);
      const rich = (await call(bob, 'GET', base)).body.plugins[0].id;
      for (const [m, p] of [['POST', base + '/check'], ['POST', base], ['POST', `${base}/${rich}/update`], ['DELETE', `${base}/${rich}`],
        ['PUT', `/api/rooms/${room.id}/personas/${grill}/plugins/${rich}`]]) {
        expect((await call(bob, m, p, { url: repo.url, entries: ['lint'] })).status, `${m} ${p}`).toBe(403);
      }

      // 009-AC16 009-AC18: Grill's Capabilities: rich loads with its cost; lint, refused by Claude
      // Code, says so and counts for nothing in the total.
      expect((await call(alice, 'POST', base, { url: repo.url, entries: ['lint'], personas: [grill] })).status).toBe(201);
      await alice.goto(`/r/${room.id}`);
      const caps = (await personaSheet(alice, 'Grill', 'Capabilities')).locator('.persona-plugins');
      await expect(caps.locator('li[data-plugin="lint"]')).toContainText('the AI host is checking it');
      await report('lint', 1, { ok: false, error: 'commands/fix.md: bad frontmatter' });
      await expect(caps.locator('li[data-plugin="rich"] input')).toBeChecked();
      await expect(caps.locator('li[data-plugin="rich"]')).toContainText(`0.1.0, main @ ${short1} · ~1,513 tokens per answer`);
      await expect(caps.locator('li[data-plugin="lint"]')).toContainText('refused by Claude Code: commands/fix.md: bad frontmatter');
      await expect(caps.locator('.plugin-total')).toHaveText('Total: ~1,513 tokens per answer.');
      await fits(alice, vp, 'capabilities');
      await shot(alice, `plugin-capabilities-${vp.width}`);
      await caps.getByLabel('lint for Grill').uncheck();
      await expect(alice.locator('#persona-status')).toHaveText('lint is off for Grill from its next answer.');
      await expect(caps.getByLabel('lint for Grill')).not.toBeChecked();
      await expect(caps.getByLabel('lint for Grill')).toBeFocused();
      // A plugin that loads at no cost still counts as loaded: the total says ~0, not "none".
      expect((await call(alice, 'POST', base, { url: repo.url, entries: ['zero'], personas: [grill] })).status).toBe(201);
      await report('zero', 1, { ok: true, details: { name: 'zero', always_on_tokens: 0, cost_known: true } });
      const zero = (await call(alice, 'GET', base)).body.plugins.find((p) => p.name === 'zero').id;
      expect((await call(alice, 'POST', `${base}/${zero}/versions/1/decision`, { approve: true })).status).toBe(200);
      await expect(caps.locator('li[data-plugin="zero"]')).toContainText('~0 tokens per answer');
      await caps.getByLabel('rich for Grill').uncheck();
      await expect(caps.locator('.plugin-total')).toHaveText('Total: ~0 tokens per answer.');
      await caps.getByLabel('zero for Grill').uncheck();
      await expect(caps.locator('.plugin-total')).toHaveText('None of them loads for Grill.');
      await closePersonas(alice);
      await bob.goto(`/r/${room.id}`);
      const bobCaps = (await personaSheet(bob, 'Grill', 'Capabilities')).locator('.persona-plugins');
      await expect(bobCaps.getByLabel('rich for Grill')).toBeDisabled();
      await expect(bobCaps).toContainText('Only room admins turn plugins on or off.');
      await closePersonas(bob);

      // 009-AC11 009-AC15: Update makes v2, its change against v1 in the sheet, then "Up to date".
      const short2 = repo.commit({ 'plugins/rich/skills/hello/SKILL.md': '---\nname: hello\ndescription: Say hello.\n---\nSay hello twice.\n',
        'plugins/rich/skills/bye/SKILL.md': '---\nname: bye\ndescription: Say bye.\n---\nSay bye.\n' }).slice(0, 7);
      await alice.goto(`/r/${room.id}#manage/plugins`);
      const update = async () => {
        await tab.getByRole('button', { name: 'More actions for the plugin rich' }).click();
        await alice.locator('#plugin-menu-' + rich).getByRole('menuitem', { name: 'Update' }).click();
      };
      await update();
      await toast(alice, `rich: new version v2, main @ ${short2}`);
      await report('rich', 2, inspected);
      await expect(row.locator('.plugin-state')).toHaveText("v1 in use · v2: waiting for the AI host's approval");
      await row.getByRole('button', { name: 'Review the plugin rich v2' }).click();
      const change = review.locator('#plugin-change');
      await expect(change).toContainText('Components added (1): skill:bye');
      await expect(change).toContainText('Files added (1): skills/bye/SKILL.md');
      await expect(change).toContainText('Files changed (1): skills/hello/SKILL.md');
      await expect(review.locator('#plugin-compare')).toHaveText(`Commits: v1 ${short1}, v2 ${short2}.`);
      await shot(alice, `plugin-review-update-${vp.width}`);
      // The row's Review goes with the decision, and the focus lands on the list once the list
      // says so. The new list is held until the sheet has closed, as a slow network delivers it.
      let release;
      const held = new Promise((r) => { release = r; });
      const list = `**/api/rooms/${room.id}/plugins`;
      await alice.route(list, async (route) => {
        if (route.request().method() === 'GET') await held;
        await route.continue();
      });
      await review.getByRole('button', { name: 'Approve', exact: true }).click();
      await expect(review).toBeHidden();
      release();
      await expect(row.locator('.plugin-state')).toHaveText('v2 in use');
      await alice.unroute(list);
      await expect(tab.locator('#plugin-list')).toBeFocused();
      await update();
      await toast(alice, 'Up to date');
      await fits(alice, vp, 'plugins tab');
      await shot(alice, `plugins-tab-${vp.width}`);

      // 009-AC12: Remove, after the confirm sheet.
      await tab.getByRole('button', { name: 'More actions for the plugin rich' }).click();
      await alice.locator('#plugin-menu-' + rich).getByRole('menuitem', { name: 'Remove' }).click();
      await confirmSheet(alice, 'Remove');
      await toast(alice, 'Removed rich');
      await expect(row).toHaveCount(0);
      await expect(tab.locator('#plugin-list li')).toHaveCount(2);
    } finally {
      host();
    }
  });
}

// 009-AC14 009-AC16: what a repo writes reaches the page as text. A plugin whose description,
// skill folder, hook command, MCP server and URL hold markup shows it verbatim in the list, the
// strip, the approval sheet and the Capabilities tab, adds no element and runs nothing.
test('plugins: hostile plugin content renders as text', async ({ browser, request, baseURL }) => {
  test.setTimeout(60_000);
  const evil = '<img src=x onerror="window.pwned=1">';
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop', { approve_all: true });
  try {
    const repo = newRepo('evil-' + Date.now().toString(36), {
      '.claude-plugin/plugin.json': JSON.stringify({ name: 'evil', description: evil }),
      [`skills/${evil}/SKILL.md`]: '---\nname: x\ndescription: x\n---\nx\n',
      'hooks/hooks.json': JSON.stringify({ hooks: { SessionStart: [{ hooks: [{ type: 'command', command: evil }] }] } }),
      '.mcp.json': JSON.stringify({ mcpServers: { [evil]: { type: 'http', url: 'javascript:window.pwned=1//' + evil } } }),
    });
    const grill = (await agentState(alice, room.id)).personas[0].id;
    const base = `/api/rooms/${room.id}/plugins`;
    const added = await call(alice, 'POST', base, { url: repo.url, personas: [grill] });
    expect(added.status, JSON.stringify(added.body)).toBe(201);
    const v = added.body.plugins[0].versions[0];
    const r = await fetch(`${baseURL}/api/rooms/${room.id}/daemon/plugins/${v.id}/report`, { method: 'POST',
      headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' },
      body: JSON.stringify({ ok: true, validate: { manifest: { warnings: [{ path: 'x', message: evil }] } } }) });
    expect(r.status).toBe(200);

    const noMarkup = async (where) => {
      expect(await alice.locator(`${where} img`).count(), where).toBe(0);
      expect(await alice.evaluate(() => window.pwned), where).toBeUndefined();
    };
    await alice.goto(`/r/${room.id}#manage/plugins`);
    const row = alice.locator('#plugin-list li[data-plugin="evil"]');
    await expect(row.locator('.plugin-name')).toContainText('evil, main @ ');
    await noMarkup('#plugin-list');
    await row.getByRole('button', { name: 'Review the plugin evil v1' }).click();
    const review = alice.locator('#plugin-review');
    await expect(review).toContainText(evil);
    await expect(review.locator('#plugin-risky code.risk').first()).toHaveText(evil);
    await expect(review.locator('#plugin-risky')).toContainText('javascript:window.pwned=1//' + evil);
    await expect(review.locator('#plugin-warnings')).toHaveText('x: ' + evil);
    await expect(review.locator('a[href^="javascript:"]')).toHaveCount(0);
    await noMarkup('#plugin-review');
    await review.getByRole('button', { name: 'Approve', exact: true }).click();
    await toast(alice, 'Approved evil v1');
    await alice.goto(`/r/${room.id}`);
    // The strip lists it while pending: check it on a second version.
    repo.commit({ 'README.md': evil });
    const up = await call(alice, 'POST', `${base}/${added.body.plugins[0].id}/update`);
    expect(up.status, JSON.stringify(up.body)).toBe(201);
    const r2 = await fetch(`${baseURL}/api/rooms/${room.id}/daemon/plugins/${up.body.versions[1].id}/report`, { method: 'POST',
      headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' }, body: JSON.stringify({ ok: true }) });
    expect(r2.status).toBe(200);
    await openStrip(alice);
    await expect(alice.locator('#approval-list li', { hasText: 'plugin evil' })).toContainText('updated the plugin evil, main @ ');
    await noMarkup('#approval-list');
    const caps = (await personaSheet(alice, 'Grill', 'Capabilities')).locator('.persona-plugins');
    await expect(caps.locator('li[data-plugin="evil"]')).toContainText('main @ ');
    await noMarkup('.persona-plugins');
  } finally {
    host();
  }
});

// 009-AC19 009-AC21 009-AC25: an ssh link the server cannot fetch offers "Ask the AI host to fetch
// it"; the request waits in Room settings › Plugins, reaches the host's daemon (a fake AI host app
// reading the daemon API), and a host whose git cannot reach the repo fails it in those words; a
// room admin dismisses it. The approval sheet marks the MCP websites off the host's allowlist.
test('plugins: ask the AI host, and the allowlist marks', async ({ browser, request, baseURL }) => {
  test.setTimeout(60_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop', { approve_all: true });
  const daemonAPI = (path, init = {}) => fetch(`${baseURL}/api/rooms/${room.id}/daemon/${path}`,
    { ...init, headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' } });
  try {
    await alice.goto(`/r/${room.id}#manage/plugins`);
    const tab = alice.locator('#manage');
    await tab.locator('#plugin-add-open').click();
    const add = alice.locator('#plugin-add');
    await add.locator('#plugin-url').fill('git@example.com:team/private.git');
    await add.getByRole('button', { name: 'Check', exact: true }).click();
    await expect(add.getByRole('alert')).toHaveText('The server fetches https links only.');
    await expect(add.locator('#plugin-host')).toContainText('Nothing is fetched until they press Fetch.');
    await add.locator('#plugin-host-personas').getByLabel(/Grill$/).check();
    await shot(alice, 'plugin-ask-host');
    await add.getByRole('button', { name: 'Ask the AI host to fetch it' }).click();
    await toast(alice, 'Asked the AI host.');
    await expect(add).toBeHidden();
    const req = tab.locator('#plugin-fetches li');
    await expect(req.locator('.plugin-name')).toHaveText('git@example.com:team/private.git');
    await expect(req.locator('.plugin-state')).toHaveText('waiting for the AI host to fetch it');

    const listed = await (await daemonAPI('plugins')).json();
    expect(listed.fetches.map((f) => [f.url, f.requested_by])).toEqual([['git@example.com:team/private.git', 'Alice']]);
    const failed = await daemonAPI(`plugin-fetches/${listed.fetches[0].id}`, { method: 'POST', body: JSON.stringify({ error: 'This AI host cannot reach the repo' }) });
    expect(failed.status).toBe(204);
    await expect(req.locator('.plugin-state')).toHaveText('This AI host cannot reach the repo.');
    await shot(alice, 'plugin-fetch-failed');
    await req.getByRole('button', { name: 'Dismiss the request for git@example.com:team/private.git' }).click();
    await expect(req).toHaveCount(0);

    // 009-AC25: the inspection lists the domain off the host's allowlist; the sheet marks it.
    const repo = newRepo('allow-' + Date.now().toString(36), {
      '.claude-plugin/plugin.json': '{"name":"docs"}\n',
      '.mcp.json': JSON.stringify({ mcpServers: { docs: { type: 'http', url: 'https://docs.example.com/mcp' }, wiki: { type: 'http', url: 'https://wiki.example.org/mcp' } } }),
    });
    const added = await call(alice, 'POST', `/api/rooms/${room.id}/plugins`, { url: repo.url });
    expect(added.status, JSON.stringify(added.body)).toBe(201);
    const v = added.body.plugins[0].versions[0];
    expect((await daemonAPI(`plugins/${v.id}/report`, { method: 'POST', body: JSON.stringify({ ok: true, allowlist_missing: ['docs.example.com'] }) })).status).toBe(200);
    const row = tab.locator('#plugin-list li[data-plugin="docs"]');
    await row.getByRole('button', { name: 'Review the plugin docs v1' }).click();
    const review = alice.locator('#plugin-review');
    await expect(review.locator('#plugin-domains')).toHaveText("Websites its MCP servers name: docs.example.com not on the AI host's allowlist, wiki.example.org.");
    await expect(review.locator('#plugin-domains .off-allowlist')).toHaveCount(1);
    await expect(review).toContainText("the host adds these from their app's Rooms tab");
    await shot(alice, 'plugin-review-allowlist');
  } finally {
    host();
  }
});

// 009-AC28, Phase 4 DoD: "Save as starter" lists the room's plugin at the version in use,
// unchecked, as documents are; checked, it goes in with Grill's activation. A room created from
// the starter has it on Grill, waiting for the AI host to check it; checked by a host that
// approves everything, it waits for that host's approval (006-AC13).
test('plugins: saved in a starter, pending in a room made from it', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const tag = Date.now().toString(36);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Sam' + tag]);
  await appointAdmin(alice, request, room.id);
  /** report posts the fake host's inspection of plugin name's first version in roomID. */
  const report = async (host, roomID, name) => {
    const v = (await call(alice, 'GET', `/api/rooms/${roomID}/plugins`)).body.plugins.find((p) => p.name === name).versions[0];
    const r = await fetch(`${baseURL}/api/rooms/${roomID}/daemon/plugins/${v.id}/report`, { method: 'POST',
      headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' }, body: JSON.stringify({ ok: true }) });
    expect(r.status).toBe(200);
  };
  const hosts = [await fakeDaemon(alice, baseURL, room.id, 'Sam laptop')];
  try {
    const repo = newRepo('starter-' + tag, market);
    const label = `lint, main @ ${repo.first.slice(0, 7)}`;
    const grill = (await agentState(alice, room.id)).personas[0].id;
    expect((await call(alice, 'POST', `/api/rooms/${room.id}/plugins`, { url: repo.url, entries: ['lint'], personas: [grill] })).status).toBe(201);
    await report(hosts[0], room.id, 'lint');

    await alice.goto(`/r/${room.id}#manage/general`);
    await alice.locator('#save-starter').click();
    const sheet = alice.locator('#starter-save-sheet');
    const box = sheet.locator('#starter-plugins label', { hasText: label });
    await expect(box.locator('input')).not.toBeChecked();
    await sheet.getByLabel('Title').fill('Plugins ' + tag);
    await box.click();
    await shot(alice, 'starter-save-plugins');
    await sheet.getByRole('button', { name: 'Save as starter' }).click();
    await expect(sheet).toBeHidden();

    await alice.goto('/profile');
    await alice.getByRole('button', { name: 'New room' }).click();
    await alice.getByLabel('Search starters').fill(tag);
    await alice.getByRole('button', { name: 'Yours: Plugins ' + tag }).click();
    await expect(alice.locator('#preview-plugins li')).toHaveText([label]);
    await alice.getByRole('button', { name: 'Use this starter' }).click();
    await expect(alice.locator('#starter-plugins input')).toBeChecked();
    await alice.getByRole('button', { name: 'Create room' }).click();
    await expect(alice.locator('#invites')).toBeVisible();
    const copy = new URL(alice.url()).pathname.split('/')[2];
    expect(copy).not.toBe(room.id);
    await alice.goto(`/r/${copy}#manage/plugins`);
    const row = alice.locator('#manage #plugin-list li[data-plugin="lint"]');
    await expect(row.locator('.plugin-name')).toHaveText(label);
    await expect(row).toContainText('On for Grill');
    await expect(row.locator('.plugin-state')).toHaveText('v1: waiting for the AI host to check it');
    hosts.push(await fakeDaemon(alice, baseURL, copy, 'Sam laptop', { approve_all: true }));
    await report(hosts[1], copy, 'lint');
    await expect(row.locator('.plugin-state')).toHaveText("v1: waiting for the AI host's approval");
  } finally {
    for (const h of hosts) h();
  }
});
