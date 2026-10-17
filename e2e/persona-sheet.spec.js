import { test, expect } from './fixtures.js';
import { roomWith, menuItem, personaRow, personaSheet, agentState, unfoldTabs } from './helpers.js';
import { adminToken } from './playwright.config.js';

const sizes = { phone: { width: 390, height: 844 }, desktop: { width: 1440, height: 900 } };
const tabs = ['identity', 'model', 'triggers', 'capabilities'];

// addSecretary adds the Secretary from the library; the Personas list stays open.
async function addSecretary(page) {
  await menuItem(page, 'Personas');
  await page.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
  await page.locator('#personas').getByRole('button', { name: 'Add', exact: true }).click();
  await expect(personaRow(page, 'Secretary')).toHaveCount(1);
}

for (const [size, viewport] of Object.entries(sizes)) {
  // 009-AC1: one row per persona on one line, the name cut by an ellipsis, then the model and
  // effort, the trigger and the state in words.
  test(`personas list: one-line rows at ${viewport.width} px`, async ({ browser, request }) => {
    const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(viewport);
    await addSecretary(alice);
    const sec = await personaSheet(alice, 'Secretary', 'Identity');
    await sec.getByLabel('Name').fill('Secretary_with_a_long_name_xx');
    await unfoldTabs(sec.locator('#persona-tabs'));
    await sec.getByRole('link', { name: 'Triggers', exact: true }).click();
    await sec.getByLabel('Speaks up').selectOption('every_voice');
    await sec.getByLabel('N', { exact: true }).fill('4');
    await sec.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(sec.locator('#persona-status')).toHaveText('Secretary_with_a_long_name_xx saved. It applies from its next turn.');
    await sec.getByRole('button', { name: 'Close' }).click();

    await expect(personaRow(alice, 'Grill')).toHaveText('🔥Grillsonnet · effort medium · on mention onlyoffline');
    const long = personaRow(alice, 'Secretary_with_a_long_name_xx');
    await expect(long.locator('.pr-meta')).toHaveText('sonnet · effort medium · every 4 voice lines');
    await expect(long.locator('.pr-state')).toHaveText('offline');
    const list = await alice.locator('#persona-list').boundingBox();
    for (const row of await alice.locator('#persona-list .persona-row').all()) {
      const box = await row.boundingBox();
      expect(box.height).toBeLessThanOrEqual(46);
      expect(box.x + box.width).toBeLessThanOrEqual(list.x + list.width + 0.5);
      // Every part sits on the row's one line.
      for (const part of await row.locator('span:visible').all()) {
        const b = await part.boundingBox();
        expect(Math.abs(b.y + b.height / 2 - (box.y + box.height / 2))).toBeLessThanOrEqual(2);
        expect(b.x + b.width).toBeLessThanOrEqual(box.x + box.width + 0.5);
      }
    }
    await expect(long.locator('.pr-name')).toHaveCSS('text-overflow', 'ellipsis');
    // The name gives way; the model, effort and trigger are never cut.
    for (const meta of await alice.locator('#persona-list .pr-meta').all()) {
      expect(await meta.evaluate((e) => e.scrollWidth <= e.clientWidth), await meta.innerText()).toBe(true);
    }
    if (size === 'phone') {
      expect(await long.locator('.pr-name').evaluate((e) => e.scrollWidth > e.clientWidth)).toBe(true);
      await expect(long.locator('.pr-meta')).toHaveJSProperty('innerText', 'sonnet · medium · every 4 voice lines');
    }
  });

  // 009-AC2: each tab is a deep link, followed on a hash change and on a fresh load; on a phone the
  // tab bar shows its current tab whole, or folds into a menu that names it.
  test(`persona sheet: a deep link per tab at ${viewport.width} px`, async ({ browser, request }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(viewport);
    const id = (await agentState(alice, room.id)).personas[0].id;
    const sheet = alice.locator('#persona-sheet');
    const shows = async (tab) => {
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
      await expect(sheet.locator('#persona-tabs [aria-current=page]')).toHaveAttribute('id', 'persona-tab-' + tab);
      for (const t of tabs) await expect(sheet.locator(`section[data-tab=${t}]`)).toBeVisible({ visible: t === tab });
    };
    for (const tab of tabs) {
      await alice.evaluate((h) => { location.hash = h; }, `#personas/${id}/${tab}`);
      await shows(tab);
    }
    await alice.reload();
    await shows('capabilities');
    if (size === 'phone') {
      // Full screen under 40rem, like Room settings.
      expect(await sheet.boundingBox()).toEqual({ x: 0, y: 0, ...viewport });
      const nav = sheet.locator('#persona-tabs');
      await expect.poll(() => nav.evaluate((e) => {
        if ('menu' in e.dataset) return e.querySelector('.tabs-menu').textContent;
        const r = e.querySelector('#persona-tab-capabilities').getBoundingClientRect();
        return r.right <= e.getBoundingClientRect().right + 0.5 ? 'Capabilities' : 'cut';
      })).toBe('Capabilities');
    }
    // An unknown tab is Identity; a persona the room does not have closes the sheet and its link.
    await alice.evaluate((h) => { location.hash = h; }, `#personas/${id}/nope`);
    await shows('identity');
    await alice.evaluate(() => { location.hash = '#personas/999999/model'; });
    await expect(sheet).toBeHidden();
    await expect(alice).not.toHaveURL(/#personas/);
    // Closing the sheet drops its link.
    await alice.evaluate((h) => { location.hash = h; }, `#personas/${id}/model`);
    await shows('model');
    await sheet.getByRole('button', { name: 'Close' }).click();
    await expect(alice).not.toHaveURL(/#/);
  });

  // 009-AC2, 009-AC3, 008-AC8: with the keyboard alone, a row opens its sheet, ⋯ opens its menu on
  // the first item, arrows move, Escape gives the focus back to ⋯, and each tab link opens its tab.
  test(`persona sheet: keyboard only at ${viewport.width} px`, async ({ browser, request }) => {
    const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(viewport);
    await menuItem(alice, 'Personas');
    await personaRow(alice, 'Grill').focus();
    await alice.keyboard.press('Enter');
    const sheet = alice.locator('#persona-sheet');
    await expect(sheet.locator('section[data-tab=identity]')).toBeVisible();
    const more = sheet.locator('#persona-more');
    await expect(more).toBeFocused();
    await alice.keyboard.press('Enter');
    const menu = alice.locator('#persona-menu');
    await expect(menu.getByRole('button', { name: 'Save Grill to the library' })).toBeFocused();
    await alice.keyboard.press('ArrowDown');
    await expect(menu.getByRole('button', { name: 'Remove Grill' })).toBeFocused();
    await alice.keyboard.press('ArrowDown');
    await expect(menu.getByRole('button', { name: 'Save Grill to the library' })).toBeFocused();
    await alice.keyboard.press('Escape');
    await expect(menu).toBeHidden();
    await expect(sheet).toBeVisible();
    await expect(more).toBeFocused();
    await alice.keyboard.press('Tab');
    await expect(sheet.getByRole('button', { name: 'Close' })).toBeFocused();
    // A bar folded on a narrow phone: Tab reaches its button, Enter opens the list, Tab walks it,
    // and a picked tab gives the focus back to the button.
    const button = sheet.locator('#persona-tabs .tabs-menu');
    const folded = await button.isVisible();
    if (folded) {
      await alice.keyboard.press('Tab');
      await expect(button).toBeFocused();
    }
    for (const [i, tab] of tabs.entries()) {
      if (folded) await alice.keyboard.press('Enter');
      for (let k = folded ? 0 : i; k <= i; k++) await alice.keyboard.press('Tab');
      await expect(sheet.locator('#persona-tab-' + tab)).toBeFocused();
      await alice.keyboard.press('Enter');
      await expect(alice).toHaveURL(new RegExp(`#personas/\\d+/${tab}$`));
      await expect(sheet.locator(`section[data-tab=${tab}]`)).toBeVisible();
      await expect(folded ? button : sheet.locator('#persona-tab-' + tab)).toBeFocused();
    }
    // Enter on ⋯'s Remove asks first (005-AC39).
    await more.focus();
    await alice.keyboard.press('Enter');
    await expect(menu.getByRole('button', { name: 'Save Grill to the library' })).toBeFocused();
    await alice.keyboard.press('ArrowDown');
    await expect(menu.getByRole('button', { name: 'Remove Grill' })).toBeFocused();
    await alice.keyboard.press('Enter');
    await expect(alice.locator('dialog.confirm h2')).toHaveText('Remove Grill?');
    await alice.keyboard.press('Escape');
    await expect(alice.locator('button.persona')).toHaveCount(1);
  });
}

// 009-AC3: Save sends every tab at once, and an invalid field on a hidden tab opens that tab.
test('persona sheet: one Save for every tab', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const sheet = await personaSheet(alice, 'Grill', 'Identity');
  await sheet.getByLabel('Name').fill('');
  await sheet.getByRole('link', { name: 'Model', exact: true }).click();
  await sheet.getByLabel('Effort').selectOption('high');
  await sheet.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sheet.locator('section[data-tab=identity]')).toBeVisible();
  await expect(alice).toHaveURL(/#personas\/\d+\/identity$/);
  await expect(sheet.getByLabel('Name')).toBeFocused();
  await sheet.getByLabel('Name').fill('Grilly');
  await sheet.getByRole('link', { name: 'Triggers', exact: true }).click();
  await sheet.getByLabel('Can follow up on its own', { exact: false }).check();
  await sheet.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sheet.locator('#persona-status')).toHaveText('Grilly saved. It applies from its next turn.');
  await expect(bob.locator('#messages')).toContainText('Alice changed 🔥 Grill: name Grill → Grilly, effort medium → high, self-call off → on.');
});

// 009-AC4: every control of the 005 Personas sheet is still there. The lists come from 005's
// room.html #personas and room.js personaForm and skillsBlock, read before 009 replaced them: the
// sheet's element ids, each persona form's field names, and its buttons and select by accessible
// name. The agent state is played to the page so the host's and the update-only controls show.
test('persona sheet: every 005 control is reachable', async ({ browser, request }) => {
  const old005 = {
    ids: ['personas-close', 'personas-status', 'persona-list', 'chain-form', 'chain-budget', 'parallel-turns', 'persona-template',
      'persona-add', 'room-skills', 'room-skill-list', 'room-skill-copy', 'room-skill-source'],
    fields: ['name', 'badge', 'colour', 'model', 'effort', 'text', 'trigger', 'every', 'autocompact', 'self_call'],
    sheetButtons: ['Save', 'Turn on the chosen skill for Grill', 'Turn off skill stamp for Grill', 'Update skill stamp of Grill to version 2'],
    menuButtons: ['Update Grill to template version 2', 'Save Grill to the library', 'Start fresh for Grill', 'Start fresh with a summary for Grill',
      'Remove Grill'],
    selects: ['Skill to turn on for Grill'],
  };
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const st = await agentState(alice, room.id);
  const me = await alice.evaluate(() => window.grillRoom.me.id);
  Object.assign(st, { host_participant_id: me, online: true });
  Object.assign(st.personas[0], { update_version: 2, every_n: 3,
    skills: [{ skill_id: 99, name: 'stamp', version: 1, scope: 'personal', update_version: 2 }] });
  const lib = { skills: [{ id: 99, name: 'stamp', version: 2, scope: 'personal' }, { id: 98, name: 'other', version: 1, scope: 'shared' }],
    disclaimer: false, no_profile: false };
  const json = (body) => (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
  await alice.route(`**/api/rooms/${room.id}/agent`, json(st));
  await alice.route(`**/api/rooms/${room.id}/skills`, json(lib));
  await alice.route(`**/api/rooms/${room.id}/events*`, (r) => r.fulfill({
    status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: `event: agent\ndata: ${JSON.stringify(st)}\n\n`,
  }));
  await alice.reload();

  await menuItem(alice, 'Personas');
  const list = alice.locator('#personas');
  for (const id of old005.ids) await expect(list.locator('#' + id), id).toHaveCount(1);
  await expect(list.locator('#room-skill-copy')).toBeVisible();
  // 009-AC1: online and idle reads "ready".
  await expect(personaRow(alice, 'Grill').locator('.pr-state')).toHaveText('ready');

  const sheet = await personaSheet(alice, 'Grill', 'Identity');
  const seen = new Set();
  for (const tab of ['Identity', 'Model', 'Triggers', 'Capabilities']) {
    await sheet.getByRole('link', { name: tab, exact: true }).click();
    await expect(sheet.locator(`section[data-tab=${tab.toLowerCase()}]`)).toBeVisible();
    // One snapshot of the shown fields: a locator list re-resolves item by item.
    const names = await sheet.locator('[name]').evaluateAll((els) => els.filter((e) => e.checkVisibility()).map((e) => e.getAttribute('name')));
    for (const n of names) seen.add(n);
    if (tab === 'Capabilities') {
      for (const name of old005.sheetButtons) await expect(sheet.getByRole('button', { name, exact: true }), name).toBeVisible();
      for (const name of old005.selects) await expect(sheet.getByRole('combobox', { name, exact: true }), name).toBeVisible();
    }
  }
  expect(old005.fields.filter((f) => !seen.has(f))).toEqual([]);
  await sheet.locator('#persona-more').click();
  for (const name of old005.menuButtons) await expect(alice.locator('#persona-menu').getByRole('button', { name, exact: true }), name).toBeVisible();
});

// 009-AC2: the sheet owns one history entry, whatever tabs were opened: Back after closing stays
// closed. Opened from Room settings › Personas, closing goes back there.
test('persona sheet: history and the way back to Room settings', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await menuItem(alice, 'Personas');
  const sheet = await personaSheet(alice, 'Grill', 'Model');
  await sheet.getByRole('link', { name: 'Triggers', exact: true }).click();
  await expect(alice).toHaveURL(/#personas\/\d+\/triggers$/);
  await sheet.getByRole('button', { name: 'Close' }).click();
  await expect(alice).not.toHaveURL(/#/);
  await alice.goBack();
  await expect(alice.locator('#persona-sheet')).toBeHidden();

  const me = await alice.evaluate(() => window.grillRoom.me.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me}`, { headers: { Authorization: `Bearer ${adminToken}` } })).status()).toBe(204);
  await alice.reload();
  await alice.evaluate(() => { location.hash = '#manage/personas'; });
  await alice.locator('#manage-personas').getByRole('link', { name: '🔥 Grill' }).click();
  await expect(alice.locator('#manage')).toBeHidden();
  await expect(sheet.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
  await sheet.getByRole('button', { name: 'Close' }).click();
  await expect(alice.locator('#manage')).toBeVisible();
  await expect(alice).toHaveURL(/#manage\/personas$/);
  await expect(alice.locator('#manage-personas')).toBeVisible();
});
