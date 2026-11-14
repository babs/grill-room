import { test, expect } from './fixtures.js';
import { roomWith, playEvents, line, live, menuItem, messageAction, agentState, openStrip } from './helpers.js';

const log = (page) => page.locator('#chat-log > p');
const myID = async (page) => Number(await page.evaluate(() => myID));
// grow redraws line m as the stream's message_edited does, at change seq; m was written after the
// page loaded, as live() adds it.
const grow = (page, m, seq) => page.evaluate((m) => replaceMessage({ ...m, ts: new Date().toISOString() }), { ...m, change_seq: seq });

// 036-AC1 036-AC2 036-AC9: 30 lines of history leave the log empty; the lists are plain lists
// named by their headings, in the room, the chat window and the docs window; the edit box's
// mention list has no live ancestor and no workaround of its own. The docs window says no line:
// it shows no chat (036 Decisions).
test('history stays silent and the lists are not live', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 1280, height: 720 });
  const history = Array.from({ length: 30 }, (_, i) => ['', line(1000001 + i, i % 3 ? 'text' : 'voice', 'old line ' + i)]);
  await playEvents(alice, room.id, history);
  await expect(alice.locator('#messages > li, #voice-lines > li')).toHaveCount(30);
  await expect(alice.locator('#chat-log')).toHaveAttribute('role', 'log');
  await expect(alice.locator('#chat-log')).toHaveAttribute('aria-live', 'polite');
  await expect(alice.locator('#chat-log')).toHaveAttribute('aria-relevant', 'additions');
  await expect(alice.locator('#chat-log')).toHaveClass('sr-only');
  await expect(log(alice)).toHaveCount(0);
  await expect(alice.getByRole('list', { name: 'Chat', exact: true })).toHaveId('messages');
  await expect(alice.getByRole('list', { name: 'Transcription', exact: true })).toHaveId('voice-lines');

  const quiet = (page) => page.evaluate(() => ['messages', 'voice-lines'].map((id) => {
    const l = document.getElementById(id);
    return [id, l.getAttribute('aria-live'), l.getAttribute('aria-busy'), l.getAttribute('aria-labelledby')];
  }));
  const want = [['messages', null, null, 'chat-title'], ['voice-lines', null, null, 'transcript-title']];
  expect(await quiet(alice)).toEqual(want);
  // Moving the voice lines between the pane and the chat sets no aria-busy either.
  await alice.setViewportSize({ width: 900, height: 720 });
  await expect(alice.locator('#voice-lines > li')).toHaveCount(0);
  expect(await quiet(alice)).toEqual(want);

  const other = await alice.context().newPage();
  for (const path of ['/chat', '/docs']) {
    await other.goto(`/r/${room.id}${path}`);
    await expect(other.locator('#room-name a')).toHaveText('Docs room');
    expect(await quiet(other), path).toEqual(want);
    await live(other, line(2e9, 'text', 'from Carol'));
    await expect(other.locator('#messages > li[data-id="2000000000"]'), path).toHaveCount(1);
    await expect(log(other), path).toHaveCount(path === '/docs' ? 0 : 1);
  }
  await other.close();

  await alice.unrouteAll();
  await alice.reload();
  await alice.locator('#body').fill('Hi there');
  await alice.locator('#body').press('Enter');
  const mine = alice.locator('#messages > li', { hasText: 'Hi there' });
  await messageAction(mine, 'Edit');
  const list = alice.locator('#messages form.edit').getByRole('listbox', { name: 'Mention', includeHidden: true });
  await expect(list).toHaveCount(1);
  await expect(list).not.toHaveAttribute('aria-live');
  expect(await list.evaluate((l) => !!l.closest('[aria-live]'))).toBe(false);
});

// 036-AC3: a line from someone else adds one entry, "Sender: text" as the line reads, files by
// name; a long one is cut at a word; the viewer's own lines add nothing.
test('a new line from someone else is said once', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await live(alice, line(1e9 + 1, 'text', 'hello there'));
  await expect(log(alice)).toHaveText(['Carol: hello there']);
  await live(alice, line(1e9 + 2, 'text', 'my own line', undefined, await myID(alice)));
  await expect(alice.locator('#messages > li', { hasText: 'my own line' })).toHaveCount(1);
  await expect(log(alice)).toHaveCount(1);

  const words = Array.from({ length: 80 }, (_, i) => 'word' + i).join(' ');
  await live(alice, line(1e9 + 3, 'text', words));
  const long = await log(alice).nth(1).textContent();
  expect(long).toMatch(/^Carol: word0 word1 .* word\d+…, more in the chat$/);
  const kept = long.slice('Carol: '.length, -'…, more in the chat'.length);
  expect(kept.length).toBeLessThanOrEqual(280);
  expect(words.startsWith(kept + ' ')).toBe(true);

  await live(alice, { ...line(1e9 + 4, 'text', ''), html: '<p><strong>Bold</strong> and <code>x</code></p><ul><li>one</li><li>two</li></ul>',
    documents: [{ id: 'd1', name: 'plan.md', format: 'markdown', size: 12 }] });
  await expect(log(alice).nth(2)).toHaveText('Carol: Bold and x one two plan.md');
  await expect(log(alice)).toHaveCount(3);
});

// 036-AC4: 6 lines in one burst give the first 3, then one "3 more new messages".
test('a burst folds past three entries', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.clock.install();
  await alice.reload();
  await expect(alice.locator('#me')).toHaveText('Alice');
  await alice.clock.pauseAt(await alice.evaluate(() => Date.now() + 1000));
  for (let i = 0; i < 6; i++) await live(alice, line(1e9 + i, 'text', 'burst ' + i));
  await expect(log(alice)).toHaveText(['Carol: burst 0', 'Carol: burst 1', 'Carol: burst 2']);
  await alice.clock.runFor(2000);
  await expect(log(alice)).toHaveText(['Carol: burst 0', 'Carol: burst 1', 'Carol: burst 2', '3 more new messages']);
});

// 036-AC3 036-AC5 036-AC6: an edit, a delete, a proposal card dismissed and a voice paragraph
// growing update the list and add nothing; voice lines add nothing by default, in the pane or the
// chat; a proposal card and a system line add one entry each, as their text alone.
test('changes and voice lines stay silent', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 1280, height: 720 });
  // Each silent step runs in a fresh burst window: a leak would show, not fold (036-AC4).
  await alice.clock.install();
  await alice.reload();
  await expect(alice.locator('#me')).toHaveText('Alice');
  const text = line(1e9 + 1, 'text', 'first draft');
  await live(alice, text);
  await expect(log(alice)).toHaveText(['Carol: first draft']);
  await grow(alice, { ...text, body: 'second draft', html: '<p>second draft</p>', edited_at: new Date().toISOString() }, 1);
  await expect(alice.locator('#messages > li[data-id="1000000001"]')).toContainText('second draft');
  await grow(alice, { ...text, deleted_at: new Date().toISOString() }, 2);
  await expect(alice.locator('#messages > li[data-id="1000000001"]')).toContainText('message deleted');

  const card = { ...line(1e9 + 2, 'system', 'Grill proposes a room: Pricing'), sender: 'Grill', proposal: { id: 7, status: 'open', trigger: 0 } };
  await live(alice, card);
  await expect(log(alice)).toHaveText(['Carol: first draft', 'Grill proposes a room: Pricing']);
  await live(alice, { ...line(1e9 + 5, 'system', 'The admin renamed the room'), sender: 'admin' });
  await expect(log(alice)).toHaveText(['Carol: first draft', 'Grill proposes a room: Pricing', 'The admin renamed the room']);
  await alice.clock.fastForward(2000);
  await grow(alice, { ...card, proposal: { ...card.proposal, status: 'dismissed' } }, 1);
  await expect(alice.locator('#messages > li.proposal.dismissed')).toHaveCount(1);

  await alice.clock.fastForward(2000);
  const voice = line(1e9 + 3, 'voice', 'spoken words');
  await live(alice, voice);
  await expect(alice.locator('#voice-lines > li[data-id="1000000003"]')).toBeVisible();
  await alice.clock.fastForward(2000);
  await grow(alice, { ...voice, body: 'spoken words and more', html: '<p>spoken words and more</p>' }, 1);
  await expect(alice.locator('#voice-lines > li[data-id="1000000003"]')).toContainText('and more');
  // In the chat, below the pane's width, with every kind shown.
  await alice.setViewportSize({ width: 900, height: 720 });
  await alice.locator('#chat-filter [data-filter=all]').click();
  await alice.clock.fastForward(2000);
  await live(alice, line(1e9 + 4, 'voice', 'in the chat'));
  await expect(alice.locator('#messages > li[data-id="1000000004"]')).toBeVisible();
  await alice.clock.fastForward(2100);
  await expect(log(alice)).toHaveCount(3);
});

// 036-AC7: "Read the transcription aloud" is off by default and kept in this browser; on, a
// paragraph from someone else says "Speaker: words", then each growth only its new words; a
// rewrite, an edit, a delete, the viewer's own paragraph and words gained while off say nothing. A
// paragraph whose speaker was not said yet (read aloud turned on midway, its opening folded, begun
// before the page loaded, any other entry added since) names the speaker with its next words.
test('the transcription read aloud says only new words', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 1280, height: 720 });
  await menuItem(alice, 'Notifications', 'me');
  const box = alice.getByRole('checkbox', { name: 'Read the transcription aloud' });
  await expect(box).not.toBeChecked();
  await box.check();
  await alice.clock.install();
  await alice.reload();
  await menuItem(alice, 'Notifications', 'me');
  await expect(box).toBeChecked();
  await alice.keyboard.press('Escape');

  const p = line(1e9 + 1, 'voice', 'first words');
  await live(alice, p);
  await expect(log(alice)).toHaveText(['Carol: first words']);
  await grow(alice, { ...p, body: 'first words and more', html: '<p>first words and more</p>' }, 1);
  await expect(log(alice)).toHaveText(['Carol: first words', 'and more']);
  await grow(alice, { ...p, body: 'first words and more of them', html: '<p>first words and more of them</p>' }, 2);
  await expect(log(alice)).toHaveText(['Carol: first words', 'and more', 'of them']);
  // Past the burst window: an entry below would show, not fold (036-AC4).
  await alice.clock.fastForward(2000);
  // Longer than before, so a missing prefix test would still find words to say.
  const redo = 'quite another paragraph, longer than the first one';
  await grow(alice, { ...p, body: redo, html: `<p>${redo}</p>` }, 3);
  await grow(alice, { ...p, body: redo + ' by hand', html: `<p>${redo} by hand</p>`, edited_at: new Date().toISOString() }, 4);
  await expect(alice.locator('#voice-lines > li[data-id="1000000001"]')).toContainText('by hand');
  const mine = line(1e9 + 2, 'voice', 'my own words', undefined, await myID(alice));
  await live(alice, mine);
  await grow(alice, { ...mine, body: 'my own words grow', html: '<p>my own words grow</p>' }, 1);
  await expect(alice.locator('#voice-lines > li[data-id="1000000002"]')).toContainText('grow');
  await alice.clock.fastForward(2000);
  await expect(log(alice)).toHaveCount(3);

  // Turned off mid-paragraph, then on: only the words gained since it is back on.
  const q = line(1e9 + 3, 'voice', 'one');
  await live(alice, q);
  await expect(log(alice).last()).toHaveText('Carol: one');
  const readAloud = async (on) => {
    await menuItem(alice, 'Notifications', 'me');
    await box.setChecked(on);
    await alice.keyboard.press('Escape');
  };
  await readAloud(false);
  await grow(alice, { ...q, body: 'one two', html: '<p>one two</p>' }, 1);
  await expect(alice.locator('#voice-lines > li[data-id="1000000003"]')).toContainText('one two');
  await readAloud(true);
  await grow(alice, { ...q, body: 'one two three', html: '<p>one two three</p>' }, 2);
  await expect(log(alice).last()).toHaveText('three');
  // Deleted, "message" would grow into "message deleted".
  const r = line(1e9 + 4, 'voice', 'message');
  await live(alice, r);
  await expect(log(alice).last()).toHaveText('Carol: message');
  await grow(alice, { ...r, deleted_at: new Date().toISOString() }, 1);
  await expect(alice.locator('#voice-lines > li[data-id="1000000004"]')).toContainText('message deleted');
  await alice.clock.fastForward(2000);
  const before = ['Carol: first words', 'and more', 'of them', 'Carol: one', 'three', 'Carol: message'];
  await expect(log(alice)).toHaveText(before);

  await readAloud(false);
  const u = line(1e9 + 5, 'voice', 'started');
  await live(alice, u);
  await grow(alice, { ...u, body: 'started off', html: '<p>started off</p>' }, 1);
  await expect(alice.locator('#voice-lines > li[data-id="1000000005"]')).toContainText('started off');
  await readAloud(true);
  await grow(alice, { ...u, body: 'started off now on', html: '<p>started off now on</p>' }, 2);
  await expect(log(alice).last()).toHaveText('Carol: now on');

  await alice.clock.fastForward(2000);
  for (let i = 0; i < 3; i++) await live(alice, line(1e9 + 10 + i, 'text', 'filler ' + i));
  const f = line(1e9 + 20, 'voice', 'opening');
  await live(alice, f);
  await alice.clock.fastForward(2000);
  await expect(log(alice).last()).toHaveText('1 more new message');
  await grow(alice, { ...f, body: 'opening words', html: '<p>opening words</p>' }, 1);
  await expect(log(alice)).toHaveText([...before, 'Carol: now on', 'Carol: filler 0', 'Carol: filler 1', 'Carol: filler 2', '1 more new message', 'Carol: words']);

  // A paragraph begun before a reload: history is silent, its growth is not.
  const h = line(1e9 + 30, 'voice', 'said before');
  await playEvents(alice, room.id, [['', h]]);
  await expect(alice.locator('#voice-lines > li[data-id="1000000030"]')).toBeVisible();
  await expect(log(alice)).toHaveCount(0);
  // Redrawn as the stream does, with the time it was first written: before this load.
  const regrow = (body, seq) => alice.evaluate((m) => replaceMessage(m), { ...h, body, html: `<p>${body}</p>`, change_seq: seq });
  await regrow('said before and after', 1);
  await regrow('said before and after again', 2);
  await expect(log(alice)).toHaveText(['Carol: and after', 'again']);

  // Another speaker's line said between two growths: the speaker is named again.
  await alice.clock.fastForward(2000);
  const pa = line(1e9 + 40, 'voice', 'a one');
  const pb = { ...line(1e9 + 41, 'voice', 'b one', undefined, 999998), sender: 'Dave' };
  await live(alice, pa);
  await grow(alice, { ...pa, body: 'a one two', html: '<p>a one two</p>' }, 1);
  await alice.clock.fastForward(2000);
  await live(alice, pb);
  await grow(alice, { ...pa, body: 'a one two three', html: '<p>a one two three</p>' }, 2);
  const earlier = ['Carol: and after', 'again', 'Carol: a one', 'two', 'Dave: b one', 'Carol: three'];
  await expect(log(alice)).toHaveText(earlier);

  // So does any other entry: a typed line (it does not end a paragraph), or a fold's flush.
  await alice.clock.fastForward(2000);
  const pc = line(1e9 + 42, 'voice', 'c one');
  await live(alice, pc);
  await grow(alice, { ...pc, body: 'c one two', html: '<p>c one two</p>' }, 1);
  await live(alice, { ...line(1e9 + 43, 'text', 'hi', undefined, 999998), sender: 'Dave' });
  await alice.clock.fastForward(2000);
  await grow(alice, { ...pc, body: 'c one two three', html: '<p>c one two three</p>' }, 2);
  await alice.clock.fastForward(2000);
  const pd = line(1e9 + 44, 'voice', 'd one');
  await live(alice, pd);
  for (let i = 0; i < 2; i++) await live(alice, line(1e9 + 45 + i, 'text', 'more ' + i));
  await grow(alice, { ...pd, body: 'd one two', html: '<p>d one two</p>' }, 1);
  await alice.clock.fastForward(2000);
  await grow(alice, { ...pd, body: 'd one two three', html: '<p>d one two three</p>' }, 2);
  await expect(log(alice)).toHaveText([...earlier, 'Carol: c one', 'two', 'Dave: hi', 'Carol: three',
    'Carol: d one', 'Carol: more 0', 'Carol: more 1', '1 more new message', 'Carol: three']);
});

// 036 Decisions, 036-AC5: feedback on the viewer's own action is said through #toast-live, not
// #chat-log: mention notices on a send (005-AC3) and an edit (016-AC21), a proposal dismissed, a
// blocked website allowed from its line or the approvals banner, a banner dismiss. A blocked
// website's outcome is said once when a redraw changes it, to everyone.
test('feedback on my own action is said as a status', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const said = alice.locator('#toast-live');
  await alice.locator('#body').fill('Hi @nobody');
  await alice.locator('#body').press('Enter');
  await expect(said).toContainText('@nobody: no persona or participant has that name. Only you see this.');
  await alice.locator('#body').fill('Second thought');
  await alice.locator('#body').press('Enter');
  await messageAction(alice.locator('#messages > li', { hasText: 'Second thought' }), 'Edit');
  const box = alice.getByRole('textbox', { name: 'New text' });
  await box.press('End');
  await box.pressSequentially(' @someone');
  await expect(alice.locator('#messages form.edit .mention-list')).toBeHidden();
  await box.press('Enter');
  await expect(said).toContainText('@someone: no persona or participant has that name. Only you see this.');

  const me = await myID(alice);
  await alice.route('**/api/rooms/*/proposals/7/dismiss', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
  await live(alice, { ...line(1e9 + 1, 'system', 'Grill proposes a room: Pricing'), sender: 'Grill', proposal: { id: 7, status: 'open', trigger: me } });
  await alice.locator('#messages > li.proposal').getByRole('button', { name: 'Dismiss' }).click();
  await expect(said).toContainText('Proposal dismissed');
  await expect(log(alice).last()).toHaveText('Grill proposes a room: Pricing');
  await expect(alice.locator('#chat-log')).not.toContainText(/nobody|Second thought/);

  // The AI host's view of blocked websites, in the chat and the approvals banner, each request
  // answered at once. The outcomes come as the stream redraws the lines (014-AC9): a room event,
  // said once to every viewer, whoever asked (036-AC5).
  const blocked = (id, domain) => ({ ...line(id, 'system', `Grill tried to reach ${domain} (blocked)`, '2099-01-01T00:00:00Z'), sender: 'Grill',
    blocked: { domain, persona: 'Grill', count: 1, decision: '' } });
  const [a, b, c] = [blocked(1e9 + 2, 'example.com'), blocked(1e9 + 3, 'other.org'), blocked(1e9 + 4, 'third.net')];
  const requests = [b, c].map((m) => ({ message_id: m.id, domain: m.blocked.domain, persona: 'Grill', count: 1 }));
  for (const act of ['allow', 'dismiss']) {
    await alice.route(`**/api/rooms/*/blocked/*/${act}`, (r) => r.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
  }
  const state = { ...(await agentState(alice, room.id)), you_host: true, egress_host: '', blocked_requests: requests };
  // A paused clock keeps every status said, each one otherwise gone after 5 s.
  await alice.clock.install();
  await playEvents(alice, room.id, [['agent', state], ['', a], ['', b], ['', c]]);
  await alice.clock.pauseAt(await alice.evaluate(() => Date.now() + 1000));
  const statuses = (page) => page.locator('#toast-live > p');
  const allowed = { ...a, body: 'Alice allowed example.com for this room', blocked: { ...a.blocked, decision: 'room' } };
  await alice.locator(`#messages > li[data-id="${a.id}"]`).getByRole('button', { name: 'Allow for this room' }).click();
  await expect(statuses(alice)).toHaveText(['Allowing example.com…']);
  await grow(alice, { ...a, blocked: { ...a.blocked, pending: 'room' } }, 1);
  await grow(alice, allowed, 2);
  await grow(alice, { ...allowed, blocked: { ...allowed.blocked, count: 2 } }, 3);
  await expect(statuses(alice)).toHaveText(['Allowing example.com…', allowed.body]);
  // Bob asked nothing and hears it too.
  await live(bob, a);
  await grow(bob, allowed, 2);
  await expect(statuses(bob).last()).toHaveText(allowed.body);

  await openStrip(alice);
  await alice.getByRole('button', { name: 'Allow for this room: other.org', exact: true }).click();
  await expect(statuses(alice).last()).toHaveText('Allowing other.org…');
  // Pending, then refused, in one redraw: the stream coalesces a line's changes.
  await grow(alice, { ...b, blocked: { ...b.blocked, error: 'the AI host app is offline' } }, 1);
  await alice.getByRole('button', { name: 'Dismiss: third.net', exact: true }).click();
  await expect(statuses(alice)).toHaveText(['Allowing example.com…', allowed.body, 'Allowing other.org…', 'Not allowed: the AI host app is offline', 'Dismissed third.net']);
  await expect(log(alice)).toHaveText([a, b, c].map((m) => m.body));
});

// 036-AC4: the "N more" entry counts in the window too: never more than 3 entries in any 2 s, a
// line just after a flush included.
test('a burst never says more than 3 in 2 s', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.clock.install();
  await alice.reload();
  await expect(alice.locator('#me')).toHaveText('Alice');
  await alice.clock.pauseAt(await alice.evaluate(() => Date.now() + 1000));
  // When each entry went out, on the page's clock.
  await alice.evaluate(() => {
    const el = document.getElementById('chat-log');
    const append = el.append.bind(el);
    window.entryTimes = [];
    el.append = (...n) => {
      window.entryTimes.push(Date.now());
      append(...n);
    };
  });
  let at = 0;
  for (const [i, t] of [0, 1900, 1950, 1960, 2010, 4010].entries()) {
    if (t > at) await alice.clock.runFor(t - at);
    at = t;
    await live(alice, line(1e9 + i, 'text', 'line ' + i));
  }
  await alice.clock.runFor(4000);
  await expect(log(alice)).toHaveText(['Carol: line 0', 'Carol: line 1', 'Carol: line 2', '1 more new message', '1 more new message', 'Carol: line 5']);
  const times = await alice.evaluate(() => window.entryTimes);
  for (const t of times) expect(times.filter((u) => u > t - 2000 && u <= t).length, String(times)).toBeLessThanOrEqual(3);
});

// 036-AC8: 25 lines, spread past the burst rule, leave the last 20 entries.
test('the log keeps 20 entries', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.clock.install();
  await alice.reload();
  await expect(alice.locator('#me')).toHaveText('Alice');
  for (let i = 0; i < 25; i++) {
    if (i && i % 3 === 0) await alice.clock.fastForward(2000);
    await live(alice, line(1e9 + i, 'text', 'line ' + i));
  }
  await expect(log(alice)).toHaveCount(20);
  await expect(log(alice).first()).toHaveText('Carol: line 5');
  await expect(log(alice).last()).toHaveText('Carol: line 24');
});
