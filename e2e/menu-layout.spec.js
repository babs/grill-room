import { test, expect } from './fixtures.js';
import { mkdirSync } from 'node:fs';
import { roomWith, upload, appointAdmin, personaSheet, personMenu, fakeDaemon } from './helpers.js';

const widths = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } };
// SHOTS=<dir> keeps a screenshot of each menu and row checked: the before and after of 009-AC38/39.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : null);

// textBox is where the first visible character under el starts, and the middle of its capitals:
// the line an icon beside it must sit on. The box of the text itself is centred higher, on the
// font's ascent and descent, which a glyph-free check would accept.
const textBox = (el) => {
  const walk = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  for (let n = walk.nextNode(); n; n = walk.nextNode()) {
    const at = n.data.search(/\S/);
    if (at < 0) continue;
    const r = document.createRange();
    r.setStart(n, at);
    r.setEnd(n, at + 1);
    const b = r.getBoundingClientRect();
    if (!b.width) continue;
    const s = getComputedStyle(n.parentElement);
    const ctx = new OffscreenCanvas(1, 1).getContext('2d');
    ctx.font = `${s.fontStyle} ${s.fontWeight} ${s.fontSize} ${s.fontFamily}`;
    const base = b.top + ctx.measureText('H').fontBoundingBoxAscent;
    const cap = ctx.measureText('H').actualBoundingBoxAscent;
    const ink = ctx.measureText(n.data.slice(at).match(/^\S/u)[0]);
    return { left: b.left, mid: base - cap / 2, ink: base - (ink.actualBoundingBoxAscent - ink.actualBoundingBoxDescent) / 2 };
  }
  return null;
};

// menuRows measures an open menu: each visible item's font size, height and text left edge, and
// the header line's text left edge.
const menuRows = (menu) => menu.evaluate((m, textBox) => {
  const box = new Function('return ' + textBox)();
  const shown = (e) => !e.hidden && e.getClientRects().length > 0;
  const items = [...m.querySelectorAll(':scope > :is(a, button, label)')].filter(shown).map((e) => ({
    text: e.textContent.trim().slice(0, 30),
    font: getComputedStyle(e).fontSize,
    line: getComputedStyle(e).lineHeight,
    height: Math.round(e.getBoundingClientRect().height),
    left: box(e).left,
  }));
  const head = m.querySelector(':scope > .menu-head');
  return { items, head: head && shown(head) && head.textContent ? box(head).left : null };
}, textBox.toString());

// expectOneStyle asserts 009-AC38 on an open menu: one font size, line height and height for every
// item, links and buttons alike, and every text, the header's too, from the same x (±1 px).
async function expectOneStyle(page, menu, name) {
  await expect(menu).toBeVisible();
  const { items, head } = await menuRows(menu);
  const why = name + ' ' + JSON.stringify({ items, head });
  expect.soft(items.length, why).toBeGreaterThan(0);
  expect.soft(new Set(items.map((i) => i.font)).size, why).toBe(1);
  expect.soft(new Set(items.map((i) => i.line)).size, why).toBe(1);
  expect.soft(new Set(items.map((i) => i.height)).size, why).toBe(1);
  const lefts = items.map((i) => i.left).concat(head === null ? [] : [head]);
  expect.soft(Math.max(...lefts) - Math.min(...lefts), why).toBeLessThanOrEqual(1);
  await shot(page, name);
  await page.keyboard.press('Escape');
  await expect(menu).toBeHidden();
}

// offCentre lists the icons of each row whose vertical centre is more than 1 px from the centre of
// the row's text (009-AC39).
const offCentre = (page, rows, label, icons) => page.locator(rows).evaluateAll((els, [label, icons, textBox]) => {
  const box = new Function('return ' + textBox)();
  const bad = [];
  for (const row of els) {
    const text = row.querySelector(label);
    if (!text || !text.getClientRects().length) continue;
    const mid = box(text).mid;
    for (const i of row.querySelectorAll(icons)) {
      const r = i.getBoundingClientRect();
      if (!r.height || getComputedStyle(i).visibility === 'hidden') continue;
      const name = `${text.textContent.trim()}: ${i.className.baseVal ?? i.className}`;
      // A text glyph drawn by ::before has no box of its own to measure: icons are SVG.
      if (/^["'].+["']$/.test(getComputedStyle(i, '::before').content)) bad.push(name + ' draws a glyph');
      const at = [['box', r.top + r.height / 2]];
      // A glyph inside the box has its own ink, which the box's centre says nothing about.
      if (i.textContent.trim()) at.push(['glyph', box(i).ink]);
      for (const [what, y] of at) if (Math.abs(y - mid) > 1) bad.push(`${name} ${what} ${(y - mid).toFixed(1)}px`);
    }
  }
  return bad;
}, [label, icons, textBox.toString()]);

for (const [size, viewport] of Object.entries(widths)) {
  // 009-AC38: every ⋯ menu, the user menu and the row menus give links and buttons one style, and
  // a menu with an icon starts every item's text at the same x.
  test(`one menu-item style at ${size}`, async ({ browser, request }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await alice.setViewportSize(viewport);
    await appointAdmin(alice, request, room.id);

    await alice.locator('#more-open').click();
    await expectOneStyle(alice, alice.locator('#more-menu'), `${size}-more-menu`);
    await alice.locator('#me').click();
    await expectOneStyle(alice, alice.locator('#me-menu'), `${size}-me-menu`);

    if (size === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /People/ }).click();
    await expectOneStyle(alice, await personMenu(alice, 'Bob'), `${size}-person-menu`);
    if (size === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /Chat/ }).click();

    await alice.locator('#body').fill('a line');
    await alice.locator('#body').press('Enter');
    await alice.locator('.msg.text', { hasText: 'a line' }).getByRole('button', { name: 'Line actions' }).click();
    await expectOneStyle(alice, alice.locator('#msg-menu'), `${size}-msg-menu`);

    await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
    await expect(alice.locator('#viewer')).toBeVisible();
    await alice.locator('#doc-more').click();
    await expectOneStyle(alice, alice.locator('#doc-menu'), `${size}-doc-menu`);
    await alice.locator('#doc-close').click();

    await personaSheet(alice, 'Grill');
    await alice.locator('#persona-more').click();
    await expectOneStyle(alice, alice.locator('#persona-menu'), `${size}-persona-menu`);

    const docs = await alice.context().newPage();
    await docs.setViewportSize(viewport);
    await docs.goto(`/r/${room.id}/docs`);
    await docs.locator('#documents').getByText('plan.md').click();
    await docs.locator('#doc-more').click();
    await expectOneStyle(docs, docs.locator('#doc-menu'), `${size}-docs-window-doc-menu`);

    await alice.goto('/profile');
    await alice.locator('#starters-builtin li', { hasText: 'Retro' }).getByRole('button', { name: 'More actions for Retro' }).click();
    await expectOneStyle(alice, alice.locator('#starters-builtin [role=menu]:popover-open'), `${size}-starter-menu`);
  });

  // 009-AC39: in People rows and the side panel's section headers, every icon, its box and any glyph
  // in it, is centred on the text's line.
  test(`icons on the text's centre line at ${size}`, async ({ browser, request, baseURL }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await alice.setViewportSize(viewport);
    await appointAdmin(alice, request, room.id);
    // Alice's app hosts the AI and Bob may ask it: the rows get every badge but audio and transcribed.
    const stop = await fakeDaemon(alice, baseURL, room.id, 'laptop');
    try {
      await expect(alice.locator('#participants .tag.role-host')).toHaveCount(1);
      if (size === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /People/ }).click();
      await expect(alice.locator('#participants li')).toHaveCount(2);
      await expect(alice.locator('#participants .tag.role-admin')).toHaveCount(1);
      await (await personMenu(alice, 'Bob')).getByRole('button', { name: 'Can ask the AI' }).click();
      await expect(alice.locator('#participants .tag.tag-ask:not(.blank)')).toHaveCount(1);
      expect.soft(await offCentre(alice, '#participants li', '.name', '.voice-dot, .tag, .tag svg, .person-more')).toEqual([]);
      expect.soft(await offCentre(alice, '#people-pane .pane-head', '.sec-label', '.fold, .sec-icon, .head-action, .head-action svg')).toEqual([]);
      await shot(alice, `${size}-people`);
      if (size === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
      await expect(alice.locator('#docs-pane .pane-head')).toBeVisible();
      expect.soft(await offCentre(alice, '#docs-pane .pane-head', '.sec-label', '.fold, .sec-icon, .head-action, .head-action svg')).toEqual([]);
      await shot(alice, `${size}-documents-head`);
      if (size === 'desktop') {
        await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
        await expect(alice.locator('#annotations .pane-head')).toBeVisible();
        expect.soft(await offCentre(alice, '#annotations .pane-head', '.sec-label', '.fold, .sec-icon, .head-action, .head-action svg')).toEqual([]);
      }
    } finally {
      stop();
    }
  });
}
