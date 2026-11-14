import { test, expect } from './fixtures.js';
import { roomWith, personaSheet, agentState, uniq } from './helpers.js';

// 035-AC12 035-AC13 035-AC16 035-AC18: a member tunes Grill and saves it to their personas: the ⋯
// item names the next version, the status line links to /profile#personas, the Identity tab names
// the source, and a second save makes v2 of the same library persona. With unsaved edits the item
// says to save first, and does nothing. Another member sees the source by its author, and no update
// from a library persona they cannot see. Once that member saves it to theirs, the first member's
// next save still names the next version of their own.
test('persona library: save to my personas from a room', async ({ browser, request }) => {
  // Unique: the server and its libraries are shared by every spec and repeat.
  const me = 'Alice' + uniq();
  const { room, pages: [alice, bob] } = await roomWith(browser, request, [me, 'Bob']);
  const sheet = await personaSheet(alice, 'Grill', 'Identity');
  const source = sheet.locator('section[data-tab=identity] > p.hint').first();
  await expect(source).toHaveText(/^From Grill v\d+, built in\.$/);
  const menu = alice.locator('#persona-menu');

  await sheet.getByLabel('Persona text').fill('Sharper.');
  await sheet.getByRole('link', { name: 'Triggers', exact: true }).click();
  await sheet.getByLabel('Speaks up').selectOption('every_n');
  await sheet.getByLabel('N', { exact: true }).fill('3');
  await sheet.locator('#persona-more').click();
  const first = menu.getByRole('button', { name: 'Save your changes first', exact: true });
  await expect(first).toBeDisabled();
  await expect(first).toHaveText('Save your changes first');
  // Reachable by keyboard: the menu opens on the reason.
  await expect(first).toBeFocused();
  await alice.keyboard.press('Enter');
  await expect(menu).toBeHidden();
  await expect(sheet.locator('#persona-status')).toHaveText('');
  await expect(sheet.getByRole('button', { name: 'Save', exact: true })).toBeEnabled();
  await sheet.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sheet.locator('#persona-status')).toHaveText('Grill saved. It applies from its next turn.');

  await sheet.locator('#persona-more').click();
  const save = menu.getByRole('button', { name: 'Save Grill to my personas', exact: true });
  await expect(save).toHaveText('Save to my personas');
  await save.click();
  await expect(sheet.locator('#persona-status')).toHaveText('Saved Grill v1 to your personas.');
  await expect(sheet.locator('#persona-status a')).toHaveAttribute('href', '/profile#personas');
  await sheet.getByRole('link', { name: 'Identity', exact: true }).click();
  await expect(source).toHaveText('From Grill v1, in your personas.');

  await sheet.locator('#persona-more').click();
  const again = menu.getByRole('button', { name: 'Save Grill to my personas as v2', exact: true });
  await expect(again).toHaveText('Save to my personas as v2');
  await again.click();
  await expect(sheet.locator('#persona-status')).toHaveText('Saved Grill v2 to your personas.');
  const mine = (await (await alice.request.get(`/api/rooms/${room.id}/templates`)).json()).filter((t) => t.section === 'personal');
  expect(mine.map((t) => [t.title, t.version, t.mine, t.text, t.every_n])).toEqual([['Grill', 2, true, 'Sharper.', 3]]);

  // A newer version Bob cannot see: offered to Alice only (035-AC18).
  const t = mine[0];
  const res = await alice.request.post(`/api/rooms/${room.id}/templates`, {
    data: { template_id: t.id, name: t.name, badge: t.badge, colour: t.colour, text: 'Sharpest.', model: t.model, effort: t.effort },
  });
  expect(res.status()).toBe(201);
  await bob.reload();
  expect((await agentState(bob, room.id)).personas[0].update_version).toBe(3);
  const bobSheet = await personaSheet(bob, 'Grill', 'Identity');
  // The library loaded: an update hidden before it would prove nothing.
  await expect(bob.locator('#persona-template option')).not.toHaveCount(0);
  await expect(bobSheet.locator('section[data-tab=identity] > p.hint').first()).toHaveText(`From Grill v2, by ${me}.`);
  await bobSheet.locator('#persona-more').click();
  await expect(bob.locator('#persona-menu').getByRole('button')).toHaveText(['Save to my personas', 'Remove']);

  // The sheet's deep link reopens it on Identity.
  await alice.reload();
  await expect(source).toHaveText('From Grill v2, in your personas.');
  await sheet.locator('#persona-more').click();
  await expect(menu.getByRole('button').first()).toHaveText('Update to v3');
  await alice.keyboard.press('Escape');

  // Bob's save links the room persona to his; Alice's save goes on with her own (035-AC13).
  await bob.locator('#persona-menu').getByRole('button', { name: 'Save Grill to my personas', exact: true }).click();
  await expect(bobSheet.locator('#persona-status')).toHaveText('Saved Grill v1 to your personas.');
  await expect(source).toHaveText('From Grill v1, by Bob.');
  await sheet.locator('#persona-more').click();
  await expect(menu.getByRole('button')).toHaveText(['Save to my personas as v4', 'Remove']);
});
