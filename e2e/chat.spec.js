import { test, expect } from '@playwright/test';
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

  alice.once('dialog', (d) => d.accept());
  await messageAction(alice.locator('.msg.text', { hasText: 'fixed line' }), 'Delete');
  await expect(bob.locator('.msg.text.deleted .body')).toHaveText('message deleted');
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
