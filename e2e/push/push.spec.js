import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test, expect, chromium } from '@playwright/test';

const admin = { Authorization: 'Bearer ' + process.env.PUSH_ADMIN_TOKEN };

// 005-AC24: Alice turns notifications on, her tab loses the focus, Bob mentions her: the push goes
// through the browser vendor's service and her service worker shows one notification naming the
// room and the kind of event, never the line. A persistent profile, as Chrome turns the Push API
// off in the off-the-record contexts Playwright's newContext makes.
test('a mention arrives as a real push notification', async ({ request, baseURL }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Push check' } })).json();
  const invite = async () => (await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json());
  const ctx = await chromium.launchPersistentContext(mkdtempSync(join(tmpdir(), 'push-')), { channel: 'chrome', headless: false, baseURL });
  try {
    await ctx.grantPermissions(['notifications'], { origin: baseURL });
    const alice = await ctx.newPage();
    const inv = await invite();
    await alice.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await alice.getByLabel('Your name').fill('Alice');
    await alice.getByRole('button', { name: 'Join the room' }).click();
    await expect(alice.locator('#me')).toHaveText('Alice');
    const bob = await (await request.post('/api/join', { data: { invite: (await invite()).token, name: 'Bob' } })).json();

    await alice.locator('#notify summary').click();
    await alice.getByRole('button', { name: 'Turn on notifications' }).click();
    await expect(alice.locator('#push-status')).toHaveText('Notifications on for this room.', { timeout: 30_000 });
    // The tab loses the focus, as when another window comes in front, and its beat says so.
    await alice.evaluate(() => { document.hasFocus = () => false; window.dispatchEvent(new Event('blur')); });
    await alice.waitForTimeout(1000);
    const said = await request.post(`/api/rooms/${room.id}/messages`, { headers: { Authorization: 'Bearer ' + bob.token }, data: { body: '@Alice the secret plan' } });
    expect(said.status()).toBe(201);
    const shown = await alice.evaluate(async () => {
      const reg = await navigator.serviceWorker.ready;
      for (let i = 0; i < 60; i++) {
        const n = await reg.getNotifications();
        if (n.length) return n.map((x) => ({ title: x.title, body: x.body, data: x.data }));
        await new Promise((r) => setTimeout(r, 1000));
      }
      return [];
    });
    expect(shown).toEqual([{ title: 'Push check', body: 'You were mentioned', data: { url: `/r/${room.id}`, count: 1 } }]);
  } finally {
    await ctx.close();
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  }
});
