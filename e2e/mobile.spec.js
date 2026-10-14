import { test, expect, devices } from '@playwright/test';
import http from 'node:http';
import { adminToken } from './playwright.config.js';
import { roomWith, menuItem, personaRow, personaSheet, closePersonas } from './helpers.js';

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
  const delivered = alice.evaluate(() => new Promise((ok) => events.addEventListener('message', (e) => { if (e.data.includes('sent while away')) ok(); })));
  const bob = await joinAPI(request, room.id, 'Bob');
  await post(request, room.id, bob, 'sent while away');
  await delivered;
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
  const pinned = () => page.evaluate(() => pinned);
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
    await panel.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
    await panel.getByRole('button', { name: 'Add', exact: true }).click();
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
