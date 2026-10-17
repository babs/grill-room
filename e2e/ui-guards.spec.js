import { test, expect } from './fixtures.js';
import { roomWith, upload, appointAdmin, personaSheet, closePersonas, commentOnDocument, signIn, startDaemon, unfoldTabs } from './helpers.js';

// One guard per defect class of the v0.0.17 visual check (docs/design.md): each failed there.
const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
// SHOTS=<dir> keeps the folded tab bars, closed and open.
const shots = process.env.SHOTS || 'test-results/shots';
const longName = 'quarterly-pricing-review-with-a-deliberately-very-long-file-name-v3-final.md';

// A new room comes with this persona.
const persona = 'Grill';
const personaTabs = ['Identity', 'Model', 'Triggers', 'Capabilities'];

// eachPersonaTab opens the persona's sheet and runs check on each of its tabs.
async function eachPersonaTab(page, check) {
  const sheet = await personaSheet(page, persona);
  for (const tab of personaTabs) {
    await unfoldTabs(sheet.locator('#persona-tabs'));
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

// emptyStyle is the look of an empty state: one class, one set of computed styles. Read once it
// shows: an empty state is hidden until its list has loaded.
const emptyStyle = async (loc) => {
  await expect(loc).toBeVisible();
  return loc.evaluate((e) => {
    const s = getComputedStyle(e);
    return [e.classList.contains('empty'), s.display, s.borderTopStyle, s.color, s.textAlign, s.fontSize, s.fontStyle, s.paddingTop, s.paddingLeft].join(' ');
  });
};

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
  dates.settings = await alice.locator('#invites-all .cell[data-label=Created] time').first().getAttribute('data-tip');
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

// The shared components (docs/design.md §5, the panels review's kit branch): each guard failed on
// the pages as they were.

// tabBars opens the four tabbed views on their last tab and runs check(page, nav selector, name) on
// each: /profile, /admin, the daemon page and the persona sheet.
async function tabBars(browser, request, size, check) {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const profile = await alice.context().newPage();
  await profile.setViewportSize(size);
  await profile.goto('/profile#account');
  await expect(profile.locator('#panel-account')).toBeVisible();
  await check(profile, '#profile-tabs', 'profile');
  const admin = await browser.newPage({ viewport: size });
  await signIn(admin, undefined, 'audit');
  await expect(admin.locator('#tab-audit')).toHaveAttribute('aria-current', 'page');
  await check(admin, 'nav.tabs[aria-label=Views]', 'admin');
  const daemon = startDaemon(alice.url().split('#')[0]);
  try {
    const host = await browser.newPage({ viewport: size });
    await host.goto((await daemon.link) + '#update');
    await expect(host.locator('#tab-update')).toHaveAttribute('aria-current', 'page');
    await check(host, 'nav.tabs[aria-label=Sections]', 'daemon');
  } finally {
    daemon.cleanup();
  }
  await alice.setViewportSize(size);
  await personaSheet(alice, persona, 'Capabilities');
  await check(alice, '#persona-tabs', 'persona sheet');
}

test('one sticky tab bar stays in view', async ({ browser, request }) => {
  const looks = {};
  await tabBars(browser, request, desktop, async (page, sel, name) => {
    const nav = page.locator(sel);
    await expect(nav).toHaveClass(/\bsticky\b/);
    // A wide screen keeps the tabs in a row: no folded menu.
    await expect(nav.locator('.tabs-menu')).toBeHidden();
    for (const a of await nav.locator(':scope > a').all()) await expect(a).toBeVisible();
    // A long panel, then the bottom of its scroll box: the bar stays at the box's top, uncovered.
    const at = await nav.evaluate((n) => {
      const spacer = Object.assign(document.createElement('div'), { className: 'guard-spacer' });
      spacer.style.height = '4000px';
      n.parentElement.append(spacer);
      let box = n.parentElement;
      while (box && !(['auto', 'scroll'].includes(getComputedStyle(box).overflowY) && box.scrollHeight > box.clientHeight)) box = box.parentElement;
      const scroller = box || document.scrollingElement;
      scroller.scrollTop = scroller.scrollHeight;
      const top = box ? box.getBoundingClientRect().top + box.clientTop : 0;
      const r = n.getBoundingClientRect();
      const a = n.querySelector('[aria-current=page]').getBoundingClientRect();
      const hit = document.elementFromPoint(a.left + a.width / 2, a.top + a.height / 2);
      const s = getComputedStyle(n);
      spacer.remove();
      return { offset: Math.round(r.top - top), covered: !n.contains(hit), look: [s.position, s.borderBottomWidth, s.borderBottomStyle, s.zIndex].join(' ') };
    });
    expect(at.offset, `${name} bar top in its scroll box`).toBeGreaterThanOrEqual(-1);
    expect(at.offset, `${name} bar top in its scroll box`).toBeLessThanOrEqual(1);
    expect(at.covered, `${name} current tab covered`).toBe(false);
    looks[name] = at.look;
  });
  expect(new Set(Object.values(looks)).size, JSON.stringify(looks)).toBe(1);
});

// On a phone a bar scrolled sideways hides that more tabs exist (2026-10-17): one that overflows
// folds into ☰ and the current tab's name, which lists every tab; one that fits shows them whole.
test('phone: a tab bar that overflows folds into a menu', async ({ browser, request }) => {
  const folded = [];
  await tabBars(browser, request, phone, async (page, sel, name) => {
    const nav = page.locator(sel);
    const button = nav.locator('.tabs-menu');
    const links = nav.locator(':scope > a');
    const current = async () => (await nav.locator('[aria-current=page]').evaluate((a) => a.firstChild.textContent)).trim();
    // A page folds its bar once drawn: wait for it to settle.
    await expect.poll(() => nav.evaluate((n) => ('menu' in n.dataset ? 'folded' : n.scrollWidth > n.clientWidth ? 'scrolls' : 'fits')), name).toMatch(/^(folded|fits)$/);
    if (!(await button.isVisible())) {
      // Unfolded, every tab lies whole inside the bar.
      const cut = await nav.evaluate((n) => {
        const b = n.getBoundingClientRect();
        return [...n.querySelectorAll(':scope > a')].filter((a) => { const r = a.getBoundingClientRect(); return r.left < b.left - 0.5 || r.right > b.right + 0.5; }).length;
      });
      expect(cut, `${name}: tabs cut`).toBe(0);
      return;
    }
    folded.push(name);
    const count = await links.count();
    await expect(button).toHaveAttribute('aria-expanded', 'false');
    await expect(button).toHaveText(await current());
    expect(await links.evaluateAll((as) => as.filter((a) => a.checkVisibility()).length), `${name}: tabs shown while folded`).toBe(0);
    await page.screenshot({ path: `${shots}/${name.replace(' ', '-')}-closed.png` });
    // Every tab is one tap away, and picking it folds the list back, focus on the button.
    for (let i = 0; i < count; i++) {
      await button.click();
      await expect(button).toHaveAttribute('aria-expanded', 'true');
      for (const a of await links.all()) {
        await expect(a).toBeVisible();
        expect((await a.boundingBox()).height, `${name}: tab target`).toBeGreaterThanOrEqual(43.5);
      }
      if (i === 0) await page.screenshot({ path: `${shots}/${name.replace(' ', '-')}-open.png` });
      const pick = links.nth(i);
      const label = (await pick.evaluate((a) => a.firstChild.textContent)).trim();
      await pick.click();
      await expect(pick).toHaveAttribute('aria-current', 'page');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toHaveText(label);
      await expect(button).toBeFocused();
      await expect(links.nth(i)).toBeHidden();
    }
    // Escape and a tap outside close the list.
    await button.click();
    await page.keyboard.press('Escape');
    await expect(button).toHaveAttribute('aria-expanded', 'false');
    await expect(button).toBeFocused();
    await expect(links.first()).toBeHidden();
    await button.click();
    const box = await nav.boundingBox();
    await page.mouse.click(box.x + box.width / 2, box.y + box.height + 8);
    await expect(button).toHaveAttribute('aria-expanded', 'false');
    await expect(links.first()).toBeHidden();
  });
  expect(folded).toEqual(expect.arrayContaining(['profile', 'admin', 'daemon']));
});

// 005-AC35 004b-AC3: whatever the font draws, the tab scrolled to leaves no sliver of the tab before
// it: CI's fonts, wider than a workstation's, hit fractional tab edges. Each size changes the edges.
// A phone folds such a bar: a narrowed bar on a wide screen still scrolls.
test('a tab bar scrolls to whole pixels at any glyph width', async ({ browser }) => {
  // The page's CSP refuses an injected style: only this test bypasses it.
  const page = await (await browser.newContext({ viewport: desktop, bypassCSP: true })).newPage();
  await signIn(page, undefined, 'audit');
  await expect(page.locator('#tab-audit')).toHaveAttribute('aria-current', 'page');
  const nav = page.locator('nav.tabs[aria-label=Views]');
  await page.addStyleTag({ content: '.tabs { max-width: 22rem }' });
  for (const px of [13, 13.13, 13.52, 14.56, 14.69, 15.08]) {
    await page.addStyleTag({ content: `.tabs a { font-size: ${px}px !important }` });
    await page.goto('/admin#libraries');
    await page.goto('/admin#audit');
    await expect.poll(() => nav.evaluate((n) => {
      const lo = n.getBoundingClientRect().left + n.clientLeft;
      const cut = [...n.querySelectorAll('a')].filter((a) => { const r = a.getBoundingClientRect(); return r.left < lo - 0.5 && r.right > lo + 0.5; }).length;
      return n.scrollLeft > 0 ? cut : 'not scrolled';
    }), `${px} px`).toBe(0);
  }
});

// token resolves a custom property to the colour the page computes for it.
const token = (page, name) => page.evaluate((n) => {
  const probe = document.createElement('span');
  probe.style.color = `var(${n})`;
  document.body.append(probe);
  const c = getComputedStyle(probe).color;
  probe.remove();
  return c;
}, name);

// A state pill's colour by its kind; on the header's chrome the dots' tokens carry the contrast.
const pillTokens = { ok: '--success', busy: '--primary-text', idle: '--text-muted', off: '--text-muted', failed: '--danger' };

test('status pills take their colour from the tokens', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(alice.url().split('#')[0]);
  try {
    const host = await browser.newPage({ viewport: desktop });
    await host.goto((await daemon.link) + '#general');
    for (const scheme of ['light', 'dark']) {
      await host.emulateMedia({ colorScheme: scheme });
      const running = host.locator('.app-header .status', { hasText: 'Running' });
      await expect(running, scheme).toHaveClass(/\bok\b/);
      expect(await running.evaluate((e) => getComputedStyle(e).color), `Running on the header, ${scheme}`).toBe(await token(host, '--success-dot'));
      await expect(host.locator('#panel-general .status').first()).toBeVisible({ timeout: 30_000 });
      const pills = await host.locator('main .status').evaluateAll((els) => els.filter((e) => e.getClientRects().length)
        .map((e) => [e.textContent.trim(), ['ok', 'busy', 'idle', 'off', 'failed'].find((k) => e.classList.contains(k)), getComputedStyle(e).color]));
      expect(pills.length, scheme).toBeGreaterThan(0);
      for (const [text, kind, color] of pills) expect(color, `${text} (${kind}), ${scheme}`).toBe(await token(host, pillTokens[kind] || '--none'));
    }
  } finally {
    daemon.cleanup();
  }
  const admin = await browser.newPage({ viewport: desktop });
  await signIn(admin, undefined, 'rooms');
  const cell = admin.locator('#room-' + room.id + ' .status');
  await expect(cell).toHaveText('No AI host');
  expect(await cell.evaluate((e) => getComputedStyle(e).color)).toBe(await token(admin, '--text-muted'));
});

test('a cut value keeps a copy button and its full text in the tip', async ({ browser, request, browserName }) => {
  test.skip(browserName === 'firefox', 'Playwright cannot grant clipboard-read in Firefox');
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const roomURL = alice.url().split('#')[0];
  const daemon = startDaemon(roomURL);
  try {
    const ctx = await browser.newContext({ viewport: phone, permissions: ['clipboard-read', 'clipboard-write'] });
    const host = await ctx.newPage();
    await host.goto((await daemon.link) + '#general');
    // The sandbox image on the Settings strip, once the server's app runs.
    const image = process.env.GRILL_ROOM_SANDBOX_FAKE_IMAGE || 'grill-room-sandbox:fake';
    const value = host.locator('.machine-strip .value');
    await expect(value).toBeVisible({ timeout: 30_000 });
    const text = value.locator('.value-text');
    // One line, cut at the phone's width, the whole value in the tip.
    expect(await text.evaluate((e) => [getComputedStyle(e).whiteSpace, getComputedStyle(e).textOverflow, e.scrollWidth > e.clientWidth])).toEqual(['nowrap', 'ellipsis', true]);
    await expect(text).toHaveAttribute('data-tip', image);
    const copy = value.getByRole('button', { name: /^Copy/ });
    const box = await copy.boundingBox();
    expect(box.x + box.width, 'copy button on screen').toBeLessThanOrEqual(phone.width);
    await copy.click();
    expect(await host.evaluate(() => navigator.clipboard.readText())).toBe(image);
    await expect(host.locator('.toast', { hasText: 'Copied' })).toBeVisible();
  } finally {
    daemon.cleanup();
  }
});

test('the daemon page draws one tip', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(alice.url().split('#')[0]);
  try {
    const host = await browser.newPage({ viewport: desktop });
    await host.goto(await daemon.link);
    await expect(host.locator('#tab-rooms')).toBeVisible();
    // Any [data-tip] gets the tip: the page's own probe, so the check needs no data.
    await host.locator('main').evaluate((m) => m.prepend(Object.assign(document.createElement('span'), { id: 'tip-probe', textContent: 'probe' })));
    await host.locator('#tip-probe').evaluate((e) => { e.dataset.tip = 'a probe'; });
    await host.locator('#tip-probe').hover();
    await expect(host.locator('#person-tip')).toBeVisible();
    const tips = await host.evaluate(() => [...document.querySelectorAll('body *')].filter((e) => e.getClientRects().length && e.textContent === 'a probe' && e.id !== 'tip-probe').map((e) => e.id));
    expect(tips).toEqual(['person-tip']);
  } finally {
    daemon.cleanup();
  }
});

test('wide sheets paint the card surface', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await alice.setViewportSize(desktop);
  for (const scheme of ['light', 'dark']) {
    await alice.emulateMedia({ colorScheme: scheme });
    const surface = await token(alice, '--surface');
    await personaSheet(alice, persona);
    for (const id of ['#personas', '#persona-sheet']) expect(await alice.locator(id).evaluate((e) => getComputedStyle(e).backgroundColor), `${id} ${scheme}`).toBe(surface);
    await closePersonas(alice);
    await alice.goto(alice.url().split('#')[0] + '#manage/general');
    expect(await alice.locator('#manage').evaluate((e) => getComputedStyle(e).backgroundColor), `#manage ${scheme}`).toBe(surface);
    await alice.goto(alice.url().split('#')[0]);
  }
});

// disabledContrast lists the visible disabled buttons whose text, as painted, is under 4.5:1 on what
// is behind it: the button's and its ancestors' opacity and backgrounds blended in.
const disabledContrast = (page) => page.evaluate(() => {
  const rgba = (c) => { const m = c.match(/[\d.]+/g).map(Number); return [m[0], m[1], m[2], m.length > 3 ? m[3] : 1]; };
  const over = (top, under) => top.slice(0, 3).map((v, i) => v * top[3] + under[i] * (1 - top[3])).concat(1);
  const behind = (e) => {
    const chain = [];
    for (let p = e; p; p = p.parentElement) chain.push(p);
    let bg = [255, 255, 255, 1];
    for (const p of chain.reverse()) bg = over(rgba(getComputedStyle(p).backgroundColor), bg);
    return bg;
  };
  const lum = (c) => { const [r, g, b] = c.slice(0, 3).map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
  const ratio = (a, b) => { const [x, y] = [lum(a), lum(b)].sort((m, n) => n - m); return (x + 0.05) / (y + 0.05); };
  const bad = [];
  for (const b of document.querySelectorAll('button:disabled, .btn[aria-disabled=true]')) {
    if (!b.getClientRects().length || !b.textContent.trim()) continue;
    let opacity = 1;
    for (let p = b; p; p = p.parentElement) opacity *= Number(getComputedStyle(p).opacity);
    const under = behind(b.parentElement);
    const face = over(rgba(getComputedStyle(b).backgroundColor), under);
    const text = over(rgba(getComputedStyle(b).color), face);
    const r = ratio(over([...text.slice(0, 3), opacity], under), over([...face.slice(0, 3), opacity], under));
    if (r < 4.5) bad.push(`${b.textContent.trim().slice(0, 30)}: ${r.toFixed(2)}`);
  }
  return bad;
});

test('disabled buttons stay readable in both themes', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await alice.setViewportSize(desktop);
  const admin = await browser.newPage({ viewport: desktop });
  await signIn(admin, undefined, 'audit');
  const views = [
    [alice, alice.url().split('#')[0] + '#manage/general', 'room settings', alice.locator('#manage').getByRole('button', { name: 'Rename', exact: true })],
    [admin, '/admin#rooms/' + room.id + '/general', '/admin room', admin.locator('#room-panel').getByRole('button', { name: 'Rename', exact: true })],
    [admin, '/admin#audit', '/admin audit', admin.getByRole('button', { name: 'Newer' }).first()],
  ];
  for (const scheme of ['light', 'dark']) {
    for (const [page, url, name, disabled] of views) {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto(url);
      await expect(disabled, `${name} ${scheme}`).toBeDisabled();
      expect(await disabledContrast(page), `${name} ${scheme}`).toEqual([]);
    }
    // A persona pill whose ▶ cannot ask is aria-disabled but keeps its own look: the kit's rule is for its buttons.
    await alice.goto(alice.url().split('#')[0]);
    const pill = alice.locator('.persona-pill:has(.persona-run[aria-disabled=true])').first();
    await expect(pill).toBeVisible();
    expect(await pill.evaluate((b) => getComputedStyle(b).backgroundColor), `persona pill ${scheme}`).toBe(await token(alice, '--surface'));
  }
});
