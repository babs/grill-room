import { request as pwRequest } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, menuItem, appointAdmin, personaSheet, startDaemon, openDaemonPage, unfoldTabs, uniq, upload, editPersona } from './helpers.js';

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
  // A row's text, its lines split by " | ": a failure names the row and what took its height.
  const rows = [...root.querySelectorAll('.list-row:not(.head), tbody tr')].filter(shown)
    .map((e) => ({ height: e.getBoundingClientRect().height, text: e.innerText.trim().replace(/\s*\n\s*/g, ' | ') }));
  const tallest = rows.reduce((a, r) => (r.height > a.height ? r : a), { height: 0, text: '' });
  const lists = [...root.querySelectorAll('.list-rows, table')].filter(shown).map((e) => e.getBoundingClientRect().width);
  const width = root.getBoundingClientRect().width - parseFloat(getComputedStyle(root).paddingLeft) - parseFloat(getComputedStyle(root).paddingRight);
  return { primaries, clipped, sideways, tallest: tallest.height, tallestText: tallest.text, share: lists.length ? Math.max(...lists) / width : 1, width,
    pageOverflow: document.documentElement.scrollWidth - innerWidth };
});

// uncapped names what in scope runs past its measure (docs/design.md §6): a hint past 70ch of its
// own font (the runner's fonts differ from a workstation's), a form or a facts list past 48rem.
const uncapped = (page, scope = 'main') => page.locator(scope).first().evaluate((root) => {
  const ctx = document.createElement('canvas').getContext('2d');
  const shown = (e) => e.getClientRects().length > 0;
  const name = (e) => `${e.tagName.toLowerCase()}${e.id ? '#' + e.id : ''}.${e.className} "${e.textContent.trim().slice(0, 30)}"`;
  const prose = [...root.querySelectorAll('.hint, .home-main > :is(#status, #confirm)')].filter(shown).filter((e) => {
    ctx.font = getComputedStyle(e).font;
    return e.getBoundingClientRect().width / ctx.measureText('0').width > 70.5;
  });
  const forms = [...root.querySelectorAll(':is(.settings-group, .stack-form, .profile-facts)')].filter(shown)
    .filter((e) => e.getBoundingClientRect().width > 48 * 16 + 0.5);
  return [...prose, ...forms].map(name);
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
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: `Layout ${size} ${'long name '.repeat(7)}${uniq()}` } })).json();
    const people = [];
    // A name at the 40-character cap, two lines unclamped on a phone.
    for (const n of ['Ada ' + size, 'Bo ' + size, 'Cy with a rather long name to wrap twice']) people.push(await member(baseURL, request, room, n));
    // The page's own session expires 12 h on, which shows at a two-digit hour ("10:42 AM") in
    // every run: at a one-digit one its phone row fits a value line less, and the guard passed by
    // the time of day.
    const off = (((10 - new Date().getUTCHours()) % 12) + 12) % 12;
    const page = await (await browser.newContext({ viewport, timezoneId: `Etc/GMT${off ? '-' + off : ''}` })).newPage();
    expect(await signIn(page, adminToken, 'rooms')).toBe(201);
    const views = ['rooms', 'profiles', 'libraries', 'pronunciation', 'instance', 'sessions', 'audit',
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
        expect.soft(m.tallest, `${view}: tallest row, ${m.tallestText}`).toBeLessThanOrEqual(120);
      }
      if (size === 'wide') {
        expect.soft(m.width, `${view}: page width`).toBeGreaterThanOrEqual(viewport.width - 64);
        if (view !== 'instance') expect.soft(m.share, `${view}: list spans the page`).toBeGreaterThanOrEqual(0.9);
        expect.soft(await uncapped(page), `${view}: past its measure`).toEqual([]);
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
    await expect(rows).toHaveCount(people.length);
    if (size !== 'phone') {
      const xs = await rows.locator('.admin-toggle').evaluateAll((bs) => bs.map((b) => Math.round(b.getBoundingClientRect().left)));
      expect.soft(new Set(xs).size, `admin buttons at ${xs}`).toBe(1);
    }
    for (const p of people) await p.dispose();
  });
}

// 004b-AC9, daemon-r2: every tab of the daemon page takes the wide screen; its prose and forms keep
// their measure (user, 2026-10-18).
test('daemon page widths at 1920 px', async ({ browser, request, baseURL }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Wide daemon' } })).json();
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const page = await (await browser.newContext({ viewport: sizes.wide })).newPage();
    await openDaemonPage(page, await daemon.link);
    for (const tab of ['rooms', 'monitoring', 'general', 'network', 'update', 'accounts']) {
      await page.locator('#tab-' + tab).click();
      await expect(page.locator('#tab-' + tab)).toHaveAttribute('aria-current', 'page');
      const w = () => page.locator('#main').evaluate((e) => e.getBoundingClientRect().width);
      await expect.poll(w, { message: tab }).toBeGreaterThanOrEqual(sizes.wide.width - 64);
      expect.soft(await uncapped(page, '#main'), `${tab}: past its measure`).toEqual([]);
    }
  } finally {
    daemon.cleanup();
  }
});

// The profile takes the wide screen as /admin's data views do; its prose and forms keep their
// measure (docs/design.md §6, user 2026-10-18).
test('profile widths at 1920 px', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  await pat.setViewportSize(sizes.wide);
  await pat.goto('/profile');
  const grid = pat.locator('#rooms');
  await expect(grid.locator('li.card').first()).toBeVisible();
  const g = await grid.evaluate((e) => ({ width: e.getBoundingClientRect().width, cols: getComputedStyle(e).gridTemplateColumns.split(' ').length }));
  expect.soft(g.width, 'rooms grid spans the page').toBeGreaterThanOrEqual(sizes.wide.width - 64);
  expect.soft(g.cols, 'room card columns').toBeGreaterThan(3);
  // The tabs as the bar lists them, so a tab added later is measured too.
  const tabs = await pat.locator('#profile-tabs a').evaluateAll((as) => as.map((a) => new URL(a.href).hash.slice(1)));
  expect(tabs).toContain('personas');
  for (const tab of tabs) {
    await pat.goto('/profile#' + tab);
    await expect(pat.locator('#panel-' + tab)).toBeVisible();
    // The status line and the confirm card show only on a magic link's landing: fill them in place.
    if (tab === 'rooms') {
      await pat.locator('#status').evaluate((e) => { e.textContent = 'x '.repeat(200); });
      await pat.locator('#confirm').evaluate((e) => { e.hidden = false; e.querySelector('p').textContent = 'x '.repeat(200); });
    }
    expect.soft(await uncapped(pat), `${tab}: past its measure`).toEqual([]);
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
    await editPersona(sheet, () => sheet.getByLabel('Name').fill('Grill2'));
    for (const tab of ['Model', 'Triggers', 'Capabilities', 'Identity']) {
      await unfoldTabs(sheet.locator('#persona-tabs'));
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

// folds reads the side drawers' folds on screen and in storage, as [people, docs].
const folds = (page) => page.evaluate(() => ['people', 'docs'].map((n) => [document.body.classList.contains('fold-' + n), localStorage.getItem('grill-room-fold-' + n)]));
// tablet measures the room beside a document: the side panel and chat widths, how far Send sits
// off the box's row, and the page's sideways overflow.
const tablet = (page) => page.evaluate(() => {
  const r = (s) => document.querySelector(s).getBoundingClientRect();
  const [box, send] = [r('#body'), r('#send > [type=submit]')];
  return { side: r('#side').width, chat: r('.chat').width, offRow: Math.abs(box.top + box.height / 2 - (send.top + send.height / 2)),
    sendAfter: send.left >= box.right, overflow: document.documentElement.scrollWidth - innerWidth };
});
// pastViewer names what in the viewer ends past its right edge, over the chat.
const pastViewer = (page) => page.locator('#viewer').evaluate((v) => {
  const edge = v.getBoundingClientRect().right + 0.5;
  return [...v.querySelectorAll('*')].filter((e) => e.getClientRects().length && e.getBoundingClientRect().right > edge)
    .map((e) => `${e.tagName.toLowerCase()}${e.id ? '#' + e.id : ''}`);
});

// 037-AC1 037-AC2: from 40 to 64rem a document folds People and Documents to the rail, unstored;
// the chat keeps 22rem with Send on the box's row. Its close gives back the folds from before, but
// one the person changed meanwhile; leaving the band and coming back undoes and redoes the fold, but
// that one. The docs window folds nothing, nor the Video stage beside drawers folded by hand.
for (const viewport of [{ width: 820, height: 1180 }, { width: 768, height: 1024 }]) {
  test(`a document on a tablet, ${viewport.width} px`, async ({ browser, request }) => {
    test.setTimeout(60_000);
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(viewport);
    const open = () => alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    const close = () => alice.locator('#doc-close').click();
    const fitted = async () => {
      await expect.poll(async () => Math.round((await tablet(alice)).side)).toBe(56);
      const m = await tablet(alice);
      expect.soft(m.chat, 'chat width').toBeGreaterThanOrEqual(352);
      expect.soft(m.offRow, 'Send off the box row').toBeLessThanOrEqual(1);
      expect.soft(m.sendAfter, 'Send after the box').toBe(true);
      expect.soft(m.overflow, 'page wider than the screen').toBeLessThanOrEqual(0);
    };

    // Unfolded before: both fold, nothing stored; back unfolded on close.
    await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
    await expect(alice.locator('#viewer')).toBeVisible();
    await expect.poll(() => folds(alice)).toEqual([[true, null], [true, null]]);
    await fitted();
    await alice.setViewportSize({ width: 1100, height: viewport.height });
    await expect.poll(() => folds(alice)).toEqual([[false, null], [false, null]]);
    await alice.setViewportSize(viewport);
    await expect.poll(() => folds(alice)).toEqual([[true, null], [true, null]]);
    await fitted();
    // The band's narrow end: the viewer keeps its toolbar and frame to itself.
    await alice.setViewportSize({ width: 656, height: viewport.height });
    await fitted();
    expect.soft(await pastViewer(alice), 'past the viewer at 656 px').toEqual([]);
    await alice.setViewportSize(viewport);
    await close();
    await expect.poll(() => folds(alice)).toEqual([[false, null], [false, null]]);

    // People folded before: it stays folded on close, Documents unfolds.
    await alice.locator('#people-fold').click();
    await open();
    await expect.poll(() => folds(alice)).toEqual([[true, '1'], [true, null]]);
    await fitted();
    await close();
    await expect.poll(() => folds(alice)).toEqual([[true, '1'], [false, null]]);

    // People unfolded while the document is open: the person's choice outlasts the close.
    await open();
    await expect.poll(() => folds(alice)).toEqual([[true, '1'], [true, null]]);
    await alice.locator('#people-fold').click();
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [true, null]]);
    expect.soft((await tablet(alice)).overflow, 'page wider than the screen').toBeLessThanOrEqual(0);
    // A turn of the tablet and back folds Documents again, not People.
    await alice.setViewportSize({ width: 1180, height: viewport.height });
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [false, null]]);
    await alice.setViewportSize(viewport);
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [true, null]]);
    await close();
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [false, null]]);
    // Unfolded and folded back by the person, People stays folded on close.
    await open();
    await alice.locator('#people-fold').click();
    await alice.locator('#people-fold').click();
    await expect.poll(() => folds(alice)).toEqual([[true, '1'], [true, null]]);
    await close();
    await expect.poll(() => folds(alice)).toEqual([[true, '1'], [false, null]]);

    // That choice holds for its document only: reopened, or another one shown, People folds again.
    const show = async (name) => {
      await upload(alice, name, 'text/markdown', '# ' + name + '\n');
      await expect(alice.locator('#doc-name')).toHaveText(name);
    };
    await open();
    await alice.locator('#people-fold').click();
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [true, null]]);
    await close();
    await open();
    await expect.poll(() => folds(alice)).toEqual([[true, '0'], [true, null]]);
    // Documents unfolded for one document folds for the next, the chat back to 22rem.
    await alice.locator('#docs-fold').click();
    await expect.poll(() => folds(alice)).toEqual([[true, '0'], [false, '0']]);
    await show('notes.md');
    await expect.poll(() => folds(alice)).toEqual([[true, '0'], [true, '0']]);
    await fitted();
    await alice.locator('#people-fold').click();
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [true, '0']]);
    await show('third.md');
    await expect.poll(() => folds(alice)).toEqual([[true, '0'], [true, '0']]);
    await alice.setViewportSize({ width: 1180, height: viewport.height });
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [false, '0']]);
    await alice.setViewportSize(viewport);
    await expect.poll(() => folds(alice)).toEqual([[true, '0'], [true, '0']]);
    await close();
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [false, '0']]);

    // A drawer set on a desktop is no choice for the tablet: there, it folds.
    await alice.setViewportSize(sizes.desktop);
    await open();
    await alice.locator('#people-fold').click();
    await alice.locator('#people-fold').click();
    await alice.setViewportSize(viewport);
    await expect.poll(() => folds(alice)).toEqual([[true, '0'], [true, '0']]);
    await fitted();
    await close();
    await expect.poll(() => folds(alice)).toEqual([[false, '0'], [false, '0']]);

    // The docs window folds nothing, and keeps its toolbar on one row.
    const docs = await alice.context().newPage();
    await docs.setViewportSize(viewport);
    await docs.goto(`/r/${room.id}/docs`);
    await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    await expect(docs.locator('#viewer')).toBeVisible();
    expect(await folds(docs)).toEqual([[false, '0'], [false, '0']]);
    expect.soft((await docs.locator('#viewer > .viewer-head').boundingBox()).height, 'docs window head').toBeLessThanOrEqual(57);
    await docs.close();

    await alice.locator('#people-fold').click();
    await alice.locator('#docs-fold').click();
    await alice.locator('#video-open').evaluate((b) => { b.hidden = false; b.click(); });
    await expect(alice.locator('#stage')).toBeVisible();
    expect.soft((await tablet(alice)).chat, 'chat beside the stage').toBeLessThan(352);
  });
}

// 037-AC2: under 40rem and from 64rem, a document folds nothing.
test('a document on a phone and a desktop folds nothing', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(sizes.desktop);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  expect(await folds(alice)).toEqual([[false, null], [false, null]]);
  expect((await tablet(alice)).side).toBeGreaterThan(56);
  await alice.locator('#doc-close').click();
  await alice.setViewportSize(sizes.phone);
  await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
  await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(alice.locator('#viewer')).toBeVisible();
  expect(await folds(alice)).toEqual([[false, null], [false, null]]);
});
