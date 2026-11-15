import { test, expect } from './fixtures.js';
import { roomWith, personaSheet, personaAction, agentState, confirmSheet, noOverflow, uniq } from './helpers.js';

// sourceLine is the Identity tab's line naming where the room persona name came from (035-AC16).
async function sourceLine(context, roomID, name) {
  const page = await context.newPage();
  await page.goto('/r/' + roomID);
  const sheet = await personaSheet(page, name, 'Identity');
  return sheet.locator('section[data-tab=identity] > p.hint').first();
}

// rowPick picks name in the ⋯ menu of by's row titled title in section sec of /profile#personas.
async function rowPick(page, sec, title, by, name) {
  const row = page.locator(`#personas-${sec} li`, { hasText: title }).filter({ hasText: 'by ' + by });
  await row.getByRole('button', { name: 'More actions for ' + title }).click();
  await page.getByRole('menuitem', { name, exact: true }).click();
}

// capture keeps the screen at a phone's and a desktop's width in the test's output (035 phase 2 DoD);
// at the phone's nothing sticks out.
async function capture(page, testInfo, name) {
  for (const [w, h] of [[390, 844], [1440, 900]]) {
    await page.setViewportSize({ width: w, height: h });
    if (w === 390) expect(await noOverflow(page), name).toEqual([]);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${w}.png`) });
  }
}

// 035-AC1 035-AC2 035-AC3 035-AC4 035-AC5 035-AC6 035-AC16: a profile tunes Grill in a room and saves it to
// its personas, finds it under Yours on /profile#personas, reads its page with its history, edits
// it to v2, shares it (exact confirm), another profile sees it under Shared with "Duplicate to
// yours" and no Edit while the room's copy reads "shared by" its author, the author stops sharing
// it (no confirm), adds it to a room and deletes it (exact confirm); the room keeps its copy, which
// then reads that its library persona was deleted.
test('persona library: the profile Personas tab', async ({ browser, request }, testInfo) => {
  // Unique: the server and its libraries are shared by every spec and repeat.
  const me = 'Alice' + uniq();
  const other = 'Bob' + uniq();
  const { room, pages: [alice, bob] } = await roomWith(browser, request, [me, other]);
  const sheet = await personaSheet(alice, 'Grill', 'Identity');
  await sheet.getByLabel('Persona text').fill('Sharper.');
  await sheet.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sheet.locator('#persona-status')).toHaveText('Grill saved. It applies from its next turn.');
  await personaAction(alice, 'Save Grill to my personas');
  await expect(sheet.locator('#persona-status')).toHaveText('Saved Grill v1 to your personas.');

  await alice.goto('/profile#personas');
  await expect(alice.locator('#tab-personas')).toHaveAttribute('aria-current', 'page');
  await expect(alice.locator('#panel-personas h3')).toHaveText(['Yours', 'Shared with everyone on this server', 'Built-in']);
  await expect(alice.locator('#count-personas')).toHaveText('1');
  const mine = alice.locator('#personas-personal li', { hasText: 'Grill' });
  await expect(mine).toHaveCount(1);
  await expect(mine).toContainText('v1 · by ' + me);
  await expect(alice.locator('#personas-builtin li').first()).toContainText('by grill-room');
  await capture(alice, testInfo, 'personas-tab');

  // 035-AC2: its page, every setting read only, and its history.
  const name = mine.getByRole('button', { name: 'Grill', exact: true });
  await name.click();
  const page = alice.locator('#persona-lib-sheet');
  await expect(page.locator('#persona-lib-title')).toHaveText(/Grill$/);
  await expect(page.locator('.persona-lib-read h3')).toHaveText(['Identity', 'Model', 'Triggers', 'Call', 'Capabilities', 'Versions']);
  await expect(page.locator('.persona-lib-read pre')).toHaveText('Sharper.');
  await expect(page.locator('.persona-lib-read')).toContainText('VoiceDefault voice');
  await expect(page.locator('#persona-lib-history li')).toHaveText([new RegExp(`^v1 · by ${me} · `)]);
  await capture(alice, testInfo, 'persona-page');
  // On a phone the page is a full-screen sheet.
  await alice.setViewportSize({ width: 390, height: 844 });
  expect((await page.boundingBox()).width).toBe(390);
  await alice.setViewportSize({ width: 1440, height: 900 });

  // 035-AC3: the author edits it to v2.
  const edit = page.getByRole('button', { name: 'Edit', exact: true });
  await edit.click();
  // 038-AC12: the e2e server has no speech: the call fields say so in their place, as on the room's sheet.
  await expect(page.getByLabel('Speaks in the call')).toHaveCount(0);
  await expect(page.locator('.persona-voice .unavailable')).toHaveText('Not available on this server');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(edit).toBeFocused();
  await edit.click();
  await page.getByLabel('Persona text').fill('Sharpest.');
  await page.getByLabel('Description').fill('Challenges the plan.');
  await page.getByRole('link', { name: 'Triggers', exact: true }).click();
  await page.getByLabel('Speaks up').selectOption('every_n');
  await page.getByLabel('N', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Save version 2', exact: true }).click();
  await expect(page.locator('#persona-lib-status')).toHaveText('Saved Grill v2.');
  await expect(edit).toBeFocused();
  await expect(page.locator('#persona-lib-history li')).toHaveText([new RegExp(`^v2 · by ${me} · `), new RegExp(`^v1 · by ${me} · `)]);
  await expect(page.locator('.persona-lib-read')).toContainText('Every 3 answers');
  // Nothing in the room changed.
  expect((await agentState(alice, room.id)).personas.map((p) => p.text)).toEqual(['Sharper.']);
  // Escape closes the page and gives the focus back to the name it opened from.
  await alice.keyboard.press('Escape');
  await expect(page).toBeHidden();
  await expect(name).toBeFocused();
  await expect(mine).toContainText('v2 · by ' + me);
  await expect(mine).toContainText('Challenges the plan.');

  // 035-AC5, 038-AC15: sharing asks, in these words.
  await rowPick(alice, 'personal', 'Grill', me, 'Share with everyone');
  await expect(alice.locator('dialog.confirm #confirm-text'))
    .toHaveText('Every profile on this server will see Grill and can add it to their rooms. Rooms keep their copies if you stop sharing.');
  await confirmSheet(alice, 'Share');
  const shared = alice.locator('#personas-instance li', { hasText: 'Grill' }).filter({ hasText: 'by ' + me });
  await expect(shared).toHaveCount(1);
  await expect(alice.locator('#personas-personal-empty')).toHaveText('No personas of yours yet. Save one from a room, or duplicate one below.');
  // The tab counts Yours.
  await expect(alice.locator('#count-personas')).toHaveText('0');
  // 035-AC16: the room's copy reads who shares it.
  await expect(await sourceLine(bob.context(), room.id, 'Grill')).toHaveText(`From Grill v1, shared by ${me}.`);

  // 035-AC4: another profile adds or duplicates it, and has no Edit.
  await bob.goto('/profile#personas');
  const theirs = bob.locator('#personas-instance li', { hasText: 'Grill' }).filter({ hasText: 'by ' + me });
  await theirs.getByRole('button', { name: 'More actions for Grill' }).click();
  await expect(bob.locator('#persona-menu-' + (await theirs.getAttribute('data-persona'))).getByRole('menuitem')).toHaveText(['Add to a room…', 'Duplicate to yours']);
  await bob.getByRole('menuitem', { name: 'Duplicate to yours' }).click();
  await expect(bob.locator('#personas p.said')).toHaveText('Duplicated as Grill.');
  await expect(bob.locator('#personas-personal li', { hasText: 'Grill' })).toContainText('v1 · by ' + other);
  await theirs.getByRole('button', { name: 'Grill', exact: true }).click();
  await expect(bob.locator('#persona-lib-sheet .persona-lib-read')).toBeVisible();
  await expect(bob.locator('#persona-lib-edit')).toHaveCount(0);
  await bob.keyboard.press('Escape');

  // Stopping needs no confirm; the other profile no longer sees it.
  await rowPick(alice, 'instance', 'Grill', me, 'Stop sharing');
  await expect(alice.locator('dialog.confirm')).toHaveCount(0);
  await expect(mine).toHaveCount(1);
  await expect(alice.locator('#personas p.said')).toHaveText('Stopped sharing Grill.');
  await expect(alice.locator('#count-personas')).toHaveText('1');
  // A list gone stale says why the action failed and reads the list again.
  await rowPick(bob, 'instance', 'Grill', me, 'Duplicate to yours');
  await expect(bob.locator('#personas p.said')).toHaveText('no such persona');
  await expect(theirs).toHaveCount(0);
  await expect(bob.locator('#personas-personal li', { hasText: 'Grill' })).toHaveCount(1);

  // 035-AC4: Add to a room… adds it whole to one of the profile's rooms.
  await rowPick(alice, 'personal', 'Grill', me, 'Add to a room…');
  const pick = alice.locator('#persona-room-sheet');
  await expect(pick.getByLabel('Room')).toHaveValue(room.id);
  await pick.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(alice.locator('#personas p.said')).toHaveText('Added Grill as Grill-2 to Docs room.');
  await expect.poll(async () => (await agentState(alice, room.id)).personas.map((p) => [p.text, p.every_n || 0]))
    .toEqual([['Sharper.', 0], ['Sharpest.', 3]]);

  // 035-AC6: deleting asks, in these words; the room keeps its copy.
  await rowPick(alice, 'personal', 'Grill', me, 'Delete…');
  await expect(alice.locator('dialog.confirm #confirm-title')).toHaveText('Delete Grill?');
  await expect(alice.locator('dialog.confirm #confirm-text')).toHaveText('Rooms keep the personas they added from it.');
  await confirmSheet(alice, 'Delete');
  await expect(mine).toHaveCount(0);
  await expect(alice.locator('#count-personas')).toHaveText('0');
  expect((await agentState(alice, room.id)).personas).toHaveLength(2);
  await expect(await sourceLine(alice.context(), room.id, 'Grill')).toHaveText('Its library persona was deleted.');
});

// 035-AC3 035-AC4 035-AC5: an edit made on a version another tab replaced is refused and keeps its
// draft, while the page reads the latest, which Reopen starts from; after sharing from the persona's page the focus
// is on its heading; "Add to a room…" for a persona deleted meanwhile says so and reads the list again.
test('persona library: stale edits, focus and gone personas', async ({ browser, request }) => {
  const me = 'Alice' + uniq();
  const { room, pages: [alice] } = await roomWith(browser, request, [me]);
  await personaSheet(alice, 'Grill', 'Identity');
  await personaAction(alice, 'Save Grill to my personas');
  await alice.goto('/profile#personas');
  const mine = alice.locator('#personas-personal li', { hasText: 'Grill' });
  const id = await mine.getAttribute('data-persona');
  await mine.getByRole('button', { name: 'Grill', exact: true }).click();
  const page = alice.locator('#persona-lib-sheet');
  await page.getByRole('button', { name: 'Edit', exact: true }).click();
  // Another tab saves v2 meanwhile.
  const v1 = await alice.evaluate((i) => fetch('/api/profile/personas/' + i).then((r) => r.json()), id);
  expect(await alice.evaluate(([i, v]) => fetch(`/api/profile/personas/${i}/versions`, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...v, version: 1, text: 'From the other tab.' }) }).then((r) => r.status), [id, v1])).toBe(201);
  await page.getByLabel('Persona text').fill('Mine.');
  await page.getByRole('button', { name: 'Save version 2', exact: true }).click();
  // The draft stays, with why, and a deliberate way to the latest version.
  await expect(page.locator('#persona-lib-edit-status')).toHaveText('version 2 exists: reopen the persona to edit it');
  await expect(page.getByLabel('Persona text')).toHaveValue('Mine.');
  const reopen = page.getByRole('button', { name: 'Reopen version 2', exact: true });
  await reopen.click();
  await expect(page.getByLabel('Persona text')).toHaveValue('From the other tab.');
  await expect(page.getByLabel('Name')).toBeFocused();
  await expect(page.getByRole('button', { name: 'Save version 3', exact: true })).toBeVisible();
  await expect(reopen).toHaveCount(0);
  // Cancel shows the latest version read only.
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page.locator('.persona-lib-read pre')).toHaveText('From the other tab.');

  // Shared from its page: the focus lands on the page's heading.
  await page.getByRole('button', { name: 'More actions for Grill' }).click();
  await alice.getByRole('menuitem', { name: 'Share with everyone' }).click();
  await confirmSheet(alice, 'Share');
  await expect(page.locator('#persona-lib-status')).toHaveText('Shared Grill with everyone.');
  await expect(page.locator('#persona-lib-title')).toBeFocused();
  await page.getByRole('button', { name: 'More actions for Grill' }).click();
  await alice.getByRole('menuitem', { name: 'Stop sharing' }).click();
  await expect(page.locator('#persona-lib-status')).toHaveText('Stopped sharing Grill.');
  await expect(page.locator('#persona-lib-title')).toBeFocused();
  await alice.keyboard.press('Escape');

  // Deleted while the room picker is open: the add says so and the list drops it.
  await mine.getByRole('button', { name: 'More actions for Grill' }).click();
  await alice.getByRole('menuitem', { name: 'Add to a room…', exact: true }).click();
  const pick = alice.locator('#persona-room-sheet');
  await expect(pick.getByLabel('Room')).toHaveValue(room.id);
  expect(await alice.evaluate((i) => fetch('/api/profile/personas/' + i, { method: 'DELETE' }).then((r) => r.status), id)).toBe(204);
  await pick.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(pick.getByRole('alert')).toHaveText('no such persona');
  await expect(mine).toHaveCount(0);
});
