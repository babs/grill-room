import { test, expect } from './fixtures.js';
import { roomWith, upload } from './helpers.js';

// pictographs lists the emoji in the side panel's section headers.
const pictographs = (page) => page.locator('#side .pane-head, #annotations .pane-head')
  .evaluateAll((els) => els.map((e) => e.textContent).join(' ').match(/\p{Extended_Pictographic}/gu) || []);

// 009-AC37: the section headers carry no emoji, an SVG icon in the text's colour instead, on the
// room page and in the docs window; the triangle comes first through one token; an empty
// Documents and an empty Notes say so.
test('section headers without emoji, and empty states', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(alice.locator('#docs-empty')).toHaveText(/^No documents yet\. Drop files here or press \+\./);
  await expect(alice.locator('#docs-empty')).toBeVisible();
  expect(await pictographs(alice)).toEqual([]);
  for (const id of ['people-title', 'docs-title']) {
    const icon = alice.locator(`#${id} svg.sec-icon`);
    await expect(icon).toBeVisible();
    expect(await icon.evaluate((e) => [e.getAttribute('stroke'), e.getBoundingClientRect().height <= parseFloat(getComputedStyle(e.parentElement).fontSize) + 1]))
      .toEqual(['currentColor', true]);
  }
  expect(await alice.locator('#docs-pane .pane-head').evaluate((h) => h.firstElementChild.id === 'docs-fold'
    && getComputedStyle(h.querySelector('.fold')).order === getComputedStyle(document.documentElement).getPropertyValue('--fold-order').trim())).toBe(true);

  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await expect(alice.locator('#docs-empty')).toBeHidden();
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Plan');
  await expect(alice.locator('#annot-empty')).toHaveText('No notes yet.');
  await expect(alice.locator('#annot-empty')).toBeVisible();
  await expect(alice.locator('#notes-title svg.sec-icon')).toBeVisible();
  expect(await pictographs(alice)).toEqual([]);

  const docs = await alice.context().newPage();
  await docs.goto(`/r/${room.id}/docs`);
  await expect(docs.locator('#docs-title svg.sec-icon')).toBeVisible();
  expect(await pictographs(docs)).toEqual([]);
});
