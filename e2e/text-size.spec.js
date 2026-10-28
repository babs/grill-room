import { test, expect } from './fixtures.js';
import { roomWith, playEvents, line, menuItem } from './helpers.js';

const size = (page, sel) => page.locator(sel).evaluate((e) => getComputedStyle(e).fontSize);
const chat = (page) => size(page, '#messages > li[data-id="1000001"] .body');
const pane = (page) => size(page, '#voice-lines > li[data-id="1000002"] .body');
const composer = (page) => size(page, '#send textarea');
const lines = [['', line(1000001, 'text', 'typed line')], ['', line(1000002, 'voice', 'spoken line')]];

async function pick(page, area, step) {
  await menuItem(page, 'Text size', 'me');
  await page.locator('#text-size').getByRole('group', { name: area }).getByLabel(step, { exact: true }).check();
  await page.locator('#text-size .close').click();
}

async function openRoom(ctx, url, roomID) {
  const page = await ctx.newPage();
  await page.goto(url);
  await playEvents(page, roomID, lines);
  return page;
}

test.use({ viewport: { width: 1280, height: 720 } });

// 028-AC8 028-AC1 028-AC2 028-AC3 028-AC6: no pick reads at today's sizes; a pick scales its area
// alone, lands on the profile, reaches the browser's other tab live and another browser on focus.
test('a text size per area follows the profile', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await playEvents(alice, room.id, lines);
  expect([await chat(alice), await composer(alice), await pane(alice)]).toEqual(['15px', '15px', '14px']);
  const tab = await openRoom(alice.context(), alice.url(), room.id);
  const other = await browser.newContext();
  await other.addCookies(await alice.context().cookies());
  const away = await openRoom(other, alice.url(), room.id);

  await pick(alice, 'Chat', 'L');
  expect([await chat(alice), await composer(alice), await pane(alice)]).toEqual(['16.875px', '16.875px', '14px']);
  await pick(alice, 'Transcription', 'XL');
  expect(await pane(alice)).toBe('17.5px');
  await expect.poll(() => alice.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((r) => r.profile.text_sizes)))
    .toEqual({ chat: 'l', transcript: 'xl' });
  await expect.poll(() => chat(tab)).toBe('16.875px');
  expect(await pane(tab)).toBe('17.5px');

  expect(await chat(away)).toBe('15px');
  await away.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect.poll(() => chat(away)).toBe('16.875px');
  expect(await pane(away)).toBe('17.5px');

  const fresh = await browser.newContext();
  await fresh.addCookies(await alice.context().cookies());
  const later = await openRoom(fresh, alice.url(), room.id);
  await expect.poll(() => chat(later)).toBe('16.875px');
  expect(await pane(later)).toBe('17.5px');
});

// 028-AC5: on a phone the chat scales down, its composer stays at 16 px.
test('the phone composer stays at 16 px', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 390, height: 800 });
  await playEvents(alice, room.id, [lines[0]]);
  expect([await chat(alice), await composer(alice)]).toEqual(['16px', '16px']);
  await pick(alice, 'Chat', 'S');
  expect([await chat(alice), await composer(alice)]).toEqual(['14px', '16px']);
  await pick(alice, 'Chat', 'XL');
  expect([await chat(alice), await composer(alice)]).toEqual(['20px', '20px']);
});

// 028-AC7: a seat without a profile keeps its pick in the browser, and sends nothing.
test('a seat without a profile keeps its pick in the browser', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.context().clearCookies({ name: 'grill_room_profile' });
  await alice.reload();
  await playEvents(alice, room.id, lines);
  expect(await alice.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((r) => r.profile))).toBeNull();
  const puts = [];
  alice.on('request', (r) => { if (r.url().endsWith('/api/profile/text-size')) puts.push(r); });
  await pick(alice, 'Chat', 'XL');
  expect(await chat(alice)).toBe('18.75px');
  await alice.reload();
  await playEvents(alice, room.id, lines);
  expect(await chat(alice)).toBe('18.75px');
  expect(puts).toEqual([]);
});
