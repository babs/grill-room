import { test, expect } from './fixtures.js';
import { roomWith, playEvents, upload, messageAction, line, live } from './helpers.js';

const pane = (page) => page.locator('#transcript');
const inPane = (page, id) => page.locator(`#voice-lines > li[data-id="${id}"]`);
const inChat = (page, id) => page.locator(`#messages > li[data-id="${id}"]`);
const fold = (page) => page.locator('#transcript-fold');
// ids lists the lines of a list, in DOM order.
const ids = (page, sel) => page.locator(sel + ' > li').evaluateAll((ls) => ls.map((l) => Number(l.dataset.id)));
const atEnd = (page, sel) => page.locator(sel).evaluate((l) => l.scrollHeight - l.clientHeight - l.scrollTop <= 1);
const myID = async (page) => Number(await page.evaluate(() => myID));
const wide = { width: 1280, height: 720 };
const narrow = { width: 900, height: 720 };
test.use({ viewport: wide });

// scrollUp leaves the pane far from its end, as a reader's wheel does.
async function scrollUp(page) {
  await page.locator('#voice-lines').hover();
  await page.mouse.wheel(0, -2000);
  await expect.poll(() => page.locator('#voice-lines').evaluate((l) => l.scrollTop)).toBe(0);
}

// 020-AC8 020-AC10: at 1280 px a voice line lands in the pane, not the chat, and the chat shows no
// filter; a paragraph append redraws the line in place, live, and a pinned pane stays at its end.
test('a voice line lands in the pane, not the chat', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await playEvents(alice, room.id, [['', line(1000001, 'text', 'typed line')], ['', line(1000002, 'voice', 'spoken line')]]);
  await expect(inPane(alice, 1000002)).toBeVisible();
  await expect(inChat(alice, 1000002)).toHaveCount(0);
  await expect(inChat(alice, 1000001)).toBeVisible();
  await expect(alice.locator('#chat-filter')).toBeHidden();
  const box = await pane(alice).boundingBox();
  const chat = await alice.locator('#messages').boundingBox();
  expect(box.width).toBe(320);
  expect(box.x).toBeGreaterThanOrEqual(chat.x + chat.width);

  for (let i = 0; i < 30; i++) await live(alice, line(1e9 + i, 'voice', 'line ' + i));
  await alice.evaluate((m) => replaceMessage(m), { ...line(1e9 + 29, 'voice', 'line 29 ' + 'and more words '.repeat(40)), change_seq: 1 });
  await expect(inPane(alice, 1e9 + 29)).toContainText('and more');
  await expect(alice.locator(`li[data-id="${1e9 + 29}"]`)).toHaveCount(1);
  await expect.poll(() => atEnd(alice, '#voice-lines')).toBe(true);
  // The segments count nothing while the pane shows the voice lines (020-AC6).
  expect(await alice.locator('#chat-filter [data-filter=voice]').textContent()).toBe('Voice');
});

// 020-AC9: with a document open, the pane sits above the messages in the chat column, at most 40 %
// of its height; the composer is the column's last element, at its bottom.
test('with a document open the pane is above the messages', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  for (let i = 0; i < 40; i++) await live(alice, line(1e9 + i, 'voice', 'line ' + i));
  await expect(inPane(alice, 1e9 + 39)).toBeVisible();
  const [col, p, msgs, send] = await Promise.all(['.chat', '#transcript', '#messages', '#send'].map((s) => alice.locator(s).boundingBox()));
  expect(p.y + p.height).toBeLessThanOrEqual(msgs.y + 1);
  expect(p.height).toBeLessThanOrEqual(col.height * 0.4 + 1);
  expect(p.x).toBe(col.x);
  expect(await alice.locator('.chat').evaluate((c) => c.lastElementChild.id)).toBe('send');
  expect(Math.abs(send.y + send.height - (col.y + col.height))).toBeLessThanOrEqual(1);
  // The newest line is in view: the capped pane stuck to its end (020-AC10).
  await expect(inPane(alice, 1e9 + 39)).toBeInViewport();
});

// 020-AC10: pinned within 40 px of the end, a new line scrolls the pane down; once the reader
// scrolled up, it is left alone. A pane that comes back, unfolded or past 64rem, is at its end.
test('the pane sticks to its end until scrolled up', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  for (let i = 0; i < 30; i++) await live(alice, line(1e9 + i, 'voice', 'line ' + i));
  await expect.poll(() => atEnd(alice, '#voice-lines')).toBe(true);
  expect(await alice.locator('#voice-lines').evaluate((l) => l.scrollHeight > l.clientHeight)).toBe(true);
  await scrollUp(alice);
  await live(alice, line(1e9 + 30, 'voice', 'while reading'));
  await expect(inPane(alice, 1e9 + 30)).toBeAttached();
  expect(await alice.locator('#voice-lines').evaluate((l) => l.scrollTop)).toBe(0);
  // Back near the end, it sticks again.
  await alice.mouse.wheel(0, 100000);
  await expect.poll(() => atEnd(alice, '#voice-lines')).toBe(true);
  await live(alice, line(1e9 + 31, 'voice', 'pinned again'));
  await expect.poll(() => atEnd(alice, '#voice-lines')).toBe(true);

  await scrollUp(alice);
  await fold(alice).click();
  await fold(alice).click();
  await live(alice, line(1e9 + 32, 'voice', 'after unfolding'));
  await expect.poll(() => atEnd(alice, '#voice-lines')).toBe(true);
  await scrollUp(alice);
  await alice.setViewportSize(narrow);
  await expect(inChat(alice, 1e9 + 32)).toHaveCount(1);
  await alice.setViewportSize(wide);
  await live(alice, line(1e9 + 33, 'voice', 'after the crossing'));
  await expect.poll(() => atEnd(alice, '#voice-lines')).toBe(true);
});

// Spec 018 review waiver: a line above the view that grows keeps what the reader looks at in
// place, with the browser's scroll anchoring (Chrome) and without it (Safari; overflow-anchor: none).
test('a line growing above the view does not move the one read', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  for (let i = 0; i < 30; i++) await live(alice, line(1e9 + i, 'voice', 'line ' + i));
  await alice.locator('#voice-lines').hover();
  await alice.mouse.wheel(0, -300);
  await expect.poll(() => alice.locator('#voice-lines').evaluate((l) => l.scrollTop > 0 && l.scrollHeight - l.clientHeight - l.scrollTop > 100)).toBe(true);
  const read = inPane(alice, 1e9 + 25);
  for (const [i, anchor] of [[0, 'auto'], [1, 'none']]) {
    await alice.locator('#voice-lines').evaluate((l, a) => { l.style.overflowAnchor = a; }, anchor);
    const before = (await read.boundingBox()).y;
    await alice.evaluate((m) => replaceMessage(m), { ...line(1e9 + i, 'voice', 'line ' + i + ' ' + 'and more words '.repeat(40)), change_seq: 1 });
    await expect(inPane(alice, 1e9 + i)).toContainText('and more');
    expect(Math.abs((await read.boundingBox()).y - before), anchor).toBeLessThanOrEqual(1);
  }
});

// 020-AC11: a voice line in the pane keeps its speaker, time, source and its author's menu; an
// edit redraws it in the pane, a delete marks it there and Undo takes it back.
test('the author edits and deletes a voice line in the pane', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const mine = { ...line(1000001, 'voice', 'what I said', undefined, await myID(alice)), sender: 'Alice', source: 'device' };
  await playEvents(alice, room.id, [['', mine]]);
  const li = inPane(alice, 1000001);
  await expect(li.locator('.sender')).toHaveText('Alice');
  await expect(li.locator('time')).toBeVisible();
  await expect(li.locator('.voice-src')).toHaveAttribute('data-tip', "Transcribed on Alice's device");
  // The e2e server has no voice line to store: it answers the edit as it would.
  await alice.route(`**/api/rooms/${room.id}/messages/1000001`, (r) => r.fulfill({ json: { ...mine, body: 'what I meant', html: '<p>what I meant</p>', edited_at: new Date().toISOString(), change_seq: 1 } }));
  await li.hover();
  await messageAction(li, 'Edit');
  await li.locator('textarea').fill('what I meant');
  await li.locator('textarea').press('Enter');
  await expect(li.locator('.body')).toHaveText('what I meant');
  await expect(inChat(alice, 1000001)).toHaveCount(0);

  await li.hover();
  await messageAction(li, 'Delete');
  await expect(li).toHaveClass(/deleting/);
  await alice.locator('.toast').getByRole('button', { name: 'Undo' }).click();
  await expect(li).not.toHaveClass(/deleting/);
});

// 020-AC11: a voice trigger's mention state shows on its line in the pane, its author's name as
// it is now, with their details card.
test('a pane line keeps its mention chip, name and card', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const me = await myID(alice);
  await playEvents(alice, room.id, [
    ['', { ...line(1000001, 'voice', 'Grill what next', undefined, me), sender: 'Alicia', mentions: [{ persona_id: 1, status: 'done', line_id: 1000002 }] }],
    ['', line(1000002, 'agent', 'the reply')],
  ]);
  const li = inPane(alice, 1000001);
  await expect(li.locator('.mention-turn')).toBeVisible();
  await expect(li.locator('.sender')).toHaveText('Alice');
  await li.locator('.sender').hover();
  await expect(alice.locator('#person-card')).toBeVisible();
  await expect(alice.locator('#person-card')).toContainText('Alice');
});

// 020-AC4 020-AC12 (Decisions): moving a line its author edits keeps the edit box in view and in
// focus, far from the list's end too, under All for this visit; the line's open menu closes.
test('a line being edited stays in sight across 64rem', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [['', { ...line(1000001, 'voice', 'what I said', undefined, await myID(alice)), sender: 'Alice' }]];
  for (let i = 0; i < 60; i++) events.push(['', line(1000002 + i, i % 2 ? 'text' : 'voice', 'filler ' + i)]);
  await alice.evaluate(() => history.replaceState(null, '', '#m1000001'));
  await playEvents(alice, room.id, events);
  await expect(inPane(alice, 1000001)).toBeInViewport();
  await inPane(alice, 1000001).hover();
  await messageAction(inPane(alice, 1000001), 'Edit');
  const box = alice.locator('li[data-id="1000001"] textarea');
  await expect(box).toBeFocused();
  await alice.setViewportSize(narrow);
  await expect(inChat(alice, 1000001).locator('textarea')).toBeVisible();
  await expect(box).toBeInViewport();
  await expect(box).toBeFocused();
  await expect(alice.locator('#chat-filter [data-filter=all]')).toHaveAttribute('aria-pressed', 'true');
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBeNull();
  await alice.setViewportSize(wide);
  await expect(inPane(alice, 1000001).locator('textarea')).toBeVisible();
  await expect(box).toBeInViewport();
  await expect(box).toBeFocused();

  await box.press('Escape');
  await expect(box).toHaveCount(0);
  await inPane(alice, 1000001).hover();
  await inPane(alice, 1000001).getByRole('button', { name: 'Line actions' }).click();
  await expect(alice.locator('#msg-menu')).toBeVisible();
  await alice.setViewportSize(narrow);
  await expect(alice.locator('#msg-menu')).toBeHidden();
});

// 020-AC8 (Decisions): a typed line being edited stays in sight while the voice lines after it
// leave the chat and come back: the chat does not jump to its end under the edit box.
test('a typed line being edited stays in sight as voice lines move', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [['', { ...line(1000001, 'text', 'what I typed', undefined, await myID(alice)), sender: 'Alice' }]];
  for (let i = 0; i < 40; i++) events.push(['', line(1000002 + i, 'voice', 'spoken ' + i)]);
  await alice.evaluate(() => localStorage.setItem('grill-room-chat-filter', 'voice'));
  await playEvents(alice, room.id, events);
  await inChat(alice, 1000001).hover();
  await messageAction(inChat(alice, 1000001), 'Edit');
  const box = inChat(alice, 1000001).locator('textarea');
  await expect(box).toBeFocused();
  await expect(box).toBeInViewport();
  for (const v of [narrow, wide]) {
    await alice.setViewportSize(v);
    await expect.poll(() => ids(alice, '#voice-lines').then((l) => l.length)).toBe(v === wide ? 40 : 0);
    await expect(box).toBeInViewport();
    await expect(box).toBeFocused();
    // Under the stored Voice the typed line would hide: All for this visit, the choice kept.
    if (v === narrow) await expect(alice.locator('#chat-filter [data-filter=all]')).toHaveAttribute('aria-pressed', 'true');
    expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');
  }
});

// 020-AC8 (Decisions): an edit box left open while the reader types in the composer holds
// nothing: the chat stays at its end, the composer keeps focus, the filter stays.
test('an abandoned edit box does not hold the chat', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [['', { ...line(1000001, 'voice', 'what I said', undefined, await myID(alice)), sender: 'Alice' }]];
  for (let i = 0; i < 60; i++) events.push(['', line(1000002 + i, i % 2 ? 'text' : 'voice', 'filler ' + i)]);
  await playEvents(alice, room.id, events);
  await inPane(alice, 1000001).hover();
  await messageAction(inPane(alice, 1000001), 'Edit');
  await expect(inPane(alice, 1000001).locator('textarea')).toBeFocused();
  await alice.locator('#body').click();
  await expect(alice.locator('#body')).toBeFocused();
  await expect.poll(() => atEnd(alice, '#messages')).toBe(true);
  for (const v of [narrow, wide]) {
    await alice.setViewportSize(v);
    await expect.poll(() => ids(alice, '#voice-lines').then((l) => l.length)).toBe(v === wide ? 31 : 0);
    await expect.poll(() => atEnd(alice, '#messages')).toBe(true);
    await expect(alice.locator('#body')).toBeFocused();
    if (v === narrow) await expect(alice.locator('#chat-filter [data-filter=written]')).toHaveAttribute('aria-pressed', 'true');
  }
});

// 020-AC12: folding brings the filter back and counts the new voice lines the chat hides, your
// own too; a reload keeps it folded; unfolding clears the count.
test('the folded pane counts new voice lines', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await live(alice, line(1e9, 'text', 'typed first'));
  await live(alice, line(1e9 + 1, 'voice', 'before folding'));
  await live(alice, line(1e9 + 2, 'text', 'typed after'));
  await expect(fold(alice)).toHaveAttribute('aria-expanded', 'true');
  await fold(alice).click();
  await expect(fold(alice)).toHaveAttribute('aria-expanded', 'false');
  await expect(alice.locator('#chat-filter')).toBeVisible();
  await expect(alice.locator('#voice-lines')).toBeHidden();
  expect((await pane(alice).boundingBox()).width).toBe(56);
  // Back in the chat once, in its place, under the stored filter: Written hides it.
  await expect(inChat(alice, 1e9 + 1)).toHaveCount(1);
  expect((await ids(alice, '#messages')).filter((id) => id >= 1e9)).toEqual([1e9, 1e9 + 1, 1e9 + 2]);
  await expect(inChat(alice, 1e9 + 1)).toBeHidden();
  await live(alice, line(1e9 + 3, 'voice', 'while folded'));
  await live(alice, line(1e9 + 4, 'voice', 'mine while folded', undefined, await myID(alice)));
  await expect(alice.locator('#transcript-count')).toHaveText('2 new');
  await alice.reload();
  await expect(fold(alice)).toHaveAttribute('aria-expanded', 'false');
  await expect(alice.locator('#chat-filter')).toBeVisible();
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-fold-transcript'))).toBe('1');
  await live(alice, line(1e9 + 5, 'voice', 'after the reload'));
  await expect(alice.locator('#transcript-count')).toHaveText('1 new');
  // The chat showing voice lines clears it: none of them is unseen.
  await alice.locator('#chat-filter [data-filter=all]').click();
  await expect(alice.locator('#transcript-count')).toBeHidden();
  await alice.locator('#chat-filter [data-filter=written]').click();
  await live(alice, line(1e9 + 6, 'voice', 'once more'));
  await expect(alice.locator('#transcript-count')).toHaveText('1 new');
  await fold(alice).click();
  await expect(fold(alice)).toHaveAttribute('aria-expanded', 'true');
  await expect(alice.locator('#transcript-count')).toBeHidden();
  await expect(alice.locator('#chat-filter')).toBeHidden();
  await expect(inPane(alice, 1e9 + 6)).toBeVisible();
});

// 020-AC13: an empty pane says so; while the STT worker is down its header says what the audio
// menu says.
test('the empty pane and voice-to-text off', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(alice.locator('#transcript-empty')).toHaveText('Nobody has been transcribed yet.');
  await expect(alice.locator('#transcript-empty')).toBeVisible();
  await expect(alice.locator('#transcript-off')).toBeHidden();
  await playEvents(alice, room.id, [['stt', { available: false, listening: false, transcribed: [], engines: [] }]]);
  await expect(alice.locator('#transcript .pane-head')).toContainText('Voice-to-text is off');
  await expect(alice.locator('#transcript-off')).toBeVisible();
  await playEvents(alice, room.id, [['stt', { available: true, listening: true, transcribed: [], engines: [] }], ['', line(1000001, 'voice', 'spoken')]]);
  await expect(alice.locator('#transcript-off')).toBeHidden();
  await expect(alice.locator('#transcript-empty')).toBeHidden();
});

// 020-AC14: an aside named Transcription, a polite live list, aria-expanded on the fold, reached
// after the chat's messages and before the composer; the fold shows a visible focus ring.
test('the pane is a labelled aside after the messages', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(alice.getByRole('complementary', { name: 'Transcription' })).toBeVisible();
  await expect(alice.locator('#voice-lines')).toHaveAttribute('aria-live', 'polite');
  await expect(fold(alice)).toHaveAttribute('aria-expanded', 'true');
  const order = await alice.evaluate(() => [...document.querySelectorAll('#messages, #transcript, #send')].map((e) => e.id));
  expect(order).toEqual(['messages', 'transcript', 'send']);
  await fold(alice).focus();
  await alice.keyboard.press('Shift+Tab');
  await alice.keyboard.press('Tab');
  await expect(fold(alice)).toBeFocused();
  expect(await fold(alice).evaluate((b) => getComputedStyle(b).outlineStyle)).not.toBe('none');
  // The triangle is 24 px to the eye, 44 px to the pointer (docs/design.md, drawer header).
  expect(await fold(alice).evaluate((b) => { const r = b.getBoundingClientRect(); const a = getComputedStyle(b, '::after'); return r.width - 2 * parseFloat(a.left); })).toBeGreaterThanOrEqual(44);
});

// 020-AC8: under 64rem, no pane and the filter shows; resizing across 64rem keeps each line once,
// in id order, in whichever list shows it, lines arriving between the crossings included.
test('crossing 64rem moves the voice lines and keeps their order', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [];
  for (let i = 0; i < 12; i++) events.push(['', line(1000001 + i, i % 3 ? 'text' : 'voice', 'line ' + i)]);
  await alice.setViewportSize(narrow);
  await playEvents(alice, room.id, events);
  await expect(pane(alice)).toBeHidden();
  await expect(alice.locator('#chat-filter')).toBeVisible();
  const all = events.map(([, m]) => m.id);
  const voice = all.filter((_, i) => i % 3 === 0);
  expect(await ids(alice, '#messages')).toEqual(all);
  let next = 2000001;
  for (const v of [wide, narrow, wide, narrow]) {
    await alice.setViewportSize(v);
    await expect(pane(alice)).toBeVisible({ visible: v === wide });
    for (const kind of ['voice', 'text', 'voice']) {
      await live(alice, line(next, kind, 'live ' + next));
      all.push(next);
      if (kind === 'voice') voice.push(next);
      next++;
    }
    if (v === wide) {
      await expect.poll(() => ids(alice, '#voice-lines')).toEqual(voice);
      expect(await ids(alice, '#messages')).toEqual(all.filter((id) => !voice.includes(id)));
    } else {
      await expect.poll(() => ids(alice, '#messages')).toEqual(all);
      expect(await ids(alice, '#voice-lines')).toEqual([]);
    }
  }
});

// 020-AC8 020-AC19: the pane shows while the chat keeps 22rem beside its 192 px: at 1024 px a side
// panel dragged to 480 px leaves just that, so it shows at 192; a larger root font leaves less, and
// it hides until the panel narrows or folds.
test('the pane needs room for the chat beside it', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await live(alice, line(1e9, 'voice', 'spoken'));
  await alice.evaluate(() => localStorage.setItem('grill-room-list-w', '480'));
  await alice.setViewportSize({ width: 1024, height: 720 });
  await alice.reload();
  await expect(pane(alice)).toBeVisible();
  expect(Math.round((await pane(alice).boundingBox()).width)).toBe(192);
  expect(Math.round((await alice.locator('#messages').boundingBox()).width)).toBe(352);
  await alice.evaluate(() => { document.documentElement.style.fontSize = '20px'; });
  await alice.setViewportSize({ width: 1030, height: 720 });
  await expect(pane(alice)).toBeHidden();
  await expect(alice.locator('#chat-filter')).toBeVisible();
  await live(alice, line(1e9 + 1, 'voice', 'no room'));
  await expect(inChat(alice, 1e9 + 1)).toHaveCount(1);
  await alice.locator('#side-handle').focus();
  for (let i = 0; i < 14; i++) await alice.keyboard.press('ArrowLeft');
  await expect(alice.locator('#side-handle')).toHaveAttribute('aria-valuenow', '256');
  await expect(pane(alice)).toBeVisible();
  await expect(alice.locator('#chat-filter')).toBeHidden();
  await expect.poll(() => ids(alice, '#voice-lines')).toEqual([1e9 + 1]);
  expect(Math.round((await alice.locator('#messages').boundingBox()).width)).toBeGreaterThanOrEqual(440);
  // Wide again, then folded into its rail: the side panel frees the room.
  for (let i = 0; i < 14; i++) await alice.keyboard.press('ArrowRight');
  await expect(pane(alice)).toBeHidden();
  await alice.locator('#people-fold').click();
  await alice.locator('#docs-fold').click();
  await expect(pane(alice)).toBeVisible();
});

// 020-AC8 020-AC7: a deep link to a voice line at 1280 px scrolls the pane to it; the filter is
// not switched and the stored choice stays.
test('a deep link to a voice line scrolls the pane', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [['', line(1000001, 'voice', 'the spoken one')]];
  for (let i = 0; i < 60; i++) events.push(['', line(1000010 + i, i % 2 ? 'text' : 'voice', 'filler ' + i)]);
  await alice.evaluate(() => { localStorage.setItem('grill-room-chat-filter', 'voice'); history.replaceState(null, '', '#m1000001'); });
  await playEvents(alice, room.id, events);
  await expect(inPane(alice, 1000001)).toBeInViewport();
  expect(await alice.locator('#voice-lines').evaluate((l) => l.scrollTop)).toBe(0);
  // The chat did not move to it: it stays at its end.
  expect(await atEnd(alice, '#messages')).toBe(true);
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');
});

// 020-AC8: the docs window has no chat and no pane: voice lines stay in its hidden chat list.
test('the docs window shows no pane', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.goto(`/r/${room.id}/docs`);
  await playEvents(alice, room.id, [['', line(1000001, 'voice', 'spoken')]]);
  await expect(inChat(alice, 1000001)).toHaveCount(1);
  await expect(pane(alice)).toBeHidden();
  expect(await ids(alice, '#voice-lines')).toEqual([]);
});

// 020-AC8 020-AC10: under All, the chat stays at its end while voice lines leave it and come back.
test('the chat stays at its end as voice lines move', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [];
  for (let i = 0; i < 60; i++) events.push(['', line(1000001 + i, i % 2 ? 'text' : 'voice', 'line ' + i)]);
  await alice.evaluate(() => localStorage.setItem('grill-room-chat-filter', 'all'));
  await alice.setViewportSize(narrow);
  await playEvents(alice, room.id, events);
  await expect.poll(() => atEnd(alice, '#messages')).toBe(true);
  for (const v of [wide, narrow]) {
    await alice.setViewportSize(v);
    await expect.poll(() => ids(alice, '#voice-lines').then((l) => l.length)).toBe(v === wide ? 30 : 0);
    await expect.poll(() => atEnd(alice, '#messages')).toBe(true);
  }
});

// 020-AC18: at 1280 px the pane's text is --fs-sm, 14 px, while the chat's is 15 px; the name and
// the time keep the chat's sizes.
test('the pane text is a step under the chat', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await playEvents(alice, room.id, [['', line(1000001, 'text', 'typed line')], ['', line(1000002, 'voice', 'spoken line')]]);
  const size = (loc) => loc.evaluate((e) => getComputedStyle(e).fontSize);
  expect(await size(inChat(alice, 1000001).locator('.body'))).toBe('15px');
  expect(await size(inPane(alice, 1000002).locator('.body'))).toBe('14px');
  for (const part of ['.meta', '.meta time']) {
    expect(await size(inPane(alice, 1000002).locator(part).first())).toBe(await size(inChat(alice, 1000001).locator(part).first()));
  }
});

const handle = (page) => page.locator('#transcript-handle');
const paneWidth = async (page) => Math.round((await pane(page).boundingBox()).width);
// drag moves the handle's middle by dx.
async function drag(page, dx) {
  const b = await handle(page).boundingBox();
  await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
  await page.mouse.down();
  await page.mouse.move(b.x + b.width / 2 + dx, b.y + b.height / 2, { steps: 5 });
  await page.mouse.up();
}

// 020-AC19: the pane's left edge is a separator, 192 to 480 px, 320 by default, 12 px wide over
// the pane's full height; a drag and ← / → resize it, and the width outlives a reload.
test('the pane resizes from its left edge', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const h = handle(alice);
  await expect(alice.getByRole('separator', { name: 'Resize the transcription pane' })).toBeVisible();
  await expect(h).toHaveAttribute('aria-valuenow', '320');
  await expect(h).toHaveAttribute('aria-valuemin', '192');
  await expect(h).toHaveAttribute('aria-valuemax', '480');
  const [hb, pb] = [await h.boundingBox(), await pane(alice).boundingBox()];
  expect(hb.width).toBeGreaterThanOrEqual(12);
  expect(hb.height).toBe(pb.height);
  expect(Math.abs(hb.x + hb.width / 2 - pb.x)).toBeLessThanOrEqual(1);
  await drag(alice, -100);
  const now = Number(await h.getAttribute('aria-valuenow'));
  expect(Math.abs(now - 420)).toBeLessThanOrEqual(1);
  expect(await paneWidth(alice)).toBe(now);
  await drag(alice, -1000);
  expect(await paneWidth(alice)).toBe(480);
  await drag(alice, 1000);
  expect(await paneWidth(alice)).toBe(192);
  await h.focus();
  await alice.keyboard.press('ArrowLeft');
  await alice.keyboard.press('ArrowLeft');
  await alice.keyboard.press('ArrowRight');
  await expect(h).toHaveAttribute('aria-valuenow', '208');
  expect(await paneWidth(alice)).toBe(208);
  await alice.reload();
  await expect(h).toHaveAttribute('aria-valuenow', '208');
  expect(await paneWidth(alice)).toBe(208);
  // The messages keep the rest of the column.
  const [msgs, p] = [await alice.locator('#messages').boundingBox(), await pane(alice).boundingBox()];
  expect(Math.round(msgs.x + msgs.width)).toBe(Math.round(p.x));
});

// 020-AC19: at 1024 px a drag, or ←, toward the chat stops with the chat at 22rem, the pane still
// shown, and aria-valuemax says where. A width kept from a wider screen is drawn narrower here and
// comes back on the wider screen; at the exact fit the pane shows and stays.
test('a drag never leaves the chat under 22rem', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(handle(alice)).toHaveAttribute('aria-valuemax', '480');
  await alice.setViewportSize({ width: 1024, height: 720 });
  await expect(pane(alice)).toBeVisible();
  // 1024 less the side panel's 256 less the chat's 352.
  const fit = 416;
  await expect(handle(alice)).toHaveAttribute('aria-valuemax', String(fit));
  await drag(alice, -1000);
  await expect(handle(alice)).toHaveAttribute('aria-valuenow', String(fit));
  expect(Math.round((await alice.locator('#messages').boundingBox()).width)).toBe(352);
  expect(await paneWidth(alice)).toBe(fit);
  await expect(pane(alice)).toBeVisible();
  await expect(alice.locator('#chat-filter')).toBeHidden();
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-transcript-w'))).toBe(String(fit));
  await handle(alice).focus();
  await alice.keyboard.press('ArrowLeft');
  await expect(handle(alice)).toHaveAttribute('aria-valuenow', String(fit));
  expect(Math.round((await alice.locator('#messages').boundingBox()).width)).toBe(352);

  await alice.evaluate(() => localStorage.setItem('grill-room-transcript-w', '480'));
  await alice.reload();
  await expect(pane(alice)).toBeVisible();
  await expect(alice.locator('#chat-filter')).toBeHidden();
  expect(await paneWidth(alice)).toBe(fit);
  await expect(handle(alice)).toHaveAttribute('aria-valuenow', String(fit));
  expect(Math.round((await alice.locator('#messages').boundingBox()).width)).toBe(352);
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-transcript-w'))).toBe('480');
  // A bare click on the handle, or ← at the cap, keeps the width kept for a wider screen.
  await handle(alice).click();
  await handle(alice).focus();
  await alice.keyboard.press('ArrowLeft');
  await expect(handle(alice)).toHaveAttribute('aria-valuenow', String(fit));
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-transcript-w'))).toBe('480');
  await alice.setViewportSize({ width: 1280, height: 720 });
  await expect.poll(() => paneWidth(alice)).toBe(480);
  await expect(handle(alice)).toHaveAttribute('aria-valuemax', '480');
  // At the fit, the pane shows and does not flip as the chat's width is observed again.
  await alice.setViewportSize({ width: 1024, height: 720 });
  await alice.evaluate((w) => localStorage.setItem('grill-room-transcript-w', String(w)), fit);
  await alice.reload();
  await expect(pane(alice)).toBeVisible();
  const flips = await alice.evaluate(() => new Promise((done) => {
    let n = 0;
    const was = () => document.body.classList.contains('transcript-room');
    let last = was();
    const mo = new MutationObserver(() => { if (was() !== last) { last = was(); n++; } });
    mo.observe(document.body, { attributes: true, attributeFilter: ['class'] });
    setTimeout(() => { mo.disconnect(); done(n); }, 1000);
  }));
  expect(flips).toBe(0);
});

// 020-AC19: folded, or above the chat with a document open, the pane has no handle.
test('no pane handle folded or with a document open', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(handle(alice)).toBeVisible();
  await fold(alice).click();
  await expect(handle(alice)).toBeHidden();
  await fold(alice).click();
  await expect(handle(alice)).toBeVisible();
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  await expect(pane(alice)).toBeVisible();
  await expect(handle(alice)).toBeHidden();
});
