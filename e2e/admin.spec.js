import { test, expect } from './fixtures.js';
import { readFileSync } from 'node:fs';
import { roomWith, startDaemon, pairDaemon, removeSandbox, tableLayout } from './helpers.js';

// Each tab of the page and the sections it holds (005-AC17).
const tabs = { rooms: ['Rooms'], accounts: ['AI accounts'], settings: ['Settings', 'This machine', 'Per server'],
  monitoring: ['Activity', 'Logs'], update: ['Update'] };

// 003-AC26 003-AC27 003-AC28 003-AC29 005-AC17: the supervisor's page shows every section in its
// tab, each tab a deep link, the tab bar pinned while scrolling; a turn shows up in the history
// with its persona, account, cause, cost and context, in the daily totals and in the CSV export;
// the filters narrow it; a login near expiry and a context above 80% raise alerts, each a badge on
// its tab. The same page fits a phone's width. Fake claude: no Claude request.
test('admin page sections, monitoring, CSV export and alerts, at desktop and phone width', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const roomURL = `${baseURL}/r/${room.id}`;
  // A login 10 minutes from expiry; an unreachable GitHub keeps the update check on this machine.
  const daemon = startDaemon(roomURL, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' }, { expiresIn: 10 * 60e3 });
  try {
    const admin = await pairDaemon(alice, daemon);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    for (const [id, names] of Object.entries(tabs)) {
      await admin.locator('#tab-' + id).click();
      await expect(admin).toHaveURL(new RegExp('#' + id + '$'));
      for (const name of names) await expect(admin.getByRole('heading', { name, exact: true })).toBeVisible();
    }
    await admin.goto(admin.url().replace(/#.*$/, '') + '#accounts');
    await expect(admin.locator('#tab-accounts')).toHaveAttribute('aria-current', 'page');
    await expect(admin.locator('#panel-accounts')).toBeVisible();

    // 003b-AC1: the server by its host name, its state, its details behind ⋯.
    await admin.locator('#tab-rooms').click();
    const card = admin.locator('#instances section.server');
    const host = new URL(baseURL).host;
    await expect(card.locator('h3')).toHaveText(host);
    await expect(card.locator('.server-head .status')).toHaveText('connected');
    await card.getByRole('button', { name: 'More actions on ' + host }).click();
    await admin.getByRole('button', { name: 'App details' }).click();
    await expect(card.locator('.server-details')).toContainText(/App binary: this app's own binary\. Process \d+, protocol 6/);
    // 005-AC42 003b-AC2: the room by its name, a link to it, its link in the tip.
    const roomCard = admin.locator('#rooms-section li.room-row');
    await expect(roomCard.locator('.room-name')).toHaveText(room.name);
    await expect(roomCard.locator('.room-name')).toHaveAttribute('href', roomURL);
    await expect(roomCard.locator('.room-name')).toHaveAttribute('data-tip', roomURL);
    await expect(roomCard.locator('.status')).toHaveText('runs the AI');
    await expect(admin.locator('#accounts li').first()).toContainText('default');
    await expect(admin.locator('#alerts-accounts')).toContainText('Claude account default: the access token expires in');
    await expect(admin.locator('#tab-accounts .tab-badge')).toHaveText('1');

    await admin.locator('#tab-settings').click();
    await admin.getByLabel('Days of history').fill('7');
    await expect(admin.locator('#machine-form .dirty')).toHaveText('1 unsaved change');
    await admin.locator('#machine-form').getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#machine-status')).toHaveText('Saved.');
    await expect(admin.locator('#settings-instance')).toHaveValue(/^[0-9a-f]{12}$/);
    await expect(admin.getByLabel('Sandbox CPUs')).toHaveValue('2');

    await alice.locator('#body').fill('FAKE_CONTEXT_FULL');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(alice.locator('#messages li.msg.agent')).toHaveCount(1, { timeout: 60_000 });

    await admin.locator('#tab-monitoring').click();
    const rows = admin.locator('#turns tbody tr');
    await expect(rows).toHaveCount(1, { timeout: 15_000 });
    for (const text of [room.name, 'Grill', 'ok', '$0.0125']) await expect(rows.first()).toContainText(text);
    // 003b-AC18: the rest is in the answer's sheet.
    await rows.first().click();
    const sheet = admin.locator('#turn-sheet');
    for (const text of ['default', 'pressed by Alice', '85%', roomURL]) await expect(sheet).toContainText(text);
    await sheet.getByRole('button', { name: 'Close' }).click();
    await expect(sheet).toBeHidden();
    await expect(admin.locator('#totals tbody tr').first()).toContainText(/\d{4}-\d{2}-\d{2}default1/);
    await expect(admin.locator('#alerts-rooms')).toContainText(`Grill in ${roomURL} uses 85% of its context`);
    await expect(admin.locator('#tab-rooms .tab-badge')).toHaveText('1');

    await admin.locator('#filter-status').selectOption('ok');
    await expect(rows).toHaveCount(1);
    await admin.locator('#filter-from').fill('2999-01-01');
    await expect(admin.locator('#monitoring-section')).toContainText('No answer matches.');
    await admin.getByRole('button', { name: 'Clear' }).click();
    await expect(rows).toHaveCount(1);

    const [file] = await Promise.all([admin.waitForEvent('download'), admin.getByRole('button', { name: 'Export CSV' }).click()]);
    expect(file.suggestedFilename()).toBe('grill-room-turns.csv');
    const [header, row, ...rest] = readFileSync(await file.path(), 'utf8').trimEnd().split('\n');
    expect(rest).toHaveLength(0);
    const cols = header.split(',');
    const cell = (name) => row.split(',')[cols.indexOf(name)];
    expect([cell('room'), cell('persona'), cell('account'), cell('cause'), cell('status'), cell('cost_usd'), cell('context_pct')])
      .toEqual([roomURL, 'Grill', 'default', 'pressed by Alice', 'ok', '0.0125', '85']);

    // The Activity tab's tables: the room by its name, its link in the tip; no cell clipped by the
    // table's box at any width; the full width of a wide screen.
    const roomCell = rows.first().locator('td[data-label=Room] .cut');
    await expect(roomCell).toHaveText(room.name);
    await expect(roomCell).toHaveAttribute('data-tip', roomURL);
    await roomCell.hover();
    await expect(admin.locator('#person-tip')).toHaveText(roomURL);
    for (const width of [390, 1440, 1920]) {
      await admin.setViewportSize({ width, height: 900 });
      for (const sel of ['#turns', '#totals']) {
        const l = await tableLayout(admin, sel);
        expect(l.clipped, `${sel} at ${width}px`).toEqual([]);
        expect(l.firstInBox, `${sel} at ${width}px`).toBe(true);
        if (width === 1920 && sel === '#turns') {
          expect(l.share).toBeGreaterThanOrEqual(0.9);
          expect(l.fits, 'no sideways scroll at 1920px').toBe(true);
        }
      }
    }
    await expect(admin.locator('#turns [title]')).toHaveCount(0);

    // A phone: every tab reachable, nothing wider than the screen, the tab bar still on screen once
    // scrolled to the bottom.
    await admin.setViewportSize({ width: 390, height: 844 });
    for (const [id, names] of Object.entries(tabs)) {
      await admin.locator('#tab-' + id).click();
      for (const name of names) {
        const h = admin.getByRole('heading', { name, exact: true });
        await h.scrollIntoViewIfNeeded();
        await expect(h).toBeInViewport();
      }
      // Tables and the tab strip scroll inside their own box; anything else past the edge is named.
      const wide = await admin.evaluate(() => [...document.querySelectorAll('body *')]
        .filter((e) => e.offsetParent !== null && !e.parentElement?.closest('.scroll, .tabs') && !['pre', 'select'].includes(e.tagName.toLowerCase()) &&
          (e.getBoundingClientRect().right > innerWidth + 1 || (getComputedStyle(e).overflowX === 'visible' && e.scrollWidth > e.clientWidth + 1)))
        .map((e) => `${e.tagName.toLowerCase()}#${e.id}.${e.className}`));
      expect(wide, id).toEqual([]);
      expect(await admin.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
      await admin.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
      await expect(admin.locator('#tabs')).toBeInViewport();
    }
    await admin.locator('#tab-rooms').click();
    await expect(admin.locator('#alerts-rooms')).toBeInViewport();
    await admin.locator('#tab-settings').click();

    // Limits apply by restarting the worker, which comes back. The page's next status is held until
    // one shows the worker down, as a loaded machine delivers it: the status line must survive. A
    // restart too quick to catch in 3 s lets the status through.
    // The limits reach the daemon but the browser sees the request fail, as Chrome does when the
    // restart's Docker networks come and go (ERR_NETWORK_CHANGED): the page checks they landed.
    let applied = false;
    await admin.route('**/api/limits', async (route) => {
      applied = true;
      await route.fetch();
      return route.abort('failed');
    });
    await admin.route('**/api/status', async (route) => {
      for (let i = 0; applied && i < 300; i++) {
        // Node reuses a kept-alive socket the supervisor may have closed: try again.
        const res = await route.fetch().catch(() => null);
        const json = await res?.json();
        if (json && !json.instances?.every((x) => x.status)) {
          applied = false;
          test.info().annotations.push({ type: 'held', description: 'the reloaded status shows the worker down' });
          return route.fulfill({ response: res, json });
        }
        await new Promise((r) => setTimeout(r, 10)); // polls the worker's restart, bounded by 300 rounds
      }
      applied = false;
      return route.continue();
    });
    // The page may still show the reply's turn running, a status poll behind: saving then asks first.
    await expect(roomCard.locator('.meta')).toHaveCount(0, { timeout: 30_000 });
    await admin.getByLabel('Sandbox memory').fill('2g');
    await admin.locator('#machine-form').getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#machine-status')).toHaveText('Saved: the servers reconnect with the new limits.', { timeout: 10_000 });
    await expect(card.locator('.server-head .status')).toHaveText('connected', { timeout: 30_000 });
    await expect(admin.getByLabel('Sandbox memory')).toHaveValue('2g');
  } finally {
    daemon.cleanup();
    await daemon.exited;
    removeSandbox(daemon.sandbox);
  }
});
