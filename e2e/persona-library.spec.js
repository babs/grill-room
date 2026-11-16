import { test, expect } from './fixtures.js';
import { readFileSync, mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { roomWith, personaSheet, agentState, uniq, appointAdmin, fakeDaemon, skillZip, patchPersona, personaRow, confirmSheet,
  noOverflow, smallTargets, openStrip, patchJSON, editPersona, openPersonas } from './helpers.js';
import { adminToken, gitPort } from './playwright.config.js';

// call sends a JSON request with page's session and returns its status and body.
const call = (page, method, path, body) => page.evaluate(async ([m, p, b]) => {
  const r = await fetch(p, { method: m, headers: { 'Content-Type': 'application/json' }, body: b === null ? undefined : JSON.stringify(b) });
  return { status: r.status, body: await r.json().catch(() => null) };
}, [method, path, body ?? null]);

// publishPlugin commits a one-plugin repo, babs, and publishes it bare where the e2e git server
// (tools/e2egit) serves it; it returns its link.
function publishPlugin(name) {
  const work = mkdtempSync(join(tmpdir(), 'grill-room-e2e-repo-'));
  mkdirSync(join(work, '.claude-plugin'));
  writeFileSync(join(work, '.claude-plugin/plugin.json'), '{"name":"babs","version":"1.0.0","description":"e2e fixture","author":{"name":"e2e"}}\n');
  mkdirSync(join(work, 'commands'));
  writeFileSync(join(work, 'commands/fix.md'), '---\ndescription: Fix.\n---\nFix it.\n');
  const env = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_AUTHOR_NAME: 'e2e',
    GIT_AUTHOR_EMAIL: 'e2e@example.invalid', GIT_COMMITTER_NAME: 'e2e', GIT_COMMITTER_EMAIL: 'e2e@example.invalid' };
  for (const args of [['init', '-q', '-b', 'main'], ['add', '-A'], ['commit', '-q', '-m', 'fixture']]) execFileSync('git', args, { cwd: work, env });
  execFileSync('git', ['clone', '-q', '--bare', work, resolve('test-results/git', name + '.git')], { env });
  rmSync(work, { recursive: true });
  return `https://example.com:${gitPort}/${name}.git`;
}

// report posts host's inspection of plugin name's first version in roomID: fine.
async function report(page, baseURL, host, roomID, name) {
  const v = (await call(page, 'GET', `/api/rooms/${roomID}/plugins`)).body.plugins.find((p) => p.name === name).versions[0];
  const r = await fetch(`${baseURL}/api/rooms/${roomID}/daemon/plugins/${v.id}/report`, { method: 'POST',
    headers: { Authorization: 'Bearer ' + host.token, 'Content-Type': 'application/json' }, body: JSON.stringify({ ok: true }) });
  expect(r.status).toBe(200);
}

// capture keeps the screen at a phone's and a desktop's width in the test's output (035 phase 3 DoD);
// at the phone's nothing sticks out and every target is 44 px.
async function capture(page, testInfo, name) {
  for (const [w, h] of [[390, 844], [1440, 900]]) {
    await page.setViewportSize({ width: w, height: h });
    if (w === 390) {
      expect(await noOverflow(page), name).toEqual([]);
      // The open sheet's targets: the page under its backdrop is not in reach.
      expect((await smallTargets(page)).filter((t) => !t.startsWith('textarea#body ')), name).toEqual([]);
    }
    await page.screenshot({ path: testInfo.outputPath(`${name}-${w}.png`) });
  }
}

// 035-AC12 035-AC13 035-AC16 035-AC18: a member tunes Grill and saves it to their personas: the ⋯
// item names the next version, the status line links to /profile#personas, the Identity tab names
// the source, and a second save makes v2 of the same library persona. With unsaved edits the item
// says to save first, and does nothing. Another member sees the source by its author, and no update
// from a library persona they cannot see. Once that member saves it to theirs, the first member's
// next save still names the next version of their own.
test('persona library: save to my personas from a room', async ({ browser, request }) => {
  // Unique: the server and its libraries are shared by every spec and repeat.
  const me = 'Alice' + uniq();
  const { room, pages: [alice, bob] } = await roomWith(browser, request, [me, 'Bob']);
  const sheet = await personaSheet(alice, 'Grill', 'Identity');
  const source = sheet.locator('section[data-tab=identity] > p.hint').first();
  await expect(source).toHaveText(/^From Grill v\d+, built in\.$/);
  const menu = alice.locator('#persona-menu');

  await editPersona(sheet, () => sheet.getByLabel('Persona text').fill('Sharper.'));
  await sheet.getByRole('link', { name: 'Triggers', exact: true }).click();
  await sheet.getByLabel('Speaks up').selectOption('every_n');
  await sheet.getByLabel('N', { exact: true }).fill('3');
  await sheet.locator('#persona-more').click();
  const first = menu.getByRole('button', { name: 'Save your changes first', exact: true });
  await expect(first).toBeDisabled();
  await expect(first).toHaveText('Save your changes first');
  // Reachable by keyboard: the menu opens on the reason.
  await expect(first).toBeFocused();
  await alice.keyboard.press('Enter');
  await expect(menu).toBeHidden();
  await expect(sheet.locator('#persona-status')).toHaveText('');
  await expect(sheet.getByRole('button', { name: 'Save', exact: true })).toBeEnabled();
  await sheet.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sheet.locator('#persona-status')).toHaveText('Grill saved. It applies from its next turn.');

  await sheet.locator('#persona-more').click();
  const save = menu.getByRole('button', { name: 'Save Grill to my personas', exact: true });
  await expect(save).toHaveText('Save to my personas');
  await save.click();
  await expect(sheet.locator('#persona-status')).toHaveText('Saved Grill v1 to your personas.');
  await expect(sheet.locator('#persona-status a')).toHaveAttribute('href', '/profile#personas');
  await sheet.getByRole('link', { name: 'Identity', exact: true }).click();
  await expect(source).toHaveText('From Grill v1, in your personas.');

  await sheet.locator('#persona-more').click();
  const again = menu.getByRole('button', { name: 'Save Grill to my personas as v2', exact: true });
  await expect(again).toHaveText('Save to my personas as v2');
  await again.click();
  await expect(sheet.locator('#persona-status')).toHaveText('Saved Grill v2 to your personas.');
  const mine = (await (await alice.request.get(`/api/rooms/${room.id}/templates`)).json()).filter((t) => t.section === 'personal');
  expect(mine.map((t) => [t.title, t.version, t.mine, t.text, t.every_n])).toEqual([['Grill', 2, true, 'Sharper.', 3]]);

  // A newer version Bob cannot see: offered to Alice only (035-AC18).
  const t = mine[0];
  const res = await alice.request.post(`/api/rooms/${room.id}/templates`, {
    data: { template_id: t.id, name: t.name, badge: t.badge, colour: t.colour, text: 'Sharpest.', model: t.model, effort: t.effort },
  });
  expect(res.status()).toBe(201);
  await bob.reload();
  expect((await agentState(bob, room.id)).personas[0].update_version).toBe(3);
  const bobSheet = await personaSheet(bob, 'Grill', 'Identity');
  // The library loaded: an update hidden before it would prove nothing.
  await expect.poll(() => bob.evaluate(() => templates.length)).toBeGreaterThan(0);
  await expect(bobSheet.locator('section[data-tab=identity] > p.hint').first()).toHaveText(`From Grill v2, by ${me}.`);
  await bobSheet.locator('#persona-more').click();
  await expect(bob.locator('#persona-menu').getByRole('button')).toHaveText(['Save to my personas', 'Remove']);

  // The sheet's deep link reopens it on Identity.
  await alice.reload();
  await expect(source).toHaveText('From Grill v2, in your personas.');
  await sheet.locator('#persona-more').click();
  await expect(menu.getByRole('button').first()).toHaveText('Update to v3');
  await alice.keyboard.press('Escape');

  // Bob's save links the room persona to his; Alice's save goes on with her own (035-AC13).
  await bob.locator('#persona-menu').getByRole('button', { name: 'Save Grill to my personas', exact: true }).click();
  await expect(bobSheet.locator('#persona-status')).toHaveText('Saved Grill v1 to your personas.');
  await expect(source).toHaveText('From Grill v1, by Bob.');
  await sheet.locator('#persona-more').click();
  await expect(menu.getByRole('button')).toHaveText(['Save to my personas as v4', 'Remove']);
});

// 035-AC10 035-AC14: a profile saves a persona with a skill and a plugin on to its personas in one
// room, then, a room admin in another room whose AI host (a fake AI host app) approves everything,
// adds it with the "+" at the end of the persona bar: the picker lists it under Yours and previews
// its model, trigger, skill and plugin; the status line names what waits for the host. The skill
// and the plugin wait for the host's approval, the host approves them and the persona, and they land.
test('persona library: add a library persona with its skill and plugin', async ({ browser, request, baseURL }, testInfo) => {
  test.setTimeout(120_000);
  const tag = uniq();
  const me = 'Alice' + tag;
  const kit = 'Kit' + tag;
  const skillName = 'stamp-' + tag;
  const { room: a, pages: [alice] } = await roomWith(browser, request, [me]);
  await appointAdmin(alice, request, a.id);
  // This host approves without asking: the plugin it inspects runs at once.
  const hosts = [await fakeDaemon(alice, baseURL, a.id, 'Alice laptop')];
  try {
    const grill = (await agentState(alice, a.id)).personas[0].id;
    const zip = [...readFileSync(skillZip(skillName, 'STAMP'))];
    const skill = await alice.evaluate(async (bytes) => (await fetch('/api/profile/skills', { method: 'POST',
      headers: { 'Content-Type': 'application/zip' }, body: new Uint8Array(bytes) })).json(), zip);
    expect((await call(alice, 'PUT', `/api/rooms/${a.id}/personas/${grill}/skills/${skill.id}`, { accept_disclaimer: true })).status).toBe(200);
    const added = await call(alice, 'POST', `/api/rooms/${a.id}/plugins`, { url: publishPlugin('kit-' + tag), ref: 'main', personas: [grill] });
    expect(added.status, JSON.stringify(added.body)).toBe(201);
    await report(alice, baseURL, hosts[0], a.id, 'babs');
    await expect.poll(async () => (await call(alice, 'GET', `/api/rooms/${a.id}/plugins`)).body.plugins[0].versions[0].state).toBe('approved');
    expect(await patchPersona(alice, a.id, grill, { name: kit })).toBe(200);
    const saved = await call(alice, 'POST', `/api/rooms/${a.id}/personas/${grill}/library`);
    expect(saved.status, JSON.stringify(saved.body)).toBe(201);
    expect([saved.body.frozen_skills.map((k) => k.name), saved.body.frozen_plugins.map((pl) => pl.name)]).toEqual([[skillName], ['babs']]);

    // Room B: Bob's AI host approves everything; Alice joins as a room admin.
    const { room: b, pages: [bob] } = await roomWith(browser, request, ['Bob' + tag]);
    hosts.push(await fakeDaemon(bob, baseURL, b.id, 'Bob laptop', { approve_all: true }));
    const inv = await (await request.post(`/api/rooms/${b.id}/invites`, { headers: { Authorization: 'Bearer ' + adminToken } })).json();
    await alice.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await alice.getByLabel('Your name').fill(me);
    await alice.getByRole('button', { name: 'Join the room' }).click();
    await expect(alice.locator('#me')).toHaveText(me);
    await appointAdmin(alice, request, b.id);

    // 035-AC14: "+" sits after the pills, outside the bar that scrolls them.
    const plus = alice.getByRole('button', { name: 'Add a persona', exact: true });
    expect(await plus.evaluate((e) => !e.closest('.persona-bar') && e.previousElementSibling.classList.contains('persona-bar'))).toBe(true);
    await plus.click();
    const picker = alice.locator('#persona-picker');
    await expect(picker.getByRole('heading', { name: 'Add a persona' })).toBeVisible();
    await expect(picker.locator('legend')).toHaveText(['Yours', 'Shared with everyone on this server', 'Built-in']);
    await expect(picker.locator('#persona-preview-none')).toHaveText('Choose a persona to see what it does.');
    await expect(picker.getByRole('button', { name: 'Add', exact: true })).toHaveAttribute('aria-disabled', 'true');
    const mine = picker.getByRole('group', { name: 'Yours' }).getByRole('radio', { name: `${kit}, v1` });
    await mine.check();
    // A redraw (the library read again) keeps the choice and the focus on it.
    await mine.focus();
    await alice.evaluate(() => renderPicker());
    await expect(mine).toBeFocused();
    await expect(mine).toBeChecked();
    await expect(picker.locator('#persona-preview-facts dt')).toHaveText(['Persona', 'Description', 'Model', 'Speaks up', 'Skills', 'Plugins']);
    await expect(picker.locator('#persona-preview-facts dd')).toHaveText([`🔥 ${kit}`, /./, 'sonnet · effort medium', 'On mention only', `${skillName} v1`, 'babs']);
    await capture(alice, testInfo, 'persona-picker');
    // With speech on the server, the preview says its voice in the call too (035-AC14).
    await alice.evaluate(() => { ttsOn = true; renderPreview(); });
    await expect(picker.locator('#persona-preview-facts dt')).toHaveText(['Persona', 'Description', 'Model', 'Speaks up', 'In the call', 'Skills', 'Plugins']);
    const inCall = picker.locator('#persona-preview-facts dd').nth(4);
    await expect(inCall).toHaveText('Default voice');
    const kept = await alice.evaluate(() => { const t = pickChoice(); const k = { speaks: t.speaks, voice: t.voice }; t.speaks = false; renderPreview(); return k; });
    await expect(inCall).toHaveText('Silent');
    await alice.evaluate(() => {
      voiceList = { voices: [{ id: 'fr-ada', name: 'Ada', lang: 'fr' }] };
      Object.assign(pickChoice(), { speaks: true, voice: 'fr-ada' });
      renderPreview();
    });
    await expect(inCall).toHaveText('Ada (French)');
    await alice.evaluate((k) => { Object.assign(pickChoice(), k); voiceList = null; ttsOn = false; renderPreview(); }, kept);
    await picker.getByRole('button', { name: 'Add', exact: true }).click();
    await expect(picker).toBeHidden();
    await expect(plus).toBeFocused();
    // 035-AC10: what waits for the host, and the persona itself under "approve everything".
    await expect(alice.locator('#toasts .toast .text')).toHaveText(
      `Added 🔥 ${kit}. Waiting for the AI host: skill ${skillName}, plugin babs. It waits for the AI host's approval.`);
    await expect(alice.locator(`button.persona[data-name="${kit}"]`)).toBeVisible();
    const kitState = async () => (await agentState(alice, b.id)).personas.find((p) => p.name === kit);
    const plugin = async () => (await call(alice, 'GET', `/api/rooms/${b.id}/plugins`)).body.plugins.find((pl) => pl.name === 'babs');
    expect((await agentState(alice, b.id)).room_skills.find((k) => k.name === skillName).pending).toBe(true);
    expect((await plugin()).versions[0].state).toBe('inspecting');

    // The host inspects the plugin, then approves the persona, the skill and the plugin.
    await report(bob, baseURL, hosts[1], b.id, 'babs');
    await expect.poll(async () => (await plugin()).versions[0].state).toBe('pending');
    await openStrip(bob);
    await bob.getByRole('button', { name: `Approve the persona ${kit}` }).click();
    await bob.getByRole('button', { name: `Approve the room skill ${skillName}` }).click();
    expect((await call(bob, 'POST', `/api/rooms/${b.id}/plugins/${(await plugin()).id}/versions/1/decision`, { approve: true })).status).toBe(200);

    // They land: the persona runs with the skill and the plugin.
    await expect.poll(async () => (await kitState()).pending || false).toBe(false);
    expect((await agentState(alice, b.id)).room_skills.find((k) => k.name === skillName).pending).toBeFalsy();
    expect((await kitState()).skills.map((k) => k.name)).toEqual([skillName]);
    const pl = await plugin();
    expect([pl.running, pl.personas]).toEqual([1, [(await kitState()).id]]);
    const sheet = await personaSheet(alice, kit, 'Capabilities');
    await expect(sheet.locator('section[data-tab=capabilities]')).toContainText(skillName);
    await expect(sheet.locator('section[data-tab=capabilities]')).toContainText('babs');
  } finally {
    for (const h of hosts) h();
  }
});

// 035-AC15: at 12 personas the picker's Add and the Personas tab's Add a persona are aria-disabled,
// focusable and drawn disabled, with "A room holds 12 personas at most." in words beside each; "+"
// stays live, the reason in its description, and opens the picker that explains. The server refuses
// a 13th in the same words. One fewer and both work again, and a double press on Add adds one.
test('persona library: a room of 12 personas', async ({ browser, request }, testInfo) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const sec = (await call(alice, 'GET', `/api/rooms/${room.id}/templates`)).body.find((t) => t.section === 'builtin' && t.title === 'Secretary');
  for (let i = 0; i < 11; i++) expect((await call(alice, 'POST', `/api/rooms/${room.id}/personas`, { template_id: sec.id })).status).toBe(201);
  const over = await call(alice, 'POST', `/api/rooms/${room.id}/personas`, { template_id: sec.id });
  expect([over.status, over.body.error]).toEqual([409, 'A room holds 12 personas at most.']);
  await expect(alice.locator('button.persona')).toHaveCount(12);

  const why = 'A room holds 12 personas at most.';
  const plus = alice.locator('#persona-new');
  await expect(plus).toHaveAccessibleName('Add a persona');
  // The bar scrolls its pills behind edge "+N"s; the "+" after it keeps its 44 px on a phone, and
  // the page never scrolls sideways.
  await alice.screenshot({ path: testInfo.outputPath('room-full-1440.png') });
  await alice.setViewportSize({ width: 390, height: 844 });
  await expect(alice.locator('.persona-more.right')).toBeVisible();
  await alice.screenshot({ path: testInfo.outputPath('room-full-390.png') });
  expect(await alice.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const box = await plus.boundingBox();
  expect([box.width, box.height]).toEqual([44, 44]);
  const right = await alice.locator('.persona-more.right').boundingBox();
  expect(box.x).toBeGreaterThanOrEqual(right.x + right.width);
  await alice.setViewportSize({ width: 1440, height: 900 });
  await expect(plus).not.toHaveAttribute('aria-disabled');
  await expect(plus).toHaveAccessibleDescription(why);
  await plus.click();
  const picker = alice.locator('#persona-picker');
  await expect(picker.locator('#persona-pick-full')).toHaveText(why);
  await expect(picker.locator('#persona-pick-full')).toBeVisible();
  await picker.getByRole('group', { name: 'Built-in' }).getByRole('radio', { name: /^Grill, v/ }).check();
  const add = picker.getByRole('button', { name: 'Add', exact: true });
  await expect(add).toHaveAttribute('aria-disabled', 'true');
  await expect(add).toHaveAccessibleDescription(why);
  const background = () => add.evaluate((b) => getComputedStyle(b).backgroundColor);
  const off = await background();
  // Playwright will not click an aria-disabled control: the keyboard presses it.
  await add.focus();
  await alice.keyboard.press('Enter');
  await expect(picker).toBeVisible();
  await expect(picker.locator('#persona-pick-status')).toHaveText('');
  await picker.getByRole('button', { name: 'Close' }).click();
  await expect(plus).toBeFocused();

  await openPersonas(alice);
  const sheetAdd = alice.locator('#ai').getByRole('button', { name: 'Add a persona', exact: true });
  await expect(sheetAdd).toHaveAttribute('aria-disabled', 'true');
  await expect(alice.locator('#personas-full')).toBeVisible();
  await expect(alice.locator('#personas-full')).toHaveText(why);
  await sheetAdd.focus();
  await alice.keyboard.press('Enter');
  await expect(picker).toBeHidden();

  // One fewer: both work again, and the reason goes.
  const last = (await agentState(alice, room.id)).personas.at(-1);
  expect((await alice.evaluate(([r, id]) => fetch(`/api/rooms/${r}/personas/${id}`, { method: 'DELETE' }).then((x) => x.status), [room.id, last.id]))).toBe(204);
  await expect(sheetAdd).toHaveAttribute('aria-disabled', 'false');
  await expect(alice.locator('#personas-full')).toBeHidden();
  await expect(plus).toHaveAccessibleDescription('');
  await alice.locator('#ai').getByRole('button', { name: 'Close' }).click();

  // The picker's Add reads enabled again; pressed twice while its request runs, it adds one.
  await plus.click();
  await picker.getByRole('group', { name: 'Built-in' }).getByRole('radio', { name: /^Grill, v/ }).check();
  await expect(add).toHaveAttribute('aria-disabled', 'false');
  expect(await background()).not.toBe(off);
  let posts = 0;
  await alice.route(`**/api/rooms/${room.id}/personas`, async (r) => {
    posts++;
    await new Promise((res) => setTimeout(res, 500));
    await r.continue();
  });
  await add.focus();
  await alice.keyboard.press('Enter');
  await alice.keyboard.press('Enter');
  await expect(picker).toBeHidden();
  await expect(alice.locator('button.persona')).toHaveCount(12);
  expect(posts).toBe(1);
});

// 035-AC17: AI in this room › Personas lists the personas as rows with a ⋯ (Edit, Save to my personas,
// Remove); "Add a persona" is its one primary button, before the auto-turn limit and room skills.
// At 390 and 1440 px.
test('persona library: the Personas tab rows', async ({ browser, request }, testInfo) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice' + uniq()]);
  await openPersonas(alice);
  const panel = alice.locator('#ai');
  const add = panel.getByRole('button', { name: 'Add a persona', exact: true });
  await expect(add).toHaveClass(/btn-primary/);
  // The one primary: a form's submit is primary unless marked otherwise (kit).
  expect(await panel.locator('button.btn-primary, form button:not([type=button]):not(.btn-secondary)').count()).toBe(1);
  expect(await add.evaluate((b) => ['chain-form', 'room-skills'].every((id) => b.compareDocumentPosition(document.getElementById(id)) & Node.DOCUMENT_POSITION_FOLLOWING))).toBe(true);

  const more = panel.getByRole('button', { name: 'More actions for Grill', exact: true });
  await more.click();
  const menu = panel.locator('.persona-rows .menu:popover-open');
  await expect(menu.getByRole('button')).toHaveText(['Edit', 'Save to my personas', 'Remove']);
  await expect(menu.getByRole('button', { name: 'Edit Grill' })).toBeFocused();
  // Someone adds or removes a persona meanwhile: the open ⋯ stays open, its focus kept (035-AC17).
  const sec = (await call(alice, 'GET', `/api/rooms/${room.id}/templates`)).body.find((t) => t.section === 'builtin' && t.title === 'Secretary');
  const added = await call(alice, 'POST', `/api/rooms/${room.id}/personas`, { template_id: sec.id });
  expect(added.status).toBe(201);
  await expect(personaRow(alice, 'Secretary')).toHaveCount(1);
  await expect(menu).toHaveCount(1);
  await expect(menu.getByRole('button', { name: 'Edit Grill' })).toBeFocused();
  expect(await alice.evaluate(([r, id]) => fetch(`/api/rooms/${r}/personas/${id}`, { method: 'DELETE' }).then((x) => x.status), [room.id, added.body.id])).toBe(204);
  await expect(personaRow(alice, 'Secretary')).toHaveCount(0);
  await expect(menu).toHaveCount(1);
  await expect(menu.getByRole('button', { name: 'Edit Grill' })).toBeFocused();
  await alice.keyboard.press('Escape');
  await expect(more).toBeFocused();
  // Removed by someone else while its ⋯ or its row holds the focus: the focus waits on Add a persona (035-AC17).
  for (const target of [() => panel.getByRole('button', { name: 'More actions for Secretary', exact: true }), () => personaRow(alice, 'Secretary')]) {
    const gone = await call(alice, 'POST', `/api/rooms/${room.id}/personas`, { template_id: sec.id });
    expect(gone.status).toBe(201);
    await target().focus();
    expect(await alice.evaluate(([r, id]) => fetch(`/api/rooms/${r}/personas/${id}`, { method: 'DELETE' }).then((x) => x.status), [room.id, gone.body.id])).toBe(204);
    await expect(personaRow(alice, 'Secretary')).toHaveCount(0);
    await expect(add).toBeFocused();
  }
  await more.focus();
  // At 390 px the settings stay whole and the state dot stays clear of the ⋯ and its focus ring.
  await alice.setViewportSize({ width: 390, height: 844 });
  const led = await panel.locator('.persona-row .led').first().boundingBox();
  const dots = await more.boundingBox();
  expect(led.x + led.width).toBeLessThanOrEqual(dots.x - 4);
  expect(await more.evaluate((b) => getComputedStyle(b).outlineOffset)).toBe('-2px');
  expect(await panel.locator('.pr-meta').first().evaluate((e) => e.scrollWidth <= e.clientWidth)).toBe(true);
  await capture(alice, testInfo, 'personas-sheet');

  // Save to my personas, as the persona sheet's ⋯ does (035-AC12).
  await more.click();
  await menu.getByRole('button', { name: 'Save Grill to my personas' }).click();
  await expect(panel.locator('#personas-status')).toHaveText('Saved Grill v1 to your personas.');
  await more.click();
  await expect(menu.getByRole('button', { name: 'Save Grill to my personas as v2' })).toHaveText('Save to my personas as v2');
  // Edit opens its sheet.
  await menu.getByRole('button', { name: 'Edit Grill' }).click();
  await expect(alice.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
  await alice.locator('#persona-sheet').getByRole('button', { name: 'Close' }).click();

  // From here the room's answer has no profile, not a page variable: each room event reloads the
  // room, and the answer to the reload this Remove sets off would put the profile back.
  await patchJSON(alice, `**/api/rooms/${room.id}`, (json) => ({ ...json, profile: null }));
  // Remove from the persona sheet's own ⋯ closes it, and the focus waits on Add a persona.
  expect((await call(alice, 'POST', `/api/rooms/${room.id}/personas`, { template_id: sec.id })).status).toBe(201);
  await panel.getByRole('button', { name: 'More actions for Secretary', exact: true }).click();
  await panel.locator('.persona-rows .menu:popover-open').getByRole('button', { name: 'Edit Secretary' }).click();
  await alice.locator('#persona-more').click();
  await alice.locator('#persona-menu').getByRole('button', { name: 'Remove Secretary', exact: true }).click();
  await confirmSheet(alice, 'Remove');
  await expect(alice.locator('#persona-sheet')).toBeHidden();
  await expect(personaRow(alice, 'Secretary')).toHaveCount(0);
  await expect(add).toBeFocused();

  // Without a profile, saving says why and does nothing.
  await alice.evaluate(() => loadRoom());
  await more.click();
  const noProfile = menu.getByRole('button', { name: 'Saving personas needs a profile', exact: true });
  await expect(noProfile).toHaveAttribute('aria-disabled', 'true');
  // Drawn off in the row menu too, not only in the id-scoped ones (035-AC12); Edit stays live.
  const looks = (item) => item.evaluate((b) => {
    const muted = document.body.appendChild(Object.assign(document.createElement('span'), { style: 'color: var(--text-muted)' }));
    const c = getComputedStyle(muted).color;
    muted.remove();
    return [getComputedStyle(b).cursor, getComputedStyle(b).color === c];
  });
  expect(await looks(noProfile)).toEqual(['default', true]);
  expect((await looks(menu.getByRole('button', { name: 'Edit Grill' })))[1]).toBe(false);
  // Hovered, it stays flat: nothing to press.
  await noProfile.hover();
  expect(await noProfile.evaluate((b) => getComputedStyle(b).backgroundColor)).toBe('rgba(0, 0, 0, 0)');
  await alice.keyboard.press('Escape');
  await alice.unroute(`**/api/rooms/${room.id}`);
  await alice.evaluate(() => loadRoom());

  // Remove asks first, as the persona sheet's ⋯ does.
  await more.click();
  await menu.getByRole('button', { name: 'Remove Grill' }).click();
  await expect(alice.locator('dialog.confirm #confirm-title')).toHaveText('Remove Grill?');
  await confirmSheet(alice, 'Remove');
  await expect(personaRow(alice, 'Grill')).toHaveCount(0);
  await expect(add).toBeFocused();
  expect((await agentState(alice, room.id)).personas).toHaveLength(0);
});

// 035-AC14: a failed add says why in the picker and reads the library again, so a persona deleted
// meanwhile leaves the list and the choice; one that fails once the picker is closed says so in the
// Personas tab's status line, or in a toast when AI in this room is closed too.
test('persona library: the picker after a failed add', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice' + uniq()]);
  const grill = (await agentState(alice, room.id)).personas[0].id;
  const saved = await call(alice, 'POST', `/api/rooms/${room.id}/personas/${grill}/library`);
  expect(saved.status).toBe(201);
  await openPersonas(alice);
  await alice.locator('#ai').getByRole('button', { name: 'Add a persona', exact: true }).click();
  const picker = alice.locator('#persona-picker');
  const yours = picker.getByRole('group', { name: 'Yours' });
  await yours.getByRole('radio', { name: 'Grill, v1' }).check();
  expect(await alice.evaluate((id) => fetch('/api/profile/personas/' + id, { method: 'DELETE' }).then((r) => r.status), saved.body.id)).toBe(204);
  const add = picker.getByRole('button', { name: 'Add', exact: true });
  await add.click();
  await expect(picker.locator('#persona-pick-status')).toHaveText('no such persona');
  await expect(yours.getByRole('radio')).toHaveCount(0);
  await expect(add).toHaveAttribute('aria-disabled', 'true');
  await expect(picker.locator('#persona-preview-none')).toBeVisible();

  // Closed while its request runs: the answer lands in the sheet's status line.
  const secretary = picker.getByRole('group', { name: 'Built-in' }).getByRole('radio', { name: /^Secretary, v/ });
  await secretary.check();
  await alice.route(`**/api/rooms/${room.id}/personas`, async (r) => {
    await new Promise((res) => setTimeout(res, 500));
    await r.fulfill({ status: 409, json: { error: 'A room holds 12 personas at most.' } });
  });
  await add.click();
  await alice.keyboard.press('Escape');
  await expect(picker).toBeHidden();
  await expect(alice.locator('#personas-status')).toHaveText('A room holds 12 personas at most.');
  await expect(alice.locator('#personas-status')).toHaveClass(/error/);
  await expect(alice.locator('#toasts .toast')).toHaveCount(0);

  // From "+", AI in this room closed: in a toast.
  await alice.locator('#ai').getByRole('button', { name: 'Close' }).click();
  await alice.locator('#persona-new').click();
  await secretary.check();
  await add.click();
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#toasts .toast .text')).toHaveText('A room holds 12 personas at most.');
});
