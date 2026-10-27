import { test, expect } from './fixtures.js';
import { roomWith, playEvents } from './helpers.js';

// The e2e server has no STT worker: voice lines reach the page as the stream would send them.
const old = '2026-01-01T10:00:00Z';
const line = (id, kind, body, ts = old) => ({ id, kind, participant_id: kind === 'system' ? 0 : 999999, sender: 'Carol', body, html: '<p>' + body + '</p>', ts, change_seq: 0 });
const seg = (page, f) => page.locator(`#chat-filter [data-filter=${f}]`);
const msg = (page, id) => page.locator(`#messages li[data-id="${id}"]`);

// 020-AC4 020-AC5: Written by default shows typed lines, persona replies and room events; Voice
// only voice lines; All everything. The choice survives a reload; history adds no count (020-AC6).
test('the filter shows written, voice or all, and keeps the choice', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await playEvents(alice, room.id, [
    ['', line(1000001, 'system', 'Bob joined')], ['', line(1000002, 'text', 'typed line')],
    ['', line(1000003, 'agent', 'persona reply')], ['', line(1000004, 'voice', 'spoken line')],
  ]);
  await expect(seg(alice, 'written')).toHaveAttribute('aria-pressed', 'true');
  for (const id of [1000001, 1000002, 1000003]) await expect(msg(alice, id)).toBeVisible();
  await expect(msg(alice, 1000004)).toBeHidden();
  await expect(seg(alice, 'voice')).toHaveText('Voice');

  await seg(alice, 'voice').click();
  await expect(msg(alice, 1000004)).toBeVisible();
  for (const id of [1000001, 1000002, 1000003]) await expect(msg(alice, id)).toBeHidden();
  await alice.reload();
  await expect(seg(alice, 'voice')).toHaveAttribute('aria-pressed', 'true');
  await expect(seg(alice, 'written')).toHaveAttribute('aria-pressed', 'false');
  await expect(msg(alice, 1000004)).toBeVisible();
  await expect(msg(alice, 1000002)).toBeHidden();

  await seg(alice, 'all').click();
  for (const id of [1000001, 1000002, 1000003, 1000004]) await expect(msg(alice, id)).toBeVisible();
  await alice.reload();
  await expect(seg(alice, 'all')).toHaveAttribute('aria-pressed', 'true');
  await expect(msg(alice, 1000004)).toBeVisible();
  await expect(alice.locator('#filter-empty')).toBeHidden();
});

// 020-AC4: a filter that hides every line says so, in a voice-only room as in a written one; in a
// room you are alone in, the invite card speaks instead.
test('a filter with nothing to show says so', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await seg(alice, 'voice').click();
  await expect(alice.locator('#filter-empty')).toHaveText('No voice messages yet.');
  await expect(alice.locator('#filter-empty')).toBeVisible();
  await playEvents(alice, room.id, [['', line(1000001, 'voice', 'only voice')]]);
  await seg(alice, 'written').click();
  await expect(alice.locator('#filter-empty')).toHaveText('No written messages yet.');
  await expect(alice.locator('#filter-empty')).toBeVisible();
  await seg(alice, 'voice').click();
  await expect(alice.locator('#filter-empty')).toBeHidden();

  const { pages: [carol] } = await roomWith(browser, request, ['Carol']);
  await seg(carol, 'voice').click();
  await expect(carol.locator('#chat-empty')).toBeVisible();
  await expect(carol.locator('#filter-empty')).toBeHidden();
});

// 020-AC6: a live line of a hidden kind counts on its segment; a paragraph append does not; showing
// the segment, or All, clears it.
test('a hidden kind counts its new lines', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const live = (m) => alice.evaluate((m) => addMessage({ ...m, ts: new Date().toISOString() }), m);
  await live(line(1e9, 'voice', 'first words'));
  await expect(seg(alice, 'voice')).toHaveText('Voice · 1');
  await alice.evaluate((m) => replaceMessage(m), { ...line(1e9, 'voice', 'first words and more'), change_seq: 1 });
  await expect(msg(alice, 1e9)).toContainText('and more');
  await expect(seg(alice, 'voice')).toHaveText('Voice · 1');
  await live(line(1e9 + 1, 'voice', 'second line'));
  await expect(seg(alice, 'voice')).toHaveText('Voice · 2');
  await expect(seg(alice, 'written')).toHaveText('Written');
  await seg(alice, 'voice').click();
  await expect(seg(alice, 'voice')).toHaveText('Voice');

  // A typed line from someone else while Voice shows counts on Written; All clears it.
  await bob.locator('#body').fill('typed while voice');
  await bob.locator('#body').press('Enter');
  await expect(alice.locator('#messages')).toContainText('typed while voice');
  await expect(seg(alice, 'written')).toHaveText('Written · 1');
  await seg(alice, 'all').click();
  await expect(seg(alice, 'written')).toHaveText('Written');
  await live(line(1e9 + 2, 'voice', 'shown at once'));
  await expect(seg(alice, 'voice')).toHaveText('Voice');
});

// 020-AC6 (Decisions): your own line never counts; sending under Voice shows All for this visit,
// the stored choice kept.
test('your own line shows under Voice and counts nowhere', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await seg(alice, 'voice').click();
  await alice.locator('#body').fill('mine under voice');
  await alice.locator('#body').press('Enter');
  const mine = alice.locator('.msg.text', { hasText: 'mine under voice' });
  await expect(mine).toBeVisible();
  await expect(seg(alice, 'all')).toHaveAttribute('aria-pressed', 'true');
  await expect(seg(alice, 'written')).toHaveText('Written');
  // A line of mine arriving while Written is hidden, as from another tab, counts nowhere either.
  await seg(alice, 'voice').click();
  await alice.evaluate(() => fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"body":"from my other tab"}' }));
  await expect(alice.locator('#messages')).toContainText('from my other tab');
  await expect(seg(alice, 'written')).toHaveText('Written');
  await alice.reload();
  await expect(seg(alice, 'voice')).toHaveAttribute('aria-pressed', 'true');
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');
});

// 020-AC6 005-AC12: live or history goes by the server's clock: a browser 30 s behind it neither
// counts the replayed history nor misses a live line, on the segment and in the title.
test('a skewed browser clock does not count history', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await bob.locator('#body').fill('said before the reload');
  await bob.locator('#body').press('Enter');
  await expect(alice.locator('#messages')).toContainText('said before the reload');
  await seg(alice, 'voice').click();
  await alice.context().addInitScript(() => {
    const now = Date.now;
    Date.now = () => now() - 30_000;
    document.hasFocus = () => false;
  });
  await alice.reload();
  await expect(alice.locator('#messages')).toContainText('said before the reload');
  await expect(seg(alice, 'written')).toHaveText('Written');
  await expect(alice).toHaveTitle('Docs room · grill-room');
  await bob.locator('#body').fill('said after the reload');
  await bob.locator('#body').press('Enter');
  await expect(alice.locator('#messages')).toContainText('said after the reload');
  await expect(seg(alice, 'written')).toHaveText('Written · 1');
  await expect(alice).toHaveTitle('(1) Docs room · grill-room');
});

// 020-AC6 005-AC12: a line sent the instant before a reload is history, on the segment and in
// the title: the room answers the server's clock to the millisecond.
test('a line sent just before a reload is not counted', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await seg(alice, 'voice').click();
  await alice.context().addInitScript(() => { document.hasFocus = () => false; });
  for (const text of ['just before one', 'just before two', 'just before three']) {
    await bob.locator('#body').fill(text);
    await bob.locator('#body').press('Enter');
    await expect(bob.locator('#body')).toHaveValue('');
    await alice.reload();
    await expect(alice.locator('#messages')).toContainText(text);
    await expect(seg(alice, 'written')).toHaveText('Written');
    await expect(alice).toHaveTitle('Docs room · grill-room');
  }
});

// 020-AC7: a deep link to a line the filter hides opens with the filter on All, the line in view;
// the stored choice stays (Decisions).
test('a deep link to a hidden line switches to All', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [['', line(1000001, 'voice', 'the spoken one')], ['', line(1000002, 'text', 'the typed one')]];
  for (let i = 0; i < 60; i++) events.push(['', line(1000010 + i, i % 2 ? 'text' : 'voice', 'filler ' + i)]);
  await alice.evaluate(() => history.replaceState(null, '', '#m1000001'));
  await playEvents(alice, room.id, events);
  await expect(seg(alice, 'all')).toHaveAttribute('aria-pressed', 'true');
  await expect(msg(alice, 1000001)).toBeInViewport();
  await alice.evaluate(() => history.replaceState(null, '', location.pathname));
  await alice.reload();
  await expect(seg(alice, 'written')).toHaveAttribute('aria-pressed', 'true');
  await expect(msg(alice, 1000001)).toBeHidden();

  // From Voice, a link to a typed line.
  await seg(alice, 'voice').click();
  await alice.evaluate(() => history.replaceState(null, '', '#m1000002'));
  await alice.reload();
  await expect(seg(alice, 'all')).toHaveAttribute('aria-pressed', 'true');
  await expect(msg(alice, 1000002)).toBeInViewport();
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');

  // A link to a line the filter shows leaves it alone.
  await alice.evaluate(() => history.replaceState(null, '', '#m1000010'));
  await alice.reload();
  await expect(msg(alice, 1000010)).toBeInViewport();
  await expect(seg(alice, 'voice')).toHaveAttribute('aria-pressed', 'true');
});

// 020-AC7: a mention chip's link to the persona's reply, hidden under Voice, shows All and the
// reply; the stored choice stays. The chip comes from the line's stored turn state, no AI host.
test('a mention chip to a hidden reply switches to All', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const events = [['', { ...line(1000001, 'voice', 'Grill what next'), mentions: [{ persona_id: 1, status: 'done', line_id: 1000002 }] }],
    ['', line(1000002, 'agent', 'the reply')]];
  for (let i = 0; i < 60; i++) events.push(['', line(1000010 + i, 'voice', 'filler ' + i)]);
  await seg(alice, 'voice').click();
  await playEvents(alice, room.id, events);
  await expect(msg(alice, 1000002)).toBeHidden();
  await msg(alice, 1000001).locator('.mention-turn').scrollIntoViewIfNeeded();
  await msg(alice, 1000001).locator('.mention-turn').click();
  await expect(seg(alice, 'all')).toHaveAttribute('aria-pressed', 'true');
  await expect(msg(alice, 1000002)).toBeInViewport();
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');
});

// 020-AC15: message text and the composer are 15 px on the room page above 40rem, 16 px up to it;
// the meta line keeps --fs-sm. The filter's targets are 44 px on a phone.
test('room message text is 15 px on a desktop, 16 px on a phone', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.locator('#body').fill('size me');
  await alice.locator('#body').press('Enter');
  const body = alice.locator('.msg.text .body', { hasText: 'size me' });
  const size = (l) => l.evaluate((e) => getComputedStyle(e).fontSize);
  await alice.setViewportSize({ width: 1280, height: 800 });
  expect([await size(body), await size(alice.locator('#body')), await size(alice.locator('.msg.text .meta').first())]).toEqual(['15px', '15px', '14px']);
  // Exactly 40rem is the phone's, as the kit's (max-width: 40rem) says.
  await alice.setViewportSize({ width: 640, height: 800 });
  expect([await size(body), await size(alice.locator('#body'))]).toEqual(['16px', '16px']);
  await alice.setViewportSize({ width: 375, height: 700 });
  expect([await size(body), await size(alice.locator('#body')), await size(alice.locator('.msg.text .meta').first())]).toEqual(['16px', '16px', '14px']);
  expect(await alice.locator('#chat-filter button').evaluateAll((bs) => bs.filter((b) => b.offsetWidth < 44 || b.offsetHeight < 44).length)).toBe(0);
});

// 020-AC16: the filter does not touch the unread title count: a typed line counts with Voice
// shown, a voice line from someone else still needs the opt-in with Written shown.
test('the unread count ignores the filter', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await seg(alice, 'voice').click();
  await alice.evaluate(() => { document.hasFocus = () => false; });
  await bob.locator('#body').fill('hidden but counted');
  await bob.locator('#body').press('Enter');
  await expect(alice).toHaveTitle('(1) Docs room · grill-room');
  await seg(alice, 'written').click();
  await alice.evaluate((m) => addMessage({ ...m, ts: new Date().toISOString() }), line(1e9, 'voice', 'not counted'));
  await expect(seg(alice, 'voice')).toHaveText('Voice · 1');
  await expect(alice).toHaveTitle('(1) Docs room · grill-room');
});
