import { test, expect } from './fixtures.js';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { roomWith, startDaemon, pairDaemon, confirmSheet, connectedRow, unfoldTabs } from './helpers.js';

const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
const wide = { width: 1920, height: 1080 };

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

// 003b-AC2 003b-AC3 003b-AC16 003b-AC20 003b-AC23, daemon-r2: at 390, 1440 and 1920 px the rooms
// are rows, not cards, at most 64 px high on a desktop; General and Network each have one save bar;
// the websites table has no Scope column and its remove buttons sit on their domain's line; the
// dev-key form stacks Name, Public key and its button, left-aligned; General, Network and Update
// use the wide page; the pills take their token's colour; no tab overflows; the page says none of
// the words docs/design.md §8 retires.
test('daemon page layout at phone, desktop and wide width', async ({ browser, request, baseURL }) => {
  test.setTimeout(90_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' });
  try {
    const admin = await pairDaemon(alice, daemon);
    const row = admin.locator('#instances .room-row');
    await expect(row.locator('.status')).toHaveText('runs the AI', { timeout: 30_000 });
    for (const size of [desktop, phone, wide]) {
      await admin.setViewportSize(size);
      const at = size.width + 'px';
      await unfoldTabs(admin.locator('#tabs'));
      await admin.locator('#tab-rooms').click();
      await expect(admin.locator('#panel-rooms .card'), at).toHaveCount(0);
      await expect(row, at).toHaveCount(1);
      const r = await row.evaluate((e) => {
        const box = e.getBoundingClientRect();
        const top = (sel) => e.querySelector(sel).getBoundingClientRect().top - box.top;
        return { height: box.height, name: top('.room-name'), more: top('.actions .more'), pause: top('.controls button') };
      });
      if (size !== phone) {
        expect(r.height, at).toBeLessThanOrEqual(64);
        expect(Math.abs(r.pause - r.name), `Pause on the name's line, ${at}`).toBeLessThan(16);
      } else {
        // On a phone the account and Pause take a second line; the name and its ⋯ share the first.
        expect(Math.abs(r.more - r.name), `⋯ on the name's line, ${at}`).toBeLessThan(16);
      }
      expect(await token(admin, '--success'), at).toBe(await row.locator('.status').evaluate((e) => getComputedStyle(e).color));

      // One save bar per settings tab, and nothing else that saves there.
      for (const tab of ['general', 'network']) {
        await unfoldTabs(admin.locator('#tabs'));
        await admin.locator('#tab-' + tab).click();
        await expect(admin.locator(`#panel-${tab} .save-bar`), `${tab} ${at}`).toHaveCount(1);
        await expect(admin.locator(`#panel-${tab} button`, { hasText: /^Save$/ }), `${tab} ${at}`).toHaveCount(1);
      }
      await unfoldTabs(admin.locator('#tabs'));
      await admin.locator('#tab-general').click();
      await expect(admin.locator('#server-group'), at).toBeVisible();
      // The two groups side by side on a desktop, one above the other on a phone.
      const [m, s] = await Promise.all(['#machine-group', '#server-group'].map((f) => admin.locator(f).boundingBox()));
      expect(size === phone ? m.x === s.x && m.y < s.y : m.x < s.x, `groups ${at}`).toBe(true);
      // The server picker heads its group.
      await expect(admin.locator('#server-group > label').first(), at).toHaveText('Server');

      await unfoldTabs(admin.locator('#tabs'));
      await admin.locator('#tab-network').click();
      await expect(admin.locator('#allowlist th'), at).toHaveText(['Domain', 'Hits', 'Remove']);
      await expect(admin.locator('#add-docs'), at).toHaveCount(0);
      // Every remove button on its domain's line, in one column.
      const removes = await admin.locator('#allowlist tbody tr').evaluateAll((trs) => trs.map((tr) => {
        const b = tr.querySelector('button').getBoundingClientRect();
        const d = tr.querySelector('code').getBoundingClientRect();
        return { dom: tr.querySelector('code').textContent, dy: Math.abs((b.top + b.bottom) / 2 - (d.top + d.bottom) / 2), x: Math.round(b.left) };
      }));
      expect(removes.length, at).toBeGreaterThan(3);
      expect(removes.filter((x) => x.dy > 12).map((x) => x.dom), `remove buttons off their line, ${at}`).toEqual([]);
      expect(new Set(removes.map((x) => x.x)).size, `remove buttons in one column, ${at}`).toBe(1);

      // The dev-key form: Name, then Public key, then the button, each starting at the same left edge.
      await unfoldTabs(admin.locator('#tabs'));
      await admin.locator('#tab-update').click();
      const f = await admin.locator('#key-form').evaluate((form) => ['#key-name', '#key-pem', 'button[type=submit]'].map((sel) => {
        const b = form.querySelector(sel).getBoundingClientRect();
        return { top: b.top, bottom: b.bottom, left: Math.round(b.left) };
      }));
      expect(f[0].bottom <= f[1].top && f[1].bottom <= f[2].top, `stacked, ${at}`).toBe(true);
      expect(new Set(f.map((x) => x.left)).size, `left-aligned, ${at}`).toBe(1);

      // Wide tabs fill the window past the narrow column's 72rem (1152 px) on a wide screen.
      for (const tab of ['general', 'network', 'update']) {
        await unfoldTabs(admin.locator('#tabs'));
        await admin.locator('#tab-' + tab).click();
        const w = await admin.locator('#main').evaluate((e) => e.getBoundingClientRect().width);
        if (size === wide) expect(w, `${tab} page width, ${at}`).toBeGreaterThan(1800);
      }

      for (const tab of ['rooms', 'accounts', 'general', 'network', 'monitoring', 'update']) {
        await unfoldTabs(admin.locator('#tabs'));
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
    await expect(connectedRow(pa, room.id)).toBeAttached({ timeout: 30_000 });
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

// daemon-r2: adding or removing a website shows in the list at once, before the app answers; a
// refused change goes back and says why; "added" is a toast that fades, and no status text stays.
// The room offers its host the app's page.
test('the websites list updates at once and rolls back on error', async ({ browser, request, baseURL }) => {
  test.setTimeout(90_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' });
  try {
    const admin = await pairDaemon(alice, daemon);
    // The room links its host to the app's page, from ⋯ and from Diagnostics, once the app is connected.
    await expect(alice.locator('#host-app-open')).not.toHaveAttribute('hidden', '');
    await expect(alice.locator('#host-app-open')).toHaveAttribute('href', 'http://127.0.0.1:47680/');
    await expect(alice.locator('#diag-host-app')).toHaveAttribute('href', 'http://127.0.0.1:47680/');
    await admin.locator('#tab-network').click();
    // The hold timeout is the tab's one field to save, through its save bar.
    await admin.getByLabel('Wait for an answer').fill('50');
    await expect(admin.locator('#network-save .dirty')).toHaveText('1 unsaved change');
    await admin.locator('#network-save').getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#network-save .dirty')).toHaveText('Everything saved');
    await expect(admin.getByLabel('Wait for an answer')).toHaveValue('50');
    const list = admin.locator('#allowlist');
    const domainRow = (d) => list.locator('tbody tr', { has: admin.locator('code', { hasText: new RegExp('^' + d.replaceAll('.', '\\.') + '$') }) });
    await expect(domainRow('pypi.org')).toHaveCount(1);

    // Each request waits for the test to let it through: the row shows while it is held.
    let release;
    let fail = false;
    await admin.route('**/api/allowlist', async (route) => {
      await new Promise((r) => { release = r; });
      if (fail) return route.fulfill({ status: 502, contentType: 'application/json', body: JSON.stringify({ error: 'the app did not answer' }) });
      return route.continue();
    });
    const held = () => expect.poll(() => typeof release).toBe('function');

    await admin.getByLabel('Domain').fill('optimistic.example');
    await admin.locator('#allowlist-form').getByRole('button', { name: 'Add' }).click();
    await held();
    await expect(domainRow('optimistic.example')).toHaveClass(/\bpending\b/);
    await expect(admin.getByLabel('Domain')).toHaveValue('');
    release();
    release = undefined;
    const toast = admin.locator('.toast', { hasText: 'optimistic.example added' });
    await expect(toast).toBeVisible();
    await expect(domainRow('optimistic.example')).not.toHaveClass(/\bpending\b/);
    await expect(admin.locator('#allowlist-error')).toBeEmpty();
    // The toast fades on its own; nothing on the page keeps saying it.
    await expect(toast).toHaveCount(0, { timeout: 10_000 });
    await expect(admin.locator('#panel-network')).not.toContainText('Saved');
    await expect(admin.locator('#panel-network')).not.toContainText('added');

    // A remove leaves at once; refused, the row comes back with the reason.
    fail = true;
    await admin.getByRole('button', { name: 'Remove pypi.org' }).click();
    await held();
    await expect(domainRow('pypi.org')).toHaveCount(0);
    release();
    release = undefined;
    await expect(domainRow('pypi.org')).toHaveCount(1);
    await expect(admin.locator('#allowlist-error')).toHaveText('The app did not answer');

    // A remove that lands stays gone, and the error goes with the next try.
    fail = false;
    await admin.getByRole('button', { name: 'Remove optimistic.example' }).click();
    await held();
    await expect(domainRow('optimistic.example')).toHaveCount(0);
    await expect(admin.locator('#allowlist-error')).toBeEmpty();
    release();
    await expect(admin.locator('.toast', { hasText: 'optimistic.example removed' })).toBeVisible();
    release = undefined;
    await admin.reload();
    await expect(domainRow('pypi.org')).toHaveCount(1);
    await expect(domainRow('optimistic.example')).toHaveCount(0);

    // Two adds in a row go one after the other: the first refused, it does not ride along with the second.
    fail = true;
    for (const d of ['first.example', 'second.example']) {
      await admin.getByLabel('Domain').fill(d);
      await admin.locator('#allowlist-form').getByRole('button', { name: 'Add' }).click();
    }
    await held();
    await expect(domainRow('first.example')).toHaveCount(1);
    await expect(domainRow('second.example')).toHaveCount(1);
    const sent = [];
    admin.on('request', (req) => { if (req.url().endsWith('/api/allowlist')) sent.push(req.postDataJSON().allowlist); });
    release();
    release = undefined;
    await expect(domainRow('first.example')).toHaveCount(0);
    fail = false;
    await held();
    release();
    await expect(admin.locator('.toast', { hasText: 'second.example added' })).toBeVisible();
    await admin.reload();
    await expect(domainRow('second.example')).toHaveCount(1);
    await expect(domainRow('first.example')).toHaveCount(0);
    expect(sent.flat()).not.toContain('first.example');
  } finally {
    daemon.cleanup();
  }
});
