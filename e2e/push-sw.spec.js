import { test, expect } from '@playwright/test';
import { createECDH, randomBytes } from 'node:crypto';
import { roomWith, menuItem } from './helpers.js';

// The service worker's side of Web Push (005-AC24), without a push service: headless Chrome has
// none. The specs call the worker's handlers in its own scope; the DevTools protocol's
// deliverPushMessage lost a push now and then, a dropped event no assertion can tell from a bug.

// worker registers /sw.js on page and returns push(payload), which shows it as a push
// would, raw(text), the same for a push's raw data, and shown(), the notifications it shows.
async function worker(page) {
  await page.context().grantPermissions(['notifications']);
  await page.goto('/');
  const [sw] = await Promise.all([
    page.context().serviceWorkers()[0] ?? page.context().waitForEvent('serviceworker'),
    page.evaluate(async () => { await navigator.serviceWorker.register('/sw.js'); await navigator.serviceWorker.ready; }),
  ]);
  return {
    push: (p) => sw.evaluate((p) => notify(p), p),
    // raw shows a push whose data is text, or none for null.
    raw: (text) => sw.evaluate((text) => notify(payload(text === null ? null : { json: () => JSON.parse(text) })), text),
    shown: () => page.evaluate(async () => (await (await navigator.serviceWorker.ready).getNotifications())
      .map((n) => ({ title: n.title, body: n.body, tag: n.tag, data: n.data }))),
  };
}

// Two batches for one room, each linking to its own line, make one notification counting both.
test('pushes for one room merge into one notification', async ({ page }) => {
  const sw = await worker(page);
  await sw.push({ room: 'Pricing', kind: 'mention', count: 1, url: '/r/abc#m1' });
  await sw.push({ room: 'Pricing', kind: 'mention', count: 1, url: '/r/abc#m7' });
  expect(await sw.shown()).toEqual([{ title: 'Pricing', body: 'You were mentioned · 2 events', tag: 'grill-room:/r/abc', data: { url: '/r/abc#m7', count: 2 } }]);
  const badge = await page.evaluate(async () => (await (await navigator.serviceWorker.ready).getNotifications())[0].badge);
  expect(new URL(badge).pathname).toBe('/static/icons/badge-96.png');
  expect((await page.request.get(badge)).headers()['content-type']).toBe('image/png');
  // Another room stands apart.
  await sw.push({ room: 'Roadmap', kind: 'synthesis', count: 1, url: '/r/def#m3' });
  expect((await sw.shown()).map((n) => n.tag).sort()).toEqual(['grill-room:/r/abc', 'grill-room:/r/def']);
});

// A push without data, or whose data is not JSON, shows a generic notification.
test('a malformed push still shows a notification', async ({ page }) => {
  const sw = await worker(page);
  const generic = { title: 'grill-room', body: 'Something happened', tag: 'grill-room:/', data: { count: 1 } };
  await sw.raw('{"room": "Pri');
  expect(await sw.shown()).toEqual([generic]);
  await sw.raw(null);
  expect(await sw.shown()).toEqual([{ ...generic, body: 'Something happened · 2 events', data: { count: 2 } }]);
});

// A click opens only a page of this origin: any other, or a url that does not parse, opens the home page.
test('a notification opens only this origin', async ({ page, baseURL }) => {
  await worker(page);
  const sw = page.context().serviceWorkers()[0];
  const opened = await sw.evaluate((urls) => urls.map((u) => sameOrigin(u).href),
    ['/r/abc#m3', 'https://evil.example/r/abc', '//evil.example/r/abc', 'javascript:alert(1)', 'http://[', undefined]);
  expect(opened).toEqual([`${baseURL}/r/abc#m3`, ...Array(5).fill(`${baseURL}/`)]);
});

// fakePush stands in for the browser's push service in the page: one subscription, kept in
// localStorage across reloads, with a real P-256 key the server accepts. window.pushCalls lists
// the subscribe and unsubscribe calls of the page load.
async function fakePush(page) {
  await page.context().grantPermissions(['notifications']);
  const ecdh = createECDH('prime256v1');
  const keys = { p256dh: ecdh.generateKeys().toString('base64url'), auth: randomBytes(16).toString('base64url') };
  await page.context().addInitScript((keys) => {
    window.pushCalls = [];
    const make = (s) => s && {
      endpoint: s.endpoint,
      options: { applicationServerKey: s.key && Uint8Array.from(s.key).buffer },
      toJSON: () => ({ endpoint: s.endpoint, keys }),
      unsubscribe: async () => { window.pushCalls.push('unsubscribe'); localStorage.removeItem('fake-push'); return true; },
    };
    PushManager.prototype.getSubscription = async () => make(JSON.parse(localStorage.getItem('fake-push') || 'null'));
    PushManager.prototype.subscribe = async (o) => {
      window.pushCalls.push('subscribe');
      const s = { endpoint: 'https://fcm.googleapis.com/fcm/send/' + crypto.randomUUID(), key: [...new Uint8Array(o.applicationServerKey)] };
      localStorage.setItem('fake-push', JSON.stringify(s));
      return make(s);
    };
  }, keys);
}

// The page's push requests until its PUT of a subscription, the last one a load makes.
async function pushRequests(page, load) {
  const seen = [];
  const log = (r) => { if (r.url().endsWith('/push') && r.method() !== 'GET') seen.push({ method: r.method(), endpoint: r.postDataJSON().subscription.endpoint }); };
  page.on('request', log);
  const [put] = await Promise.all([page.waitForResponse((r) => r.url().endsWith('/push') && r.request().method() === 'PUT'), load()]);
  page.off('request', log);
  expect(put.status()).toBe(204);
  return seen;
}

// A subscription made with another VAPID key, as after the instance's key changed, is deleted on
// the server and in the browser, and made again with the instance's key; one made with it stays.
test('a subscription with a stale VAPID key is made again', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await fakePush(alice);
  const old = 'https://fcm.googleapis.com/fcm/send/old';
  await alice.evaluate(([room, old]) => {
    localStorage.setItem('grill-room-push:' + room, '1');
    localStorage.setItem('fake-push', JSON.stringify({ endpoint: old, key: [4, ...Array(64).fill(7)] }));
  }, [room.id, old]);

  const seen = await pushRequests(alice, () => alice.reload());
  const fresh = await alice.evaluate(() => JSON.parse(localStorage.getItem('fake-push')));
  expect(seen).toEqual([{ method: 'DELETE', endpoint: old }, { method: 'PUT', endpoint: fresh.endpoint }]);
  expect(await alice.evaluate(() => window.pushCalls)).toEqual(['unsubscribe', 'subscribe']);
  const { key } = await (await alice.request.get(`/api/rooms/${room.id}/push`)).json();
  expect(Buffer.from(fresh.key).toString('base64url')).toBe(key);

  expect(await pushRequests(alice, () => alice.reload())).toEqual([{ method: 'PUT', endpoint: fresh.endpoint }]);
  expect(await alice.evaluate(() => window.pushCalls)).toEqual([]);
});

// A subscription the browser renews reaches the server for each room turned on in this browser,
// with its "every line" choice; a room turned off is left out.
test('a renewed subscription is handed to the server', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await fakePush(alice);
  await alice.evaluate((room) => localStorage.setItem('grill-room-push:' + room, '1'), room.id);
  await pushRequests(alice, () => alice.reload());
  const sw = alice.context().serviceWorkers()[0];
  const puts = [];
  alice.context().on('request', (r) => { if (r.serviceWorker() && r.method() === 'PUT') puts.push(r); });
  const renewed = await alice.evaluate(async () => {
    const sub = (await (await navigator.serviceWorker.ready).pushManager.getSubscription()).toJSON();
    return { ...sub, endpoint: 'https://fcm.googleapis.com/fcm/send/renewed' };
  });
  await sw.evaluate((sub) => resubscribe(null, { toJSON: () => sub }), renewed);
  await expect.poll(() => puts.length).toBe(1);
  expect(puts[0].url()).toMatch(new RegExp(`/api/rooms/${room.id}/push$`));
  expect(puts[0].postDataJSON()).toEqual({ subscription: renewed, all_messages: false });
  expect((await puts[0].response()).status()).toBe(204);

  await menuItem(alice, 'Notifications', 'me');
  await alice.locator('#push-toggle').click();
  await expect(alice.locator('#push-status')).toHaveText('Notifications off for this room.');
  expect(await sw.evaluate(async () => (await (await caches.open('grill-room-push')).match('/rooms')).json())).toEqual({});
  await sw.evaluate((sub) => resubscribe(null, { toJSON: () => sub }), renewed);
  expect(puts).toHaveLength(1);
});

// The toggle reads on only while the browser allows notifications: revoked, it offers them again.
for (const [perm, label] of [['granted', 'Turn off notifications'], ['denied', 'Turn on notifications'], ['default', 'Turn on notifications']]) {
  test(`turned on, permission ${perm}: the toggle says ${label}`, async ({ browser, request }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await fakePush(alice);
    await alice.context().addInitScript((perm) => Object.defineProperty(Notification, 'permission', { get: () => perm, configurable: true }), perm);
    await alice.evaluate((room) => localStorage.setItem('grill-room-push:' + room, '1'), room.id);
    await alice.reload();
    await expect(alice.locator('#push-toggle')).toHaveText(label);
  });
}
