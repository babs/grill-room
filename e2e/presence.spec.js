import { test, expect } from './fixtures.js';
import { roomWith, smallTargets } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const phone = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };

async function phonePage(browser, room, request) {
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext(phone)).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Carol');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Carol');
  return page;
}

const row = (page, name) => page.locator('#participants li, #participants-away li', { has: page.locator('.name', { hasText: name }) });

// 005-AC6, 005-AC7, 005-AC14 at phone width: the list opens over the chat, shows who is in the room
// with their role badges and audio state from the server, and moves someone who left to "Not here"
// once the grace runs out.
test('participant list on a phone follows the server presence', async ({ browser, request }) => {
  test.setTimeout(90_000);
  const { room, pages: [, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const carol = await phonePage(browser, room, request);
  const carolID = await carol.evaluate(() => myID);
  expect((await request.put(`/api/rooms/${room.id}/admins/${carolID}`, { headers: admin })).status()).toBe(204);

  // 005-AC36: the phone's People tab.
  await expect(carol.locator('.layout > aside')).toBeHidden();
  await carol.locator('#tabbar').getByRole('button', { name: /People/ }).click();
  await expect(carol.locator('.layout > aside')).toBeVisible();
  await expect(carol.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol']);
  await expect(carol.locator('#tab-people-count')).toHaveText('3');
  // 008-AC18: you on the name, a room admin's @ in the sigil column.
  const me = row(carol, 'Carol');
  await expect(me.locator('.name')).toHaveClass(/\bme\b/);
  await expect(me.locator('.sigil')).toHaveText('@');
  await expect(me.locator('.sigil')).toHaveAttribute('data-tip', 'Room admin');
  await expect(row(carol, 'Bob').locator('.name')).not.toHaveClass(/\bme\b/);
  await expect(row(carol, 'Bob').locator('.sigil')).toHaveText('');
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-state', 'none');

  // A second tab of Bob's in the audio, muted: everyone sees it, in the audio or not.
  const beat = (body) => bob.evaluate(([id, b]) => fetch(`/api/rooms/${id}/presence`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b),
  }).then((r) => r.status), [room.id, body]);
  expect(await beat({ tab: 'bob-second', audio: true, muted: true })).toBe(204);
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-state', 'muted');
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-tip', 'Muted');
  // That tab closes: its audio goes at once, Bob stays, his first tab still open.
  expect(await beat({ tab: 'bob-second', gone: true })).toBe(204);
  await expect(row(carol, 'Bob').locator('.audio')).toHaveAttribute('data-state', 'none');

  // Bob closes his page: within the grace he stays, then he is not here, collapsed, seen just now.
  await bob.context().close();
  // Within the 20 s grace he is still listed: checked at once, no wait needed.
  await expect(carol.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol']);
  await expect(carol.locator('#participants li .name')).toHaveText(['Alice', 'Carol'], { timeout: 30_000 });
  await expect(carol.locator('#away summary')).toHaveText('Not here (1)');
  await expect(row(carol, 'Bob')).toBeHidden();
  await carol.locator('#away summary').click();
  await expect(row(carol, 'Bob')).not.toContainText('seen');

  await carol.locator('#tabbar').getByRole('button', { name: /Chat/ }).click();
  await expect(carol.locator('.layout > aside')).toBeHidden();
});

// 005-AC7, 005-AC22: each audio state, the speaking pulse's neighbours, the deafened icon and the
// "transcribed" badge, from presence and stt events played to the page as the server sends them.
test('participant list shows audio states and transcription', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol', 'Dan', 'Eve']);
  const ids = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.participants.map((p) => p.id)), room.id);
  const [a, b, c, d, e] = ids;
  const away = new Date(Date.now() - 5 * 60_000).toISOString();
  const people = [
    { id: a, here: true, audio: true, mic: true, muted: false, deaf: false },
    { id: b, here: true, audio: true, mic: false, muted: true, deaf: false },
    { id: c, here: true, audio: true, mic: false, muted: false, deaf: true },
    { id: d, here: true, audio: false, mic: false, muted: false, deaf: false },
    { id: e, here: false, audio: false, mic: false, muted: false, deaf: false, seen_at: away },
  ];
  await alice.route(`**/api/rooms/${room.id}/events*`, (r) => r.fulfill({
    status: 200, headers: { 'Content-Type': 'text/event-stream' },
    body: `event: presence\ndata: ${JSON.stringify({ people })}\n\n`
      + `event: stt\ndata: ${JSON.stringify({ available: true, listening: true, transcribed: [a, b] })}\n\n`,
  }));
  await alice.reload();
  const state = (name) => row(alice, name).locator('.audio');
  await expect(state('Alice')).toHaveAttribute('data-state', 'live');
  await expect(state('Alice')).toHaveAttribute('data-tip', 'Mic live');
  // 009-AC36: the meaning shows in the styled tip, not a native title.
  await state('Alice').hover();
  await expect(alice.locator('#person-tip')).toHaveText('Mic live');
  await expect(alice.locator('#participants [title], #participants-away [title]')).toHaveCount(0);
  await expect(state('Bob')).toHaveAttribute('data-state', 'muted');
  await expect(state('Carol')).toHaveAttribute('data-state', 'listening');
  await expect(state('Carol')).toHaveAttribute('data-tip', 'Listening only, deafened: hears nobody');
  await expect(state('Carol')).toHaveAttribute('data-icon', 'deafened');
  await expect(state('Dan')).toHaveAttribute('data-state', 'none');
  for (const [name, n] of [['Alice', 1], ['Bob', 1], ['Carol', 0], ['Dan', 0]]) {
    await expect(row(alice, name).locator('.transcribed')).toHaveCount(n);
  }
  await expect(alice.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol', 'Dan']);
  await expect(alice.locator('#away summary')).toHaveText('Not here (1)');
  await alice.locator('#away summary').click();
  // 008-AC18: when they were seen is in the card, not the row.
  await expect(row(alice, 'Eve')).not.toContainText('seen');
  await row(alice, 'Eve').locator('.name').focus();
  await expect(alice.locator('#person-card')).toContainText('Seen 5 minutes ago');
});

// 005-AC19: event lines show their time, as messages do.
test('event lines show their time', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const line = alice.locator('.msg.system', { hasText: 'Alice joined' });
  await expect(line.locator('time')).toHaveText(/^\d{2}:\d{2}/);
  await expect(line.locator('time')).toHaveAttribute('datetime', /^\d{4}-\d{2}-\d{2}T/);
});

// 008-AC13 008-AC14: one row per person, of one height, with roles, the audio state and
// "transcribed" on the same line, at 1440 and 390 px; the drawer headers' actions share one style.
for (const [label, size] of [['desktop', { width: 1440, height: 900 }], ['phone', { width: 390, height: 844 }]]) {
  test(`people rows stay one line, ${label}`, async ({ browser, request }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol', 'Dan with a rather long name to cut']);
    const ids = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.participants.map((p) => p.id)), room.id);
    expect((await request.put(`/api/rooms/${room.id}/admins/${ids[0]}`, { headers: admin })).status()).toBe(204);
    const [a, b, c, d] = ids;
    const people = [
      { id: a, here: true, audio: true, mic: true, muted: false, deaf: false },
      { id: b, here: true, audio: true, mic: false, muted: true, deaf: false },
      { id: c, here: true, audio: true, mic: false, muted: false, deaf: true },
      { id: d, here: true, audio: false, mic: false, muted: false, deaf: false },
    ];
    await alice.route(`**/api/rooms/${room.id}/events*`, (r) => r.fulfill({
      status: 200, headers: { 'Content-Type': 'text/event-stream' },
      body: `event: presence\ndata: ${JSON.stringify({ people })}\n\n`
        + `event: stt\ndata: ${JSON.stringify({ available: true, listening: true, transcribed: [a, b] })}\n\n`,
    }));
    await alice.setViewportSize(size);
    await alice.reload();
    if (label === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /People/ }).click();
    const alices = alice.locator('#participants li', { hasText: 'Alice' });
    await expect(alices.locator('.sigil')).toHaveText('@');
    await expect(alices.locator('.transcribed')).toHaveAccessibleName('The transcription hears this voice');
    await expect(alice.locator('#participants li .audio[data-state=live]')).toHaveCount(1);
    const rows = await alice.locator('#participants li').evaluateAll((lis) => lis.map((li) => {
      const r = li.getBoundingClientRect();
      const name = li.querySelector('.name');
      const tags = li.querySelector('.tags').getBoundingClientRect();
      return {
        h: Math.round(r.height),
        oneLine: Math.abs(tags.top + tags.height / 2 - (name.getBoundingClientRect().top + name.getBoundingClientRect().height / 2)) < 2,
        nameLines: Math.round(name.getBoundingClientRect().height / parseFloat(getComputedStyle(name).lineHeight)),
        inside: tags.right <= r.right + 0.5,
        icons: [...li.querySelectorAll('.tags .tag:not(.blank)')].map((e) => Math.round(e.getBoundingClientRect().height)),
        // One glyph per square: nothing spills out of it.
        spill: [...li.querySelectorAll('.tags .tag')].filter((e) => e.scrollWidth > e.clientWidth + 0.5).map((e) => e.textContent),
      };
    }));
    expect(rows).toHaveLength(4);
    expect(new Set(rows.map((r) => r.h)), JSON.stringify(rows)).toEqual(new Set([rows[0].h]));
    for (const r of rows) {
      expect(r, JSON.stringify(r)).toMatchObject({ oneLine: true, nameLines: 1, inside: true, spill: [] });
      expect(new Set(r.icons).size, JSON.stringify(r)).toBeLessThanOrEqual(1);
    }
    expect(rows[0].icons.length).toBe(2);
    // 008-AC13: each badge column at one x and one 20 px square on every row.
    const cols = await alice.locator('#participants li').evaluateAll((lis) => {
      const by = {};
      for (const li of lis) {
        for (const t of li.querySelectorAll('.tag')) {
          const r = t.getBoundingClientRect();
          const col = [...t.classList].find((c) => c.startsWith('tag-'));
          (by[col] ||= new Set()).add([Math.round(r.left), Math.round(r.width), Math.round(r.height)].join(','));
        }
      }
      return Object.fromEntries(Object.entries(by).map(([k, v]) => [k, [...v]]));
    });
    expect(Object.keys(cols)).toEqual(['tag-transcribed', 'tag-audio']);
    for (const [col, boxes] of Object.entries(cols)) {
      expect(boxes, col).toHaveLength(1);
      expect(boxes[0].split(',').slice(1), col).toEqual(['20', '20']);
    }
    if (label === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
    const actions = await alice.locator('#docs-pane .pane-head :is(a, button)').evaluateAll((els) => els
      .filter((e) => e.getBoundingClientRect().width)
      .map((e) => { const s = getComputedStyle(e); return [Math.round(e.getBoundingClientRect().width), Math.round(e.getBoundingClientRect().height), s.borderTopColor, s.backgroundColor, s.color].join(' '); }));
    expect(actions.length, 'the Documents header actions').toBeGreaterThanOrEqual(2);
    expect(new Set(actions), JSON.stringify(actions)).toEqual(new Set([actions[0]]));
  });
}

// 008-AC13: no setting sits in a row; a room admin reaches a person's actions from its ⋯ with the
// keyboard, renames them and makes them a room admin; a plain member gets no ⋯ actions.
test('a person menu, not in-place settings', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const ids = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.participants.map((p) => p.id)), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${ids[0]}`, { headers: admin })).status()).toBe(204);
  await alice.reload();
  await expect(alice.locator('#participants :is(input, label)')).toHaveCount(0);
  await expect(bob.locator('#participants .person-more:not([disabled])')).toHaveCount(0);

  const more = row(alice, 'Bob').locator('.person-more');
  await more.focus();
  await alice.keyboard.press('Enter');
  const menu = alice.locator('#person-menu');
  await expect(menu.getByRole('button', { name: 'Rename' })).toBeFocused();
  await expect(menu.locator('button')).toHaveText(['Rename', 'Make room admin']);
  await alice.keyboard.press('ArrowDown');
  await expect(menu.getByRole('button', { name: 'Make room admin' })).toBeFocused();
  await alice.keyboard.press('Escape');
  await expect(menu).toBeHidden();
  await expect(more).toBeFocused();

  await alice.keyboard.press('Enter');
  await alice.keyboard.press('Enter');
  await expect(alice.locator('#rename-title')).toHaveText('Rename Bob');
  await expect(alice.locator('#rename-profile-label')).toBeHidden();
  await alice.getByLabel('Display name').fill('Robert');
  await alice.locator('#rename-form').getByRole('button', { name: 'Rename' }).click();
  await expect(row(alice, 'Robert')).toBeVisible();
  await expect(bob.locator('#me')).toHaveText('Robert');

  // A second click on ⋯ closes its menu; another row's ⋯ takes over.
  const robert = row(alice, 'Robert').locator('.person-more');
  const self = row(alice, 'Alice').locator('.person-more');
  await robert.click();
  await expect(menu).toBeVisible();
  await robert.click();
  await expect(menu).toBeHidden();
  await expect(robert).toHaveAttribute('aria-expanded', 'false');
  await robert.click();
  await expect(robert).toHaveAttribute('aria-expanded', 'true');
  await expect(self).toBeDisabled();
  await alice.keyboard.press('Escape');
  await alice.setViewportSize({ width: 390, height: 844 });
  await alice.locator('#tabbar').getByRole('button', { name: /People/ }).click();
  await row(alice, 'Robert').locator('.person-more').click();
  expect(await smallTargets(alice), 'person menu at 390 px').toEqual([]);
  await menu.getByRole('button', { name: 'Make room admin' }).click();
  await expect(row(alice, 'Robert').locator('.sigil')).toHaveText('@');
});

// 009-AC36: a presence change re-renders People while a badge tip is open: the tip follows its
// badge's new meaning, anchored under it, never left at the page's corner.
test('a badge tip survives a presence re-render', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const audio = row(alice, 'Bob').locator('.audio');
  const beat = (body) => bob.evaluate(([id, b]) => fetch(`/api/rooms/${id}/presence`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b),
  }).then((r) => r.status), [room.id, body]);
  expect(await beat({ tab: 'bob-second', audio: true })).toBe(204);
  await expect(audio).toHaveAttribute('data-state', 'listening');
  await audio.hover();
  const tip = alice.locator('#person-tip');
  await expect(tip).toHaveText('Listening only');
  expect(await beat({ tab: 'bob-second', audio: true, muted: true })).toBe(204);
  await expect(audio).toHaveAttribute('data-state', 'muted');
  await expect(tip).toHaveText('Muted');
  const [t, a] = [await tip.boundingBox(), await audio.boundingBox()];
  expect(Math.abs(t.y - (a.y + a.height + 4)) <= 1 || Math.abs(t.y + t.height - (a.y - 4)) <= 1).toBe(true);
});

// 008-AC18: in a narrow side panel, a phone and a desktop, every name shows and starts at one x, @
// marks a room admin in its own column, you are marked on the name, and no row says when it was seen.
for (const [label, size, side] of [['260 px panel', { width: 1440, height: 900 }, 260], ['phone', { width: 390, height: 844 }, 0],
  ['desktop', { width: 1440, height: 900 }, 0]]) {
  test(`people names win the row, ${label}`, async ({ browser, request }) => {
    const long = 'Babs (laptop), a long name to cut it';
    const { room, pages: [alice] } = await roomWith(browser, request, ['Damien', 'Eric', 'Eric', long, 'Babs (host)']);
    const ids = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.participants.map((p) => p.id)), room.id);
    const [me, eric, , babs, host] = ids;
    for (const id of [me, babs]) expect((await request.put(`/api/rooms/${room.id}/admins/${id}`, { headers: admin })).status()).toBe(204);
    const seen = new Date(Date.now() - 5 * 86_400_000).toISOString();
    const people = [{ id: me, here: true, audio: true, mic: true }, { id: eric, here: true, audio: true, muted: true }]
      .concat(ids.slice(2).map((id) => ({ id, here: false, seen_at: seen })));
    const agent = { online: true, can_trigger: [eric, babs], daemons: [], turn: null, running: [], personas: [], queue: [], models: [], efforts: [],
      cap_requests: [], changes: [], room_skills: [], chain: { budget: 25, count: 0, suspended: false }, parallel_turns: false, host_participant_id: host };
    await alice.route(`**/api/rooms/${room.id}/events*`, (r) => r.fulfill({
      status: 200, headers: { 'Content-Type': 'text/event-stream' },
      body: `event: presence\ndata: ${JSON.stringify({ people })}\n\nevent: agent\ndata: ${JSON.stringify(agent)}\n\n`,
    }));
    await alice.setViewportSize(size);
    await alice.reload();
    // The page's CSP refuses a style tag; CSSOM is allowed.
    if (side) await alice.evaluate((w) => { for (const [k, v] of [['flex', 'none'], ['width', w + 'px']]) document.getElementById('side').style.setProperty(k, v, 'important'); }, side);
    if (label === 'phone') await alice.locator('#tabbar').getByRole('button', { name: /People/ }).click();
    await alice.locator('#away summary').click();
    const rows = alice.locator('#participants li, #participants-away li');
    await expect(rows).toHaveCount(5);
    await expect(alice.locator('#participants-away .host-badge')).toHaveCount(1);
    const got = await rows.evaluateAll((lis) => lis.map((li) => {
      const name = li.querySelector('.name');
      const r = name.getBoundingClientRect();
      return { text: name.textContent, left: Math.round(r.left), width: Math.round(r.width), cut: name.scrollWidth > name.clientWidth,
        sigil: li.querySelector('.sigil')?.textContent ?? '', me: name.classList.contains('me'), row: li.textContent };
    }));
    const by = (t) => got.find((g) => g.text === t);
    for (const g of got) {
      expect(g.width, JSON.stringify(g)).toBeGreaterThanOrEqual(24);
      expect(g.row, JSON.stringify(g)).not.toMatch(/seen/i);
    }
    expect(new Set(got.map((g) => g.left)).size, JSON.stringify(got)).toBe(1);
    expect(by('Damien')).toMatchObject({ sigil: '@', me: true });
    expect(by(long)).toMatchObject({ sigil: '@', me: false });
    if (side) expect(by(long).cut).toBe(true);
    expect(got.filter((g) => g.text === 'Eric').map((g) => g.sigil)).toEqual(['', '']);
    // No badge stands for you, an admin or can ask the AI any more.
    await expect(alice.locator('.tag:is(.tag-you, .tag-admin, .tag-ask)')).toHaveCount(0);
    // Two people of one name: the card tells them apart.
    await rows.filter({ has: alice.locator('.name', { hasText: /^Eric$/ }) }).last().locator('.name').focus();
    await expect(alice.locator('#person-card')).toContainText('Seen 5 days ago');
    await expect(alice.locator('#person-card')).toContainText(/Joined /);
    // Can ask the AI left the row for the card.
    await rows.filter({ hasText: long }).locator('.name').focus();
    await expect(alice.locator('#person-card')).toContainText('Can ask the AI');
  });
}
