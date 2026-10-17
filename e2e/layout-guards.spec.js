import { request as pwRequest } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, menuItem, appointAdmin, personaSheet, startDaemon } from './helpers.js';

// Layout guards of the /admin lists, the room sheets and the data pages (004b-AC16): one primary
// action per view, no action past an edge, a phone's list in short cards with no sideways scroll,
// and data pages near the full width of a wide screen.
const admin = { Authorization: `Bearer ${adminToken}` };
const sizes = { phone: { width: 390, height: 844 }, desktop: { width: 1440, height: 900 }, wide: { width: 1920, height: 1080 } };

// probe measures what is visible inside scope: the primary buttons enabled, the controls that an
// edge or a scroll box cuts, the boxes that scroll sideways, the tallest list row, and the share
// of scope's width the widest list takes.
const probe = (page, scope) => page.locator(scope).first().evaluate((root) => {
  const swatch = Object.assign(document.createElement('i'), { hidden: false });
  swatch.style.background = 'var(--primary)';
  document.body.append(swatch);
  const primary = getComputedStyle(swatch).backgroundColor;
  swatch.remove();
  const shown = (e) => e.getClientRects().length > 0 && getComputedStyle(e).visibility !== 'hidden';
  const name = (e) => `${e.tagName.toLowerCase()}${e.id ? '#' + e.id : ''} "${(e.getAttribute('aria-label') || e.textContent).trim().slice(0, 30)}"`;
  const controls = [...root.querySelectorAll('button, a.btn, [popovertarget], input, select')].filter(shown);
  const primaries = controls.filter((e) => !e.disabled && getComputedStyle(e).backgroundColor === primary).map(name);
  const clipped = controls.filter((e) => {
    const r = e.getBoundingClientRect();
    if (r.left < -0.5 || r.right > innerWidth + 0.5) return true;
    for (let a = e.parentElement; a && a !== document.body; a = a.parentElement) {
      if (getComputedStyle(a).overflowX === 'visible') continue;
      const b = a.getBoundingClientRect();
      if (r.left < b.left - 0.5 || r.right > b.right + 0.5) return true;
    }
    return false;
  }).map(name);
  const sideways = [root, ...root.querySelectorAll('*')].filter((e) => shown(e) && !e.matches('.tabs, .tabs *')
    && ['auto', 'scroll'].includes(getComputedStyle(e).overflowX) && e.scrollWidth > e.clientWidth + 1).map(name);
  const rows = [...root.querySelectorAll('.list-row:not(.head), tbody tr')].filter(shown).map((e) => e.getBoundingClientRect().height);
  const lists = [...root.querySelectorAll('.list-rows, table')].filter(shown).map((e) => e.getBoundingClientRect().width);
  const width = root.getBoundingClientRect().width - parseFloat(getComputedStyle(root).paddingLeft) - parseFloat(getComputedStyle(root).paddingRight);
  return { primaries, clipped, sideways, tallest: Math.max(0, ...rows), share: lists.length ? Math.max(...lists) / width : 1, width,
    pageOverflow: document.documentElement.scrollWidth - innerWidth };
});

// member joins room through an API context, which keeps the profile its join creates.
async function member(baseURL, request, room, name) {
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await pwRequest.newContext({ baseURL });
  expect((await ctx.post('/api/join', { data: { invite: inv.token, name } })).status()).toBe(201);
  return ctx;
}

for (const [size, viewport] of Object.entries(sizes)) {
  // 004b-AC1 004b-AC3 004b-AC5 004b-AC6: every /admin view and room tab, at 390, 1440 and 1920 px.
  test(`/admin views, ${size}`, async ({ browser, request, baseURL }) => {
    test.setTimeout(90_000);
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: `Layout ${size} ${'long name '.repeat(7)}${Date.now()}` } })).json();
    const people = [];
    for (const n of ['Ada', 'Bo']) people.push(await member(baseURL, request, room, n + ' ' + size));
    // The page's own session expires 12 h on, which shows at a two-digit hour ("10:42 AM") in
    // every run: at a one-digit one its phone row fits a value line less, and the guard passed by
    // the time of day.
    const off = (((10 - new Date().getUTCHours()) % 12) + 12) % 12;
    const page = await (await browser.newContext({ viewport, timezoneId: `Etc/GMT${off ? '-' + off : ''}` })).newPage();
    expect(await signIn(page, adminToken, 'rooms')).toBe(201);
    const views = ['rooms', 'profiles', 'libraries', 'instance', 'sessions', 'audit',
      ...['general', 'invites', 'admins', 'export'].map((t) => `rooms/${room.id}/${t}`)];
    for (const view of views) {
      await page.goto('/admin#' + view);
      await expect(page.locator('main h2').first()).toBeVisible();
      // A list draws once its data came: wait for one row, or the view's empty line.
      await expect(page.locator('main :is(.list-row:not(.head), tbody tr, li, .empty, form, dl, .warn)').filter({ visible: true }).first()).toBeVisible();
      const m = await probe(page, 'main');
      expect.soft(m.primaries.length, `${view}: primaries ${m.primaries}`).toBeLessThanOrEqual(1);
      expect.soft(m.clipped, `${view}: clipped`).toEqual([]);
      expect.soft(m.pageOverflow, `${view}: page wider than the screen`).toBeLessThanOrEqual(0);
      if (size === 'phone') {
        expect.soft(m.sideways, `${view}: scrolls sideways`).toEqual([]);
        expect.soft(m.tallest, `${view}: tallest row`).toBeLessThanOrEqual(120);
      }
      if (size === 'wide') {
        if (view === 'instance') expect.soft(m.width, 'a form keeps 72rem').toBeLessThanOrEqual(72 * 16);
        else {
          expect.soft(m.width, `${view}: page width`).toBeGreaterThanOrEqual(viewport.width - 64);
          expect.soft(m.share, `${view}: list spans the page`).toBeGreaterThanOrEqual(0.9);
        }
      }
    }
    // 004b-AC2: a recovery link, once made, widens nothing: every action stays in reach.
    await page.goto('/admin#profiles');
    await expect(page.locator('#profiles :is(.list-row:not(.head), tbody tr)').first()).toBeVisible();
    const more = page.locator('main [aria-label^="More actions on"]').first();
    if (await more.count()) await more.click();
    await page.getByRole('button', { name: /^Generate a recovery link for / }).first().click();
    await expect(page.getByRole('textbox', { name: /^Recovery link for / })).toBeVisible();
    const after = await probe(page, 'main');
    expect.soft(after.clipped, 'profiles with a recovery link: clipped').toEqual([]);
    expect.soft(after.pageOverflow, 'profiles with a recovery link: page wider than the screen').toBeLessThanOrEqual(0);
    await page.keyboard.press('Escape');

    // 004b-AC4: Admins in columns, the admin button at one x on every row.
    await page.goto(`/admin#rooms/${room.id}/admins`);
    const rows = page.locator('#room-admins > li');
    await expect(rows).toHaveCount(2);
    if (size !== 'phone') {
      const xs = await rows.locator('.admin-toggle').evaluateAll((bs) => bs.map((b) => Math.round(b.getBoundingClientRect().left)));
      expect.soft(new Set(xs).size, `admin buttons at ${xs}`).toBe(1);
    }
    for (const p of people) await p.dispose();
  });
}

// 004b-AC9, daemon-r2: the daemon page's tabs take the wide screen, General, Network and Update
// too; AI accounts keeps the 72rem column.
test('daemon page widths at 1920 px', async ({ browser, request, baseURL }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Wide daemon' } })).json();
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const page = await (await browser.newContext({ viewport: sizes.wide })).newPage();
    await page.goto(await daemon.link);
    for (const [tab, wide] of [['rooms', true], ['monitoring', true], ['general', true], ['network', true], ['update', true], ['accounts', false]]) {
      await page.locator('#tab-' + tab).click();
      await expect(page.locator('#tab-' + tab)).toHaveAttribute('aria-current', 'page');
      const w = () => page.locator('#main').evaluate((e) => e.getBoundingClientRect().width);
      if (wide) await expect.poll(w, { message: tab }).toBeGreaterThanOrEqual(sizes.wide.width - 64);
      else await expect.poll(w, { message: tab }).toBeLessThanOrEqual(72 * 16);
    }
  } finally {
    daemon.cleanup();
  }
});

for (const [size, viewport] of Object.entries({ phone: sizes.phone, desktop: sizes.desktop })) {
  // 004b-AC10 004b-AC11 004b-AC12 004b-AC13: the room's sheets.
  test(`room sheets, ${size}`, async ({ browser, request }) => {
    test.setTimeout(60_000);
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(viewport);
    await appointAdmin(alice, request, room.id);

    // One Save for the persona sheet, on every tab, with what it would save (004b-AC10).
    const sheet = await personaSheet(alice, 'Grill', 'Identity');
    await sheet.getByLabel('Name').fill('Grill2');
    for (const tab of ['Model', 'Triggers', 'Capabilities', 'Identity']) {
      await sheet.locator('#persona-tabs').getByRole('link', { name: tab, exact: true }).click();
      await expect(sheet.getByRole('button', { name: 'Save', exact: true })).toHaveCount(1);
      await expect.soft(sheet.locator('.settings-foot .dirty')).toHaveText('1 unsaved change');
      await expect(sheet.getByRole('button', { name: 'Save', exact: true })).toBeInViewport();
    }
    await sheet.getByLabel('Name').fill('Grill');
    await expect.soft(sheet.locator('.settings-foot .dirty')).toHaveText('No unsaved changes');
    await expect.soft(sheet.getByRole('button', { name: 'Save', exact: true })).toBeDisabled();
    await sheet.getByRole('button', { name: 'Close' }).click();
    await alice.keyboard.press('Escape');

    // Room settings: one heading row above the tabs, Delete last on General, one primary per tab (004b-AC11).
    await alice.goto(alice.url().split('#')[0] + '#manage/general');
    const manage = alice.locator('#manage');
    await expect(manage.locator('#room-panel')).toBeVisible();
    const top = await manage.evaluate((d) => d.getBoundingClientRect().top);
    const tabsTop = await manage.locator('#room-panel nav.tabs').evaluate((n) => n.getBoundingClientRect().top);
    expect.soft(tabsTop - top, 'heading above the tabs').toBeLessThanOrEqual(80);
    const del = await manage.locator('#delete-confirm').evaluate((e) => e.getBoundingClientRect().top);
    const starter = await manage.locator('#save-starter').evaluate((e) => e.getBoundingClientRect().top);
    expect.soft(del, 'Delete comes last').toBeGreaterThan(starter);
    for (const t of ['general', 'invites', 'admins', 'export']) {
      await alice.goto(alice.url().split('#')[0] + '#manage/' + t);
      await expect(manage.locator('#panel-' + t)).toBeVisible();
      const m = await probe(alice, '#manage');
      expect.soft(m.primaries.length, `${t}: ${m.primaries}`).toBeLessThanOrEqual(1);
      expect.soft(m.clipped, t).toEqual([]);
    }
    await manage.getByRole('button', { name: 'Close' }).first().click();

    // Notifications: full screen on a phone, the push explanation before the tab-title setting (004b-AC12).
    await menuItem(alice, 'Notifications', 'me');
    const notify = alice.locator('#notify');
    const box = await notify.boundingBox();
    if (size === 'phone') expect.soft(box).toEqual({ x: 0, y: 0, width: viewport.width, height: viewport.height });
    const hint = await notify.locator('p.hint').first().boundingBox();
    const count = await notify.locator('#count-voice').boundingBox();
    expect.soft(count.y, 'the tab-title setting comes after the explanation').toBeGreaterThan(hint.y);
    await notify.getByRole('button', { name: 'Close' }).click();

    // Diagnostics: no label or value wraps on a desktop (004b-AC13).
    if (size === 'desktop') {
      await menuItem(alice, 'Diagnostics', 'me');
      await expect(alice.locator('#diag-net dd').first()).toBeVisible();
      const wrapped = await alice.locator('#diag :is(dt, dd)').evaluateAll((els) => els
        .filter((e) => e.getClientRects().length && e.getBoundingClientRect().height > parseFloat(getComputedStyle(e).lineHeight) * 1.5)
        .map((e) => e.textContent));
      expect.soft(wrapped).toEqual([]);
    }
  });
}
