import { test, expect } from '@playwright/test';
import { roomWith, startDaemon, pairDaemon, menuItem, confirmSheet, unfold } from './helpers.js';

const agentLines = (page) => page.locator('#messages li.msg.agent');

// addSecretary adds the Secretary from the library in the personas panel, applies the trigger
// fields given, and leaves the panel open.
async function addSecretary(page, fields = {}) {
  const panel = page.locator('#personas');
  await menuItem(page, 'Personas');
  await panel.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
  await panel.getByRole('button', { name: 'Add', exact: true }).click();
  const sec = panel.locator('.persona-form', { hasText: 'Secretary' });
  await expect(sec).toHaveCount(1);
  if (fields.everyN) {
    await unfold(sec, 'advanced');
    await sec.getByLabel('Speaks up every N answers').fill(String(fields.everyN));
    await sec.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(panel.locator('#personas-status')).toHaveText('Secretary saved. It applies from its next turn.');
  }
  return panel;
}

async function say(page, text) {
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
}

// 002-AC24 002-AC25 002-AC26 002-AC27: Grill and Secretary call each other with @name (the fake
// claude's ping-pong) until the chain budget of 3; the fourth call is dropped, with the banner and a
// line in the chat, on a phone too. Suspending the limit shows a permanent red warning until it is
// restored; re-arm starts the count over. Real daemon and Docker sandbox, fake claude.
test('triggers: chain pause, re-arm, suspend', async ({ browser, request, baseURL }) => {
  test.setTimeout(240_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  let panel = await addSecretary(alice);
  // Only the host, or whom they allow to press, steers the chain: there is no host yet.
  await expect(panel.getByLabel('Auto-turn limit')).toBeDisabled();
  await panel.getByRole('button', { name: 'Close' }).click();

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await menuItem(alice, 'Personas');
    panel = alice.locator('#personas');
    await panel.getByLabel('Auto-turn limit').fill('3');
    await panel.getByRole('button', { name: 'Set' }).click();
    await expect(panel.locator('#personas-status')).toHaveText('Auto-turn limit set to 3.');
    await panel.getByRole('button', { name: 'Close' }).click();
    await expect(alice.locator('#chain-count')).toBeHidden();
    await expect(alice.locator('#chain-count')).toHaveText('auto 0/3');
    await say(alice, 'FAKE_PINGPONG Grill Secretary');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(alice.locator('#chain-paused')).toBeVisible({ timeout: 120_000 });
    await expect(alice.locator('#chain-count')).toHaveText('auto 3/3');
    await expect(alice.locator('#messages')).toContainText('The agent chain reached its budget of 3 turns: Grill did not run.');
    await expect(agentLines(alice).locator('.meta strong')).toHaveText(['🔥 Grill', '📝 Secretary', '🔥 Grill', '📝 Secretary']);
    await expect(agentLines(alice).last().locator('.body')).toHaveText('FAKE_PINGPONG Grill Secretary: over to @Grill.');

    // A late joiner on a phone sees the banner, and the message box stays on screen.
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: { Authorization: 'Bearer e2e-admin-token' } })).json();
    const phone = await (await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true })).newPage();
    await phone.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await phone.getByLabel('Your name').fill('Bob');
    await phone.getByRole('button', { name: 'Join the room' }).click();
    await expect(phone.locator('#chain-paused')).toBeVisible();
    await expect(phone.locator('#chain-paused')).toContainText('The AI host, or someone they let ask the AI, can resume them.');
    await expect(phone.getByRole('button', { name: 'Resume auto turns' })).toBeHidden();
    await expect(phone.locator('#body')).toBeInViewport();

    // Suspending lifts the pause behind a permanent red warning; restoring the limit brings the
    // pause back, since the count is still at the budget.
    await alice.locator('#chain-suspend').click();
    await confirmSheet(alice, 'Remove the limit');
    for (const p of [alice, phone]) {
      await expect(p.locator('#chain-unlimited')).toBeVisible();
      await expect(p.locator('#chain-unlimited')).toContainText("The AI host's own cap still applies.");
      await expect(p.locator('#chain-paused')).toBeHidden();
      await expect(p.locator('#chain-count')).toHaveText('auto 3, no limit');
    }
    await alice.getByRole('button', { name: 'Restore the limit' }).click();
    await expect(phone.locator('#chain-unlimited')).toBeHidden();
    await expect(phone.locator('#chain-paused')).toBeVisible();
    await expect(phone.locator('#messages')).toContainText('Alice restored the chain limit of 3.');

    await alice.getByRole('button', { name: 'Resume auto turns' }).click();
    for (const p of [alice, phone]) {
      await expect(p.locator('#chain-paused')).toBeHidden();
      await expect(p.locator('#chain-count')).toHaveText('auto 0/3');
      await expect(p.locator('#messages')).toContainText('Alice re-armed the agent chain.');
    }
  } finally {
    daemon.cleanup();
  }
});

// 002-AC22: with the Secretary set to every 2 rounds in the personas panel, two presses of Grill
// make it run by itself, as one chain link.
test('triggers: every N rounds', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const panel = await addSecretary(alice, { everyN: 2 });
  await expect(alice.locator('#messages')).toContainText('Alice changed 📝 Secretary: every N rounds off → 2.');
  await panel.getByRole('button', { name: 'Close' }).click();

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    for (let round = 1; round <= 2; round++) {
      await expect(alice.getByRole('button', { name: 'Ask Grill' })).toBeEnabled();
      await alice.getByRole('button', { name: 'Ask Grill' }).click();
      await expect(agentLines(alice).filter({ has: alice.locator('.meta strong', { hasText: 'Grill' }) })).toHaveCount(round, { timeout: 60_000 });
    }
    await expect(agentLines(alice).locator('.meta strong')).toHaveText(['🔥 Grill', '🔥 Grill', '📝 Secretary'], { timeout: 60_000 });
    await expect(alice.locator('#chain-count')).toHaveText('auto 1/25');
  } finally {
    daemon.cleanup();
  }
});

// 002-AC23 002-AC26: a message mentioning @secretary, in any case, queues the Secretary's turn as
// a press of its author, and starts the chain over; an @name in code calls no one.
test('triggers: human @mention', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const panel = await addSecretary(alice);
  await panel.getByRole('button', { name: 'Close' }).click();

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    // A mention calls no one until the room hears the host can run it.
    await expect(alice.locator('button.persona[data-name="Secretary"] .led')).toHaveAttribute('aria-label', 'Secretary ready', { timeout: 30_000 });
    await say(alice, 'In code, `@secretary` calls no one.');
    await say(alice, '@SECRETARY FAKE_WHOAMI please');
    const sec = agentLines(alice).filter({ hasText: 'You are Secretary' });
    await expect(sec.locator('.meta strong')).toHaveText('📝 Secretary', { timeout: 60_000 });
    await expect(agentLines(alice)).toHaveCount(1);
    await expect(alice.locator('#chain-count')).toHaveText('auto 0/25');
  } finally {
    daemon.cleanup();
  }
});
