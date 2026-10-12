import { test, expect } from '@playwright/test';
import { roomWith, startDaemon, pairDaemon } from './helpers.js';

const led = (page, name) => page.locator(`button.persona[data-name="${name}"] .led`);
const lastAgent = (page) => page.locator('#messages li.msg.agent').last();

// openPanel opens the personas panel once its library has loaded: the load replaces the template
// options, and with them a choice made before it.
async function openPanel(page) {
  await Promise.all([page.waitForResponse((r) => r.url().endsWith('/templates')), page.getByRole('button', { name: 'Personas' }).click()]);
  return page.locator('#personas');
}

// edit sets fields of a persona in the personas panel and saves; the panel stays open on its status.
async function edit(page, name, fields) {
  const panel = await openPanel(page);
  const form = panel.locator('.persona-form', { has: page.getByRole('heading', { name: new RegExp(` ${name}$`) }) });
  for (const [label, value] of Object.entries(fields)) await form.getByLabel(label).selectOption(value);
  await form.getByRole('button', { name: 'Save' }).click();
  return { status: panel.locator('#personas-status') };
}

async function close(page) {
  await page.locator('#personas').getByRole('button', { name: 'Close' }).click();
}

async function ask(page, persona, count) {
  await page.locator('#body').fill('FAKE_WHOAMI');
  await page.locator('#body').press('Enter');
  await page.getByRole('button', { name: 'Ask ' + persona }).click();
  await expect(page.locator('#messages li.msg.agent')).toHaveCount(count, { timeout: 60_000 });
}

// 002-AC19 002-AC20 002-AC21 002-AC36: Alice caps her daemon at sonnet and high on its page. Bob
// asks Grill for opus and max: the banner shows it to both, the turn runs capped and its synthesis
// says so; Alice approves and the next turn runs as asked. Bob's Secretary asking for opus is
// refused: everyone reads the outcome and it keeps the cap. With "approve everything" on, Bob's new
// persona and his change wait, shown as pending, until Alice approves them. Real daemon and Docker
// sandbox, fake claude.
test('host cap and approvals', async ({ browser, request, baseURL }) => {
  test.setTimeout(240_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    await admin.getByLabel('Max model').selectOption('sonnet');
    await admin.getByLabel('Max effort').selectOption('high');
    await admin.getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#settings-status')).toHaveText('Saved.');
    await admin.reload();
    await expect(admin.getByLabel('Max model')).toHaveValue('sonnet');
    await expect(admin.getByLabel('Max effort')).toHaveValue('high');
    await expect(led(alice, 'Grill')).toHaveAttribute('aria-label', 'Grill ready');

    let { status } = await edit(bob, 'Grill', { Model: 'opus', Effort: 'max' });
    await expect(status).toHaveText('Grill saved. It applies from its next turn.');
    await close(bob);
    const banner = '🔥 Grill asks for opus · effort max, above the host cap: it runs at sonnet · effort high.';
    await expect(bob.locator('#approval-list li')).toHaveText([banner + ' The host decides.']);
    await expect(alice.locator('#approval-list li')).toContainText([banner]);

    await ask(alice, 'Grill', 1);
    for (const p of [alice, bob]) {
      await expect(lastAgent(p).locator('.meta')).toContainText('requested opus · effort max → ran claude-sonnet-fake · effort high (host cap)');
      await expect(lastAgent(p).locator('.body')).toContainText('(effort high)');
    }

    await alice.getByRole('button', { name: 'Approve opus · effort max for Grill' }).click();
    for (const p of [alice, bob]) {
      await expect(p.locator('#approvals')).toBeHidden();
      await expect(p.locator('#messages')).toContainText(/Alice approved opus · effort max for 🔥 Grill: it runs as asked from its next turn\./);
    }
    await ask(alice, 'Grill', 2);
    await expect(lastAgent(bob).locator('.model')).toHaveText('claude-opus-fake');
    await expect(lastAgent(bob).locator('.effort')).toHaveText('effort max');
    await expect(lastAgent(bob).locator('.capped')).toHaveCount(0);
    // The approval is an exception: the daemon's cap stays.
    await admin.reload();
    await expect(admin.getByLabel('Max model')).toHaveValue('sonnet');

    // Refused: the Secretary keeps running at the cap, and Bob reads why.
    await openPanel(bob);
    await bob.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
    await bob.locator('#personas').getByRole('button', { name: 'Add', exact: true }).click();
    await expect(bob.locator('#personas .persona-form')).toHaveCount(2);
    await close(bob);
    ({ status } = await edit(bob, 'Secretary', { Model: 'opus' }));
    await expect(status).toHaveText('Secretary saved. It applies from its next turn.');
    await close(bob);
    await alice.getByRole('button', { name: 'Refuse opus · effort medium for Secretary' }).click();
    await expect(bob.locator('#messages')).toContainText(
      'Alice refused opus · effort medium for 📝 Secretary: it keeps running at the host cap, sonnet · effort medium.');
    await expect(bob.locator('#approvals')).toBeHidden();
    await ask(alice, 'Secretary', 3);
    await expect(lastAgent(bob).locator('.meta')).toContainText(/requested opus · effort medium → ran sonnet · effort medium · \+claude-side-fake \S+ \(host cap\)/);

    // Approve everything: Bob's new persona and his change wait for Alice.
    await admin.getByLabel('Approve every new persona and persona change').check();
    await admin.getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#settings-status')).toHaveText('Saved.');
    await openPanel(bob);
    await bob.locator('#persona-template').selectOption({ label: '🔥 Default v1' });
    await bob.locator('#personas').getByRole('button', { name: 'Add', exact: true }).click();
    await expect(bob.locator('#personas-status')).toHaveText('Grill-2 added. It waits for the host\'s approval.');
    await close(bob);
    ({ status } = await edit(bob, 'Secretary', { Effort: 'low' }));
    await expect(status).toHaveText('Secretary: the change waits for the host\'s approval.');
    await close(bob);
    for (const p of [alice, bob]) {
      await expect(led(p, 'Grill-2')).toHaveAttribute('aria-label', 'Grill-2 waits for the host\'s approval');
      await expect(p.locator('button.persona[data-name="Grill-2"]')).toBeDisabled();
    }
    await expect(bob.locator('#approval-list li')).toHaveText([
      '🔥 Grill-2, added by Bob: new persona. The host decides.',
      'Bob asks to change 📝 Secretary: effort medium → low. The host decides.',
    ]);
    await alice.getByRole('button', { name: 'Approve the persona Grill-2' }).click();
    await alice.getByRole('button', { name: 'Approve the change to Secretary' }).click();
    // A refusal settles the level it was asked for: asking opus with another effort asks again.
    await expect(bob.locator('#approval-list li')).toHaveText([
      '📝 Secretary asks for opus · effort low, above the host cap: it runs at sonnet · effort low. The host decides.',
    ]);
    await expect(led(bob, 'Grill-2')).toHaveAttribute('aria-label', 'Grill-2 ready');
    await expect(bob.locator('#messages')).toContainText('Alice approved Bob\'s change to 📝 Secretary: effort medium → low.');
    await ask(alice, 'Secretary', 4);
    await expect(lastAgent(bob).locator('.effort')).toHaveText('effort low');
  } finally {
    daemon.cleanup();
  }
});
