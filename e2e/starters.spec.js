import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, confirmSheet, smallTargets, noOverflow, rowAction } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// starterAction picks name in the ⋯ menu of the starter row titled title.
async function starterAction(page, list, title, name) {
  const row = page.locator(`#starters-${list} li`, { hasText: title });
  await row.getByRole('button', { name: 'More actions for ' + title }).click();
  await page.getByRole('menuitem', { name }).click();
}

async function audited(request, action, target) {
  const log = await (await request.get(`/api/admin/audit?action=${action}&target=${encodeURIComponent(target)}`, { headers: admin })).json();
  return log.entries;
}

// 006-AC1 006-AC2 006-AC3 006-AC5: a profile duplicates a built-in into Yours, which another
// profile does not see, shares it with everyone, and the super-admin removes it from /admin
// Libraries; publishing and removing are in the audit log.
test('starter library: duplicate, share, delete, /admin', async ({ browser, request }) => {
  // Unique names: repeated runs share the server and its library.
  const me = 'Alice' + Date.now().toString(36);
  const { pages: [alice, bob] } = await roomWith(browser, request, [me, 'Bob']);
  await alice.goto('/profile#starters');
  await expect(alice.locator('#starters-builtin li')).toHaveCount(4);
  await expect(alice.locator('#starters-builtin li').first()).toContainText('Spec grill');
  await expect(alice.locator('#starters-builtin li').first()).toContainText('by grill-room');

  await starterAction(alice, 'builtin', 'Retro', 'Duplicate to yours');
  const mine = alice.locator('#starters-personal li', { hasText: 'Retro' });
  await expect(mine).toContainText('v1 · by ' + me);
  // Its row menu's keyboard: Escape closes it and gives the focus back to ⋯.
  const more = mine.getByRole('button', { name: 'More actions for Retro' });
  await more.click();
  await expect(alice.getByRole('menuitem', { name: 'Duplicate to yours' })).toBeFocused();
  await alice.keyboard.press('ArrowDown');
  await expect(alice.getByRole('menuitem', { name: 'Edit' })).toBeFocused();
  await alice.keyboard.press('Escape');
  await expect(more).toBeFocused();

  await bob.goto('/profile#starters');
  await expect(bob.locator('#starters-builtin li')).toHaveCount(4);
  await expect(bob.locator('#starters-personal')).toHaveCount(0);
  await expect(bob.locator('#starters-instance li', { hasText: me })).toHaveCount(0);

  await starterAction(alice, 'personal', 'Retro', 'Share with everyone');
  await confirmSheet(alice, 'Share');
  await expect(alice.locator('#starters-instance li', { hasText: me })).toContainText('Retro');
  await bob.reload();
  const shared = bob.locator('#starters-instance li', { hasText: me });
  await expect(shared).toContainText('Retro');
  // Not his to share or delete: his menu only duplicates.
  await shared.getByRole('button', { name: 'More actions for Retro' }).click();
  await expect(bob.getByRole('menuitem')).toHaveText(['Duplicate to yours']);
  await bob.keyboard.press('Escape');

  const page = await (await browser.newContext()).newPage();
  expect(await signIn(page, adminToken, 'libraries')).toBe(201);
  const row = page.locator('#starters .list-row', { hasText: 'Retro' }).filter({ hasText: me });
  await expect(row).toHaveCount(1);
  await expect(row.locator('.cell').nth(1)).toHaveText(/1$/);
  await rowAction(row, 'Remove the starter Retro');
  await confirmSheet(page, 'Remove');
  await expect(row).toHaveCount(0);
  const removed = await audited(request, 'starter.delete', 'Retro');
  expect(removed[0].actor).toBe('super-admin');
  expect(removed[0].details.version).toBe(1);
  const published = await audited(request, 'starter.publish', 'Retro');
  expect(published[0].actor).toMatch(/^profile:/);

  // A personal one its owner deletes from the profile page.
  await starterAction(alice, 'builtin', 'Doc review', 'Duplicate to yours');
  await starterAction(alice, 'personal', 'Doc review', 'Delete');
  await confirmSheet(alice, 'Delete');
  await expect(alice.locator('#starters-personal')).toHaveCount(0);
  await expect(alice.locator('#starters-instance li', { hasText: me })).toHaveCount(0);
});

const widths = [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }];
// Each built-in with the field it asks, and what the room then holds.
const builtins = [
  { title: 'Spec grill', search: 'SPEC GRILL', field: 'Topic', value: 'Billing', room: 'Grill: Billing', personas: ['Grill', 'Secretary', 'Rodin'] },
  { title: 'Retro', search: 'retro', field: 'Team', value: 'Ops', room: 'Retro: Ops', personas: ['Facilitator', 'Secretary'] },
  { title: 'Doc review', search: 'doc review', field: 'Document', value: 'RFC 12', room: 'Review: RFC 12', personas: ['Reviewer', 'Secretary'] },
  { title: 'Incident', search: 'incident', field: 'Incident', value: 'API down', room: 'Incident: API down', personas: ['Incident-lead', 'Secretary'] },
];

for (const vp of widths) {
  // 006-AC8 006-AC9 006-AC10 006-AC11 006-AC14: "New room" opens the picker, with a search and a
  // preview; every built-in and Blank lead to the editor, where a required field keeps "Create
  // room" disabled with the reason; the room opens with the starter's personas, its agenda first
  // in the documents, and Room settings names the starter.
  test(`new room from each built-in and Blank, ${vp.name}`, async ({ browser, request }) => {
    test.setTimeout(90_000);
    const me = 'Ann' + Date.now().toString(36);
    const { pages: [page] } = await roomWith(browser, request, [me]);
    await page.setViewportSize(vp);
    for (const b of builtins) {
      await page.goto('/profile');
      await page.getByRole('button', { name: 'New room' }).click();
      const sheet = page.locator('#room-create-sheet');
      await expect(sheet.locator('#starter-preview')).toContainText('Blank');
      await sheet.getByLabel('Search starters').fill(b.search);
      await expect(sheet.locator('#pick-builtin li')).toHaveCount(1);
      await sheet.getByRole('button', { name: 'Built-in: ' + b.title }).click();
      const preview = sheet.locator('#starter-preview');
      await expect(preview.locator('h3')).toHaveText(b.title);
      await expect(preview).toContainText('v1 · by grill-room');
      await expect(preview.locator('.starter-personas li')).toHaveText([new RegExp(`${b.personas[0]} · sonnet · effort (high|medium) · only when asked`),
        /Secretary · sonnet · effort medium · every 20 voice lines/, ...b.personas.slice(2).map((n) => new RegExp(`${n} · sonnet · effort high · only when asked`))]);
      await expect(preview.locator('.starter-agenda')).toContainText('- [ ]');
      await sheet.getByRole('button', { name: 'Use this starter' }).click();
      const create = sheet.getByRole('button', { name: 'Create room' });
      await expect(create).toBeDisabled();
      await expect(sheet.locator('#starter-missing')).toHaveText(`Fill in ${b.field} to create the room.`);
      await expect(sheet.getByLabel('Your name in the room')).toHaveValue(me);
      if (vp.width < 400) {
        expect(await smallTargets(page), b.title).toEqual([]);
        expect(await noOverflow(page)).toEqual([]);
      }
      await sheet.getByLabel(b.field, { exact: true }).fill(b.value);
      await expect(create).toBeEnabled();
      await create.click();
      await expect(page.locator('#room-name')).toHaveText(b.room);
      await page.locator('#invites').getByRole('button', { name: 'Close' }).click();
      await expect(page.locator('button.persona')).toHaveText(b.personas.map((n) => new RegExp(n)));
      if (vp.width > 400) await expect(page.locator('#documents li').first()).toContainText('agenda.md');
      await page.goto(page.url().split('?')[0] + '#manage');
      await expect(page.locator('#created-from')).toHaveText(`Created from the starter ${b.title} v1.`);
    }
    // Blank: Grill alone, no agenda, no starter named.
    await page.goto('/profile');
    await page.getByRole('button', { name: 'New room' }).click();
    await page.getByRole('button', { name: 'Use this starter' }).click();
    await page.getByLabel('Room name').fill('Plain ' + vp.name);
    await page.getByRole('button', { name: 'Create room' }).click();
    await expect(page.locator('#room-name')).toHaveText('Plain ' + vp.name);
    await page.locator('#invites').getByRole('button', { name: 'Close' }).click();
    await expect(page.locator('button.persona')).toHaveText([/Grill/]);
    await page.goto(page.url().split('?')[0] + '#manage');
    await expect(page.locator('#room-panel')).toBeVisible();
    await expect(page.locator('#created-from')).toHaveCount(0);
  });
}

// 006-AC15 006-AC16 006-AC17 006-AC3: a room admin saves a room as a starter from Room settings,
// picking a document; an undeclared {{x}} in the agenda or the room name blocks the save, naming it,
// and a field used nowhere only warns. Shared, the starter makes a room with the same personas,
// agenda and document, the field filled in.
test('save a room as a starter, share it, create a room from it', async ({ browser, request }) => {
  test.setTimeout(90_000);
  const me = 'Sam' + Date.now().toString(36);
  const title = 'Kickoff ' + me;
  const { pages: [page] } = await roomWith(browser, request, [me]);
  await page.goto('/profile');
  await page.getByRole('button', { name: 'New room' }).click();
  await page.getByRole('button', { name: 'Built-in: Spec grill' }).click();
  await page.getByRole('button', { name: 'Use this starter' }).click();
  await page.getByLabel('Topic').fill('Search');
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-name')).toHaveText('Grill: Search');
  await page.locator('#invites').getByRole('button', { name: 'Close' }).click();
  await page.locator('#upload-input').setInputFiles({ name: 'plan.md', mimeType: 'text/markdown', buffer: Buffer.from('# Plan\n') });
  await expect(page.locator('#documents li', { hasText: 'plan.md' })).toHaveCount(1);
  await page.goto(page.url().split('?')[0].split('#')[0] + '#manage');
  await page.getByRole('button', { name: 'Save as starter' }).click();
  const sheet = page.locator('#starter-save-sheet');
  await expect(sheet.getByLabel('Title')).toHaveValue('Grill: Search');
  await expect(sheet.locator('#starter-docs input[type=checkbox]')).not.toBeChecked();
  await sheet.getByLabel('Title').fill(title);
  await sheet.locator('#starter-docs label', { hasText: 'plan.md' }).click();
  const agenda = sheet.getByLabel('Agenda (agenda.md)');
  await expect(agenda).toHaveValue(/grill of Search/);
  await agenda.fill((await agenda.inputValue()) + '\n- [ ] Owner: {{owner}}');
  const save = sheet.getByRole('button', { name: 'Save as starter' });
  await expect(sheet.locator('#starter-undeclared')).toHaveText('{{owner}} is no field: add it to the fields, or remove it.');
  await expect(save).toBeDisabled();
  await sheet.getByRole('button', { name: '+ Add a field' }).click();
  await sheet.locator('.starter-field').last().getByLabel('Label').fill('Owner');
  await expect(sheet.locator('.starter-field').last()).toContainText('{{owner}}');
  await expect(sheet.locator('#starter-undeclared')).toHaveCount(0);
  await sheet.locator('.starter-field').last().getByLabel('Required').check();
  // The room name too: an undeclared key blocks, a declared one passes.
  await sheet.getByLabel('Default room name').fill('Kickoff {{team}}');
  await expect(sheet.locator('#starter-undeclared')).toHaveText('{{team}} is no field: add it to the fields, or remove it.');
  await sheet.getByLabel('Default room name').fill('Kickoff for ');
  await sheet.getByRole('button', { name: 'Insert Owner' }).click();
  await expect(sheet.locator('#starter-undeclared')).toHaveCount(0);
  await sheet.getByRole('button', { name: '+ Add a field' }).click();
  await sheet.locator('.starter-field').last().getByLabel('Label').fill('Unused');
  await expect(sheet.locator('#starter-unused')).toHaveText('Not used in the agenda or the room name: Unused.');
  await expect(save).toBeEnabled();
  await sheet.getByRole('button', { name: 'Remove the field Unused' }).click();
  await sheet.getByLabel('Default room name').fill('Kickoff for {{owner}}');
  await save.click();
  await expect(sheet).toBeHidden();
  await expect(page.locator('.toast')).toContainText('to your starters');

  await page.goto('/profile#starters');
  const mine = page.locator('#starters-personal li', { hasText: title });
  await expect(mine).toContainText('v1 · by ' + me);
  await mine.getByRole('button', { name: 'More actions for ' + title }).click();
  await page.getByRole('menuitem', { name: 'Share with everyone' }).click();
  await confirmSheet(page, 'Share');
  await expect(page.locator('#starters-instance li', { hasText: title })).toHaveCount(1);

  await page.locator('#tab-rooms').click();
  await page.getByRole('button', { name: 'New room' }).click();
  await page.getByLabel('Search starters').fill(me);
  await page.getByRole('button', { name: 'Shared with everyone: ' + title }).click();
  await expect(page.locator('#starter-preview .starter-personas li')).toHaveText([/Grill/, /Secretary/, /Rodin/]);
  await page.getByRole('button', { name: 'Use this starter' }).click();
  await expect(page.getByRole('button', { name: 'Create room' })).toBeDisabled();
  await page.getByLabel('Owner').fill('Bea');
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-name')).toHaveText('Kickoff for Bea');
  await page.locator('#invites').getByRole('button', { name: 'Close' }).click();
  await expect(page.locator('button.persona')).toHaveText([/Grill/, /Secretary/, /Rodin/]);
  await expect(page.locator('#documents li')).toHaveCount(2);
  await expect(page.locator('#documents li').first()).toContainText('agenda.md');
  await expect(page.locator('#documents li').nth(1)).toContainText('plan.md');
});
