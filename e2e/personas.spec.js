import { test, expect } from './fixtures.js';
import { roomWith, startDaemon, pairDaemon, menuItem, openStrip, personaSheet, closePersonas, appointAdmin, agentState, patchPersona } from './helpers.js';

const led = (page, name) => page.locator(`button.persona[data-name="${name}"] .led`);

// 002-AC13 002-AC14 002-AC15 002-AC17 002-AC18 002-AC42: Alice adds the Secretary from the library
// and edits it in the personas panel, each change a line in the chat; a press while Grill works is
// queued with its position and can be cancelled; once Grill is done the queued Secretary runs by
// itself with its edited text and effort, and each synthesis carries its persona's badge, model
// and effort. Real daemon and Docker sandbox, fake claude.
test('personas: add, edit, queue', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await expect(alice.locator('button.persona')).toHaveCount(1);
  await expect(alice.locator('button.persona[data-name="Grill"]')).toContainText('🔥');

  const panel = alice.locator('#personas');
  await menuItem(alice, 'Personas');
  await expect(panel.locator('.persona-row')).toHaveCount(1);
  await expect(panel.locator('#persona-template option')).toHaveCount(4);
  await panel.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
  await panel.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(panel.locator('.persona-row')).toHaveCount(2);
  for (const p of [alice, bob]) {
    await expect(p.locator('#messages')).toContainText('Alice added the persona 📝 Secretary, from the template "Secretary" v1.');
    await expect(p.locator('button.persona')).toHaveText([/^🔥Grill/, /^📝Secretary/]);
  }
  // One Save sends every tab (009-AC3): an effort on Model and a text on Identity.
  const sec = await personaSheet(alice, 'Secretary', 'Model');
  await sec.getByLabel('Effort').selectOption('low');
  await sec.getByRole('link', { name: 'Identity', exact: true }).click();
  await sec.getByLabel('Persona text').fill('You are Scribe, who writes it all down.\nNothing else.');
  await sec.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sec.locator('#persona-status')).toHaveText('Secretary saved. It applies from its next turn.');
  await expect(bob.locator('#messages')).toContainText('Alice changed 📝 Secretary: effort medium → low, persona text.');
  // A bad name is refused by the room, and says why.
  await sec.getByLabel('Name').fill('two words');
  await sec.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(sec.locator('#persona-status')).toHaveText('name must be 1 to 30 letters, digits, - or _');
  await closePersonas(alice);

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await expect(led(alice, 'Secretary')).toHaveAttribute('aria-label', 'Secretary ready');
    await alice.locator('#body').fill('FAKE_SLOW');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(led(bob, 'Grill')).toHaveAttribute('aria-label', 'Grill answering');
    await alice.getByRole('button', { name: 'Ask Secretary' }).click();
    for (const p of [alice, bob]) {
      await expect(led(p, 'Secretary')).toHaveAttribute('aria-label', 'Secretary waiting, position 1');
      await expect(p.locator('#queue-list li')).toHaveText([/^📝 Secretary, pressed by Alice/]);
    }
    await expect(bob.getByRole('button', { name: 'Cancel the queued turn of Secretary' })).toHaveCount(0);
    // Grill's answer runs: the strip shows it first, the queue behind "+1 more" (005-AC38).
    await openStrip(alice);
    await alice.getByRole('button', { name: 'Cancel the queued turn of Secretary' }).click();
    await expect(alice.locator('#queue')).toBeHidden();
    await alice.locator('#body').fill('FAKE_WHOAMI');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Secretary' }).click();
    await expect(alice.locator('#queue-list li')).toHaveCount(1);
    await expect(led(alice, 'Grill')).toHaveAttribute('aria-label', 'Grill answering');

    const grill = alice.locator('#messages li.msg.agent', { hasText: 'Slow turn done.' });
    await expect(grill.locator('.meta strong')).toHaveText('🔥 Grill', { timeout: 60_000 });
    const scribe = alice.locator('#messages li.msg.agent', { hasText: 'You are Scribe' });
    await expect(scribe.locator('.body')).toHaveText('You are Scribe, who writes it all down. (effort low)', { timeout: 60_000 });
    await expect(scribe.locator('.meta strong')).toHaveText('📝 Secretary');
    await expect(scribe.locator('.model')).toHaveText('sonnet');
    await expect(scribe.locator('.effort')).toHaveText('effort low');
    await expect(alice.locator('#queue')).toBeHidden();
    await expect(led(alice, 'Secretary')).toHaveAttribute('aria-label', 'Secretary ready');
  } finally {
    daemon.cleanup();
  }
});

// 032-AC5 032-AC6: the library's Timekeeper, added to a room whose Grill keeps time, takes the role
// with its room settings, all said in the chat; the Secretary added next brings none.
test('personas: the Timekeeper from the library', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const grill = (await agentState(alice, room.id)).personas[0];
  expect(await patchPersona(alice, room.id, grill.id, { time_keeper: true })).toBe(200);
  const panel = alice.locator('#personas');
  await menuItem(alice, 'Personas');
  await panel.locator('#persona-template').selectOption({ label: '⏱️ Timekeeper v1' });
  await panel.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(alice.locator('#messages')).toContainText('Alice added the persona ⏱️ Timekeeper, from the template "Timekeeper" v1, every N minutes off → 5, '
    + 'now keeps time (was 🔥 Grill), speak over after off → 20 s.');
  await expect(alice.locator('button.persona')).toHaveText([/^🔥Grill/, /^⏱️Timekeeper/]);
  await panel.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
  await panel.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(alice.locator('#messages')).toContainText('Alice added the persona 📝 Secretary, from the template "Secretary" v1.');
  const ps = (await agentState(alice, room.id)).personas;
  expect(ps.map((p) => [p.name, !!p.time_keeper, p.cut_in_s || 0, p.every_minutes || 0, p.speaks]))
    .toEqual([['Grill', false, 0, 0, true], ['Timekeeper', true, 20, 5, true], ['Secretary', false, 0, 0, true]]);
});

// 032-AC3: the time keeper's turn with an empty answer posts nothing, and its pill says "nothing to
// say" until its next turn. Real daemon and Docker sandbox, fake claude.
test('personas: a silent time keeper turn', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const grill = (await agentState(alice, room.id)).personas[0];
  expect(await patchPersona(alice, room.id, grill.id, { time_keeper: true })).toBe(200);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await expect(led(alice, 'Grill')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    await alice.locator('#body').fill('FAKE_SILENT');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    const pill = alice.locator('button.persona[data-name="Grill"]');
    await expect(pill.locator('.pstate')).toHaveText('nothing to say', { timeout: 60_000 });
    await expect(led(alice, 'Grill')).toHaveAttribute('aria-label', 'Grill ready, nothing to say last time');
    await expect(alice.locator('#messages li.msg.agent')).toHaveCount(0);
  } finally {
    daemon.cleanup();
  }
});

// 032-AC13, 032-AC14: a time keeper's turn rating the drift high shows a red ⓘ on its pill, its
// reason as the tip and in aria-label; a later turn rating it medium hides it. Real daemon and
// Docker sandbox, fake claude.
test('personas: the time keeper rates the drift', async ({ browser, request, baseURL }) => {
  test.setTimeout(150_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const grill = (await agentState(alice, room.id)).personas[0];
  expect(await patchPersona(alice, room.id, grill.id, { time_keeper: true })).toBe(200);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await expect(led(alice, 'Grill')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    const pill = alice.locator('button.persona[data-name="Grill"]');
    const drift = pill.locator('.drift');
    await expect(drift).toBeHidden();
    const ask = async (level, n) => {
      await alice.locator('#body').fill('FAKE_DRIFT ' + level);
      await alice.locator('#body').press('Enter');
      await alice.getByRole('button', { name: 'Ask Grill' }).click();
      await expect(alice.locator('#messages li.msg.agent')).toHaveCount(n, { timeout: 60_000 });
    };
    await ask('high', 1);
    await expect(drift.locator('svg')).toBeVisible();
    await expect(drift).toHaveAttribute('aria-label', 'Off track: Fake drift high.');
    await expect(drift).toHaveAttribute('data-tip', 'Fake drift high.');
    await expect(pill).toHaveAttribute('aria-label', /^Grill, off track: Fake drift high: Grill /);
    await ask('medium', 2);
    await expect(drift).toBeHidden();
    await expect(drift).not.toHaveAttribute('aria-label');
  } finally {
    daemon.cleanup();
  }
});
