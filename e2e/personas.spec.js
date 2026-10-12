import { test, expect } from '@playwright/test';
import { roomWith, startDaemon, pairDaemon } from './helpers.js';

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
  await expect(alice.getByRole('button', { name: 'Ask Grill' })).toContainText('🔥');

  const panel = alice.locator('#personas');
  await alice.getByRole('button', { name: 'Personas' }).click();
  await expect(panel.locator('.persona-form')).toHaveCount(1);
  await expect(panel.locator('#persona-template option')).toHaveCount(2);
  await panel.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
  await panel.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(panel.locator('.persona-form')).toHaveCount(2);
  for (const p of [alice, bob]) {
    await expect(p.locator('#messages')).toContainText('Alice added the persona 📝 Secretary, from the template "Secretary" v1.');
    await expect(p.locator('button.persona')).toHaveText(['🔥Grill', '📝Secretary']);
  }
  const sec = panel.locator('.persona-form', { hasText: 'Secretary' });
  await sec.getByLabel('Effort').selectOption('low');
  await sec.getByLabel('Persona text').fill('You are Scribe, who writes it all down.\nNothing else.');
  await sec.getByRole('button', { name: 'Save' }).click();
  await expect(panel.locator('#personas-status')).toHaveText('Secretary saved. It applies from its next turn.');
  await expect(bob.locator('#messages')).toContainText('Alice changed 📝 Secretary: effort medium → low, persona text.');
  // A bad name is refused by the room, and says why.
  await sec.getByLabel('Name').fill('two words');
  await sec.getByRole('button', { name: 'Save' }).click();
  await expect(panel.locator('#personas-status')).toHaveText('name must be 1 to 30 letters, digits, - or _');
  await panel.getByRole('button', { name: 'Close' }).click();

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await expect(led(alice, 'Secretary')).toHaveAttribute('aria-label', 'Secretary ready');
    await alice.locator('#body').fill('FAKE_SLOW');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(led(bob, 'Grill')).toHaveAttribute('aria-label', 'Grill working');
    await alice.getByRole('button', { name: 'Ask Secretary' }).click();
    for (const p of [alice, bob]) {
      await expect(led(p, 'Secretary')).toHaveAttribute('aria-label', 'Secretary queued, position 1');
      await expect(p.locator('#queue-list li')).toHaveText([/^📝 Secretary, pressed by Alice/]);
    }
    await expect(bob.getByRole('button', { name: 'Cancel the queued turn of Secretary' })).toHaveCount(0);
    await alice.getByRole('button', { name: 'Cancel the queued turn of Secretary' }).click();
    await expect(alice.locator('#queue')).toBeHidden();
    await alice.locator('#body').fill('FAKE_WHOAMI');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask Secretary' }).click();
    await expect(alice.locator('#queue-list li')).toHaveCount(1);
    await expect(led(alice, 'Grill')).toHaveAttribute('aria-label', 'Grill working');

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
