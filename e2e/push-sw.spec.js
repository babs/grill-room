import { test, expect } from '@playwright/test';

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
