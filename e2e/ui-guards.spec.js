import { test, expect } from './fixtures.js';
import { roomWith, upload, appointAdmin, personaSheet, closePersonas, commentOnDocument } from './helpers.js';

// One guard per defect class of the v0.0.17 visual check (docs/design.md): each failed there.
const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
const longName = 'quarterly-pricing-review-with-a-deliberately-very-long-file-name-v3-final.md';

// A new room comes with this persona.
const persona = 'Grill';
const personaTabs = ['Identity', 'Model', 'Triggers', 'Capabilities'];

// eachPersonaTab opens the persona's sheet and runs check on each of its tabs.
async function eachPersonaTab(page, check) {
  const sheet = await personaSheet(page, persona);
  for (const tab of personaTabs) {
    await sheet.locator('#persona-tabs').getByRole('link', { name: tab, exact: true }).click();
    await expect(sheet.locator(`section[data-tab="${tab.toLowerCase()}"]`)).toBeVisible();
    await check(sheet, tab);
  }
}

// chromeFaults lists the emoji and arrow glyphs drawn as icons, and the native title tooltips, in
// what is on screen now: persona badges, which people choose, and message text are content.
const chromeFaults = (page) => page.evaluate(() => {
  const glyph = /[\p{Extended_Pictographic}\u2190-\u21FF\u2700-\u27BF\u22EF\u2715\u25B8-\u25BE]/u;
  const content = '#messages, #doc-frame, .badge, .pr-badge, .persona-badge';
  const shown = (e) => e.getClientRects().length > 0 && getComputedStyle(e).visibility !== 'hidden';
  const bad = [];
  for (const e of document.body.querySelectorAll('*')) {
    // A frame's title is its accessible name; Markdown in a message may carry one.
    if (e.hasAttribute('title') && !e.closest('.msg .body, #doc-frame')) bad.push(`title on ${e.tagName.toLowerCase()}${e.id ? '#' + e.id : ''}: ${e.getAttribute('title').slice(0, 40)}`);
    if (e.closest(content)) continue;
    if (!shown(e)) continue;
    const own = [...e.childNodes].filter((n) => n.nodeType === Node.TEXT_NODE).map((n) => n.data).join('');
    for (const pseudo of ['::before', '::after']) {
      const c = getComputedStyle(e, pseudo).content;
      if (glyph.test(c)) bad.push(`${pseudo} glyph on ${e.tagName.toLowerCase()}.${e.className}: ${c}`);
    }
    if (glyph.test(own)) bad.push(`glyph in ${e.tagName.toLowerCase()}${e.id ? '#' + e.id : ''}: ${own.trim().slice(0, 30)}`);
  }
  return bad;
});

// ownSmall lists the controls under sel whose own box is under 44 px, with no ::after allowance:
// on a phone a finger needs the real box (docs/design.md §6).
const ownSmall = (page, sel) => page.locator(sel).evaluateAll((els) => els.filter((e) => e.getClientRects().length)
  .map((e) => [e.id || e.className || e.textContent.trim().slice(0, 20), Math.round(e.getBoundingClientRect().width), Math.round(e.getBoundingClientRect().height)])
  .filter(([, w, h]) => w < 43.5 || h < 43.5));

test('phone: the open document keeps a readable title', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  await alice.locator('#tabbar [data-tab=docs]').click();
  await upload(alice, longName, 'text/markdown', '# Pricing\n\nText.\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  // v0.0.17 squeezed it to 78 px behind the toolbar.
  expect(await alice.locator('#doc-name').evaluate((e) => e.getBoundingClientRect().width)).toBeGreaterThanOrEqual(phone.width * 0.6);
});

test('no native file input shows', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  for (const size of [desktop, phone]) {
    await alice.setViewportSize(size);
    await alice.goto(alice.url().split('#')[0]);
    if (size === phone) await alice.locator('#tabbar [data-tab=docs]').click();
    if (!(await alice.locator('#documents li').count())) {
      await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
      await commentOnDocument(alice, 'Check this.');
    } else await alice.locator('#documents li').first().click();
    if (size === phone) await alice.getByRole('button', { name: /Notes/ }).click();
    await expect(alice.locator('.annot-reply').first()).toBeVisible();
    await alice.getByRole('button', { name: 'Comment on the document' }).click();
    await expect(alice.locator('#annot-new')).toBeVisible();
    await expect(alice.locator('input[type=file]:visible')).toHaveCount(0);
    // The paperclip is the way in, and a chosen file shows as a chip.
    await alice.locator('#annot-new input[type=file]').setInputFiles({ name: 'shot.png', mimeType: 'image/png', buffer: Buffer.from('x') });
    await expect(alice.locator('#annot-new .file-chip')).toContainText('shot.png');
    await expect(alice.locator('input[type=file]:visible')).toHaveCount(0);
    await alice.locator('#annot-cancel').click();
  }
});

test('no emoji icon and no native title in the room chrome', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await commentOnDocument(alice, 'Check this.');
  for (const size of [desktop, phone]) {
    await alice.setViewportSize(size);
    const states = [['room', null]];
    for (const [menu, open] of [['more menu', '#more-open'], ['user menu', '#me'], ['document menu', '#doc-more']]) states.push([menu, open]);
    for (const [name, open] of states) {
      if (size === phone && name === 'document menu') await alice.locator('#tabbar [data-tab=docs]').click();
      if (open) await alice.locator(open).click();
      expect(await chromeFaults(alice), `${name} at ${size.width}`).toEqual([]);
      if (open) await alice.keyboard.press('Escape');
    }
  }
  await alice.setViewportSize(desktop);
  await eachPersonaTab(alice, async (_, tab) => expect(await chromeFaults(alice), `persona ${tab}`).toEqual([]));
  await closePersonas(alice);
  for (const tab of ['general', 'invites', 'admins', 'personas', 'skills', 'plugins', 'documents', 'export']) {
    await alice.goto(alice.url().split('#')[0] + '#manage/' + tab);
    await expect(alice.locator(`#panel-${tab}`)).toBeVisible();
    expect(await chromeFaults(alice), `settings ${tab}`).toEqual([]);
  }
});

test('phone: targets are 44 px on their own box', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  await alice.locator('#tabbar [data-tab=docs]').click();
  expect(await ownSmall(alice, '#docs-pane .pane-head :is(a, button)'), 'docs header').toEqual([]);
  await alice.locator('#tabbar [data-tab=people]').click();
  await expect(alice.locator('#participants li')).toHaveCount(1);
  expect(await ownSmall(alice, '#participants li'), 'people rows').toEqual([]);
});

test('sheets keep their top across tabs', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await alice.setViewportSize(desktop);
  const tops = [];
  await eachPersonaTab(alice, async (sheet) => tops.push(Math.round(await sheet.locator('.sheet-head').evaluate((e) => e.getBoundingClientRect().top))));
  expect(new Set(tops).size, 'persona sheet ' + tops).toBe(1);
  await closePersonas(alice);
  const url = alice.url().split('#')[0];
  const settings = [];
  for (const tab of ['general', 'invites', 'admins', 'personas', 'skills', 'plugins', 'documents', 'export']) {
    await alice.goto(url + '#manage/' + tab);
    await expect(alice.locator(`#panel-${tab}`)).toBeVisible();
    settings.push(Math.round(await alice.locator('#manage .sheet-head').evaluate((e) => e.getBoundingClientRect().top)));
  }
  expect(new Set(settings).size, 'room settings ' + settings).toBe(1);
});

// emptyStyle is the look of an empty state: one class, one set of computed styles.
const emptyStyle = (loc) => loc.evaluate((e) => {
  const s = getComputedStyle(e);
  return [e.classList.contains('empty'), s.display, s.borderTopStyle, s.color, s.textAlign, s.fontSize, s.fontStyle, s.paddingTop, s.paddingLeft].join(' ');
});

test('one empty-state style', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await alice.setViewportSize(desktop);
  const styles = {};
  styles.documents = await emptyStyle(alice.locator('#docs-empty'));
  const caps = await personaSheet(alice, persona, 'Capabilities');
  styles.skills = await emptyStyle(caps.getByText(/^None on\./).first());
  styles.plugins = await emptyStyle(caps.getByText(/^No plugins in this room yet/).first());
  await closePersonas(alice);
  const url = alice.url().split('#')[0];
  for (const [tab, text] of [['skills', 'None yet'], ['documents', 'No deleted document'], ['plugins', 'No plugins yet']]) {
    await alice.goto(url + '#manage/' + tab);
    styles['settings ' + tab] = await emptyStyle(alice.locator(`#panel-${tab}`).getByText(text, { exact: false }).first());
  }
  await alice.goto(url);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  styles.notes = await emptyStyle(alice.locator('#annot-empty'));
  expect(new Set(Object.values(styles)).size, JSON.stringify(styles, null, 1)).toBe(1);
  expect(Object.values(styles)[0].startsWith('true ')).toBe(true);
});

test('one date format', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await alice.setViewportSize(desktop);
  // en-US medium date and short time: "Oct 14, 2026, 8:12 PM".
  const format = /^[A-Z][a-z]{2} \d{1,2}, \d{4}, \d{1,2}:\d{2}\s[AP]M$/;
  const dates = {};
  await alice.locator('#participants .name').first().focus();
  dates.card = (await alice.locator('#person-card li', { hasText: /^Joined / }).textContent()).replace(/^Joined /, '');
  // Invite dates show in Room settings › Invites only: the popup holds the shareable link (011-AC26).
  await alice.goto(alice.url().split('#')[0] + '#manage/invites');
  dates.settings = (await alice.locator('#invites-all td[data-label=Created]').first().textContent()).trim();
  await alice.goto(alice.url().split('#')[0]);
  await alice.locator('#body').fill('hello');
  await alice.locator('#body').press('Enter');
  dates.message = await alice.locator('#messages li.msg:not(.system) time').first().getAttribute('data-tip');
  for (const [where, d] of Object.entries(dates)) expect(d, where).toMatch(format);
});

test("a control's tip never takes the next row's click", async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await upload(alice, 'a.md', 'text/markdown', '# A\n');
  await upload(alice, 'b.md', 'text/markdown', '# B\n');
  const rows = alice.locator('#documents button.doc');
  await expect(rows).toHaveCount(2);
  await rows.first().hover();
  await expect(alice.locator('#person-tip')).toBeVisible();
  // v0.0.17 had no tip there; a hoverable tip under the first row covered the second.
  await rows.nth(1).click({ timeout: 3000 });
  await expect(alice.locator('#doc-name')).toHaveText(await rows.nth(1).textContent());
});
