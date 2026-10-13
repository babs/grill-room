import { test, expect } from '@playwright/test';
import { roomWith, startDaemon, pairDaemon, upload, menuItem, stopDaemon, confirmSheet, unfold, personMenu } from './helpers.js';

const led = (page) => page.locator('button.persona[data-name="Grill"] .led');
const press = (page) => page.getByRole('button', { name: 'Ask Grill' });
// newSession is the host's button for a persona, in the personas panel.
const newSession = (page) => page.getByRole('button', { name: 'Start fresh for Grill' });
async function withPanel(page, fn) {
  await menuItem(page, 'Personas');
  await fn(page.locator('#personas'));
  await page.locator('#personas').getByRole('button', { name: 'Close' }).click();
}

// 001-AC30 001-AC31 001-AC32: the LED is red and the button off with no host daemon; the first
// paired daemon makes its participant host; the host grants the button, a granted participant
// runs a turn whose synthesis and document reach everyone, the host revokes the right and hands
// the role over; when the host's daemon stops the LED goes red again. Real daemons and Docker
// sandboxes, with a fake claude.
test('agent LED, button and rights', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  for (const p of [alice, bob]) {
    await expect(led(p)).toHaveAttribute('aria-label', 'Grill offline');
    await expect(led(p)).not.toHaveClass(/\bon\b/);
    await expect(press(p)).toBeDisabled();
    await expect(p.locator('#agent-status')).toHaveText('No one runs the AI for this room right now.');
  }
  // Transcripts keep recording with the agent off.
  await bob.locator('#body').fill('Still talking.');
  await bob.locator('#body').press('Enter');
  await expect(alice.locator('#messages')).toContainText('Still talking.');

  const roomURL = `${baseURL}/r/${room.id}`;
  const daemonA = startDaemon(roomURL);
  const daemonB = startDaemon(roomURL);
  try {
    const adminA = await pairDaemon(alice, daemonA);
    for (const p of [alice, bob]) await expect(led(p)).toHaveAttribute('aria-label', 'Grill ready');
    await expect(led(alice)).toHaveClass(/\bon\b/);
    await expect(alice.locator('#participants li', { hasText: 'Alice' }).locator('.host-badge')).toHaveAttribute('aria-label', 'host');
    await expect(press(alice)).toBeEnabled();
    await expect(press(bob)).toBeDisabled();
    await withPanel(alice, async (panel) => {
      await unfold(panel.locator('.persona-form').first(), 'more');
      await expect(newSession(alice)).toBeVisible();
    });
    await withPanel(bob, async (panel) => {
      await expect(panel.locator('.persona-form')).toHaveCount(1);
      await expect(newSession(bob)).toHaveCount(0);
    });
    // Only the host sees the rights controls.
    await expect(bob.locator('#participants li', { hasText: 'Alice' }).locator('.person-more')).toBeDisabled();

    // A refused press says why, even with the agent online.
    await alice.route('**/turns', (r) => r.fulfill({ status: 409, contentType: 'application/json', body: '{"error":"turn in progress"}' }));
    await press(alice).click();
    await expect(alice.locator('#agent-status')).toHaveText('turn in progress');
    await alice.unroute('**/turns');

    await (await personMenu(alice, 'Bob')).getByRole('button', { name: 'Can ask the AI', pressed: false }).click();
    await expect(press(bob)).toBeEnabled();
    await press(bob).click();
    await expect(led(alice)).toHaveAttribute('aria-label', 'Grill answering');
    for (const p of [alice, bob]) {
      await expect(p.locator('#messages li.msg.agent').last()).toContainText('Wrote docs/notes.md, turn 1.', { timeout: 60_000 });
      // 005-AC10: the model asked for, found in what the fake claude reports, its side model second
      // with its cost share.
      const meta = p.locator('#messages li.msg.agent').last().locator('.meta');
      await expect(meta.locator('.model')).toHaveText('sonnet');
      await expect(meta.locator('.model-note')).toHaveText(/^\+claude-side-fake (<1|\d+)%$/);
      await expect(meta.locator('.model-note')).not.toHaveClass(/mismatch/);
      await expect(meta).toContainText(/sonnet · effort medium · \+claude-side-fake/);
      // 002-AC18 002-AC42: the synthesis names its persona and the effort it ran with.
      await expect(meta.locator('strong')).toHaveText('🔥 Grill');
      await expect(meta.locator('.effort')).toHaveText('effort medium');
      await expect(p.locator('#documents')).toContainText('notes.md');
    }
    await expect(led(bob)).toHaveAttribute('aria-label', 'Grill ready');

    await (await personMenu(alice, 'Bob')).getByRole('button', { name: 'Can ask the AI', pressed: true }).click();
    await expect(press(bob)).toBeDisabled();

    // Hand-over: Bob's daemon connects, Alice makes him host.
    await pairDaemon(bob, daemonB);
    await (await personMenu(alice, 'Bob')).getByRole('button', { name: 'Make AI host' }).click();
    await expect(bob.locator('#participants li', { hasText: 'Bob' }).locator('.host-badge')).toHaveAttribute('aria-label', 'host');
    await expect(press(bob)).toBeEnabled();
    await expect(press(alice)).toBeDisabled();
    await expect((await personMenu(bob, 'Alice')).getByRole('button', { name: 'Can ask the AI' })).toBeVisible();
    await bob.keyboard.press('Escape');
    await expect(alice.locator('#participants li', { hasText: 'Bob' }).locator('.person-more')).toBeDisabled();

    // The host can start a new session after confirming.
    await menuItem(bob, 'Personas');
    await unfold(bob.locator('#personas .persona-form').first(), 'more');
    await newSession(bob).click();
    await confirmSheet(bob, 'Start fresh');
    await bob.locator('#personas').getByRole('button', { name: 'Close' }).click();
    await expect(bob.locator('#messages li.msg.agent').last()).toContainText('turn 1.', { timeout: 60_000 });
    await expect(bob.locator('#messages li.msg.agent')).toHaveCount(2);

    // Bob's daemon stops: Alice's daemon is the only one left and takes the role back.
    daemonB.proc.kill('SIGTERM');
    expect(await daemonB.exited).toBe(0);
    await expect(alice.locator('#participants li', { hasText: 'Alice' }).locator('.host-badge')).toHaveAttribute('aria-label', 'host');
    await expect(press(alice)).toBeEnabled();
    await stopDaemon(adminA);
    expect(await daemonA.exited).toBe(0);
    for (const p of [alice, bob]) {
      await expect(led(p)).toHaveAttribute('aria-label', 'Grill offline');
      await expect(press(p)).toBeDisabled();
    }
  } finally {
    daemonA.cleanup();
    daemonB.cleanup();
  }
});

// 001-AC39: the agent opens its own anchored annotation on an upload, answers on it, closes it,
// and a participant reply reopens it. Real daemon and Docker sandbox, fake claude.
test('agent annotation loop', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await upload(alice, 'plan.md', 'text/markdown', '# Rollout plan\n\nRollback first, then freeze.\n');
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  const say = async (text) => {
    await alice.locator('#body').fill(text);
    await alice.locator('#body').press('Enter');
    await expect(alice.locator('#messages')).toContainText(text);
  };
  try {
    await pairDaemon(alice, daemon);
    await expect(led(alice)).toHaveAttribute('aria-label', 'Grill ready');
    await say('FAKE_ANNOTATE');
    await press(alice).click();
    const thread = alice.locator('#annot-list li.annot', { hasText: 'fake question' });
    await expect(thread).toContainText('"Rollback"', { timeout: 60_000 });
    await expect(thread).toContainText('🔥 Grill');
    await expect(thread.locator('.annot-badge')).toHaveText('open');
    await thread.getByLabel('Reply').fill('Ten minutes.');
    await thread.getByRole('button', { name: 'Reply' }).click();
    await expect(thread).toContainText('Ten minutes.');

    await expect(led(alice)).toHaveAttribute('aria-label', 'Grill ready');
    await say('FAKE_CLOSE');
    await press(alice).click();
    await expect(thread).toContainText('settled', { timeout: 60_000 });
    await expect(thread.locator('.annot-badge')).toHaveText('done');
    await thread.getByLabel('Reply').fill('Not settled yet.');
    await thread.getByRole('button', { name: 'Reply' }).click();
    await expect(thread.locator('.annot-badge')).toHaveText('reopened');
  } finally {
    daemon.cleanup();
  }
});
