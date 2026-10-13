import { test, expect } from '@playwright/test';
import { createECDH, randomBytes } from 'node:crypto';
import { roomWith } from './helpers.js';

// The service worker's side of Web Push (005-AC24), without a push service: headless Chrome has
// none, so the DevTools protocol hands the pushes to the worker.

// worker registers /sw.js on page and returns push(payload), which delivers one push to it, and
// shown(), the notifications it shows.
async function worker(page) {
  await page.context().grantPermissions(['notifications']);
  await page.goto('/');
  const scope = await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.register('/sw.js');
    await navigator.serviceWorker.ready;
    return reg.scope;
  });
  const cdp = await page.context().newCDPSession(page);
  const registration = new Promise((resolve) => cdp.on('ServiceWorker.workerRegistrationUpdated', ({ registrations }) => {
    const r = registrations.find((x) => x.scopeURL === scope && !x.isDeleted);
    if (r) resolve(r.registrationId);
  }));
  await cdp.send('ServiceWorker.enable');
  const registrationId = await registration;
  const origin = new URL(scope).origin;
  return {
    push: (data) => cdp.send('ServiceWorker.deliverPushMessage', { origin, registrationId, data: typeof data === 'string' ? data : JSON.stringify(data) }),
    shown: () => page.evaluate(async () => (await (await navigator.serviceWorker.ready).getNotifications())
      .map((n) => ({ title: n.title, body: n.body, tag: n.tag, data: n.data }))),
  };
}

// Two batches for one room, each linking to its own line, make one notification counting both.
test('pushes for one room merge into one notification', async ({ page }) => {
  const sw = await worker(page);
  await sw.push({ room: 'Pricing', kind: 'mention', count: 1, url: '/r/abc#m1' });
  await expect.poll(sw.shown).toHaveLength(1);
  await sw.push({ room: 'Pricing', kind: 'mention', count: 1, url: '/r/abc#m7' });
  await expect.poll(async () => (await sw.shown())[0]?.data.count).toBe(2);
  expect(await sw.shown()).toEqual([{ title: 'Pricing', body: 'You were mentioned · 2 events', tag: 'grill-room:/r/abc', data: { url: '/r/abc#m7', count: 2 } }]);
  // Another room stands apart.
  await sw.push({ room: 'Roadmap', kind: 'synthesis', count: 1, url: '/r/def#m3' });
  await expect.poll(sw.shown).toHaveLength(2);
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
