import { test, expect } from './fixtures.js';
import { roomWith, messageAction } from './helpers.js';

// 002-AC8 002-AC9 002-AC11: Markdown renders inert; an author edits and deletes a line and the
// other browser sees each change live, with the earlier version one click away.
test('edit and delete a message, live in a second browser', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await expect(alice.locator('#messages')).toContainText('Bob joined');

  await alice.locator('#body').fill('**bold** [site](https://example.com) <img src=x onerror="window.pwned=1">');
  await alice.locator('#body').press('Enter');
  const onBob = bob.locator('.msg.text', { hasText: 'bold' });
  await expect(onBob.locator('.body strong')).toHaveText('bold');
  await expect(onBob.locator('.body a')).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(onBob.locator('.body a')).toHaveAttribute('target', '_blank');
  await expect(onBob.locator('.body')).toContainText('<img src=x');
  expect(await bob.evaluate(() => window.pwned)).toBeUndefined();
  await expect(onBob.getByRole('button', { name: 'Line actions' })).toHaveCount(0);

  const mine = alice.locator('.msg.text', { hasText: 'bold' });
  await messageAction(mine, 'Edit');
  await alice.getByRole('textbox', { name: 'New text' }).fill('_fixed_ line');
  await alice.getByRole('textbox', { name: 'New text' }).press('Enter');

  const edited = bob.locator('.msg.text', { hasText: 'fixed line' });
  await expect(edited.locator('.body em')).toHaveText('fixed');
  await edited.getByRole('button', { name: 'Show earlier versions' }).click();
  await expect(edited.getByRole('list', { name: 'Earlier versions' })).toContainText('**bold** [site](https://example.com)');
  await expect(alice.locator('.msg.text', { hasText: 'fixed line' }).getByRole('button', { name: 'Show earlier versions' })).toBeVisible();

  // 005-AC39: no native dialog; the delete goes once its toast ends, 5 s.
  await messageAction(alice.locator('.msg.text', { hasText: 'fixed line' }), 'Delete');
  await expect(alice.locator('#toasts .toast')).toContainText('Message deleted');
  await expect(bob.locator('.msg.text.deleted .body')).toHaveText('message deleted', { timeout: 10_000 });
  await expect(bob.locator('#messages')).not.toContainText('fixed line');
  await expect(alice.locator('.msg.text.deleted .body')).toHaveText('message deleted');
});

// 002-AC9 002-AC40: an edit made while a page was away reaches it when it comes back.
test('an edit made while the page was away shows on return', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await alice.locator('#body').fill('before the break');
  await alice.locator('#body').press('Enter');
  await expect(bob.locator('#messages')).toContainText('before the break');

  await bob.evaluate(() => events.close());
  const mine = alice.locator('.msg.text', { hasText: 'before the break' });
  await messageAction(mine, 'Edit');
  await alice.getByRole('textbox', { name: 'New text' }).fill('after the break');
  await alice.getByRole('textbox', { name: 'New text' }).press('Enter');
  await expect(alice.locator('#messages')).toContainText('after the break');

  await bob.evaluate(() => resume());
  await expect(bob.locator('.msg.text', { hasText: 'after the break' })).toHaveCount(1);
  await expect(bob.locator('#messages')).not.toContainText('before the break');
});

// 002-AC9 002-AC40: a return from the background replays edits the page already shows; an edit
// box open on one of them keeps what was typed.
test('a replayed edit leaves an open edit box alone', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.locator('#body').fill('draft one');
  await alice.locator('#body').press('Enter');
  const mine = alice.locator('.msg.text', { hasText: 'draft one' });
  await messageAction(mine, 'Edit');
  await alice.getByRole('textbox', { name: 'New text' }).fill('draft two');
  await alice.getByRole('textbox', { name: 'New text' }).press('Enter');
  await expect(alice.locator('.msg.text', { hasText: 'draft two' })).toHaveCount(1);

  await messageAction(alice.locator('.msg.text', { hasText: 'draft two' }), 'Edit');
  await alice.getByRole('textbox', { name: 'New text' }).fill('half typed');
  // As after a page load: the stream has no edit cursor and replays every edit.
  await alice.evaluate(() => new Promise((replayed) => {
    const apply = replaceMessage;
    replaceMessage = (m) => { apply(m); replayed(); };
    editSeq = 0;
    resume();
  }));
  await expect(alice.getByRole('textbox', { name: 'New text' })).toHaveValue('half typed');
});

// 005-AC39: Undo in the delete's toast keeps the line, and nothing reaches the server.
test('a deleted line comes back with Undo', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.locator('#body').fill('keep me');
  await alice.locator('#body').press('Enter');
  const line = alice.locator('.msg.text', { hasText: 'keep me' });
  let deletes = 0;
  alice.on('request', (r) => { if (r.method() === 'DELETE') deletes++; });
  await messageAction(line, 'Delete');
  await expect(line).toHaveClass(/deleting/);
  await alice.locator('#toasts').getByRole('button', { name: 'Undo' }).click();
  await expect(line).not.toHaveClass(/deleting/);
  await expect(alice.locator('#toasts .toast')).toHaveCount(0);
  await alice.reload();
  await expect(alice.locator('.msg.text', { hasText: 'keep me' }).locator('.body')).toHaveText('keep me');
  expect(deletes).toBe(0);
});

// A line typed while the previous one is still being sent stays in the box: the first send's
// answer must not wipe it (it did, and a mention typed next was lost in CI).
test('the next line typed during a send is kept', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  let release;
  const held = new Promise((ok) => { release = ok; });
  let first = true;
  await alice.route('**/messages', async (r) => {
    if (r.request().method() === 'POST' && first) {
      first = false;
      await held;
    }
    await r.continue();
  });
  await alice.locator('#body').fill('line one');
  await alice.locator('#body').press('Enter');
  await alice.locator('#body').fill('line two');
  const answered = alice.waitForResponse((r) => r.url().endsWith('/messages') && r.request().method() === 'POST');
  release();
  await answered;
  await expect(alice.locator('.msg.text', { hasText: 'line one' })).toHaveCount(1);
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('.msg.text', { hasText: 'line two' })).toHaveCount(1);
  await expect(alice.locator('#body')).toHaveValue('');
});
