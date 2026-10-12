import { test, expect, request as pwRequest } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { adminToken } from './playwright.config.js';
import { signIn, noOverflow, skillZip } from './helpers.js';

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
    const name = `Rooms ${vp.name} ${Date.now()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
    const alice = await member(baseURL, request, room, 'Alice');
    const plan = '# Plan\n\nCrème brûlée.\n';
    expect((await alice.ctx.post(`/api/rooms/${room.id}/documents`, { multipart: { file: { name: 'plan.md', mimeType: 'text/markdown', buffer: Buffer.from(plan) } } })).status()).toBe(201);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'rooms')).toBe(201);
    const row = page.locator('#room-' + room.id);
    await expect(row).toContainText(name);
    await expect(row.locator('td').nth(3)).toHaveText('1');
    await expect(row.locator('td').nth(4)).toHaveText('1');
    await expect(row.locator('td').nth(5)).toHaveText('none');
    await fits(page, vp.width);

    // Sorting by name, both ways.
    const names = () => page.locator('#rooms tbody tr td:first-child').allTextContents();
    const sorted = (list, desc) => [...list].sort((a, b) => (a.toLowerCase() < b.toLowerCase() ? -1 : a.toLowerCase() > b.toLowerCase() ? 1 : 0) * (desc ? -1 : 1));
    await page.getByRole('button', { name: 'Name', exact: true }).click();
    await expect(page.locator('#rooms th[aria-sort="ascending"]')).toHaveText('Name');
    expect(await names()).toEqual(sorted(await names(), false));
    await page.getByRole('button', { name: 'Name', exact: true }).click();
    await expect(page.locator('#rooms th[aria-sort="descending"]')).toHaveText('Name');
    expect(await names()).toEqual(sorted(await names(), true));

    await row.getByRole('button', { name: 'Manage' }).click();
    const panel = page.locator('#room-panel');
    await expect(panel.locator('#volume')).toContainText(`Uploads${Buffer.byteLength(plan)} B`);
    await expect(panel.locator('#volume')).toContainText('Room skills0 B');
    await expect(panel).toContainText('of 200 MB used, the instance default.');
    await expect(panel.locator('#export')).toBeDisabled();
    await fits(page, vp.width);

    // Quota: 1 MB, then an upload past it is refused; back to the default.
    await panel.locator('#quota').fill('1');
    await panel.getByRole('button', { name: 'Set quota' }).click();
    await expect(panel).toContainText('of 1.0 MB used.');
    await expect(row).toContainText('/ 1.0 MB');
    await audited(request, 'room.quota', room.id);
    const big = await alice.ctx.post(`/api/rooms/${room.id}/documents`, { multipart: { file: { name: 'big.md', mimeType: 'text/markdown', buffer: Buffer.alloc(1 << 20, 'a') } } });
    expect(big.status()).toBe(413);
    expect((await big.json()).error).toBe('this upload would take the room over its 1 MB limit');
    await panel.getByRole('button', { name: 'Use the default' }).click();
    await expect(panel).toContainText('of 200 MB used, the instance default.');

    // Rename.
    await panel.locator('#room-name').fill(name + ' renamed');
    await panel.getByRole('button', { name: 'Rename' }).click();
    await expect(row.locator('td').first()).toHaveText(name + ' renamed');
    await audited(request, 'room.rename', room.id);

    // Invites: a new link shown once, then revoked.
    const invites = panel.locator('#invites tbody tr');
    await expect(invites).toHaveCount(1);
    await panel.getByRole('button', { name: 'New invite link' }).click();
    await expect(panel.locator('#invite-link')).toHaveValue(/\/join#\S+$/);
    await expect(invites).toHaveCount(2);
    await invites.nth(1).getByRole('button', { name: 'Revoke' }).click();
    await expect(invites.nth(1)).toContainText('revoked');
    await audited(request, 'invite.create', room.id);
    await audited(request, 'invite.revoke', room.id);
    await fits(page, vp.width);

    // Delete asks for the name typed.
    const del = panel.getByRole('button', { name: 'Delete the room' });
    await expect(del).toBeDisabled();
    await panel.locator('#delete-confirm').fill(name);
    await expect(del).toBeDisabled();
    await panel.locator('#delete-confirm').fill(name + ' renamed');
    await del.click();
    await expect(row).toHaveCount(0);
    await expect(panel).toHaveCount(0);
    await audited(request, 'room.delete', room.id);
    expect((await alice.ctx.get(`/api/rooms/${room.id}`)).status()).toBe(401);
    await alice.ctx.dispose();
  });

  // 004-AC3: the profiles view lists name, email, rooms, skill count and last activity, and deletes
  // a profile: its participant stays in the room, detached.
  test(`profiles view and delete, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const name = `Pat ${vp.name} ${Date.now()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Profiles ' + name } })).json();
    const pat = await member(baseURL, request, room, name);
    expect((await pat.ctx.post('/api/profile/skills', { headers: { 'Content-Type': 'application/zip' }, data: readFileSync(skillZip('mine' + Date.now(), 'X')) })).status()).toBe(201);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'profiles')).toBe(201);
    const row = page.locator('#profiles tbody tr', { hasText: name });
    await expect(row).toHaveCount(1);
    await expect(row.locator('td').nth(1)).toHaveText('—');
    await expect(row.locator('td').nth(2)).toHaveText('Profiles ' + name);
    await expect(row.locator('td').nth(3)).toHaveText('1');
    await fits(page, vp.width);
    page.once('dialog', (d) => d.accept());
    await row.getByRole('button', { name: 'Delete' }).click();
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
    const stamp = `${vp.name}${Date.now()}`;
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Library ' + stamp } })).json();
    const lib = await member(baseURL, request, room, 'Lib');
    const tpl = { name: 'Critic', badge: '🧐', colour: '#112233', text: 'Find the flaw.', title: 'Critic ' + stamp };
    expect((await lib.ctx.post(`/api/rooms/${room.id}/templates`, { data: tpl })).status()).toBe(201);
    const skill = 'shared' + stamp;
    expect((await lib.ctx.post('/api/profile/skills?scope=shared', { headers: { 'Content-Type': 'application/zip' }, data: readFileSync(skillZip(skill, 'X')) })).status()).toBe(201);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'libraries')).toBe(201);
    const t = page.locator('#templates tbody tr', { hasText: tpl.title });
    await expect(t).toHaveText(new RegExp(`${tpl.title}Libv1`));
    await expect(page.locator('#templates tbody tr', { hasText: 'shipped' }).first()).toBeVisible();
    const k = page.locator('#skills tbody tr', { hasText: skill });
    await expect(k).toHaveText(new RegExp(`${skill}Libv1`));
    await fits(page, vp.width);
    page.once('dialog', (d) => d.accept());
    await t.getByRole('button', { name: 'Remove' }).click();
    await expect(t).toHaveCount(0);
    page.once('dialog', (d) => d.accept());
    await k.getByRole('button', { name: 'Remove' }).click();
    await expect(k).toHaveCount(0);
    await audited(request, 'template.remove', tpl.title);
    await audited(request, 'skill.remove', skill);
    await lib.ctx.dispose();
  });

  // 004-AC5: the instance view shows version, database size and free disk, STT, daemons and SMTP.
  test(`instance view, ${vp.name}`, async ({ browser }) => {
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'instance')).toBe(201);
    const dl = page.locator('#instance');
    await expect(dl).toContainText(/Versionv\d+\.\d+\.\d+/);
    await expect(dl).toContainText(/Database\d+(\.\d)? (KB|MB)/);
    await expect(dl).toContainText(/Disk\d+(\.\d)? (MB|GB|TB) free of \d+(\.\d)? (GB|TB)/);
    await expect(dl).toContainText('Speech to textoff: no audio');
    await expect(dl).toContainText(/Daemons\d+ connected/);
    await expect(dl).toContainText('Email (SMTP)not configured');
    await fits(page, vp.width);
  });
}
