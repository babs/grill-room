import { fileURLToPath } from 'node:url';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, appointAdmin, menuItem, fakeDaemon } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const settingsTabs = [['general', 'General'], ['admins', 'Members'], ['invites', 'Invites'], ['links', 'Links'], ['documents', 'Deleted documents'], ['export', 'Export']];
const aiTabs = [['status', 'Status'], ['personas', 'Personas'], ['plugins', 'Plugins'], ['network', 'Websites and connectors'],
  ['glossary', 'Words to recognise'], ['voices', 'Call voices']];

// tabsOf reads a bar's tabs, ids and labels in order, folded or not.
const tabsOf = (nav) => nav.locator(':scope > a').evaluateAll((as) => as.map((a) => [a.id.replace(/^tab-/, ''), a.textContent.trim()]));

// go sets the hash as a click on a link would, and returns how many entries that added to the history.
async function go(page, hash) {
  const before = await page.evaluate(() => history.length);
  await page.evaluate((h) => { location.hash = h; }, hash);
  return async () => (await page.evaluate(() => history.length)) - before;
}

// shows waits for sheet on tab, the hash saying so.
async function shows(page, sheet, tab, hash) {
  await expect(page.locator('#' + sheet)).toBeVisible();
  await expect(page.locator(`#${sheet} #tab-${tab}`)).toHaveAttribute('aria-current', 'page');
  await expect(page.locator(`#${sheet} #panel-${tab}`)).toBeVisible();
  await expect.poll(() => page.evaluate(() => location.hash)).toBe(hash);
}

// closeSheet closes it by Escape and waits for its close event, a task later, to drop the hash: the
// same hash set before that fires no hashchange.
async function closeSheet(page, sheet) {
  await page.keyboard.press('Escape');
  await expect(page.locator('#' + sheet)).toBeHidden();
  await expect.poll(() => page.evaluate(() => location.hash)).toBe('');
}

// 040-AC1 040-AC2 040-AC3: both tab sets of a room admin, in order; every old link lands on its
// sheet and tab, replacing the history entry, the Room settings sheet never opening for a moved tab.
test('room admin: two sheets, the old links land on their new place', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  await alice.evaluate(() => {
    window.manageOpened = 0;
    const d = document.getElementById('manage');
    new MutationObserver(() => { if (d.open) window.manageOpened++; }).observe(d, { attributes: true, attributeFilter: ['open'] });
  });
  const moved = [['personas', 'personas'], ['plugins', 'plugins'], ['network', 'network'], ['glossary', 'glossary'], ['voices', 'voices'], ['skills', 'personas']];
  for (const [old, tab] of moved) {
    const added = await go(alice, '#manage/' + old);
    await shows(alice, 'ai', tab, '#ai/' + tab);
    expect(await added(), `#manage/${old}: replaced`).toBe(1);
    // #manage/skills opens AI › Personas on the room skills.
    if (old === 'skills') await expect(alice.locator('#ai-skills')).toBeInViewport();
    await closeSheet(alice, 'ai');
  }
  expect(await alice.evaluate(() => window.manageOpened), 'Room settings opened for a moved tab').toBe(0);
  for (const [hash, tab] of [['#links', 'links'], ['#manage/admins', 'admins'], ['#manage/documents', 'documents']]) {
    const added = await go(alice, hash);
    await shows(alice, 'manage', tab, hash);
    expect(await added(), hash).toBe(1);
    await closeSheet(alice, 'manage');
  }
  await go(alice, '#manage');
  await shows(alice, 'manage', 'general', '#manage');
  expect(await tabsOf(alice.locator('#manage nav.tabs'))).toEqual(settingsTabs);
  await closeSheet(alice, 'manage');
  await go(alice, '#ai');
  await shows(alice, 'ai', 'status', '#ai');
  expect(await tabsOf(alice.locator('#ai nav.tabs'))).toEqual(aiTabs);
  await closeSheet(alice, 'ai');
  // A tab that does not exist opens the sheet's first.
  const added = await go(alice, '#ai/nope');
  await shows(alice, 'ai', 'status', '#ai/status');
  expect(await added()).toBe(1);
});

// 040-AC3 040-AC2: a plain member's tabs; a link to a tab they lack opens the sheet's first tab
// they have, the hash saying so.
test('plain member: their tabs, and the first one for a tab they lack', async ({ browser, request }) => {
  const { pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await go(bob, '#manage/invites');
  await shows(bob, 'manage', 'invites', '#manage/invites');
  expect(await tabsOf(bob.locator('#manage nav.tabs'))).toEqual(settingsTabs.filter(([id]) => ['invites', 'links'].includes(id)));
  await closeSheet(bob, 'manage');
  await go(bob, '#ai/plugins');
  await shows(bob, 'ai', 'plugins', '#ai/plugins');
  expect(await tabsOf(bob.locator('#ai nav.tabs'))).toEqual(aiTabs.filter(([id]) => id !== 'voices'));
  await closeSheet(bob, 'ai');
  for (const [hash, sheet, tab] of [['#manage/general', 'manage', 'invites'], ['#ai/voices', 'ai', 'status'], ['#manage/voices', 'ai', 'status']]) {
    const added = await go(bob, hash);
    await shows(bob, sheet, tab, `#${sheet}/${tab}`);
    expect(await added(), hash).toBe(1);
    await expect(bob.locator('#ai #panel-voices')).toHaveCount(0);
    await closeSheet(bob, sheet);
  }
  // On load too.
  const url = bob.url().split('#')[0];
  await bob.goto('about:blank');
  await bob.goto(url + '#ai/voices');
  await shows(bob, 'ai', 'status', '#ai/status');
});

// 040-AC1 040-AC6: the room menu's order; Personas opens AI › Personas; Back and Forward move
// between the AI tabs with the sheet open; Escape, ✕ and a click outside give the focus back to
// the room's name, as 039 does, and drop the hash.
test('AI in this room: menu, tabs in the history, focus on close', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 1280, height: 800 });
  await alice.locator('#room-open').click();
  const items = await alice.locator('#room-menu > :is(a, button):visible').evaluateAll((els) => els.map((e) => e.textContent.trim()));
  const at = ['Room settings', 'AI in this room', 'Personas', 'Call and voice-to-text…', 'Open documents in a new window'].map((n) => items.indexOf(n));
  expect(at.every((i, k) => i >= 0 && (k === 0 || i > at[k - 1])), items.join(' | ')).toBe(true);
  await alice.keyboard.press('Escape');
  await menuItem(alice, 'Personas');
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await closeSheet(alice, 'ai');

  await menuItem(alice, 'AI in this room');
  await shows(alice, 'ai', 'status', '#ai');
  const sheet = alice.locator('#ai');
  await sheet.getByRole('link', { name: 'Plugins', exact: true }).click();
  await shows(alice, 'ai', 'plugins', '#ai/plugins');
  await sheet.getByRole('link', { name: 'Websites and connectors', exact: true }).click();
  await shows(alice, 'ai', 'network', '#ai/network');
  await alice.goBack();
  await shows(alice, 'ai', 'plugins', '#ai/plugins');
  await alice.goBack();
  await shows(alice, 'ai', 'status', '#ai');
  await alice.goForward();
  await shows(alice, 'ai', 'plugins', '#ai/plugins');

  const opener = alice.locator('#room-open');
  for (const how of ['Escape', 'close', 'outside']) {
    if (!(await sheet.isVisible())) {
      await menuItem(alice, 'AI in this room');
      await sheet.getByRole('link', { name: 'Words to recognise', exact: true }).click();
      await shows(alice, 'ai', 'glossary', '#ai/glossary');
    }
    if (how === 'Escape') await alice.keyboard.press('Escape');
    else if (how === 'close') await sheet.locator('.sheet-head .close').click();
    else await alice.mouse.click(2, 798);
    await expect(sheet, how).toBeHidden();
    await expect(opener, how).toBeFocused();
    expect(await alice.evaluate(() => location.hash), how).toBe('');
  }
});

// 040-AC2: in an ended room #ai/<tab> opens nothing and the hash goes, as #ai does (039).
test('ended room: no AI sheet, no hash', async ({ browser, request }) => {
  const { room, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  const id = await bob.evaluate((r) => fetch(`/api/rooms/${r}`).then((x) => x.json()).then((d) => d.me.id), room.id);
  expect((await request.delete(`/api/admin/rooms/${room.id}/participants/${id}`, { headers: admin, data: { block: false } })).status()).toBe(204);
  await expect(bob.locator('#banner')).toHaveText('Your access to this room has ended.');
  for (const hash of ['#ai/plugins', '#ai', '#manage/plugins', '#manage/skills']) {
    await bob.evaluate((h) => { location.hash = h; }, hash);
    await expect.poll(() => bob.evaluate(() => location.hash), hash).toBe('');
    await expect(bob.locator('#ai'), hash).toBeHidden();
    // No tab mounts in the closed sheet either: a plugins list would fetch for nothing.
    await expect(bob.locator('#ai-settings > *'), hash).toHaveCount(0);
  }
});

// 040-AC3 040-AC6: room.js alone says which AI tab shows. A plain member's #ai/voices shows Status;
// promoted while there, they get the Call voices tab in the bar, and Status keeps showing alone.
test('plain member on Status, promoted: no Call voices under Status', async ({ browser, request }) => {
  const { room, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  // Loaded there: the redirect waits for the room to say who Bob is, which comes after the page's
  // modules have read the hash.
  const url = bob.url().split('#')[0];
  await bob.route((u) => u.pathname === `/api/rooms/${room.id}`, async (r) => {
    await new Promise((done) => setTimeout(done, 1000));
    await r.continue();
  });
  await bob.goto('about:blank');
  await bob.goto(url + '#ai/voices');
  await shows(bob, 'ai', 'status', '#ai/status');
  const id = await bob.evaluate((r) => fetch(`/api/rooms/${r}`).then((x) => x.json()).then((d) => d.me.id), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${id}`, { headers: admin })).status()).toBe(204);
  await expect(bob.locator('#ai #tab-voices')).toHaveCount(1);
  await expect(bob.locator('#ai #tab-status')).toHaveAttribute('aria-current', 'page');
  await expect(bob.locator('#ai #panel-status')).toBeVisible();
  await expect(bob.locator('#ai #panel-voices')).toHaveCount(0);
  await expect(bob.locator('#ai-settings > *')).toHaveCount(0);
});

// 040-AC2 040-AC3: a deep link followed before the room has said who the viewer is waits for it: a
// room admin's #ai/voices, set while the first room answer is still being read, opens Call voices.
test('room admin: #ai/voices before the role is known', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  // Holds the page's first read of the room answer, after its status is known, until the test lets it go.
  await alice.addInitScript((id) => {
    const json = Response.prototype.json;
    let held = false;
    Response.prototype.json = function () {
      if (held || !this.url.endsWith('/api/rooms/' + id)) return json.call(this);
      held = true;
      return new Promise((done) => { window.releaseRoom = done; }).then(() => json.call(this));
    };
  }, room.id);
  const url = alice.url().split('#')[0];
  await alice.goto('about:blank');
  await alice.goto(url);
  await expect.poll(() => alice.evaluate(() => typeof window.releaseRoom)).toBe('function');
  await alice.evaluate(() => { location.hash = '#ai/voices'; });
  // Still held: the sheet is open, the role unknown, the hash kept.
  await expect(alice.locator('#ai')).toBeVisible();
  await expect(alice.locator('#ai #tab-voices')).toHaveCount(0);
  expect(await alice.evaluate(() => [window.grillRoom, location.hash])).toEqual([undefined, '#ai/voices']);
  await alice.evaluate(() => window.releaseRoom());
  await shows(alice, 'ai', 'voices', '#ai/voices');
});

// 040-AC6: a room reload between the sheet's close and its close event, which comes a task late
// with the hash still there, does not open the sheet again.
test('a room reload while the AI sheet closes keeps it closed', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await go(alice, '#ai/plugins');
  await shows(alice, 'ai', 'plugins', '#ai/plugins');
  const reopened = await alice.evaluate(async () => {
    const d = document.getElementById('ai');
    /** @type {Event | null} */
    let held = null;
    // Held back on its way: the room's own listeners see it only once the reload ran.
    addEventListener('close', (e) => { if (e.target === d && !held) { held = e; e.stopImmediatePropagation(); } }, { capture: true });
    d.close();
    await new Promise((r) => setTimeout(r, 100));
    await loadRoom();
    const open = d.open;
    d.dispatchEvent(new Event('close'));
    return [!!held, open];
  });
  expect(reopened).toEqual([true, false]);
  await expect(alice.locator('#ai')).toBeHidden();
  await expect.poll(() => alice.evaluate(() => location.hash)).toBe('');
  await expect(alice.locator('#ai-settings > *')).toHaveCount(0);
});

// 040-AC5: each sheet's bar, as a room admin, at 390, 640, 820 and 1440 px, at the default text
// size and at XL (028's XL and the browser's text at 20 px): every tab whole inside the bar, or the
// bar folded into its menu.
test('both tab bars fold whenever they overflow', async ({ browser, request }) => {
  test.setTimeout(120_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const shots = fileURLToPath(new URL('./test-results/040-captures/', import.meta.url));
  const url = alice.url().split('#')[0];
  for (const xl of [false, true]) {
    for (const width of [390, 640, 820, 1440]) {
      await alice.setViewportSize({ width, height: 900 });
      await alice.goto('about:blank');
      await alice.goto(url);
      await expect(alice.locator('#me')).toHaveText('Alice');
      if (xl) {
        // 028's XL is read on load; the browser's text size is the root's.
        await alice.evaluate(() => localStorage.setItem('grill-room-text-size', JSON.stringify({ chat: 'xl', transcript: 'xl' })));
        await alice.reload();
        await expect(alice.locator('#me')).toHaveText('Alice');
        await alice.evaluate(() => { document.documentElement.style.fontSize = '20px'; });
      }
      for (const [sheet, tab] of [['manage', 'general'], ['ai', 'status']]) {
        await alice.evaluate((h) => { location.hash = h; }, `#${sheet}/${tab}`);
        const nav = alice.locator(`#${sheet} nav.tabs`);
        await expect(nav.locator('#tab-' + tab)).toHaveAttribute('aria-current', 'page');
        const name = `${sheet} at ${width} px${xl ? ', XL' : ''}`;
        await expect.poll(() => nav.evaluate((n) => {
          if ('menu' in n.dataset) {
            const b = n.querySelector('.tabs-menu').getBoundingClientRect();
            const box = n.getBoundingClientRect();
            return b.width > 0 && b.left >= box.left - 0.5 && b.right <= box.right + 0.5 ? 'folded' : 'menu cut';
          }
          const box = n.getBoundingClientRect();
          const cut = [...n.querySelectorAll(':scope > a')].filter((a) => {
            const r = a.getBoundingClientRect();
            return r.left < box.left - 0.5 || r.right > box.right + 0.5;
          }).map((a) => a.textContent);
          return cut.length || n.scrollWidth > n.clientWidth + 0.5 ? 'cut: ' + cut.join(', ') : 'whole';
        }), name).toMatch(/^(folded|whole)$/);
        await alice.screenshot({ path: `${shots}${sheet}-${width}${xl ? '-xl' : ''}.png` });
        await closeSheet(alice, sheet);
      }
    }
  }
});

// errorLines lists the AI sheet's error and status lines that say something.
const errorLines = (page) => page.locator('#ai .said').evaluateAll((els) => els.map((e) => e.textContent.trim()).filter(Boolean));

// 040-AC6: an error stays with its tab and its opening: another tab, or the sheet opened again on
// the same tab, starts without it. An empty status line takes no room above a moved tab.
test('AI tabs: an error goes with its tab and its opening', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    await alice.route((u) => u.pathname === `/api/rooms/${room.id}/egress-delegation`, (r) => r.fulfill({ status: 500, json: { error: 'egress down' } }));
    const fail = async () => {
      await go(alice, '#ai/network');
      await shows(alice, 'ai', 'network', '#ai/network');
      // Not check(): the box goes back unticked on the error.
      await alice.locator('#egress-admins').click();
      await expect.poll(() => errorLines(alice)).toEqual(['egress down']);
      await expect(alice.locator('#ai [role=alert]', { hasText: 'egress down' })).toHaveCSS('color', await alice.evaluate(() => {
        const i = document.body.appendChild(Object.assign(document.createElement('i'), { style: 'color: var(--danger)' }));
        const c = getComputedStyle(i).color;
        i.remove();
        return c;
      }));
    };
    await fail();
    await go(alice, '#ai/plugins');
    await shows(alice, 'ai', 'plugins', '#ai/plugins');
    expect(await errorLines(alice)).toEqual([]);
    const bands = await alice.locator('#ai .said:empty').evaluateAll((els) => els.map((e) => e.getBoundingClientRect().height));
    expect(bands.length).toBeGreaterThan(0);
    expect(bands.every((h) => h === 0), `empty status lines ${bands}`).toBe(true);
    await closeSheet(alice, 'ai');

    await fail();
    await closeSheet(alice, 'ai');
    await go(alice, '#ai/network');
    await shows(alice, 'ai', 'network', '#ai/network');
    expect(await errorLines(alice)).toEqual([]);
  } finally {
    host();
  }
});

// 040-AC6: Status's own link to another tab hides the panel that held the focus: the focus goes to
// the tab shown, or to the folded bar's menu on a phone, never to the page.
test('a Status link hands the focus to the tab it shows', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    for (const [width, to] of [[1280, '#ai #tab-network'], [390, '#ai nav.tabs > .tabs-menu']]) {
      await alice.setViewportSize({ width, height: 800 });
      await menuItem(alice, 'AI in this room');
      await shows(alice, 'ai', 'status', '#ai');
      const link = alice.locator('#ai-choices').getByRole('link', { name: 'AI in this room › Websites and connectors' });
      await link.focus();
      await alice.keyboard.press('Enter');
      await shows(alice, 'ai', 'network', '#ai/network');
      await expect(alice.locator(to), `${width} px`).toBeFocused();
      await closeSheet(alice, 'ai');
    }
  } finally {
    host();
  }
});
