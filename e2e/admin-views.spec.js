import { request as pwRequest } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { readFileSync } from 'node:fs';
import { adminToken } from './playwright.config.js';
import { signIn, noOverflow, skillZip, confirmSheet, rowAction, uniq } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const widths = [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }];

// member joins room as name through a fresh API context, which keeps the profile cookie the join
// sets; it returns that context and the participant's token.
async function member(baseURL, request, room, name) {
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await pwRequest.newContext({ baseURL });
  const res = await ctx.post('/api/join', { data: { invite: inv.token, name } });
  expect(res.status()).toBe(201);
  return { ctx, token: (await res.json()).token };
}

async function fits(page, width) {
  if (width > 400) return;
  expect(await noOverflow(page)).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
}

async function audited(request, action, target) {
  const log = await (await request.get(`/api/admin/audit?action=${action}&target=${encodeURIComponent(target)}`, { headers: admin })).json();
  expect(log.entries.length, `${action} on ${target}`).toBeGreaterThan(0);
  expect(log.entries[0].details.via).toBe('session');
}

for (const vp of widths) {
  // 004-AC2 004-AC9 004-AC19: the rooms view sorts, breaks a room's volume down against its quota,
  // and renames, sets the quota, manages invites and deletes once the name is typed; uploads then
  // honour the quota. Every action is audited (004-AC6).
  test(`rooms view and actions, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const name = `Rooms ${vp.name} ${uniq()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
    const alice = await member(baseURL, request, room, 'Alice');
    const plan = '# Plan\n\nCrème brûlée.\n';
    expect((await alice.ctx.post(`/api/rooms/${room.id}/documents`, { multipart: { file: { name: 'plan.md', mimeType: 'text/markdown', buffer: Buffer.from(plan) } } })).status()).toBe(201);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'rooms')).toBe(201);
    const row = page.locator('#room-' + room.id);
    await expect(row).toContainText(name);
    await expect(row.locator('.cell').nth(1)).toHaveText(/1$/);
    await expect(row.locator('.cell').nth(2)).toHaveText(/1$/);
    await expect(row.locator('.cell').nth(3)).toHaveText('No AI host');
    await fits(page, vp.width);

    // Sorting by name, both ways, from the column heads: a phone shows cards, newest first (005-AC43, 004b-AC1).
    if (vp.width > 400) {
      const names = () => page.locator('#rooms .list-row:not(.head) .name').allTextContents();
      const sorted = (list, desc) => [...list].sort((a, b) => (a.toLowerCase() < b.toLowerCase() ? -1 : a.toLowerCase() > b.toLowerCase() ? 1 : 0) * (desc ? -1 : 1));
      await page.getByRole('button', { name: 'Name', exact: true }).click();
      await expect(page.locator('#rooms .head [aria-sort="ascending"]')).toHaveText('Name');
      expect(await names()).toEqual(sorted(await names(), false));
      await page.getByRole('button', { name: 'Name', exact: true }).click();
      await expect(page.locator('#rooms .head [aria-sort="descending"]')).toHaveText('Name');
      expect(await names()).toEqual(sorted(await names(), true));
    }

    // 005-AC16: a room opens its own view, with tabs, each a deep link.
    await row.getByRole('link', { name: 'Manage ' + name }).click();
    await expect(page).toHaveURL(new RegExp(`#rooms/${room.id}$`));
    const panel = page.locator('#room-panel');
    const tab = async (label) => {
      await panel.getByRole('link', { name: label, exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`#rooms/${room.id}/${label.toLowerCase()}$`));
    };
    await expect(panel.locator('#volume')).toContainText(`Uploads${Buffer.byteLength(plan)} B`);
    await expect(panel.locator('#volume')).toContainText('Room skills0 B');
    await expect(panel.locator('#storage')).toContainText('of 200 MB, the server default.');
    await tab('Export');
    await expect(panel.locator('#export')).toHaveAttribute('href', `/api/admin/rooms/${room.id}/export`);
    await fits(page, vp.width);
    await tab('General');

    // Quota: 1 MB, then an upload past it is refused; back to the default.
    await panel.locator('#quota').fill('1');
    await panel.getByRole('button', { name: 'Set quota' }).click();
    await expect(panel.locator('#storage')).toContainText(/of 1\.0 MB\.$/);
    await audited(request, 'room.quota', room.id);
    const big = await alice.ctx.post(`/api/rooms/${room.id}/documents`, { multipart: { file: { name: 'big.md', mimeType: 'text/markdown', buffer: Buffer.alloc(1 << 20, 'a') } } });
    expect(big.status()).toBe(413);
    expect((await big.json()).error).toBe('this upload would take the room over its 1 MB limit');
    await panel.getByRole('button', { name: 'Use the default' }).click();
    await expect(panel.locator('#storage')).toContainText('of 200 MB, the server default.');

    // Rename.
    await panel.locator('#room-name-input').fill(name + ' renamed');
    await panel.getByRole('button', { name: 'Rename' }).click();
    await expect(panel.locator('h2')).toContainText(name + ' renamed');
    await audited(request, 'room.rename', room.id);
    await page.getByRole('link', { name: 'All rooms' }).click();
    await expect(row.locator('.name')).toHaveText(name + ' renamed');
    await expect(row).toContainText('/ 200 MB');

    // Invites, from a deep link: a new link shown once, then revoked.
    await page.goto(`/admin#rooms/${room.id}/invites`);
    await expect(panel.getByRole('link', { name: 'Invites', exact: true })).toHaveAttribute('aria-current', 'page');
    const invites = panel.locator('#invites-all .list-row:not(.head)');
    await expect(invites).toHaveCount(1);
    await panel.getByRole('button', { name: 'New invite link' }).click();
    await expect(panel.locator('#invite-link-new')).toHaveValue(/\/join#\S+$/);
    await expect(invites).toHaveCount(2);
    await rowAction(invites.nth(1), /^Revoke/);
    await expect(page.locator('#revoke-nobody')).toHaveText('Nobody joined through this invite.');
    await page.locator('#revoke-sheet').getByRole('button', { name: 'Revoke', exact: true }).click();
    await expect(page.locator('.toast')).toHaveText('Revoked. Nobody lost access.');
    await expect(invites.nth(1)).toContainText('revoked');
    await audited(request, 'invite.create', room.id);
    await audited(request, 'invite.revoke', room.id);
    await fits(page, vp.width);

    // Delete asks for the name typed.
    await tab('General');
    const del = panel.getByRole('button', { name: 'Delete the room' });
    await expect(del).toBeDisabled();
    await panel.locator('#delete-confirm').fill(name);
    await expect(del).toBeDisabled();
    await panel.locator('#delete-confirm').fill(name + ' renamed');
    await del.click();
    await expect(page).toHaveURL(/#rooms$/);
    await expect(panel).toHaveCount(0);
    await expect(row).toHaveCount(0);
    await audited(request, 'room.delete', room.id);
    expect((await alice.ctx.get(`/api/rooms/${room.id}`)).status()).toBe(401);
    await alice.ctx.dispose();
  });

  // 004-AC8 004-AC6: a room the super-admin created has no room admin; the super-admin appoints a
  // participant with a profile, who may then rename it, and removes them again. Both are audited.
  test(`room admins appointed from /admin, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const name = `Admins ${vp.name} ${uniq()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
    const alice = await member(baseURL, request, room, 'Alice');
    expect((await alice.ctx.patch(`/api/rooms/${room.id}`, { data: { name: 'by Alice' } })).status()).toBe(403);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'rooms')).toBe(201);
    await page.locator('#room-' + room.id).getByRole('link', { name: 'Manage ' + name }).click();
    await page.locator('#room-panel').getByRole('link', { name: 'Admins', exact: true }).click();
    const admins = page.locator('#room-panel #room-admins');
    await expect(admins.locator('> li')).toHaveCount(1);
    await admins.getByRole('button', { name: 'Make Alice a room admin' }).click();
    await expect(admins.locator('> li', { hasText: 'Alice' }).locator('.badge.role')).toHaveText('admin');
    await audited(request, 'room_admin.add', room.id);
    await fits(page, vp.width);
    expect((await alice.ctx.patch(`/api/rooms/${room.id}`, { data: { name: name + ' by Alice' } })).status()).toBe(200);
    await admins.getByRole('button', { name: 'Remove Alice from the room admins' }).click();
    await expect(admins.getByRole('button', { name: 'Make Alice a room admin' })).toBeVisible();
    await audited(request, 'room_admin.remove', room.id);
    expect((await alice.ctx.patch(`/api/rooms/${room.id}`, { data: { name: 'by Alice' } })).status()).toBe(403);
    await alice.ctx.dispose();
  });

  // 004-AC3: the profiles view lists name, email, rooms, skill count and last activity, and deletes
  // a profile: its participant stays in the room, detached.
  test(`profiles view and delete, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const name = `Pat ${vp.name} ${uniq()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Profiles ' + name } })).json();
    const pat = await member(baseURL, request, room, name);
    expect((await pat.ctx.post('/api/profile/skills', { headers: { 'Content-Type': 'application/zip' }, data: readFileSync(skillZip('mine' + uniq(), 'X')) })).status()).toBe(201);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'profiles')).toBe(201);
    const row = page.locator('#profiles .list-row', { hasText: name });
    await expect(row).toHaveCount(1);
    await expect(row.locator('.meta')).toHaveText('no email');
    await expect(row.locator('.rooms')).toHaveText('1 room');
    await expect(row.locator('.rooms')).toHaveAttribute('data-tip', 'Profiles ' + name);
    await expect(row.locator('.cell').nth(1)).toHaveText(/1$/);
    await fits(page, vp.width);
    await rowAction(row, /^Delete the profile/);
    await confirmSheet(page, 'Delete');
    await expect(row).toHaveCount(0);
    await audited(request, 'profile.delete', name);
    expect((await pat.ctx.get('/api/profile')).status()).toBe(200);
    expect((await (await pat.ctx.get('/api/profile')).json()).profile).toBeFalsy();
    expect((await request.get(`/api/rooms/${room.id}`, { headers: { Authorization: `Bearer ${pat.token}` } })).status()).toBe(200);
    await pat.ctx.dispose();
  });

  // 004-AC4: the libraries view lists persona templates and shared skills with author and version,
  // and removes them; shipped templates stay.
  test(`libraries view and removal, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const stamp = `${vp.name}${uniq()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Library ' + stamp } })).json();
    const lib = await member(baseURL, request, room, 'Lib');
    const tpl = { name: 'Critic', badge: '🧐', colour: '#112233', text: 'Find the flaw.', model: 'sonnet', effort: 'medium', title: 'Critic ' + stamp };
    expect((await lib.ctx.post(`/api/rooms/${room.id}/templates`, { data: tpl })).status()).toBe(201);
    const skill = 'shared' + stamp;
    expect((await lib.ctx.post('/api/profile/skills?scope=shared', { headers: { 'Content-Type': 'application/zip' }, data: readFileSync(skillZip(skill, 'X')) })).status()).toBe(201);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'libraries')).toBe(201);
    const t = page.locator('#templates .list-row', { hasText: tpl.title });
    await expect(t).toHaveText(new RegExp(`${tpl.title}Libv1`));
    await expect(page.locator('#templates .list-row', { hasText: 'shipped' }).first()).toBeVisible();
    const k = page.locator('#skills .list-row', { hasText: skill });
    await expect(k).toHaveText(new RegExp(`${skill}Libv1`));
    await fits(page, vp.width);
    await rowAction(t, /^Remove/);
    await confirmSheet(page, 'Remove');
    await expect(t).toHaveCount(0);
    await rowAction(k, /^Remove/);
    await confirmSheet(page, 'Remove');
    await expect(k).toHaveCount(0);
    await audited(request, 'template.remove', tpl.title);
    await audited(request, 'skill.remove', skill);
    await lib.ctx.dispose();
  });

  // 013-AC31: the Pronunciation view lists and searches the lexicon, adds a term, refuses a
  // duplicate, flips a style in one click, edits and removes; each change is audited.
  test(`pronunciation view, ${vp.name}`, async ({ browser, request }) => {
    const term = `Grafana${vp.name}${uniq()}`;
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'pronunciation')).toBe(201);
    const gh = page.locator('#pronunciations .list-row', { hasText: 'guite-heub' });
    await expect(gh).toContainText('GitHub');
    await expect(gh.getByRole('button', { name: 'GitHub: English-style, switch to French-style' })).toBeVisible();
    await page.getByLabel('Search').fill('clusteur');
    await expect(page.locator('#pronunciations .list-row:not(.head)')).toHaveCount(1);
    await expect(page.locator('#pronunciations .list-row:not(.head)')).toContainText('cluster');
    await page.getByLabel('Search').fill('');
    await fits(page, vp.width);
    await page.screenshot({ path: `test-results/pronunciation-${vp.name}.png` });

    const sheet = page.locator('#term-sheet');
    await page.getByRole('button', { name: 'Add a term' }).click();
    await sheet.getByLabel('Term').fill('github');
    await sheet.getByLabel('English', { exact: true }).fill('git hub');
    await sheet.getByRole('button', { name: 'Add' }).click();
    await expect(sheet.getByRole('alert')).toContainText('the lexicon has that term already');
    await sheet.getByLabel('Term').fill(term);
    await sheet.getByLabel('French, English-style').fill('gra-fa-na');
    await sheet.getByRole('button', { name: 'Add' }).click();
    await expect(sheet).toBeHidden();
    const row = page.locator('#pronunciations .list-row', { hasText: term });
    await expect(row).toContainText('gra-fa-na');

    await row.getByRole('button', { name: `${term}: English-style, switch to French-style` }).click();
    await expect(row.getByRole('button', { name: `${term}: French-style, switch to English-style` })).toBeVisible();
    const list = await (await request.get('/api/admin/pronunciations', { headers: admin })).json();
    expect(list.entries.find((e) => e.term === term).style).toBe('fr');

    await rowAction(row, /^Edit/);
    await sheet.getByLabel('French, French-style').fill('grafana');
    await sheet.getByRole('button', { name: 'Save' }).click();
    await expect(row).toContainText('grafana');
    await rowAction(row, /^Remove/);
    await confirmSheet(page, 'Remove');
    await expect(row).toHaveCount(0);
    for (const action of ['tts.lexicon.add', 'tts.lexicon.edit', 'tts.lexicon.delete']) await audited(request, action, term);
  });

  // 004-AC5: the Server view shows version, database size and free disk, STT, AI host apps and SMTP.
  test(`instance view, ${vp.name}`, async ({ browser }) => {
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'instance')).toBe(201);
    const dl = page.locator('#instance');
    await expect(dl).toContainText(/Versionv\d+\.\d+\.\d+/);
    await expect(dl).toContainText(/Database\d+(\.\d)? (KB|MB)/);
    await expect(dl).toContainText(/Disk\d+(\.\d)? (MB|GB|TB) free of \d+(\.\d)? (GB|TB)/);
    await expect(dl).toContainText('Speech to textoff: no audio');
    await expect(dl).toContainText(/AI host apps\d+ connected/);
    await expect(dl).toContainText('Email (SMTP)not configured');
    await fits(page, vp.width);
  });
}

// 021-AC11: an open instance offers no provider sign-in on /admin, and never asks for one.
test('the admin login of an open instance asks no provider', async ({ page }) => {
  const asked = [];
  page.on('request', (r) => { if (r.url().includes('/api/admin/login/oidc')) asked.push(r.url()); });
  await page.goto('/admin');
  await expect(page.locator('#token')).toBeVisible();
  await expect(page.locator('#provider-sign-in')).toHaveCount(0);
  await page.waitForTimeout(500);
  expect(asked).toEqual([]);
});
