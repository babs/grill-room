import { test, expect } from './fixtures.js';
import { noOverflow, appointAdmin, upload } from './helpers.js';
import { adminToken, db } from './playwright.config.js';
import { execSQL } from './pgdb.js';

const admin = { Authorization: `Bearer ${adminToken}` };

const newRoom = async (request, name) => (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
const invitePath = async (request, roomID) => {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  return new URL(inv.url).pathname + new URL(inv.url).hash;
};

async function join(page, request, roomID, name) {
  await page.goto(await invitePath(request, roomID));
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
}

// joinAll seats one new browser's profile in each room, the first one making the profile.
async function joinAll(browser, request, rooms, name) {
  const page = await (await browser.newContext()).newPage();
  for (const r of rooms) await join(page, request, r.id, name);
  return page;
}

// post writes a line in a room through the browser's seat there: the room's activity moves. It
// answers the line's id.
const post = (page, roomID, body) => page.evaluate(async ([id, body]) => {
  const res = await fetch(`/api/rooms/${id}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body }) });
  if (!res.ok) throw new Error('post: HTTP ' + res.status);
  return (await res.json()).id;
}, [roomID, body]);

// openRoom loads a room and waits for the page's own read of the profile.
async function openRoom(page, roomID) {
  const own = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
  await page.goto('/r/' + roomID);
  await own;
}

async function openSwitch(page) {
  await page.locator('#room-open').click();
  await page.locator('#room-menu').getByRole('button', { name: 'Switch room…', exact: true }).click();
  await expect(page.locator('#room-menu')).toBeHidden();
  await expect(page.locator('#room-switch')).toBeVisible();
}

const switcher = (page) => page.locator('#room-switch');
const names = (page) => switcher(page).locator('.switch-room .switch-name');
const links = (page) => switcher(page).locator(':scope > a:not(.switch-room)');

// recoveryLink is the link the join page left to the profile it made (005-AC32).
const recoveryLink = async (page) => {
  const link = await page.evaluate(() => sessionStorage.getItem('grill_room_recovery'));
  expect(link).toBeTruthy();
  return new URL(link).pathname + new URL(link).hash;
};

// signInWith opens a profile's recovery link in page, over the profile this browser held.
async function signInWith(page, link) {
  await page.goto(link);
  await page.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(page.locator('#status')).toHaveText('Signed in on this device.');
}

// 041-AC1 041-AC2 041-AC6: the last entry of the room menu opens the five rooms of the profile with
// the latest activity, newest first, the current room left out; captures at 390 and 1440 px, rows
// 44 px high on a phone.
test('the switcher lists five rooms by activity, the current one left out', async ({ browser, request }, testInfo) => {
  test.setTimeout(60_000);
  const rooms = [];
  for (let i = 0; i < 7; i++) rooms.push(await newRoom(request, `Room ${i}` + (i === 3 ? ' with a name long enough to end in an ellipsis on a phone' : '')));
  const alice = await joinAll(browser, request, rooms, 'Alice');
  // Oldest first: R4 is the sixth newest of the other rooms, R0 the room open.
  for (const i of [4, 2, 6, 1, 5, 3, 0]) await post(alice, rooms[i].id, 'line in room ' + i);
  await openRoom(alice, rooms[0].id);
  // 041-AC1: last entry of the room menu.
  await alice.locator('#room-open').click();
  await expect(alice.locator('#room-menu > :is(a, button):visible').last()).toHaveText('Switch room…');
  await alice.keyboard.press('Escape');
  await openSwitch(alice);
  await expect(switcher(alice).locator('.menu-head')).toHaveText('Your rooms');
  await expect(switcher(alice)).toHaveAccessibleName('Your rooms');
  await expect(names(alice)).toHaveText([rooms[3].name, 'Room 5', 'Room 1', 'Room 6', 'Room 2']);
  // 041-AC2: a link to the room; the meta line as /profile writes it; the name is the accessible name's start.
  const first = switcher(alice).locator('.switch-room').first();
  await expect(first).toHaveAttribute('href', '/r/' + encodeURIComponent(rooms[3].id));
  await expect(first.locator('.switch-meta')).toHaveText('active just now');
  await expect(first).toHaveAccessibleName(rooms[3].name + ' active just now');
  await expect(first.locator('.switch-name')).toHaveCSS('text-overflow', 'ellipsis');
  await expect(links(alice)).toHaveText(['All my rooms', 'New room…']);
  await expect(links(alice).first()).toHaveAttribute('href', '/profile#rooms');
  await expect(links(alice).last()).toHaveAttribute('href', '/profile#rooms/new');
  // 041-AC6: rows are the popover's own children.
  expect(await switcher(alice).evaluate((m) => [...m.querySelectorAll('a')].every((a) => a.parentElement === m))).toBe(true);
  for (const [w, h] of [[390, 844], [1440, 900]]) {
    await alice.setViewportSize({ width: w, height: h });
    // 009-AC38: rows and links share one menu-row style: one height, font and text edge.
    const style = await switcher(alice).locator(':scope > a').evaluateAll((as) => as.map((a) => {
      const s = getComputedStyle(a);
      return [Math.round(a.getBoundingClientRect().height), s.fontSize, s.lineHeight, s.paddingLeft].join(' ');
    }));
    expect(new Set(style).size, style.join(' | ')).toBe(1);
    if (w === 390) {
      expect(await noOverflow(alice)).toEqual([]);
      const heights = await switcher(alice).locator(':scope > a').evaluateAll((as) => as.map((a) => a.getBoundingClientRect().height));
      expect(heights).toHaveLength(7);
      for (const rowH of heights) expect(rowH).toBeGreaterThanOrEqual(44);
      // The long name ends in an ellipsis inside the row.
      expect(await first.locator('.switch-name').evaluate((s) => s.scrollWidth > s.clientWidth)).toBe(true);
    }
    await alice.screenshot({ path: testInfo.outputPath(`switcher-${w}.png`) });
  }
  // 041-AC1: in the room menu's place.
  await alice.keyboard.press('Escape');
  await alice.locator('#room-open').click();
  const menuBox = await alice.locator('#room-menu').boundingBox();
  await alice.locator('#switch-open').click();
  const box = await switcher(alice).boundingBox();
  expect([box.x, box.y]).toEqual([menuBox.x, menuBox.y]);
  // A row opens its room.
  await switcher(alice).getByRole('link', { name: /^Room 5/ }).click();
  await expect(alice).toHaveURL('/r/' + rooms[5].id);
  await expect(alice.locator('#room-name .name')).toHaveText('Room 5');
  // 041-AC4 041-AC5: "New room…" opens /profile's new-room sheet.
  await openSwitch(alice);
  await links(alice).last().click();
  await expect(alice.locator('#room-create-sheet')).toBeVisible();
  await expect(alice).toHaveURL(/\/profile#rooms$/);
});

// 041-AC4: a profile with only this room says so above the two links; "room admin" on a row.
test('a profile with only this room', async ({ browser, request }) => {
  const room = await newRoom(request, 'Only room');
  const alice = await joinAll(browser, request, [room], 'Alice');
  await openRoom(alice, room.id);
  await openSwitch(alice);
  await expect(switcher(alice).locator('.switch-note')).toHaveText('No other rooms yet.');
  await expect(switcher(alice).locator('.switch-room')).toHaveCount(0);
  await expect(links(alice)).toHaveText(['All my rooms', 'New room…']);
  // 041-AC2: a room the viewer admins says so.
  const other = await newRoom(request, 'Admined');
  await join(alice, request, other.id, 'Alice');
  await appointAdmin(alice, request, other.id);
  await openRoom(alice, room.id);
  await openSwitch(alice);
  await expect(switcher(alice).locator('.switch-meta')).toHaveText(['active just now · room admin']);
});

// 041-AC1 041-AC5: a browser without a profile has no "Switch room…", and its /profile#rooms/new
// shows the sign-in card, no sheet.
test('a browser without a profile has no switcher', async ({ browser, request }) => {
  const room = await newRoom(request, 'No profile room');
  const alice = await joinAll(browser, request, [room], 'Alice');
  await alice.context().clearCookies({ name: 'grill_room_profile' });
  await openRoom(alice, room.id);
  expect(await alice.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((r) => r.profile))).toBeNull();
  await alice.locator('#room-open').click();
  await expect(alice.locator('#room-menu')).toBeVisible();
  await expect(alice.locator('#switch-open')).toBeHidden();
  // A later read that fails keeps it hidden: the seat stands in only before a first answer.
  await alice.keyboard.press('Escape');
  await alice.route('**/api/profile', (r) => r.fulfill({ status: 500 }));
  const failed = alice.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
  await alice.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await failed;
  await expect(alice.locator('#switch-open')).toHaveJSProperty('hidden', true);
  await alice.unroute('**/api/profile');
  await alice.goto('/profile#rooms/new');
  await expect(alice.locator('#signed-out')).toBeVisible();
  // newRoom's own read of the profile has answered: the sheet stays shut.
  await alice.waitForLoadState('networkidle');
  await expect(alice.locator('#room-create-sheet')).toBeHidden();
});

// 041-AC1: the list follows the browser's profile cookie, not the seat's profile.
test('a seat of another profile lists the browser profile rooms', async ({ browser, request }) => {
  const [shared, aliceOwn, bobOwn] = [await newRoom(request, 'Shared'), await newRoom(request, 'Alice only'), await newRoom(request, 'Bob only')];
  const alice = await joinAll(browser, request, [shared, aliceOwn], 'Alice');
  const bob = await joinAll(browser, request, [bobOwn], 'Bob');
  // Alice's browser signs in to Bob's profile: its seat in Shared stays Alice's.
  await signInWith(alice, await recoveryLink(bob));
  await openRoom(alice, shared.id);
  await expect(alice.locator('#me')).toHaveText('Alice');
  await openSwitch(alice);
  await expect(names(alice)).toHaveText(['Bob only']);
});

// 041-AC3 041-AC12: every open reads the profile again (041-AC12 drops 041-AC3's minute), one
// that fails says so, "All my rooms" still there; the next good read draws the rows again.
test('a failed read says the rooms could not load', async ({ browser, request }) => {
  const [here, there] = [await newRoom(request, 'Here'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [here, there], 'Alice');
  await openRoom(alice, here.id);
  await openSwitch(alice);
  await expect(names(alice)).toHaveText(['There']);
  await alice.keyboard.press('Escape');
  await alice.route('**/api/profile', (r) => r.fulfill({ status: 500, body: 'down' }));
  let reads = 0;
  alice.on('request', (r) => { if (new URL(r.url()).pathname === '/api/profile') reads++; });
  const failed = alice.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
  await openSwitch(alice);
  await failed;
  await expect(switcher(alice).locator('.switch-note')).toHaveText('Your rooms could not load.');
  await expect(switcher(alice).locator('.switch-room')).toHaveCount(0);
  await expect(links(alice).first()).toHaveText('All my rooms');
  expect(reads).toBe(1);
  await alice.keyboard.press('Escape');
  // A network error fails the same way.
  await alice.unroute('**/api/profile');
  await alice.route('**/api/profile', (r) => r.abort());
  await openSwitch(alice);
  await expect(switcher(alice).locator('.switch-note')).toHaveText('Your rooms could not load.');
  await alice.keyboard.press('Escape');
  await alice.unroute('**/api/profile');
  await openSwitch(alice);
  await expect(names(alice)).toHaveText(['There']);
});

// 041-AC3: a sign-in to another profile in another tab replaces the list on the next read, never
// merges it; the open popover redraws.
test('a profile replaced in another tab replaces the list', async ({ browser, request }) => {
  const [here, aliceRoom, bobRoom] = [await newRoom(request, 'Here'), await newRoom(request, 'Alice room'), await newRoom(request, 'Bob room')];
  const alice = await joinAll(browser, request, [here, aliceRoom], 'Alice');
  const bob = await joinAll(browser, request, [bobRoom], 'Bob');
  const link = await recoveryLink(bob);
  await openRoom(alice, here.id);
  await openSwitch(alice);
  await expect(names(alice)).toHaveText(['Alice room']);
  const tab = await alice.context().newPage();
  await signInWith(tab, link);
  // The room's tab comes back into view: the page reads the profile again.
  const read = alice.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
  await alice.bringToFront();
  await alice.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await read;
  await expect(switcher(alice)).toBeVisible();
  await expect(names(alice)).toHaveText(['Bob room']);
  // Signed out elsewhere: the switcher closes and its entry goes.
  await alice.context().clearCookies({ name: 'grill_room_profile' });
  const gone = alice.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
  await alice.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await gone;
  await expect(switcher(alice)).toBeHidden();
  await expect(alice.locator('#switch-open')).toBeHidden();
});

// 041-AC5: /profile#rooms/new opens the new-room sheet on the Rooms tab; closing it leaves #rooms.
test('/profile#rooms/new opens the new-room sheet', async ({ browser, request }) => {
  const room = await newRoom(request, 'Start');
  const alice = await joinAll(browser, request, [room], 'Alice');
  await alice.goto('/profile#rooms/new');
  const sheet = alice.locator('#room-create-sheet');
  await expect(sheet).toBeVisible();
  await expect(sheet.locator('#room-create-title')).toHaveText('New room: pick a starter');
  await expect(alice.locator('#tab-rooms')).toHaveAttribute('aria-current', 'page');
  await expect(alice.locator('#panel-rooms')).toBeVisible();
  expect(await alice.evaluate(() => scrollY)).toBe(0);
  await alice.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  await expect(alice).toHaveURL(/\/profile#rooms$/);
  await expect(alice.locator('#panel-rooms')).toBeVisible();
  // The fragment set again on the open page opens it too.
  await alice.evaluate(() => { location.hash = 'rooms/new'; });
  await expect(sheet).toBeVisible();
  await sheet.getByRole('button', { name: 'Close' }).first().click();
  await expect(alice).toHaveURL(/\/profile#rooms$/);
  // Back from a room made there lands on the Rooms tab, the sheet shut.
  await alice.goto('/profile#rooms/new');
  await alice.getByRole('button', { name: 'Use this starter' }).click();
  await alice.getByLabel('Room name').fill('Made here');
  await alice.getByRole('button', { name: 'Create room' }).click();
  await expect(alice.locator('#room-name')).toHaveText('Made here');
  await alice.goBack();
  await expect(alice).toHaveURL(/\/profile#rooms$/);
  await expect(alice.locator('#panel-rooms')).toBeVisible();
  await expect(sheet).toBeHidden();
});

// 041-AC6 041-AC3: the keyboard path. Enter on "Switch room…" puts the focus in the popover, on
// "Loading your rooms…" until the first read lands, then on the first row; Tab and Shift+Tab move
// through rows; Escape returns the focus to the room's name; Space opens it too.
test('the switcher by keyboard', async ({ browser, request }) => {
  const [here, a, b] = [await newRoom(request, 'Here'), await newRoom(request, 'Room A'), await newRoom(request, 'Room B')];
  const alice = await joinAll(browser, request, [here, a, b], 'Alice');
  await post(alice, a.id, 'older');
  await post(alice, b.id, 'newer');
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let reads = 0;
  await alice.route('**/api/profile', async (r) => {
    reads++;
    await held;
    await r.continue();
  });
  await alice.goto('/r/' + here.id);
  await expect(alice.locator('#room-name .name')).toHaveText('Here');
  await alice.locator('#room-open').click();
  await alice.locator('#switch-open').focus();
  await alice.keyboard.press('Enter');
  await expect(switcher(alice)).toBeVisible();
  const note = switcher(alice).locator('.switch-note');
  await expect(note).toHaveText('Loading your rooms…');
  await expect(note).toBeFocused();
  await expect(links(alice)).toHaveText(['All my rooms', 'New room…']);
  // One request at a time: opening while the first read runs starts no second one; it queues one
  // more read, after it (041-AC12).
  expect(reads).toBe(1);
  release();
  await expect(names(alice)).toHaveText(['Room B', 'Room A']);
  await expect.poll(() => reads).toBe(2);
  const rows = switcher(alice).locator('.switch-room');
  await expect(rows.first()).toBeFocused();
  await alice.keyboard.press('Tab');
  await expect(rows.nth(1)).toBeFocused();
  await alice.keyboard.press('Shift+Tab');
  await expect(rows.first()).toBeFocused();
  await alice.keyboard.press('Escape');
  await expect(switcher(alice)).toBeHidden();
  await expect(alice.locator('#room-open')).toBeFocused();
  await alice.unroute('**/api/profile');
  await alice.keyboard.press('Enter');
  await expect(alice.locator('#room-menu')).toBeVisible();
  await alice.locator('#switch-open').focus();
  await alice.keyboard.press('Space');
  await expect(switcher(alice)).toBeVisible();
  await expect(rows.first()).toBeFocused();
  await alice.keyboard.press('Enter');
  await expect(alice).toHaveURL('/r/' + b.id);
});

// 041-AC6: docs and chat windows have no switcher.
test('docs and chat windows have no switcher', async ({ browser, request }) => {
  const [here, there] = [await newRoom(request, 'Here'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [here, there], 'Alice');
  for (const path of ['/docs', '/chat']) {
    const own = alice.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
    await alice.goto('/r/' + here.id + path);
    await own;
    // The room menu never opens there: the entry's own state is what proves it.
    await expect(alice.locator('#switch-open')).toHaveJSProperty('hidden', true);
    await expect(switcher(alice)).toBeHidden();
  }
});

// 041-AC3: one profile read at a time never drops a later one. A read that hangs gives up after
// 10 s; a return to the tab meanwhile reads again once it settles; a text size save waits for it.
test('a hung profile read does not hold the page', async ({ browser, request }) => {
  test.setTimeout(60_000);
  const room = await newRoom(request, 'Here');
  const alice = await joinAll(browser, request, [room], 'Alice');
  let reads = 0;
  // The first read never answers.
  await alice.route('**/api/profile', (r) => (++reads === 1 ? undefined : r.continue()));
  const puts = [];
  alice.on('request', (r) => { if (r.url().endsWith('/api/profile/text-size')) puts.push(r); });
  await alice.goto('/r/' + room.id);
  await expect.poll(() => reads).toBe(1);
  await alice.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await alice.locator('#me').click();
  await alice.locator('#text-size-open').click();
  await alice.locator('#text-size').getByRole('group', { name: 'Chat' }).getByLabel('L', { exact: true }).check();
  await expect.poll(() => puts.length, { timeout: 20_000 }).toBe(1);
  expect(reads).toBe(2);
});

// Phase 2: unread counts.
const row = (page, name) => switcher(page).locator('.switch-room').filter({ has: page.locator('.switch-name', { hasText: new RegExp('^' + name + '$') }) });
// unread reads the count the profile answers for a room, as the switcher draws it.
const unread = (page, roomID) => page.evaluate(async (id) => (await (await fetch('/api/profile')).json()).profile.rooms.find((r) => r.room_id === id).unread ?? 0, roomID);
// reads records the room page's marker writes in page's browser context, every page of it.
function reads(page) {
  const puts = [];
  page.context().on('request', (r) => { if (r.method() === 'PUT' && new URL(r.url()).pathname.endsWith('/read')) puts.push(r.postDataJSON().through); });
  return puts;
}

// 041-AC8 041-AC12: Bob writes in B while Alice is in A: her switcher and /profile › Rooms say 1;
// opening B moves the marker, and back in A, and in a second browser of Alice's, the count is gone.
test('a line in another room shows in the switcher until read', async ({ browser, request }) => {
  const [a, b] = [await newRoom(request, 'Room A'), await newRoom(request, 'Room B')];
  const alice = await joinAll(browser, request, [a, b], 'Alice');
  const link = await recoveryLink(alice);
  const bob = await joinAll(browser, request, [b], 'Bob');
  const other = await (await browser.newContext()).newPage();
  await signInWith(other, link);
  const puts = reads(alice);
  await openRoom(alice, a.id);
  const said = await post(bob, b.id, 'news in B');
  await openSwitch(alice);
  await expect(row(alice, 'Room B').locator('.badge')).toHaveText('1');
  await expect(row(alice, 'Room B')).toHaveAccessibleName('Room B 1 unread active just now');
  // The sign-in left other on /profile: a fragment alone would not load it again.
  await other.reload();
  const card = other.locator('#rooms .card').filter({ hasText: 'Room B' });
  await expect(card.locator('.badge')).toHaveText('1');
  await expect(card.getByRole('link')).toHaveAccessibleName('Room B 1 unread');
  // Row height stays one menu line with a badge (009-AC38).
  const style = await switcher(alice).locator(':scope > a').evaluateAll((as) => as.map((el) => Math.round(el.getBoundingClientRect().height)));
  expect(new Set(style).size).toBe(1);
  await row(alice, 'Room B').click();
  await expect(alice.locator('#room-name .name')).toHaveText('Room B');
  await expect.poll(() => puts.at(-1)).toBe(said);
  await expect.poll(() => unread(alice, b.id)).toBe(0);
  await openRoom(alice, a.id);
  await openSwitch(alice);
  await expect(row(alice, 'Room B')).toHaveAccessibleName('Room B active just now');
  await expect(row(alice, 'Room B').locator('.badge')).toHaveCount(0);
  await other.reload();
  await expect(card).toBeVisible();
  await expect(card.locator('.badge')).toHaveCount(0);
});

// 041-AC12: a line naming Alice marks the count "@"; 100 unread lines show "99+".
test('a mention shows @, and 100 lines 99+', async ({ browser, request }) => {
  test.setTimeout(60_000);
  const [a, b] = [await newRoom(request, 'Room A'), await newRoom(request, 'Room B')];
  const alice = await joinAll(browser, request, [a, b], 'Alice');
  const bob = await joinAll(browser, request, [b], 'Bob');
  await openRoom(alice, a.id);
  await post(bob, b.id, '@Alice a question');
  await openSwitch(alice);
  await expect(row(alice, 'Room B').locator('.badge')).toHaveText('1 · @');
  await expect(row(alice, 'Room B')).toHaveAccessibleName('Room B 1 unread, mentions you active just now');
  await alice.keyboard.press('Escape');
  await bob.evaluate(async (id) => {
    for (let i = 0; i < 99; i++) {
      const res = await fetch(`/api/rooms/${id}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body: 'line ' + i }) });
      if (!res.ok) throw new Error('post: HTTP ' + res.status);
    }
  }, b.id);
  await openSwitch(alice);
  await expect(row(alice, 'Room B').locator('.badge')).toHaveText('99+ · @');
  await expect(row(alice, 'Room B')).toHaveAccessibleName('Room B 99+ unread, mentions you active just now');
  await alice.goto('/profile#rooms');
  await expect(alice.locator('#rooms .card').filter({ hasText: 'Room B' }).locator('.badge')).toHaveText('99+ · @');
  expect(await unread(alice, b.id)).toBe(100);
});

// 041-AC8: a voice line, which the Written filter hides, newest in the room moves the marker to it.
test('a voice line last does not stall the marker', async ({ browser, request }) => {
  const [room, there] = [await newRoom(request, 'Call room'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [room, there], 'Alice');
  const bob = await joinAll(browser, request, [room], 'Bob');
  await alice.setViewportSize({ width: 900, height: 720 });
  const puts = reads(alice);
  await openRoom(alice, room.id);
  const text = await post(bob, room.id, 'typed line');
  // Sent once the 5 s since the load's move are past.
  await expect.poll(() => puts.at(-1), { timeout: 10_000 }).toBe(text);
  // The e2e server has no STT worker: the voice line goes in the database as the worker's would,
  // and Bob's edit wakes the streams, which send every line past the last one.
  const bobID = await bob.evaluate(() => myID);
  const ts = new Date().toISOString().replace('Z', '000000Z');
  expect(execSQL(db, `INSERT INTO messages(room_id, ts, kind, participant_id, sender, body, source)
    VALUES ('${room.id}', '${ts}', 'voice', ${bobID}, 'Bob', 'spoken last', 'server')`)).toBe('1');
  const voice = await bob.evaluate(async ([id, since]) => (await (await fetch(`/api/rooms/${id}/messages?since=${since}`)).json()).messages.at(-1), [room.id, text]);
  expect(voice.kind).toBe('voice');
  await bob.evaluate(([id, msg]) => fetch(`/api/rooms/${id}/messages/${msg}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: '{"body":"typed line, edited"}' }), [room.id, text]);
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(voice.id);
  await expect(alice.locator('#chat-filter [data-filter=written]')).toHaveAttribute('aria-pressed', 'true');
  await expect(alice.locator(`#messages li[data-id="${voice.id}"]`)).toBeHidden();
  await expect.poll(() => puts.at(-1), { timeout: 10_000 }).toBe(voice.id);
  const profile = await alice.evaluate(async () => (await (await fetch('/api/profile')).json()).profile.id);
  expect(execSQL(db, `UPDATE room_reads SET read_through = read_through WHERE profile_id = ${profile} AND room_id = '${room.id}' AND read_through = ${voice.id}`)).toBe('1');
});

// 041-AC8: no move while the chat is scrolled up, the tab not focused, or in a docs window; the
// move comes once the chat is back at its end, or the tab focused again.
test('the marker moves only while the chat is read', async ({ browser, request }) => {
  test.setTimeout(60_000);
  const [here, there] = [await newRoom(request, 'Here'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [here, there], 'Alice');
  const bob = await joinAll(browser, request, [here], 'Bob');
  let last;
  for (let i = 0; i < 40; i++) last = await post(bob, here.id, 'filler ' + i);
  const puts = reads(alice);
  await alice.clock.install();
  await openRoom(alice, here.id);
  await expect.poll(() => puts.at(-1)).toBe(last);
  // Scrolled up: not pinned.
  await alice.clock.fastForward(1500);
  await alice.locator('#messages').evaluate((el) => { el.scrollTop = 0; });
  await expect.poll(() => alice.evaluate(() => isPinned(messages))).toBe(false);
  const up = await post(bob, here.id, 'while scrolled up');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(up);
  await alice.clock.fastForward(6000);
  expect(puts.at(-1)).toBe(last);
  expect(await unread(alice, here.id)).toBe(1);
  await alice.locator('#messages').evaluate((el) => { el.scrollTop = el.scrollHeight; });
  await expect.poll(() => puts.at(-1)).toBe(up);
  // Not focused.
  await alice.evaluate(() => { window.realFocus = document.hasFocus; document.hasFocus = () => false; });
  const away = await post(bob, here.id, 'while away');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(away);
  await alice.clock.fastForward(6000);
  expect(puts.at(-1)).toBe(up);
  await alice.evaluate(() => { document.hasFocus = window.realFocus; window.dispatchEvent(new Event('focus')); });
  await expect.poll(() => puts.at(-1)).toBe(away);
  // A docs window never moves it.
  const n = puts.length;
  await alice.goto('/r/' + here.id + '/docs');
  const docs = await post(bob, here.id, 'while in docs');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(docs);
  await alice.clock.fastForward(6000);
  expect(puts.length).toBe(n);
  // A chat window does.
  await alice.goto('/r/' + here.id + '/chat');
  await expect.poll(() => puts.at(-1)).toBe(docs);
  // A phone's Docs tab hides the chat: no move until the Chat tab is back.
  await alice.setViewportSize({ width: 390, height: 844 });
  await alice.goto('/r/' + here.id);
  await expect.poll(() => puts.at(-1)).toBe(docs);
  await alice.locator('#tabbar [data-tab=docs]').click();
  const tabbed = await post(bob, here.id, 'while on the Docs tab');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(tabbed);
  await alice.clock.fastForward(6000);
  expect(puts.at(-1)).toBe(docs);
  await alice.locator('#tabbar [data-tab=chat]').click();
  await expect.poll(() => puts.at(-1)).toBe(tabbed);
});

// 041-AC8: a line made eligible within the 5 s window goes out on pagehide, with keepalive.
test('leaving the page sends the last eligible line', async ({ browser, request }) => {
  const [here, there] = [await newRoom(request, 'Here'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [here, there], 'Alice');
  const bob = await joinAll(browser, request, [here], 'Bob');
  const first = await post(bob, here.id, 'first');
  const puts = reads(alice);
  await alice.clock.install();
  await openRoom(alice, here.id);
  await expect.poll(() => puts.at(-1)).toBe(first);
  // Timers held: the 5 s window stays open whatever the machine's pace.
  await alice.clock.pauseAt(await alice.evaluate(() => Date.now() + 100));
  const next = await post(bob, here.id, 'next');
  await expect(alice.locator('#messages')).toContainText('next');
  expect(puts).toEqual([first]);
  await alice.goto('/r/' + there.id);
  // Playwright may not report a request of a page going away: the server's count tells.
  await expect.poll(() => unread(alice, here.id)).toBe(0);
});

// 041-AC8: what the timer or pagehide sends is the newest line seen while the chat was read, never
// one that came after, with the chat scrolled up.
test('a line that came while the chat was not read stays unread', async ({ browser, request }) => {
  const [here, there] = [await newRoom(request, 'Here'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [here, there], 'Alice');
  const bob = await joinAll(browser, request, [here], 'Bob');
  let last;
  for (let i = 0; i < 40; i++) last = await post(bob, here.id, 'filler ' + i);
  const puts = reads(alice);
  await alice.clock.install();
  await openRoom(alice, here.id);
  await expect.poll(() => puts.at(-1)).toBe(last);
  // Timers held: the 5 s window stays open whatever the machine's pace.
  await alice.clock.pauseAt(await alice.evaluate(() => Date.now() + 100));
  const seen = await post(bob, here.id, 'seen');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(seen);
  await alice.locator('#messages').evaluate((el) => { el.scrollTop = 0; });
  await expect.poll(() => alice.evaluate(() => isPinned(messages))).toBe(false);
  const unseen = await post(bob, here.id, 'unseen');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(unseen);
  expect(puts).toEqual([last]);
  await alice.goto('/r/' + there.id);
  await expect.poll(() => unread(alice, here.id)).toBe(1);
});

// 041-AC8: a marker write that fails is sent again 5 s later, by its own timer.
test('a failed marker write is sent again', async ({ browser, request }) => {
  const [here, there] = [await newRoom(request, 'Here'), await newRoom(request, 'There')];
  const alice = await joinAll(browser, request, [here, there], 'Alice');
  const bob = await joinAll(browser, request, [here], 'Bob');
  const said = await post(bob, here.id, 'line');
  const puts = reads(alice);
  let fail = true;
  await alice.route(`**/api/rooms/${here.id}/read`, (r) => {
    if (!fail) return r.continue();
    fail = false;
    return r.fulfill({ status: 500 });
  });
  await alice.clock.install();
  await openRoom(alice, here.id);
  await expect.poll(() => puts.length).toBe(1);
  expect(await unread(alice, here.id)).toBe(1);
  await alice.clock.fastForward(6000);
  await expect.poll(() => puts).toEqual([said, said]);
  await expect.poll(() => unread(alice, here.id)).toBe(0);
});

// 041-AC8: a modal sheet over the chat holds the marker; closing it moves it.
test('a sheet over the chat holds the marker', async ({ browser, request }) => {
  const room = await newRoom(request, 'Here');
  const alice = await joinAll(browser, request, [room], 'Alice');
  const bob = await joinAll(browser, request, [room], 'Bob');
  const puts = reads(alice);
  await alice.clock.install();
  await openRoom(alice, room.id);
  await expect.poll(() => puts.length).toBe(1);
  await alice.locator('#room-open').click();
  await alice.locator('#invite-open').click();
  await expect.poll(() => alice.evaluate(() => $('invites').matches(':modal'))).toBe(true);
  const under = await post(bob, room.id, 'under the sheet');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(under);
  await alice.clock.fastForward(6000);
  expect(puts).toHaveLength(1);
  await alice.keyboard.press('Escape');
  await expect.poll(() => alice.evaluate(() => $('invites').open)).toBe(false);
  await expect.poll(() => puts.at(-1)).toBe(under);
});

// 041-AC8: a page hidden sends its eligible line at once: a phone may not fire pagehide after.
test('hiding the page sends the last eligible line', async ({ browser, request }) => {
  const room = await newRoom(request, 'Here');
  const alice = await joinAll(browser, request, [room], 'Alice');
  const bob = await joinAll(browser, request, [room], 'Bob');
  const first = await post(bob, room.id, 'first');
  const puts = reads(alice);
  await alice.clock.install();
  await openRoom(alice, room.id);
  await expect.poll(() => puts.at(-1)).toBe(first);
  await alice.clock.pauseAt(await alice.evaluate(() => Date.now() + 100));
  const next = await post(bob, room.id, 'next');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(next);
  expect(puts).toEqual([first]);
  await alice.evaluate(() => {
    Object.defineProperty(document, 'hidden', { get: () => true });
    Object.defineProperty(document, 'visibilityState', { get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(() => puts).toEqual([first, next]);
});

// 041-AC8: a short chat folded beside a document is not read; unfolding it, which scrolls nothing,
// moves the marker.
test('unfolding a short chat moves the marker', async ({ browser, request }) => {
  const room = await newRoom(request, 'Here');
  const alice = await joinAll(browser, request, [room], 'Alice');
  const bob = await joinAll(browser, request, [room], 'Bob');
  const puts = reads(alice);
  await alice.clock.install();
  await openRoom(alice, room.id);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await alice.locator('#chat-fold').click();
  await expect(alice.locator('#messages')).toBeHidden();
  await alice.clock.fastForward(6000);
  const n = puts.length;
  const said = await post(bob, room.id, 'while folded');
  await expect.poll(() => alice.evaluate(() => lastID)).toBe(said);
  await alice.clock.fastForward(6000);
  expect(puts).toHaveLength(n);
  await alice.locator('#chat-unfold').click();
  // Nothing to scroll: no scroll event can be what moves it.
  expect(await alice.locator('#messages').evaluate((el) => el.scrollHeight <= el.clientHeight)).toBe(true);
  await expect.poll(() => puts.at(-1)).toBe(said);
});

// 041-AC12: a refresh of /profile's rooms keeps the focus on the card that held it.
test('a rooms refresh keeps the focused card', async ({ browser, request }) => {
  const [a, b] = [await newRoom(request, 'Room A'), await newRoom(request, 'Room B')];
  const alice = await joinAll(browser, request, [a, b], 'Alice');
  const bob = await joinAll(browser, request, [b], 'Bob');
  await alice.goto('/profile#rooms');
  const card = alice.locator('#rooms .card').filter({ hasText: 'Room B' });
  await card.getByRole('link').focus();
  await post(bob, b.id, 'news');
  const read = alice.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
  await alice.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await read;
  await expect(card.locator('.badge')).toHaveText('1');
  await expect(card.getByRole('link')).toBeFocused();
});
