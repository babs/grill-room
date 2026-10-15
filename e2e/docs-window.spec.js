import { test, expect } from './fixtures.js';
import { readFileSync } from 'node:fs';
import { adminToken } from './playwright.config.js';
import { roomWith, upload, smallTargets, noOverflow } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const desktop = { width: 1440, height: 900 };
const wide = { width: 1920, height: 1080 };
const phone = { width: 390, height: 844 };

const box = (page, sel) => page.locator(sel).boundingBox();
const width = async (page, sel) => (await box(page, sel)).width;
// stored reads the widths and folds this browser keeps (008-AC4).
const stored = (page, key) => page.evaluate((k) => localStorage.getItem(k), key);

// docsWindow opens the room's document manager in a page of its own, at size.
async function docsWindow(page, room, size, path = '') {
  const docs = await page.context().newPage();
  await docs.setViewportSize(size);
  await docs.goto(`/r/${room.id}/docs${path}`);
  await expect(docs.locator('#room-name a')).toHaveText('Docs room');
  return docs;
}

// drag moves a resize handle to x, as a mouse would.
async function drag(page, handle, x) {
  const b = await box(page, handle);
  await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
  await page.mouse.down();
  await page.mouse.move(x, b.y + b.height / 2, { steps: 4 });
  await page.mouse.up();
}

// 008-AC1 008-AC2: a steady list beside an empty state, a handle that drags within 192..480 px and
// moves by 16 px with the arrows.
test('the docs window list keeps its width and resizes', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await expect(alice.locator('#documents')).toContainText('plan.md');
  const docs = await docsWindow(alice, room, desktop);

  await expect(docs.locator('#viewer-empty')).toHaveText('Pick a document to read it here.');
  expect(await width(docs, '#side')).toBeCloseTo(256, 0);
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(docs.locator('#viewer-empty')).toBeHidden();
  expect(await width(docs, '#side')).toBeCloseTo(256, 0);
  await docs.locator('#doc-close').click();
  await expect(docs.locator('#viewer-empty')).toBeVisible();
  expect(await width(docs, '#side')).toBeCloseTo(256, 0);

  const handle = docs.getByRole('separator', { name: 'Resize the side panel' });
  await expect(handle).toHaveAttribute('aria-valuemin', '192');
  await expect(handle).toHaveAttribute('aria-valuemax', '480');
  await expect(handle).toHaveAttribute('aria-valuenow', '256');
  const hb = await handle.boundingBox();
  expect(hb.width).toBeGreaterThanOrEqual(12);
  expect(hb.height).toBeCloseTo((await box(docs, '#side')).height, 0);

  await drag(docs, '#side-handle', 100);
  expect(await width(docs, '#side')).toBeCloseTo(192, 0);
  await drag(docs, '#side-handle', 900);
  expect(await width(docs, '#side')).toBeCloseTo(480, 0);
  await drag(docs, '#side-handle', 300);
  expect(await width(docs, '#side')).toBeCloseTo(300, 0);
  expect(await stored(docs, 'grill-room-list-w')).toBe('300');

  await handle.focus();
  await docs.keyboard.press('ArrowLeft');
  await expect(handle).toHaveAttribute('aria-valuenow', '284');
  expect(await width(docs, '#side')).toBeCloseTo(284, 0);
  await docs.keyboard.press('ArrowRight');
  await docs.keyboard.press('ArrowRight');
  await expect(handle).toHaveAttribute('aria-valuenow', '316');
  expect(await stored(docs, 'grill-room-list-w')).toBe('316');
  // The room page has the same handle on its side panel.
  await expect(alice.locator('#side-handle')).toBeVisible();
});

// 008-AC3 008-AC4: Notes resizes within 256..640 px; a narrow window takes from Notes first, then
// from the list, never from the document's 384 px, and never from what is stored. The widths come
// back after a reload, on the room page and in another room.
test('notes resize, give way first, and widths persist', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await expect(alice.locator('#documents')).toContainText('plan.md');
  const docs = await docsWindow(alice, room, desktop);
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(docs.locator('#annotations')).toBeVisible();

  const notes = docs.getByRole('separator', { name: 'Resize the notes' });
  await expect(notes).toHaveAttribute('aria-valuenow', '320');
  expect(await width(docs, '#annotations')).toBeCloseTo(320, 0);
  await drag(docs, '#notes-handle', 1400);
  expect(await width(docs, '#annotations')).toBeCloseTo(256, 0);
  await drag(docs, '#notes-handle', 100);
  expect(await width(docs, '#annotations')).toBeCloseTo(640, 0);
  await notes.focus();
  await docs.keyboard.press('ArrowRight');
  await expect(notes).toHaveAttribute('aria-valuenow', '624');

  // The list at its widest: at 1024 px Notes goes to its minimum, then the list gives way.
  await docs.locator('#side-handle').focus();
  for (let i = 0; i < 15; i++) await docs.keyboard.press('ArrowRight');
  await expect(docs.locator('#side-handle')).toHaveAttribute('aria-valuenow', '480');
  await docs.setViewportSize({ width: 1180, height: 900 });
  expect(await width(docs, '#side')).toBeCloseTo(480, 0);
  expect(await width(docs, '#annotations')).toBeLessThan(624);
  await docs.setViewportSize({ width: 1024, height: 900 });
  expect(await width(docs, '#annotations')).toBeCloseTo(256, 0);
  expect(await width(docs, '#side')).toBeLessThan(480);
  expect(await width(docs, '#doc-frame')).toBeGreaterThanOrEqual(384);
  // Under 64rem nothing resizes, and nothing stored changes.
  await docs.setViewportSize({ width: 900, height: 900 });
  await expect(docs.locator('#side-handle')).toBeHidden();
  await docs.setViewportSize(desktop);
  expect(await stored(docs, 'grill-room-list-w')).toBe('480');
  expect(await stored(docs, 'grill-room-notes-w')).toBe('624');

  await docs.locator('#side-handle').focus();
  for (let i = 0; i < 12; i++) await docs.keyboard.press('ArrowLeft');
  await notes.focus();
  for (let i = 0; i < 9; i++) await docs.keyboard.press('ArrowRight');
  await docs.reload();
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  expect(await width(docs, '#side')).toBeCloseTo(288, 0);
  expect(await width(docs, '#annotations')).toBeCloseTo(480, 0);

  // The room page, the chat folded so the viewer has the room.
  await alice.setViewportSize(desktop);
  await alice.reload();
  await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await alice.locator('#chat-fold').click();
  expect(await width(alice, '#side')).toBeCloseTo(288, 0);
  expect(await width(alice, '#annotations')).toBeCloseTo(480, 0);
  await alice.locator('#chat-unfold').click();

  // Another room, in the same browser.
  const other = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Other room' } })).json();
  const inv = await (await request.post(`/api/rooms/${other.id}/invites`, { headers: admin })).json();
  await alice.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await alice.getByLabel('Your name').fill('Alice');
  await alice.getByRole('button', { name: 'Join the room' }).click();
  await expect(alice.locator('#room-name')).toHaveText('Other room');
  expect(await width(alice, '#side')).toBeCloseTo(288, 0);
});

// 008-AC5: the docs window folds its list to a rail and the document takes the width; the room
// page folds People and Documents one by one, and becomes a rail once both are folded.
test('drawers fold, one by one, and stay folded', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await expect(alice.locator('#documents')).toContainText('plan.md');
  const docs = await docsWindow(alice, room, desktop);
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(docs.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Plan');
  await expect(docs.locator('#people-fold')).toBeHidden();
  // The triangle names its drawer and points down while open, right once folded.
  const fold = docs.getByRole('button', { name: 'Documents', exact: true });
  await expect(fold).toHaveAttribute('aria-expanded', 'true');
  const turn = () => docs.locator('#docs-fold').evaluate((e) => getComputedStyle(e, '::before').transform);
  expect(await turn()).toBe('none');
  // 008-AC14: the triangle, the external-link action and + share one size, with 44 px to hit.
  await alice.setViewportSize(desktop);
  const sizes = await alice.locator('#docs-pane .pane-head :is(.fold, .head-action)').evaluateAll((els) => els.map((e) => {
    const r = e.getBoundingClientRect();
    const hit = getComputedStyle(e, '::after');
    return [Math.round(r.width), Math.round(r.height), Math.round(r.width - 2 * parseFloat(hit.left)), Math.round(r.height - 2 * parseFloat(hit.top))];
  }));
  expect(sizes).toEqual([[24, 24, 44, 44], [24, 24, 44, 44], [24, 24, 44, 44]]);
  const before = await width(docs, '#doc-frame');
  // A click anywhere on the header folds it, its actions aside.
  await docs.locator('#docs-title').click();
  await expect(fold).toHaveAttribute('aria-expanded', 'false');
  expect(await turn()).not.toBe('none');
  expect(await width(docs, '#side')).toBeCloseTo(56, 0);
  await expect(docs.locator('#docs-pane .sec-icon')).toBeVisible();
  expect(await width(docs, '#doc-frame')).toBeCloseTo(before + 256 - 56, 0);
  await expect(docs.locator('#documents')).toBeHidden();
  await expect(docs.locator('#side-handle')).toBeHidden();
  await docs.reload();
  await expect(docs.locator('#docs-fold')).toHaveAttribute('aria-expanded', 'false');
  expect(await width(docs, '#side')).toBeCloseTo(56, 0);
  await docs.locator('#docs-fold').click();
  expect(await width(docs, '#side')).toBeCloseTo(256, 0);
  await expect(docs.locator('#documents')).toBeVisible();

  await alice.setViewportSize(desktop);
  await alice.locator('#people-fold').click();
  await expect(alice.locator('#participants')).toBeHidden();
  await expect(alice.locator('#documents')).toBeVisible();
  // The header's own actions do not fold it.
  await alice.locator('#upload-open').click();
  await expect(alice.locator('#docs-fold')).toHaveAttribute('aria-expanded', 'true');
  expect(await width(alice, '#side')).toBeCloseTo(256, 0);
  await alice.locator('#docs-fold').click();
  expect(await width(alice, '#side')).toBeCloseTo(56, 0);
  await alice.reload();
  await expect(alice.locator('#people-fold')).toHaveAttribute('aria-expanded', 'false');
  await expect(alice.locator('#docs-fold')).toHaveAttribute('aria-expanded', 'false');
  expect(await width(alice, '#side')).toBeCloseTo(56, 0);
  await alice.locator('#people-fold').click();
  expect(await width(alice, '#side')).toBeCloseTo(256, 0);
  await expect(alice.locator('#participants')).toBeVisible();
  await expect(alice.locator('#documents')).toBeHidden();
  expect(await stored(alice, 'grill-room-fold-docs')).toBe('1');
  expect(await stored(alice, 'grill-room-fold-people')).toBe('0');
});

// 008-AC6: the docs window's room name leads back to the room; the room page's does not.
test('the docs window links back to the room', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(alice.locator('#room-name a')).toHaveCount(0);
  const docs = await docsWindow(alice, room, phone);
  const back = docs.getByRole('link', { name: 'Docs room' });
  await expect(back).toHaveAttribute('data-tip', 'Back to the room');
  expect((await back.boundingBox()).height).toBeGreaterThanOrEqual(44);
  // Room events reload the header: the link stays the same element, with its focus, and follows a
  // rename.
  const held = await back.elementHandle();
  await back.focus();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  expect((await request.post('/api/join', { data: { invite: inv.token, name: 'Bob' } })).status()).toBeLessThan(300);
  expect((await request.patch(`/api/rooms/${room.id}`, { headers: admin, data: { name: 'Renamed room' } })).status()).toBeLessThan(300);
  await expect(docs.locator('#room-name a')).toHaveText('Renamed room');
  expect(await held.evaluate((e) => e.isConnected && document.activeElement === e)).toBe(true);
  await held.click();
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}$`));
  await expect(docs.locator('#messages')).toBeVisible();
});

const long = '# Plan\n\n' + Array.from({ length: 120 }, (_, i) => `Paragraph ${i} of the plan, long enough to wrap across the page at a narrow width and to read.`).join('\n\n') + '\n\n[Back to the top](#plan)\n';

// 008-AC7: Narrow, Large and Full, without reloading the frame, kept across a reload; no width
// button for HTML.
test('markdown width toggle', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', long);
  await upload(alice, 'page.html', 'text/html', '<!doctype html><title>p</title><h1>Page</h1>');
  await expect(alice.locator('#documents li')).toHaveCount(2);
  await alice.evaluate(() => { localStorage.setItem('grill-room-annotations', '0'); localStorage.setItem('grill-room-fold-docs', '1'); });
  const docs = await docsWindow(alice, room, wide);
  await docs.locator('#docs-fold').click();
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await docs.locator('#docs-fold').click();
  await expect(docs.locator('#annotations')).toBeHidden();
  const frame = docs.frameLocator('#doc-frame iframe');
  await expect(frame.locator('h1')).toHaveText('Plan');
  const measure = () => frame.locator('.container').evaluate((c) => ({
    w: c.getBoundingClientRect().width, room: document.body.clientWidth - parseFloat(getComputedStyle(document.body).paddingLeft) - parseFloat(getComputedStyle(document.body).paddingRight),
    marker: window.marker, y: Math.round(scrollY),
  }));
  const button = docs.getByRole('button', { name: /^Text width/ });
  await expect(button).toHaveAccessibleName('Text width: Narrow');
  let m = await measure();
  expect(m.w).toBeLessThanOrEqual(896);
  await frame.locator('body').evaluate(() => { window.marker = 'kept'; scrollTo(0, 600); });

  await button.click();
  await expect(button).toHaveAccessibleName('Text width: Large');
  await expect.poll(async () => (await measure()).w).toBeGreaterThan(896);
  m = await measure();
  expect(m.w).toBeLessThanOrEqual(1400);
  expect(m.marker).toBe('kept');
  expect(m.y).toBe(600);

  await button.click();
  await expect(button).toHaveAccessibleName('Text width: Full');
  await expect.poll(async () => { const x = await measure(); return x.w - x.room; }).toBeCloseTo(0, 0);
  expect((await measure()).marker).toBe('kept');

  await docs.reload();
  await docs.locator('#docs-fold').click();
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(button).toHaveAccessibleName('Text width: Full');
  await expect(frame.locator('h1')).toHaveText('Plan');
  m = await measure();
  expect(m.w).toBeGreaterThan(1400);
  expect(m.w).toBeCloseTo(m.room, 0);
  // 008-AC9: a link within the document scrolls it and adds no history entry.
  const entries = await docs.evaluate(() => history.length);
  await frame.locator('body').evaluate(() => scrollTo(0, document.body.scrollHeight));
  await frame.getByRole('link', { name: 'Back to the top' }).click();
  await expect.poll(() => frame.locator('body').evaluate(() => scrollY)).toBeLessThan(100);
  expect(await docs.evaluate(() => history.length)).toBe(entries);

  await docs.locator('#documents').getByRole('button', { name: 'page.html' }).click();
  await expect(frame.locator('h1')).toHaveText('Page');
  await expect(button).toBeHidden();
  // Word shows as its Markdown copy: the button is there.
  await upload(docs, 'plan.docx', 'application/octet-stream', readFileSync('../internal/convert/testdata/fixture.docx'));
  await expect(docs.locator('#doc-meta')).toContainText('Markdown copy');
  await expect(button).toBeVisible();
});

// 008-AC8: one ⋯ for Download, Open in a new window, Rename and Delete, with their rules (Download
// for uploads keeps its 005 rule), driven by the keyboard; at 390 px the title is one line in one row of tools.
test('document actions menu', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  const name = 'a-rather-long-document-name-for-the-plan-of-the-quarter-with-every-team-and-every-milestone.md';
  await upload(alice, name, 'text/markdown', '# Plan\n');
  await expect(alice.locator('#doc-name')).toHaveText(name);
  const order = await alice.locator('.viewer-head > *, .doc-tools > *').evaluateAll((els) => els
    .filter((e) => e.getBoundingClientRect().width && !e.classList.contains('doc-tools')).map((e) => e.id || e.className));
  expect(order).toEqual(['doc-close', 'doc-title', 'annot-toggle', 'doc-width', 'chat-fold', 'doc-more']);
  // One control height at 1440 px (005-AC30).
  const heights = await alice.locator('.viewer-head button:visible').evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().height)));
  expect(new Set(heights)).toEqual(new Set([36]));

  await alice.locator('#chat-fold').focus();
  await alice.keyboard.press('Tab');
  await expect(alice.locator('#doc-more')).toBeFocused();
  await alice.keyboard.press('Enter');
  const menu = alice.locator('#doc-menu');
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('link', { name: 'Download' })).toBeFocused();
  // Alice is no room admin: no Delete.
  await expect(menu.getByRole('button', { name: 'Delete' })).toBeHidden();
  await alice.keyboard.press('ArrowDown');
  await expect(menu.getByRole('link', { name: 'Open in a new window' })).toBeFocused();
  await alice.keyboard.press('Tab');
  await expect(menu.getByRole('button', { name: 'Rename' })).toBeFocused();
  await alice.keyboard.press('Escape');
  await expect(menu).toBeHidden();
  await expect(alice.locator('#doc-more')).toBeFocused();
  await alice.keyboard.press('Enter');
  await expect(menu.getByRole('link', { name: 'Download' })).toBeFocused();
  await alice.keyboard.press('ArrowUp');
  await expect(menu.getByRole('button', { name: 'Rename' })).toBeFocused();
  await alice.keyboard.press('Enter');
  await expect(menu).toBeHidden();
  await expect(alice.getByLabel('New name')).toBeFocused();
  await alice.locator('#doc-rename-cancel').click();

  // The docs window has no "Open in a new window"; a room admin gets Delete.
  const me = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  const id = (await alice.evaluate((r) => fetch(`/api/rooms/${r}/documents`).then((x) => x.json()), room.id))[0].id;
  // A long name never widens the viewer past the window.
  const wide1024 = await docsWindow(alice, room, { width: 1024, height: 800 }, '/' + id);
  await expect(wide1024.locator('#doc-name')).toHaveText(name);
  expect(await noOverflow(wide1024)).toEqual([]);
  await wide1024.close();
  const docs = await docsWindow(alice, room, phone, '/' + id);
  await docs.locator('#doc-more').click();
  const dmenu = docs.locator('#doc-menu');
  await expect(dmenu.getByRole('link', { name: 'Download' })).toBeFocused();
  await expect(dmenu.getByRole('link', { name: 'Open in a new window' })).toBeHidden();
  await expect(dmenu.getByRole('button', { name: 'Delete' })).toBeVisible();
  expect(await smallTargets(docs), 'doc menu at 390 px').toEqual([]);
  await docs.keyboard.press('Escape');

  // At 390 px: the title on one line of its own, ellipsed, the full name as its tip; one row of tools.
  const title = docs.locator('#doc-name');
  await expect(title).toHaveAttribute('data-tip', name);
  const t = await title.evaluate((e) => ({ lines: Math.round(e.getBoundingClientRect().height / parseFloat(getComputedStyle(e).lineHeight)), cut: e.scrollWidth > e.clientWidth }));
  expect(t).toEqual({ lines: 1, cut: true });
  const tops = await docs.locator('.doc-tools button:visible').evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().top)));
  expect(new Set(tops).size).toBe(1);
});

// 008-AC9: on a phone one pane at a time, list, document, Notes, and Back steps through them in
// reverse, in the docs window and in the room's Docs tab.
test('phone: one pane at a time, Back steps back', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await expect(alice.locator('#documents')).toContainText('plan.md');
  await alice.locator('#doc-close').click();

  const docs = await alice.context().newPage();
  await docs.setViewportSize(phone);
  await docs.goto('/profile');
  await docs.goto(`/r/${room.id}/docs`);
  const panes = async (page) => ({
    list: await page.locator('#documents').isVisible(), doc: await page.locator('#doc-frame').isVisible(), notes: await page.locator('#annotations').isVisible(),
  });
  const full = async (page, sel) => expect(await width(page, sel)).toBeCloseTo(phone.width, 0);
  for (const page of [docs, alice]) {
    if (page === alice) {
      await alice.setViewportSize(phone);
      await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
    }
    await expect(page.locator('#documents')).toBeVisible();
    for (const sel of ['#side-handle', '#notes-handle', '#docs-fold', '#people-fold']) expect(await box(page, sel), sel).toBeNull();
    expect(await smallTargets(page), 'list').toEqual([]);
    await page.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    await expect(page.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Plan');
    expect(await panes(page)).toEqual({ list: false, doc: true, notes: false });
    await full(page, '#viewer');
    expect(await smallTargets(page), 'document').toEqual([]);
    await page.getByRole('button', { name: /Notes/ }).click();
    expect(await panes(page)).toEqual({ list: false, doc: false, notes: true });
    await full(page, '#annotations');
    await expect(page.getByRole('button', { name: 'Back to the document' })).toBeVisible();
    expect(await smallTargets(page), 'notes').toEqual([]);
    await page.goBack();
    await expect(page.locator('#annotations')).toBeHidden();
    expect(await panes(page)).toEqual({ list: false, doc: true, notes: false });
    // Back then Forward brings Notes again, and one ✕ still closes the document.
    await page.getByRole('button', { name: /Notes/ }).click();
    await page.goBack();
    await expect(page.locator('#doc-frame')).toBeVisible();
    await page.goForward();
    await expect(page.locator('#annotations')).toBeVisible();
    await page.getByRole('button', { name: 'Close document' }).click();
    await expect(page.locator('#documents')).toBeVisible();
    await page.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    await expect(page.locator('#doc-frame')).toBeVisible();
    // A double tap on "← Back" steps back once.
    await page.getByRole('button', { name: /Notes/ }).click();
    await page.locator('#notes-back').evaluate((b) => { b.click(); b.click(); });
    await expect(page.locator('#doc-frame')).toBeVisible();
    await page.goBack();
    await expect(page.locator('#documents')).toBeVisible();
    expect(await panes(page)).toEqual({ list: true, doc: false, notes: false });
  }
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs$`));
  await docs.goBack();
  await expect(docs).toHaveURL(/\/profile$/);

  // A reload keeps the browser's entries but starts the page afresh: one ✕ still closes.
  await docs.goto(`/r/${room.id}/docs`);
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs/[^/]+$`));
  await docs.reload();
  await expect(docs.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Plan');
  await docs.getByRole('button', { name: 'Close document' }).click();
  await expect(docs.locator('#documents')).toBeVisible();
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs$`));
  // A double tap on ✕ steps back once: the docs window stays.
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(docs.locator('#doc-frame')).toBeVisible();
  await docs.getByRole('button', { name: /Notes/ }).click();
  await docs.locator('#doc-close').evaluate((b) => { b.click(); b.click(); });
  await expect(docs.locator('#documents')).toBeVisible();
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs$`));
  await docs.goBack();
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs$`));
  await expect(docs.locator('#documents')).toBeVisible();

  // Back from the Chat tab leaves the room, past the document still open in the Docs tab.
  await alice.goto('/profile');
  await alice.goto(`/r/${room.id}`);
  await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
  await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(alice.locator('#doc-frame')).toBeVisible();
  await alice.getByRole('button', { name: /Notes/ }).click();
  await alice.locator('#tabbar').getByRole('button', { name: /Chat/ }).click();
  await alice.goBack();
  await expect(alice).toHaveURL(/\/profile$/);
  await alice.goto(`/r/${room.id}`);

  // A document opened wide, then the phone turned upright: Notes, then ✕, stay in the room.
  await alice.setViewportSize({ width: 900, height: 844 });
  await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Plan');
  await alice.setViewportSize(phone);
  await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
  await alice.getByRole('button', { name: /Notes/ }).click();
  await expect(alice.locator('#annotations')).toBeVisible();
  await alice.getByRole('button', { name: 'Close document' }).click();
  await expect(alice.locator('#documents')).toBeVisible();
  await expect(alice).toHaveURL(new RegExp(`/r/${room.id}$`));
});

// 008-AC3: with the chat shown, the room page keeps the chat's share; the 384 px floor holds once
// the chat is folded.
test('room page: the chat keeps its share beside Notes', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 1024, height: 800 });
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#annotations')).toBeVisible();
  expect(await width(alice, '.chat')).toBeGreaterThanOrEqual(250);
  await alice.locator('#chat-fold').click();
  expect(await width(alice, '#doc-frame')).toBeGreaterThanOrEqual(384);
});
