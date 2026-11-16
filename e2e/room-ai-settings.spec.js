import { fileURLToPath } from 'node:url';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, appointAdmin, menuItem, openMenu, fakeDaemon, skillsAtTop, uniq, openPersonas, personaRow, personaAction, patchPersona, agentState, confirmSheet, skillZip, unfoldTabs, sheetTab } from './helpers.js';

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
    // Short enough that the room skills start below the fold unless the sheet scrolls to them.
    if (old === 'skills') await alice.setViewportSize({ width: 1280, height: 420 });
    const added = await go(alice, '#manage/' + old);
    await shows(alice, 'ai', tab, '#ai/' + tab);
    expect(await added(), `#manage/${old}: replaced`).toBe(1);
    // #manage/skills opens AI › Personas on the room skills.
    if (old === 'skills') await expect.poll(() => skillsAtTop(alice)).toBe('at top');
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
  const items = await (await openMenu(alice)).locator(':scope > :is(a, button):visible').evaluateAll((els) => els.map((e) => e.textContent.trim()));
  const at = ['Room settings', 'AI in this room', 'Personas', 'Call and voice-to-text…', 'Open documents in a new window'].map((n) => items.indexOf(n));
  expect(at.every((i, k) => i >= 0 && (k === 0 || i > at[k - 1])), items.join(' | ')).toBe(true);
  await alice.keyboard.press('Escape');
  await menuItem(alice, 'Personas');
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await closeSheet(alice, 'ai');

  await menuItem(alice, 'AI in this room');
  await shows(alice, 'ai', 'status', '#ai');
  const sheet = alice.locator('#ai');
  await sheetTab(alice, 'ai', 'Plugins');
  await shows(alice, 'ai', 'plugins', '#ai/plugins');
  await sheetTab(alice, 'ai', 'Websites and connectors');
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
      await sheetTab(alice, 'ai', 'Words to recognise');
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
// the tab shown, or to the bar's menu once the bar folds (040-AC5), never to the page. The six
// tabs fold in the 44rem sheet at any viewport: a sheet widened to the window shows them whole.
test('a Status link hands the focus to the tab it shows', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  // The page's CSP refuses an injected style: only this page bypasses it, Alice's session with it.
  const wide = await (await browser.newContext({ bypassCSP: true, storageState: await alice.context().storageState() })).newPage();
  await wide.goto(alice.url().split('#')[0]);
  await expect(wide.locator('#me')).toHaveText('Alice');
  await wide.addStyleTag({ content: '#ai.sheet { width: 100vw !important; max-width: 100vw !important }' });
  try {
    for (const [page, width, folded] of [[alice, 1280, true], [alice, 390, true], [wide, 1280, false]]) {
      const at = `${width} px${folded ? '' : ', widened'}`;
      await page.setViewportSize({ width, height: 800 });
      await menuItem(page, 'AI in this room');
      await shows(page, 'ai', 'status', '#ai');
      const link = page.locator('#ai-choices').getByRole('link', { name: 'AI in this room › Websites and connectors' });
      await link.focus();
      await page.keyboard.press('Enter');
      await shows(page, 'ai', 'network', '#ai/network');
      expect(await page.locator('#ai nav.tabs').evaluate((n) => 'menu' in n.dataset), at).toBe(folded);
      await expect(page.locator(folded ? '#ai nav.tabs > .tabs-menu' : '#ai #tab-network'), at).toBeFocused();
      await closeSheet(page, 'ai');
    }
  } finally {
    host();
  }
});

// 040-AC7: the Personas tab is the whole Personas sheet, one list of personas and one of room
// skills in the page. A plain member copies a skill in and sees no Remove; a room admin removes it
// there, after the same confirm as Room settings › Skills had. A list redrawn while that confirm is
// open keeps the focus on the Remove pressed, then on Add a persona once the skill went.
test('AI › Personas: one list, room skills with Remove for room admins', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await appointAdmin(alice, request, room.id);
  for (const id of ['#persona-list', '#room-skill-list', '#persona-add', '#chain-form', '#parallel-turns', '#room-skill-copy']) {
    await expect(alice.locator(id), id).toHaveCount(1);
    await expect(alice.locator('#panel-personas ' + id), id).toHaveCount(1);
  }
  await expect(alice.locator('#personas, #manage-personas, #room-admin-skills')).toHaveCount(0);

  const profile = await bob.context().newPage();
  await profile.goto('/profile');
  await unfoldTabs(profile.locator('#profile-tabs'));
  await profile.locator('#tab-skills').click();
  for (const name of ['bobs', 'bobt']) {
    await profile.locator('#panel-skills').getByRole('button', { name: 'Add a skill' }).click();
    await profile.locator('#skill-file').setInputFiles(skillZip(name, 'BOB-v1'));
    await profile.locator('#skill-sheet').getByRole('button', { name: 'Add skill', exact: true }).click();
    await expect(profile.locator('#skill-status')).toHaveText(`Added ${name} v1.`);
  }
  await profile.close();

  await openPersonas(bob);
  const copy = bob.locator('#room-skill-copy');
  await copy.getByLabel('Copy one of your personal skills into the room').selectOption({ label: 'bobs v1' });
  await copy.getByRole('button', { name: 'Copy' }).click();
  await expect(bob.locator('#personas-status')).toHaveText('bobs v1 copied into the room.');
  await expect(bob.locator('#room-skill-list li')).toHaveText(['bobs v1, copied by Bob']);
  await expect(bob.locator('#room-skill-list button')).toHaveCount(0);

  await openPersonas(alice);
  const remove = alice.getByRole('button', { name: 'Remove the room skill bobs' });
  await expect(alice.locator('#room-skill-list li')).toHaveText(['bobs v1, copied by Bob Remove']);
  // Refused: the server's reason in the tab's status line, as an error, the skill kept.
  await alice.route(`**/api/rooms/${room.id}/skills/*`, (r) => (r.request().method() === 'DELETE' ? r.fulfill({ status: 403, json: { error: 'room admins only' } }) : r.continue()));
  await remove.click();
  await confirmSheet(alice, 'Remove');
  await expect(alice.locator('#personas-status')).toHaveText('room admins only');
  await expect(alice.locator('#personas-status')).toHaveClass(/error/);
  await expect(alice.locator('#room-skill-list li')).toHaveText(['bobs v1, copied by Bob Remove']);
  await alice.unrouteAll();
  // Demoted, then appointed again, with the tab open: Remove goes and comes back.
  // Bob first: the room keeps at least one room admin.
  const bobID = await bob.evaluate(() => window.grillRoom.me.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${bobID}`, { headers: admin })).status()).toBe(204);
  const me = await alice.evaluate(() => window.grillRoom.me.id);
  expect((await request.delete(`/api/rooms/${room.id}/admins/${me}`, { headers: admin })).status()).toBe(204);
  await expect(alice.locator('#room-skill-list button')).toHaveCount(0);
  await expect(alice.locator('#room-skill-list li')).toHaveText(['bobs v1, copied by Bob']);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me}`, { headers: admin })).status()).toBe(204);
  await expect(remove).toHaveCount(1);
  await remove.click();
  await expect(alice.locator('dialog.confirm #confirm-title')).toHaveText('Remove the room skill bobs?');
  await expect(alice.locator('dialog.confirm #confirm-text')).toHaveText('Personas that have it on lose it.');
  // Bob copies bobt meanwhile: Alice cancels, back on bobs' redrawn Remove.
  await copy.getByLabel('Copy one of your personal skills into the room').selectOption({ label: 'bobt v1' });
  await copy.getByRole('button', { name: 'Copy' }).click();
  await expect(alice.locator('#room-skill-list li')).toHaveCount(2);
  await alice.locator('dialog.confirm').getByRole('button', { name: 'Cancel' }).click();
  await expect(remove).toBeFocused();
  // Bob, a room admin now, removes bobt while Alice confirms bobs.
  await remove.click();
  await bob.getByRole('button', { name: 'Remove the room skill bobt' }).click();
  await confirmSheet(bob, 'Remove');
  await expect(alice.locator('#room-skill-list li')).toHaveText(['bobs v1, copied by Bob Remove']);
  await confirmSheet(alice, 'Remove');
  for (const page of [alice, bob]) await expect(page.locator('#room-skill-list li')).toHaveText(['None yet.']);
  await expect(alice.locator('#personas-status')).toHaveText('Removed the room skill bobs.');
  await expect(alice.locator('#personas-status')).not.toHaveClass(/error/);
  await expect(alice.locator('#ai')).toBeVisible();
  await expect(alice.locator('#persona-add')).toBeFocused();
});

// 040-AC8: a persona's sheet opened from AI › Personas, by its row or its ⋯ Edit, stacks over it;
// closed by Escape, ✕ or Back it returns to #ai/personas, the focus on that persona's row. Back and
// Forward move between the AI tabs and the persona sheet.
test('AI › Personas: the persona sheet returns to its row', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 1280, height: 800 });
  const sheet = alice.locator('#persona-sheet');
  await openPersonas(alice);
  for (const how of ['Escape', 'close', 'back']) {
    await personaRow(alice, 'Grill').click();
    // The title stays from the last opening: the sheet itself says it opened.
    await expect(sheet).toBeVisible();
    await expect(sheet.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
    await expect(alice.locator('#ai'), how).toBeVisible();
    await expect(alice.locator('dialog[open]')).toHaveCount(2);
    if (how === 'Escape') await alice.keyboard.press('Escape');
    else if (how === 'close') await sheet.getByRole('button', { name: 'Close' }).click();
    else await alice.goBack();
    await expect(sheet, how).toBeHidden();
    await expect(alice.locator('#panel-personas'), how).toBeVisible();
    await expect.poll(() => alice.evaluate(() => location.hash), how).toBe('#ai/personas');
    await expect(personaRow(alice, 'Grill'), how).toBeFocused();
  }
  // From the row's ⋯ Edit, whose menu item is gone once the sheet closes.
  await alice.locator('#persona-list').getByRole('button', { name: 'More actions for Grill' }).click();
  await alice.getByRole('button', { name: 'Edit Grill', exact: true }).click();
  await expect(sheet).toBeVisible();
  await expect(sheet.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
  await alice.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  await expect(personaRow(alice, 'Grill')).toBeFocused();

  // 040-AC6: closed from the Personas tab, the AI sheet gives the focus back to its opener, the room's name.
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#ai')).toBeHidden();
  await expect(alice.locator('#room-open')).toBeFocused();

  // Back and Forward: Status, Personas, the persona sheet, and back down again.
  await menuItem(alice, 'AI in this room');
  await shows(alice, 'ai', 'status', '#ai');
  await sheetTab(alice, 'ai', 'Personas');
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await personaRow(alice, 'Grill').click();
  await expect(sheet).toBeVisible();
  await alice.goBack();
  await expect(sheet).toBeHidden();
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await alice.goBack();
  await shows(alice, 'ai', 'status', '#ai');
  await alice.goForward();
  await shows(alice, 'ai', 'personas', '#ai/personas');
  // Forward reopens it with the focus on the bar, folded at any viewport (040-AC5): closing still lands on the row.
  await expect(alice.locator('#ai nav.tabs')).toHaveAttribute('data-menu');
  await alice.locator('#ai nav.tabs > .tabs-menu').focus();
  await expect(alice.locator('#ai nav.tabs > .tabs-menu')).toBeFocused();
  await alice.goForward();
  await expect(sheet).toBeVisible();
  await expect(sheet.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
  await expect(alice.locator('#ai')).toBeVisible();
  await alice.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await expect(personaRow(alice, 'Grill')).toBeFocused();
});

// 040-AC8 035-AC17: the picker cancelled gives the focus back to Add a persona; a persona removed
// while its row holds the focus, by another session or from its own sheet, hands it to Add a
// persona. A live change from another session redraws the list, the focus kept. 040-AC7: the tab
// opened again reads the library afresh.
test('AI › Personas: picker, removal and a live change keep the focus', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await openPersonas(alice);
  const add = alice.locator('#persona-add');
  for (const how of ['Cancel', 'Escape']) {
    await add.click();
    await expect(alice.locator('#persona-picker')).toBeVisible();
    if (how === 'Cancel') await alice.locator('#persona-picker').getByRole('button', { name: 'Cancel' }).click();
    else await alice.keyboard.press('Escape');
    await expect(alice.locator('#persona-picker'), how).toBeHidden();
    await expect(alice.locator('#ai'), how).toBeVisible();
    await expect(add, how).toBeFocused();
  }

  // A library persona published while the sheet was closed: the tab opened again reads it, before
  // the picker's own read.
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#ai')).toBeHidden();
  const title = 'Fresh ' + uniq();
  const published = await alice.request.post(`/api/rooms/${room.id}/templates`, {
    data: { title, name: 'Fresh', badge: '🌱', colour: '#2f855a', text: 'Fresh eyes.', model: 'sonnet', effort: 'medium' },
  });
  expect(published.status()).toBe(201);
  await openPersonas(alice);
  await expect.poll(() => alice.evaluate((t) => templates.some((x) => x.title === t), title)).toBe(true);
  await add.click();
  await expect(alice.locator('#persona-picker').getByRole('group', { name: 'Yours' }).getByRole('radio', { name: new RegExp(`^${title}, v1`) })).toHaveCount(1);
  await alice.locator('#persona-picker').getByRole('button', { name: 'Cancel' }).click();
  await expect(add).toBeFocused();

  // Bob adds a persona and renames Grill: the list follows, the focus stays on Grill's row. The
  // picker's close event, a task late, gives the focus back to Add a persona first.
  await alice.evaluate(() => new Promise((done) => setTimeout(done)));
  await personaRow(alice, 'Grill').focus();
  const tpls = await bob.evaluate((id) => fetch(`/api/rooms/${id}/templates`).then((r) => r.json()), room.id);
  const sec = tpls.find((t) => t.name === 'Secretary');
  expect(await bob.evaluate(([id, t]) => fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ template_id: t }) }).then((r) => r.status), [room.id, sec.id])).toBe(201);
  const grill = (await agentState(bob, room.id)).personas.find((p) => p.name === 'Grill');
  expect(await patchPersona(bob, room.id, grill.id, { name: 'Griller' })).toBe(200);
  await expect(alice.locator('#persona-list .pr-name')).toHaveText(['Griller', 'Secretary']);
  await expect(personaRow(alice, 'Griller')).toBeFocused();

  // Removed by Bob while its row holds the focus.
  expect(await bob.evaluate(([id, p]) => fetch(`/api/rooms/${id}/personas/${p}`, { method: 'DELETE' }).then((r) => r.status), [room.id, grill.id])).toBe(204);
  await expect(alice.locator('#persona-list .pr-name')).toHaveText(['Secretary']);
  await expect(add).toBeFocused();

  // Removed from its own sheet's ⋯.
  await personaRow(alice, 'Secretary').click();
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  await personaAction(alice, 'Remove Secretary');
  await confirmSheet(alice, 'Remove');
  await expect(alice.locator('#persona-sheet')).toBeHidden();
  await expect(alice.locator('#persona-list .persona-row')).toHaveCount(0);
  await expect(add).toBeFocused();
  await expect.poll(() => alice.evaluate(() => location.hash)).toBe('#ai/personas');
});

// 040-AC8: on a phone AI in this room and the persona sheet over it fill the screen.
test('AI › Personas: full screen on a phone', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const phone = { width: 390, height: 844 };
  await alice.setViewportSize(phone);
  await openPersonas(alice);
  const fills = (sel) => alice.locator(sel).evaluate((e) => {
    const r = e.getBoundingClientRect();
    return [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height), e.scrollWidth <= e.clientWidth];
  });
  expect(await fills('#ai')).toEqual([0, 0, phone.width, phone.height, true]);
  await personaRow(alice, 'Grill').click();
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  expect(await fills('#persona-sheet')).toEqual([0, 0, phone.width, phone.height, true]);
  await alice.keyboard.press('Escape');
  await expect(personaRow(alice, 'Grill')).toBeFocused();
});

// 040-AC8: Back while the picker is open over AI › Personas takes the picker with the tab: to
// another tab, or out of the sheet.
test('AI › Personas: Back closes the picker with the tab', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const picker = alice.locator('#persona-picker');
  await menuItem(alice, 'AI in this room');
  await shows(alice, 'ai', 'status', '#ai');
  await sheetTab(alice, 'ai', 'Personas');
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await alice.locator('#persona-add').click();
  await expect(picker).toBeVisible();
  await alice.goBack();
  await shows(alice, 'ai', 'status', '#ai');
  await expect(picker).toBeHidden();
  await expect(alice.locator('dialog[open]')).toHaveCount(1);

  await alice.goForward();
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await alice.locator('#persona-add').click();
  await expect(picker).toBeVisible();
  await alice.goBack();
  await alice.goBack();
  await expect.poll(() => alice.evaluate(() => location.hash)).toBe('');
  await expect(picker).toBeHidden();
  await expect(alice.locator('#ai')).toBeHidden();
  await expect(alice.locator('dialog[open]')).toHaveCount(0);
});

// 040-AC2: #manage/skills followed while AI › Personas already shows scrolls it at once, and the ask
// does not outlive the visit: a later redraw, or the next visit, leaves the scroll alone.
test('AI › Personas: #manage/skills scrolls once, on its visit', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await alice.setViewportSize({ width: 1280, height: 420 });
  // The tab's own reads done first: their redraw would scroll for a missing one.
  await Promise.all([alice.waitForResponse((r) => r.url().endsWith(`/api/rooms/${room.id}/agent`)), openPersonas(alice)]);
  await alice.evaluate(() => new Promise((done) => setTimeout(done, 200)));
  const top = () => alice.locator('#ai').evaluate((d) => d.scrollTop);
  expect(await top()).toBe(0);
  await go(alice, '#manage/skills');
  await expect.poll(() => alice.evaluate(() => location.hash)).toBe('#ai/personas');
  await expect.poll(() => skillsAtTop(alice), { timeout: 1000 }).toBe('at top');
  expect(await top()).toBeGreaterThan(0);
  await alice.locator('#ai').evaluate((d) => { d.scrollTop = 0; });
  const grill = (await agentState(bob, room.id)).personas[0];
  expect(await patchPersona(bob, room.id, grill.id, { name: 'Griller' })).toBe(200);
  await expect(personaRow(alice, 'Griller')).toHaveCount(1);
  expect(await top()).toBe(0);

  // Asked before the personas are known, then left for another tab: the next visit starts at the top.
  let release = () => {};
  const held = new Promise((done) => { release = done; });
  await alice.route((u) => u.pathname === `/api/rooms/${room.id}/events`, async (r) => {
    await held;
    await r.continue();
  });
  const url = alice.url().split('#')[0];
  await alice.goto('about:blank');
  await alice.goto(url + '#manage/skills');
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await go(alice, '#ai/plugins');
  await shows(alice, 'ai', 'plugins', '#ai/plugins');
  release();
  await expect.poll(() => alice.evaluate(() => agentSeen)).toBe(true);
  await go(alice, '#ai/personas');
  await shows(alice, 'ai', 'personas', '#ai/personas');
  await expect(personaRow(alice, 'Griller')).toHaveCount(1);
  expect(await top()).toBe(0);
});

// 040-AC8: a persona sheet reached by Back from an AI tab, then closed, stays in the room on that
// tab: from a sheet the persona bar opened, and from a deep link the page loaded on.
test('AI › Personas: a persona sheet reached by Back closes onto the AI tab', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const url = alice.url().split('#')[0];
  const grill = (await agentState(alice, room.id)).personas[0].id;
  const sheet = alice.locator('#persona-sheet');
  for (const how of ['pushed', 'deep link']) {
    if (how === 'pushed') await go(alice, `#personas/${grill}/capabilities`);
    else {
      await alice.goto('about:blank');
      await alice.goto(`${url}#personas/${grill}/capabilities`);
    }
    await expect(sheet, how).toBeVisible();
    await sheet.getByRole('link', { name: 'AI in this room › Plugins' }).click();
    await shows(alice, 'ai', 'plugins', '#ai/plugins');
    await expect(sheet, how).toBeHidden();
    await alice.goBack();
    await expect(sheet, how).toBeVisible();
    await expect(alice.locator('#ai'), how).toBeVisible();
    await alice.keyboard.press('Escape');
    await expect(sheet, how).toBeHidden();
    await shows(alice, 'ai', 'plugins', '#ai/plugins');
    expect(alice.url(), how).toBe(url + '#ai/plugins');
    await closeSheet(alice, 'ai');
  }
});

// 040-AC8: the persona sheet's close event comes a task late; a Back that opened the sheet again
// meanwhile keeps it open, on its own hash.
test('AI › Personas: a late close event leaves a reopened persona sheet alone', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const grill = (await agentState(alice, room.id)).personas[0].id;
  await openPersonas(alice);
  await personaRow(alice, 'Grill').click();
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  const state = await alice.evaluate(async (hash) => {
    const d = document.getElementById('persona-sheet');
    /** @type {Event | null} */
    let held = null;
    // Held back on its way: the page's listeners see it only once the sheet opened again.
    addEventListener('close', (e) => { if (e.target === d && !held) { held = e; e.stopImmediatePropagation(); } }, { capture: true });
    location.hash = '#ai/personas';
    await new Promise((r) => setTimeout(r, 100));
    location.hash = hash;
    await new Promise((r) => setTimeout(r, 100));
    d.dispatchEvent(new Event('close'));
    await new Promise((r) => setTimeout(r, 100));
    return [!!held, d.open, location.hash];
  }, `#personas/${grill}/identity`);
  expect(state).toEqual([true, true, `#personas/${grill}/identity`]);
  await expect(alice.locator('#ai')).toBeVisible();
});
