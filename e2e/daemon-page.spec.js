import { test, expect } from './fixtures.js';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { roomWith, startDaemon, pairDaemon, confirmSheet } from './helpers.js';

const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };

// token resolves a custom property to the colour the page computes for it.
const token = (page, name) => page.evaluate((n) => {
  const probe = document.createElement('span');
  probe.style.color = `var(${n})`;
  document.body.append(probe);
  const c = getComputedStyle(probe).color;
  probe.remove();
  return c;
}, name);

// daemonToken reads the token a test daemon keeps for roomURL, from its owner-only file.
function daemonToken(daemon, roomURL) {
  const file = readdirSync(daemon.stateDir, { recursive: true }).find((f) => String(f).endsWith('tokens.json'));
  return JSON.parse(readFileSync(join(daemon.stateDir, String(file)), 'utf8'))[roomURL];
}

// tokenWorks reports whether the room server still takes a daemon token.
const tokenWorks = async (request, roomID, tok) => (await request.post(`/api/rooms/${roomID}/daemon/status`,
  { headers: { Authorization: `Bearer ${tok}` }, data: { can_host: false, reason: 'probe' } })).status() !== 401;

// 003b-AC2 003b-AC3 003b-AC14 003b-AC15 003b-AC16 003b-AC17 003b-AC20 003b-AC23: at 1440 and 390 px
// the rooms are rows, not cards, at most 64 px high at 1440; each settings group has one Save; the
// allowlist's remove buttons sit on their domain's line; the pills take their token's colour; no
// tab overflows; the page says none of the words docs/design.md §8 retires.
test('daemon page layout at desktop and phone width', async ({ browser, request, baseURL }) => {
  test.setTimeout(90_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' });
  try {
    const admin = await pairDaemon(alice, daemon);
    const row = admin.locator('#instances .room-row');
    await expect(row.locator('.status')).toHaveText('runs the AI', { timeout: 30_000 });
    for (const size of [desktop, phone]) {
      await admin.setViewportSize(size);
      const at = size.width + 'px';
      await admin.locator('#tab-rooms').click();
      await expect(admin.locator('#panel-rooms .card'), at).toHaveCount(0);
      await expect(row, at).toHaveCount(1);
      const r = await row.evaluate((e) => {
        const box = e.getBoundingClientRect();
        const top = (sel) => e.querySelector(sel).getBoundingClientRect().top - box.top;
        return { height: box.height, name: top('.room-name'), more: top('.actions .more'), pause: top('.controls button') };
      });
      if (size === desktop) {
        expect(r.height, at).toBeLessThanOrEqual(64);
        expect(Math.abs(r.pause - r.name), `Pause on the name's line, ${at}`).toBeLessThan(16);
      } else {
        // On a phone the account and Pause take a second line; the name and its ⋯ share the first.
        expect(Math.abs(r.more - r.name), `⋯ on the name's line, ${at}`).toBeLessThan(16);
      }
      expect(await token(admin, '--success'), at).toBe(await row.locator('.status').evaluate((e) => getComputedStyle(e).color));

      await admin.locator('#tab-settings').click();
      for (const form of ['#machine-form', '#settings-form']) {
        await expect(admin.locator(form + ' button[type=submit]'), `${form} ${at}`).toHaveCount(1);
        await expect(admin.locator(form + ' button[type=submit]'), `${form} ${at}`).toHaveText('Save');
      }
      await expect(admin.locator('#panel-settings form button', { hasText: /^Save/ }), at).toHaveCount(2);
      // Every remove button on its domain's line, in one column.
      const removes = await admin.locator('#allowlist tbody tr').evaluateAll((trs) => trs.map((tr) => {
        const b = tr.querySelector('button').getBoundingClientRect();
        const d = tr.querySelector('code').getBoundingClientRect();
        return { dom: tr.querySelector('code').textContent, dy: Math.abs((b.top + b.bottom) / 2 - (d.top + d.bottom) / 2), x: Math.round(b.left) };
      }));
      expect(removes.length, at).toBeGreaterThan(3);
      expect(removes.filter((x) => x.dy > 12).map((x) => x.dom), `remove buttons off their line, ${at}`).toEqual([]);
      expect(new Set(removes.map((x) => x.x)).size, `remove buttons in one column, ${at}`).toBe(1);
      // The two groups side by side at 1440, one above the other at 390.
      const [m, s] = await Promise.all(['#machine-form', '#settings-form'].map((f) => admin.locator(f).boundingBox()));
      expect(size === desktop ? m.x < s.x : m.x === s.x && m.y < s.y, `groups ${at}`).toBe(true);

      for (const tab of ['rooms', 'accounts', 'settings', 'monitoring', 'update']) {
        await admin.locator('#tab-' + tab).click();
        expect(await admin.evaluate(() => document.documentElement.scrollWidth), `${tab} ${at}`).toBeLessThanOrEqual(size.width);
        // 003b-AC20: what the page says, its tips and its labels included; the log lines are the app's own.
        const said = await admin.locator('#panel-' + tab).evaluate((p) => [p.innerText.replace(p.querySelector('#logs')?.innerText || '\0', ''),
          ...[...p.querySelectorAll('[data-tip], [aria-label]')].flatMap((e) => [e.getAttribute('data-tip') || '', e.getAttribute('aria-label') || ''])].join('\n'));
        // A path the host typed, as a config folder, may hold any of them.
        expect(said.match(/(?<![/-])\b(instances?|supervisors?|workers?|daemons?)\b(?![-/])/gi), `${tab} ${at}`).toBeNull();
      }
    }
  } finally {
    daemon.cleanup();
  }
});

// 003b-AC5 003b-AC6 003b-AC7 003b-AC11: Alice and Bob each run the AI host app. Alice pauses: the
// room moves the AI host role to Bob; Bob pauses too: the room says why no one answers. Revoked on
// the server, Alice's room shows Connect, which pairs again. Bob removes the room: his token is
// revoked and the row goes.
test('pause fails over, connect pairs again, remove revokes the token', async ({ browser, request, baseURL }) => {
  test.setTimeout(150_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const roomURL = `${baseURL}/r/${room.id}`;
  const env = { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' };
  const da = startDaemon(roomURL, env);
  const db = startDaemon(roomURL, env);
  try {
    const pa = await pairDaemon(alice, da);
    await expect(alice.locator('#messages')).toContainText('Alice holds the agent(s).', { timeout: 30_000 });
    const pb = await pairDaemon(bob, db);
    const rowA = pa.locator('.room-row');
    const rowB = pb.locator('.room-row');
    await expect(rowB.locator('.status')).toHaveText('standby', { timeout: 30_000 });
    await expect(rowB.locator('.status')).toHaveClass(/\bidle\b/);

    await rowA.getByRole('button', { name: /^Pause hosting / }).click();
    await expect(rowA.locator('.status')).toHaveText('paused');
    await expect(rowA.locator('.status')).toHaveClass(/\boff\b/);
    await expect(alice.locator('#messages')).toContainText('Bob holds the agent(s).', { timeout: 30_000 });
    await expect(rowB.locator('.status')).toHaveText('runs the AI', { timeout: 30_000 });

    await rowB.getByRole('button', { name: /^Pause hosting / }).click();
    await expect(alice.locator('#agent-status')).toHaveText("paused on the host's machine", { timeout: 30_000 });
    // The pause outlives a reload of the page: it is saved with the server.
    await pb.reload();
    await expect(rowB.locator('.status')).toHaveText('paused');
    await rowB.getByRole('button', { name: /^Resume / }).click();
    await expect(rowB.locator('.status')).toHaveText('runs the AI', { timeout: 30_000 });
    await rowA.getByRole('button', { name: /^Resume / }).click();
    await expect(rowA.locator('.status')).toHaveText('standby', { timeout: 30_000 });

    // Revoked on the server: the row says so and offers Connect, which asks to pair again.
    const tokA = daemonToken(da, roomURL);
    expect((await request.delete(`/api/rooms/${room.id}/daemon`, { headers: { Authorization: `Bearer ${tokA}` } })).status()).toBe(204);
    expect(await tokenWorks(request, room.id, tokA)).toBe(false);
    await expect(rowA.locator('.status')).toHaveText('not paired', { timeout: 30_000 });
    await expect(rowA.locator('.status')).toHaveClass(/\bfailed\b/);
    await rowA.getByRole('button', { name: /^Connect / }).click();
    await expect(pa.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
    await expect(rowA.locator('.status')).toHaveText('waiting for approval');
    await expect(rowA.locator('.status')).toHaveClass(/\bbusy\b/);
    const [approval] = await Promise.all([alice.context().waitForEvent('page'), pa.getByRole('link', { name: 'approve in the room' }).click()]);
    await approval.getByRole('button', { name: 'Approve' }).click();
    await expect(pa.locator('#pairing')).toHaveText('Paired. Connected.', { timeout: 30_000 });
    await expect(rowA.locator('.status')).toHaveText('standby', { timeout: 30_000 });
    await approval.close();

    // Bob removes the room: asked first, his token revoked, the row gone, Alice's app takes over.
    const tokB = daemonToken(db, roomURL);
    await rowB.getByRole('button', { name: /^More actions on / }).click();
    await pb.getByRole('button', { name: 'Remove this room' }).click();
    await confirmSheet(pb, 'Remove');
    await expect(rowB).toHaveCount(0, { timeout: 30_000 });
    expect(await tokenWorks(request, room.id, tokB)).toBe(false);
    expect(daemonToken(db, roomURL)).toBeUndefined();
    await expect(rowA.locator('.status')).toHaveText('runs the AI', { timeout: 30_000 });
  } finally {
    da.cleanup();
    db.cleanup();
  }
});
