import { devices } from '@playwright/test';
import { test, expect } from './fixtures.js';
import http from 'node:http';
import { adminToken } from './playwright.config.js';
import { roomWith, menuItem, personaRow, personaSheet, closePersonas, line, live, smallTargets, playEvents, messageAction, addFromLibrary, patchJSON } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// joinAPI joins roomID as name without a browser and returns the participant's token.
async function joinAPI(request, roomID, name) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  const res = await request.post('/api/join', { data: { invite: new URL(inv.url).hash.slice(1), name } });
  expect(res.status()).toBe(201);
  return (await res.json()).token;
}

async function post(request, roomID, token, body) {
  const res = await request.post(`/api/rooms/${roomID}/messages`, { headers: { Authorization: `Bearer ${token}` }, data: { body } });
  expect(res.status()).toBe(201);
}

// setVisibility fakes the page going to the background and back, as a phone does on app switch.
const setVisibility = (page, state) => page.evaluate((s) => {
  Object.defineProperty(document, 'visibilityState', { value: s, configurable: true });
  Object.defineProperty(document, 'hidden', { value: s === 'hidden', configurable: true });
  document.dispatchEvent(new Event('visibilitychange'));
}, state);

// flakyProxy stands in for the ingress in front of the room server: down() cuts every open
// connection and answers 502 until up(), as traefik does while a Recreate rollout swaps the pod.
function flakyProxy(target) {
  const sockets = new Set();
  const p = { refused: 0, isDown: false };
  const server = http.createServer((req, res) => {
    if (p.isDown) {
      if (req.url.includes('/events')) p.refused++;
      res.writeHead(502).end('Bad Gateway');
      return;
    }
    const up = http.request(target + req.url, { method: req.method, headers: req.headers }, (r) => {
      res.writeHead(r.statusCode, r.headers);
      r.pipe(res);
    });
    up.on('error', () => res.destroy());
    req.pipe(up);
  });
  server.on('connection', (s) => { sockets.add(s); s.on('close', () => sockets.delete(s)); });
  p.down = () => { p.isDown = true; for (const s of sockets) s.destroy(); };
  p.up = () => { p.isDown = false; };
  p.close = () => { server.close(); for (const s of sockets) s.destroy(); };
  return new Promise((ok) => server.listen(0, '127.0.0.1', () => {
    p.url = `http://127.0.0.1:${server.address().port}`;
    ok(p);
  }));
}

// 002-AC40: EventSource gives up for good once a reconnect gets a non-200, which is what a redeploy hands
// it: the page must retry by itself and catch up on what it missed.
test('a stream cut by a redeploy comes back and catches up', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const bob = await joinAPI(request, room.id, 'Bob');
  const proxy = await flakyProxy(baseURL);
  try {
    // The room cookie ignores the port: the page reaches the same session through the proxy.
    await alice.goto(`${proxy.url}/r/${room.id}`);
    await expect(alice.locator('#messages')).toContainText('Bob joined');
    await expect(alice.locator('#reconnecting')).toBeHidden();

    proxy.down();
    await expect(alice.locator('#reconnecting')).toBeVisible();
    await post(request, room.id, bob, 'sent while down');
    // The browser's own retry got the 502: from here only the page can reopen the stream.
    await expect.poll(() => proxy.refused, { timeout: 10_000 }).toBeGreaterThan(0);
    proxy.up();

    await expect(alice.locator('#messages')).toContainText('sent while down', { timeout: 10_000 });
    await expect(alice.locator('#reconnecting')).toBeHidden();
    await post(request, room.id, bob, 'live again');
    await expect(alice.locator('#messages')).toContainText('live again');
    await expect(alice.locator('.msg', { hasText: 'sent while down' })).toHaveCount(1);
    await expect(alice.locator('.msg', { hasText: 'Alice joined' })).toHaveCount(1);
  } finally {
    proxy.close();
  }
});

// 002-AC40: a phone suspends a background tab's stream without the page noticing: on return the page must
// reopen it from the last message it holds and reload the room state.
test('a page back from the background resumes the stream and reloads the room', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect(alice.locator('#messages')).toContainText('Alice joined');
  // The suspended stream: still open in the page, delivering nothing.
  await alice.evaluate(() => { events.onmessage = null; });
  await setVisibility(alice, 'hidden');

  // The stream delivers the line to the page, which no longer draws it.
  // The listener is in place before the line is sent: a line that came first would never resolve it.
  await alice.evaluate(() => { window.awayLine = new Promise((ok) => events.addEventListener('message', (e) => { if (e.data.includes('sent while away')) ok(); })); });
  const bob = await joinAPI(request, room.id, 'Bob');
  await post(request, room.id, bob, 'sent while away');
  await alice.evaluate(() => window.awayLine);
  await expect(alice.locator('#messages')).not.toContainText('sent while away');

  const resumed = alice.waitForRequest((r) => /\/events\?since=\d+&tab=\w+$/.test(r.url()));
  await setVisibility(alice, 'visible');
  await resumed;
  await expect(alice.locator('#messages')).toContainText('sent while away');
  await expect(alice.locator('#participants li .name, #participants-away li .name')).toHaveText(['Alice', 'Bob']);
  await expect(alice.locator('.msg', { hasText: 'Alice joined' })).toHaveCount(1);
  await post(request, room.id, bob, 'live after resume');
  await expect(alice.locator('#messages')).toContainText('live after resume');
});

// 002-AC39: the message box must stay above the on-screen keyboard: Android shrinks the viewport
// (interactive-widget=resizes-content), iOS Safari only shrinks the visual viewport.
for (const name of ['iPhone 15', 'Pixel 7']) {
  test(`${name}: the message box stays above the keyboard`, async ({ browser, request }) => {
    const { defaultBrowserType, ...device } = devices[name];
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Phone' } })).json();
    const bob = await joinAPI(request, room.id, 'Bob');
    for (let i = 0; i < 40; i++) await post(request, room.id, bob, `line ${i}`);
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const page = await (await browser.newContext(device)).newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill('Alice');
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#messages')).toContainText('Alice joined');
    await expect(page.locator('meta[name=viewport]')).toHaveAttribute('content', /interactive-widget=resizes-content/);

    const { width, height } = page.viewportSize();
    const keyboard = 300;
    // fits checks the box and the last message sit above the keyboard and the list still scrolls.
    const fits = async (visible) => {
      await expect.poll(async () => (await page.locator('#body').boundingBox()).y + (await page.locator('#body').boundingBox()).height).toBeLessThanOrEqual(visible + 1);
      await expect.poll(async () => { const b = await page.locator('#messages').boundingBox(); return b.y + b.height; }).toBeLessThanOrEqual(visible + 1);
      await expect.poll(() => page.evaluate(() => messages.lastElementChild.getBoundingClientRect().bottom - messages.getBoundingClientRect().bottom)).toBeLessThanOrEqual(1);
      expect(await page.locator('#messages').evaluate((m) => m.scrollHeight > m.clientHeight)).toBe(true);
    };
    await fits(height);

    // iOS: the keyboard covers the page and shrinks only the visual viewport.
    await page.getByLabel('Message').focus();
    await page.evaluate((h) => {
      Object.defineProperty(visualViewport, 'height', { get: () => h, configurable: true });
      visualViewport.dispatchEvent(new Event('resize'));
    }, height - keyboard);
    await fits(height - keyboard);
    await page.evaluate(() => { delete visualViewport.height; visualViewport.dispatchEvent(new Event('resize')); });

    // Android: the keyboard resizes the viewport itself.
    await page.setViewportSize({ width, height: height - keyboard });
    await fits(height - keyboard);
  });
}

// 002-AC39: a keyboard opened or closed while the page is pinch-zoomed must not leave the page cut
// to the window it had before, with bands of nothing above or below it.
test('a pinch zoom leaves the page fitting the window', async ({ browser, request }) => {
  const { defaultBrowserType, ...device } = devices['Pixel 7'];
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Phone zoom' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await browser.newContext(device);
  const page = await ctx.newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Alice');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#messages')).toContainText('Alice joined');
  const cdp = await ctx.newCDPSession(page);
  const { width, height } = page.viewportSize();
  const fits = (step) => expect.poll(() => page.evaluate(() => {
    const b = document.body.getBoundingClientRect();
    return [Math.round(b.top), Math.round(b.bottom - innerHeight)];
  }), step).toEqual([0, 0]);
  const zoom = (pageScaleFactor) => cdp.send('Emulation.setPageScaleFactor', { pageScaleFactor });
  // Android resizes the window for its keyboard (interactive-widget=resizes-content).
  const keyboard = (up) => page.setViewportSize({ width, height: up ? height - 300 : height });

  await keyboard(true);
  await fits('keyboard opened');
  await zoom(1.3);
  await fits('zoomed with the keyboard up');
  await keyboard(false);
  await fits('keyboard closed while zoomed');
  await keyboard(true);
  await fits('keyboard opened while zoomed');
  await zoom(1);
  await fits('zoom back with the keyboard up');
  await keyboard(false);
  await fits('keyboard closed');

  // iOS shifts the visual viewport down to the box: zoomed, that shift must go too.
  const fake = (props) => page.evaluate((props) => {
    for (const [k, v] of Object.entries(props)) Object.defineProperty(visualViewport, k, { get: () => v, configurable: true });
    visualViewport.dispatchEvent(new Event('resize'));
  }, props);
  const top = () => page.evaluate(() => Math.round(document.body.getBoundingClientRect().top));
  await fake({ offsetTop: 200 });
  await expect.poll(top, 'shifted at scale 1').toBe(200);
  await fake({ scale: 1.3 });
  await expect.poll(top, 'shift dropped while zoomed').toBe(0);
});

// 002-AC39: a scroll up right after a viewport resize keeps a pinned chat at the newest line; any
// other scroll up unpins it.
test('a resize does not unpin the chat', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Phone pin' } })).json();
  const bob = await joinAPI(request, room.id, 'Bob');
  for (let i = 0; i < 40; i++) await post(request, room.id, bob, `line ${i}`);
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext({ viewport: { width: 390, height: 844 } })).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Alice');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#messages')).toContainText('Alice joined');
  const gap = () => page.evaluate(() => messages.lastElementChild.getBoundingClientRect().bottom - messages.getBoundingClientRect().bottom);
  await expect.poll(gap).toBeLessThanOrEqual(1);
  const pinned = () => page.evaluate(() => isPinned(messages));
  const scrollUp = async (target, between) => {
    await page.evaluate(({ target, between }) => {
      if (target) globalThis[target].dispatchEvent(new Event('resize'));
      if (between) messages.dispatchEvent(new Event(between));
      messages.scrollTop -= 300;
    }, { target, between });
  };
  const repin = async () => {
    await page.evaluate(() => { messages.scrollTop = messages.scrollHeight; });
    await expect.poll(pinned).toBe(true);
  };
  // No resize: a scroll up unpins, and a new line leaves the reader where they are.
  await page.evaluate(() => { resizedAt = -Infinity; });
  await scrollUp();
  await expect.poll(pinned, 'a scroll up with no resize').toBe(false);
  await post(request, room.id, bob, 'one more');
  await expect(page.locator('#messages')).toContainText('one more');
  expect(await gap()).toBeGreaterThan(250);
  for (const target of ['window', 'visualViewport']) {
    await repin();
    await scrollUp(target);
    await expect.poll(gap, target + ' resize').toBeLessThanOrEqual(1);
    expect(await pinned(), target + ' resize').toBe(true);
  }
  // The reader's own input after the resize, or a pinch zoom, leaves the scroll up theirs.
  for (const between of ['wheel', 'touchstart', 'keydown']) {
    await repin();
    await scrollUp('window', between);
    await expect.poll(pinned, between).toBe(false);
  }
  await repin();
  await page.evaluate(() => Object.defineProperty(visualViewport, 'scale', { get: () => 2, configurable: true }));
  await scrollUp('visualViewport');
  await expect.poll(pinned, 'pinch zoom').toBe(false);
});

// 002-AC15 on a phone: the persona buttons, the personas panel and a persona's sheet fit the
// screen, and an edit made there is saved.
for (const name of ['iPhone 15', 'Pixel 7']) {
  test(`${name}: the personas panel fits the screen`, async ({ browser, request }) => {
    const { defaultBrowserType, ...device } = devices[name];
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Phone personas' } })).json();
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const page = await (await browser.newContext(device)).newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill('Alice');
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#messages')).toContainText('Alice joined');
    const { width, height } = page.viewportSize();
    const inside = async (loc) => {
      const b = await loc.boundingBox();
      expect(b.x).toBeGreaterThanOrEqual(0);
      expect(b.x + b.width).toBeLessThanOrEqual(width + 1);
    };

    await menuItem(page, 'Personas');
    const panel = page.locator('#personas');
    await addFromLibrary(page, 'Secretary');
    await expect(panel.locator('.persona-row')).toHaveCount(2);
    await inside(panel);
    expect((await panel.boundingBox()).height).toBeLessThanOrEqual(height + 1);
    await inside(personaRow(page, 'Secretary'));
    // The second persona's sheet fits too, its Save in reach (009-AC2).
    const sec = await personaSheet(page, 'Secretary', 'Model');
    await inside(sec);
    await sec.getByLabel('Model').selectOption('haiku');
    const save = sec.getByRole('button', { name: 'Save', exact: true });
    await save.scrollIntoViewIfNeeded();
    await inside(save);
    await save.click();
    await expect(sec.locator('#persona-status')).toHaveText('Secretary saved. It applies from its next turn.');
    await closePersonas(page);
    await expect(page.locator('#messages')).toContainText('Alice changed 📝 Secretary: model sonnet → haiku.');
    for (const b of await page.locator('button.persona').all()) await inside(b);
    await inside(page.locator('#body'));
  });
}

// The phone room (spec 037) at 390 × 844.
const phone = { width: 390, height: 844 };
const rect = (page, sel) => page.locator(sel).evaluate((e) => {
  const r = e.getBoundingClientRect();
  return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, width: r.width, height: r.height, mid: r.top + r.height / 2 };
});
// addPersonas adds n personas to the room from its templates, in turn.
const addPersonas = (page, id, n) => page.evaluate(async ([id, n]) => {
  const tpls = await (await fetch(`/api/rooms/${id}/templates`)).json();
  for (let i = 0; i < n; i++) {
    await fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ template_id: tpls[i % tpls.length].id }) });
  }
}, [id, n]);
// withVoice turns the room's voice on, as a server with LiveKit and the room's setting would.
async function withVoice(page, roomID) {
  await patchJSON(page, `**/api/rooms/${roomID}`, (json) => ({ ...json, voice: true }));
  await page.evaluate(() => loadRoom());
}

// 037-AC3: a phone shows the chat filter once the room has voice, a voice line or someone in its
// audio, with the filter last chosen, and keeps it; until then the chat shows every line and the
// stored choice stays. A server that offers audio is no voice yet.
test('the phone chat filter waits for voice', async ({ request, browser }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.evaluate(() => localStorage.setItem('grill-room-chat-filter', 'voice'));
  await alice.setViewportSize(phone);
  await withVoice(alice, room.id);
  await alice.reload();
  await expect(alice.locator('#voice-join')).toBeVisible();
  await expect(alice.locator('#messages')).toContainText('Alice joined');
  const filter = alice.locator('#chat-filter');
  await expect(filter).toBeHidden();
  await expect(alice.locator('#messages')).toHaveAttribute('data-filter', 'all');
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');
  await live(alice, line(1e9, 'voice', 'spoken'));
  await expect(filter).toBeVisible();
  await expect(filter.getByRole('button', { name: /Voice/ })).toHaveAttribute('aria-pressed', 'true');
  await expect(alice.locator('#messages')).toHaveAttribute('data-filter', 'voice');

  // Nobody in the audio keeps it hidden; someone joining shows it, and it stays once they leave.
  const person = (audio) => ['presence', { people: [{ id: 999999, here: true, audio }] }];
  await playEvents(alice, room.id, [person(false)]);
  await alice.waitForFunction(() => presence.has('999999'));
  await expect(filter).toBeHidden();
  await playEvents(alice, room.id, [person(true), person(false)]);
  await alice.waitForFunction(() => presence.get('999999')?.audio === false);
  await expect(filter).toBeVisible();
  // From 40rem, as before: the filter shows in a room without voice.
  await alice.unroute(`**/api/rooms/${room.id}`);
  await alice.unroute(`**/api/rooms/${room.id}/events*`);
  await alice.setViewportSize({ width: 900, height: 844 });
  await alice.reload();
  await expect(alice.locator('#messages')).toContainText('Alice joined');
  await expect(filter).toBeVisible();
});

// 037-AC3: the filter coming back keeps a line holding focus in view: an open edit box of a written
// line under a stored Voice shows All for the visit, the box still focused.
test('the phone filter coming back keeps an open edit box', async ({ request, browser }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.evaluate(() => localStorage.setItem('grill-room-chat-filter', 'voice'));
  await alice.setViewportSize(phone);
  await alice.reload();
  await alice.locator('#body').fill('my written line');
  await alice.locator('#body').press('Enter');
  const mine = alice.locator('#messages > li', { hasText: 'my written line' });
  await messageAction(mine, 'Edit');
  const box = alice.locator('#messages form.edit textarea');
  await expect(box).toBeFocused();
  await alice.evaluate(() => events.dispatchEvent(new MessageEvent('presence', { data: JSON.stringify({ people: [{ id: 999999, here: true, audio: true }] }) })));
  await expect(alice.locator('#chat-filter')).toBeVisible();
  await expect(alice.locator('#chat-filter [data-filter=all]')).toHaveAttribute('aria-pressed', 'true');
  await expect(box).toBeVisible();
  await expect(box).toBeFocused();
  expect(await alice.evaluate(() => localStorage.getItem('grill-room-chat-filter'))).toBe('voice');
});

// 037-AC4: at rest the phone composer is one row, its placeholder on one line; the box grows with
// its text up to 5 lines, then scrolls, and shrinks back once sent. From 40rem it is as before.
test('the phone composer rests on one row and grows to 5 lines', async ({ request, browser }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  const body = alice.locator('#body');
  // The wide layout's grid goes once room.js has seen the resize.
  await expect(alice.locator('body')).not.toHaveClass(/transcript-room/);
  const send = await rect(alice, '#send');
  // A failure names what took the height.
  const parts = await alice.evaluate(() => [...document.getElementById('send').children].filter((e) => e.getClientRects().length)
    .map((e) => `${e.id || e.tagName.toLowerCase()} ${Math.round(e.getBoundingClientRect().height)}`).join(', '));
  expect(send.height, 'composer at rest: ' + parts).toBeLessThanOrEqual(60);
  const [plus, box, submit] = await Promise.all(['#plus-open', '#body', '#send > [type=submit]'].map((s) => rect(alice, s)));
  expect(Math.abs(plus.mid - box.mid), '+ on the row').toBeLessThanOrEqual(1);
  expect(Math.abs(submit.mid - box.mid), 'Send on the row').toBeLessThanOrEqual(1);
  expect(await body.evaluate((t) => {
    const ctx = document.createElement('canvas').getContext('2d');
    const s = getComputedStyle(t);
    ctx.font = s.font;
    return ctx.measureText(t.placeholder).width <= t.clientWidth - parseFloat(s.paddingLeft) - parseFloat(s.paddingRight);
  }), 'placeholder on one line').toBe(true);
  const height = () => body.evaluate((t) => t.getBoundingClientRect().height);
  const rest = await height();
  expect(rest, 'one line').toBeLessThanOrEqual(44.5);
  const lines = (n) => Array.from({ length: n }, (_, i) => 'line ' + i).join('\n');
  await body.fill(lines(3));
  const three = await height();
  expect(three).toBeGreaterThan(rest);
  await body.fill(lines(4));
  const four = await height();
  await body.fill(lines(5));
  const five = await height();
  expect(five).toBeGreaterThan(four);
  expect(await body.evaluate((t) => t.scrollHeight <= t.clientHeight), '5 lines without a scroll').toBe(true);
  await body.fill(lines(9));
  expect(await height(), 'capped at 5 lines').toBeCloseTo(five, 0);
  expect(await body.evaluate((t) => t.scrollHeight > t.clientHeight), 'then scrolls').toBe(true);
  await body.press('Enter');
  await expect(alice.locator('#messages')).toContainText('line 8');
  await expect(body).toHaveValue('');
  expect(await height(), 'back to one line').toBeCloseTo(rest, 0);

  await alice.setViewportSize({ width: 900, height: 844 });
  await expect(alice.locator('#plus-open')).toBeHidden();
  await expect(alice.getByRole('button', { name: 'Attach files' })).toBeVisible();
});

// 037-AC4: the empty box stays one row, its placeholder cut rather than wrapped, at 375 px, and
// beside a raised hand at the XL text size (028).
test('the phone composer stays one row when narrower', async ({ request, browser }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const rest = () => expect.poll(() => rect(alice, '#send').then((r) => r.height)).toBeLessThanOrEqual(60);
  await alice.setViewportSize({ width: 375, height: 812 });
  await rest();
  await alice.evaluate(() => localStorage.setItem('grill-room-text-size', JSON.stringify({ chat: 'xl' })));
  await alice.setViewportSize(phone);
  await alice.reload();
  await alice.getByRole('button', { name: 'Attach and more' }).click();
  await alice.locator('#plus-menu').getByRole('button', { name: 'Raise my hand' }).click();
  await expect(alice.locator('#hand-raise')).toBeVisible();
  await rest();
  await alice.setViewportSize({ width: 375, height: 812 });
  await rest();
});

// 037-AC5 037-AC8: "+" opens the composer's tools by keyboard, its first item focused, arrows
// moving, Escape back on "+"; each item does what its button does, in its state; a raised hand
// shows in the row to be lowered in one tap. Every target 44 px, "+" with its focus ring.
test('the phone composer "+" menu', async ({ request, browser }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  const menu = alice.locator('#plus-menu');
  const focused = () => alice.evaluate(() => document.activeElement.id);
  await alice.locator('#body').focus();
  await alice.keyboard.press('Shift+Tab');
  expect(await focused()).toBe('plus-open');
  expect(await alice.locator('#plus-open').evaluate((e) => getComputedStyle(e).outlineStyle)).toBe('solid');
  await alice.keyboard.press('Enter');
  await expect(menu).toBeVisible();
  await expect.poll(focused).toBe('plus-attach');
  // A desktop browser at this width offers the capture: the item shows as its button would.
  await expect(menu.getByRole('button')).toHaveText(['Attach files', 'Capture screen', 'Raise my hand']);
  expect(await focused()).toBe('plus-attach');
  await alice.keyboard.press('ArrowDown');
  expect(await focused()).toBe('plus-capture');
  await alice.keyboard.press('ArrowDown');
  expect(await focused()).toBe('plus-hand');
  expect(await smallTargets(alice), 'menu open').toEqual([]);
  await alice.keyboard.press('Escape');
  await expect(menu).toBeHidden();
  expect(await focused()).toBe('plus-open');

  await alice.locator('#plus-open').click();
  const [chooser] = await Promise.all([alice.waitForEvent('filechooser'), menu.getByRole('button', { name: 'Attach files' }).click()]);
  expect(chooser.isMultiple()).toBe(true);
  await expect(menu).toBeHidden();

  // The hand's state follows its button while the menu is open.
  await alice.evaluate(() => { document.getElementById('hand-raise').disabled = true; });
  await alice.locator('#plus-open').click();
  await expect(menu.getByRole('button', { name: 'Raise my hand' })).toBeDisabled();
  await alice.evaluate(() => { document.getElementById('hand-raise').disabled = false; });
  await expect(menu.getByRole('button', { name: 'Raise my hand' })).toBeEnabled();
  await menu.getByRole('button', { name: 'Raise my hand' }).click();
  const hand = alice.locator('#hand-raise');
  await expect(hand).toBeVisible();
  await expect(hand).toHaveAccessibleName('Lower my hand');
  const [h, b] = await Promise.all([rect(alice, '#hand-raise'), rect(alice, '#body')]);
  expect([Math.round(h.width), Math.round(h.height)]).toEqual([44, 44]);
  expect(Math.abs(h.mid - b.mid), 'the hand on the row').toBeLessThanOrEqual(1);
  await alice.locator('#plus-open').click();
  await expect(menu.getByRole('button', { name: 'Lower my hand' })).toBeVisible();
  await alice.keyboard.press('Escape');
  await hand.click();
  await expect(hand).toBeHidden();
  expect(await smallTargets(alice), 'composer').toEqual([]);
});

// 037-AC6 037-AC8: a phone's pills are compact, on their own row under the room's name, with or
// without voice; a 44 px slot follows the bar for 035's "Add a persona"; the name keeps 6rem.
test('the phone header', async ({ request, browser }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  // One full pill fits a phone's row: compact all the same.
  await alice.setViewportSize(phone);
  await expect(alice.locator('.persona-pill')).toHaveCount(1);
  await expect(alice.locator('#persona-buttons')).toHaveClass(/compact/);
  expect((await request.patch(`/api/rooms/${room.id}`, { headers: admin, data: { name: 'A room name long enough to take the header from the personas' } })).status()).toBe(200);
  await addPersonas(alice, room.id, 2);
  await alice.setViewportSize(phone);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(3);
  const check = async (label) => {
    const [header, name, bar, zone] = await Promise.all(['.room-header', '#room-open .name', '#persona-buttons', '#agent'].map((s) => rect(alice, s)));
    await expect(alice.locator('#persona-buttons')).toHaveClass(/compact/);
    expect(Math.min(name.right, header.right) - name.left, `${label}: name`).toBeGreaterThanOrEqual(96);
    expect(bar.top, `${label}: pills under the name`).toBeGreaterThanOrEqual(name.bottom);
    expect(zone.right - bar.right, `${label}: slot after the bar`).toBeGreaterThanOrEqual(44);
    // The bar scrolls its pills: the page itself never scrolls sideways.
    expect(await alice.evaluate(() => document.documentElement.scrollWidth - innerWidth), label).toBeLessThanOrEqual(0);
    expect(await smallTargets(alice), label).toEqual([]);
  };
  await check('3 personas');
  await addPersonas(alice, room.id, 6);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(9);
  await check('9 personas');
  await expect(alice.locator('.persona-more.right')).toBeVisible();
  await withVoice(alice, room.id);
  await expect(alice.locator('#voice-join')).toBeVisible();
  await check('9 personas, voice');
  await expect(alice.locator('.persona-more.right')).toBeVisible();
});

// 037-AC7: three personas, no voice, no audio: the messages take three quarters of the screen.
test('the phone gives the messages the screen', async ({ request, browser }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await addPersonas(alice, room.id, 2);
  await alice.setViewportSize(phone);
  // A server with audio, nobody in it.
  await withVoice(alice, room.id);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(3);
  await expect(alice.locator('#messages')).toContainText('Bob joined');
  await expect(alice.locator('#voice-join')).toBeVisible();
  await expect(alice.locator('#chat-filter')).toBeHidden();
  // 039-AC10's checklist shares the empty chat's area with the messages: the reading area is both.
  const reading = await alice.evaluate(() => ['messages', 'chat-ready'].reduce((h, id) => h + document.getElementById(id).getBoundingClientRect().height, 0));
  expect(reading).toBeGreaterThanOrEqual(633);
});
