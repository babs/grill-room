import { test, expect } from './fixtures.js';
import { roomWith, menuItem } from './helpers.js';

// A 2 × 1 PNG: the server takes images by their content.
const png = 'iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAYAAAD0In+KAAAAEUlEQVR42mNk+M9Qz8DAwAAADjQB/5tYdNkAAAAASUVORK5CYII=';
const row = (page, name) => page.locator('#participants li', { has: page.locator('.name', { hasText: name }) });
const seg = (page, f) => page.locator(`#chat-filter [data-filter=${f}]`);

// openChat picks "Open chat in a new window" in the room menu and answers the window it opened.
async function openChat(page) {
  const [chat] = await Promise.all([page.context().waitForEvent('page'), menuItem(page, 'Open chat in a new window')]);
  await expect(chat.locator('#me')).toHaveText(await page.locator('#me').textContent());
  return chat;
}

// 031-AC8 031-AC9 031-AC11: the room menu opens the chat in a window of its own, one per room; it
// holds the whole chat and nothing of the call or the panes; what is written, sent and raised
// there shows in the room; a file sent opens in the docs window; its title counts unread lines.
test('the chat in a window of its own', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await alice.setViewportSize({ width: 1440, height: 900 });
  // A window opened by the page copies its session storage: the recovery link stays behind.
  await alice.evaluate(() => sessionStorage.setItem('grill_room_recovery', 'https://example.test/r'));
  const chat = await openChat(alice);
  await expect(chat).toHaveURL(new RegExp(`/r/${room.id}/chat$`));
  await expect(chat).toHaveTitle('Chat · Docs room · grill-room');
  expect(await chat.evaluate(() => sessionStorage.getItem('grill_room_recovery'))).toBeNull();
  // 031-AC8: a second click reuses that window.
  const pages = alice.context().pages().length;
  await menuItem(alice, 'Open chat in a new window');
  await expect(chat).toHaveURL(new RegExp(`/r/${room.id}/chat$`));
  expect(alice.context().pages().length).toBe(pages);

  // 031-AC9: the chat only.
  for (const sel of ['#voice', '#voice-join', '#video-open', '#stage', '#people-pane', '#docs-pane', '#viewer', '#agent', '#tabbar']) {
    await expect(chat.locator(sel), sel).toBeHidden();
  }
  // From 64rem voice lines run in the Transcription pane beside the chat (020-AC8); narrower, the filter.
  await expect(chat.locator('#transcript')).toBeVisible();
  await chat.setViewportSize({ width: 900, height: 720 });
  for (const sel of ['#messages', '#chat-filter', '#send', '#attach-open', '#hand-raise']) await expect(chat.locator(sel), sel).toBeVisible();
  await expect(chat.locator('#room-name a')).toHaveAttribute('href', `/r/${room.id}`);

  // Written with a mention, an attached file and a pasted image.
  const body = chat.locator('#body');
  await body.pressSequentially('Hi @bo');
  await chat.locator('#mention-list').getByRole('option', { name: /Bob/ }).click();
  await expect(body).toHaveValue('Hi @Bob ');
  await chat.locator('#attach-input').setInputFiles({ name: 'plan.md', mimeType: 'text/markdown', buffer: Buffer.from('# Plan\n\nShip it.\n') });
  await chat.evaluate((b64) => {
    const dt = new DataTransfer();
    dt.items.add(new File([Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))], 'image', { type: 'image/png' }));
    document.getElementById('body').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  }, png);
  await expect(chat.getByRole('list', { name: 'Files to send' }).locator('li')).toHaveCount(2);
  await body.press('Enter');
  const sent = bob.locator('#messages li.msg', { hasText: 'Hi @Bob' });
  await expect(sent.locator('.mention-chip')).toHaveText('@Bob');
  await expect(sent.locator('.msg-doc')).toContainText('plan.md');
  await expect(sent.locator('.msg-thumb img')).toHaveCount(1);
  await expect(alice.locator('#messages')).toContainText('Hi @Bob');

  // A file in a message opens in the docs window, the room's one.
  const line = chat.locator('#messages li.msg', { hasText: 'Hi @Bob' });
  const [docs] = await Promise.all([chat.context().waitForEvent('page'), line.locator('.msg-doc').click()]);
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs/\\d+$`));
  await expect(docs.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Plan');
  await expect(chat.locator('#viewer')).toBeHidden();
  const open = alice.context().pages().length;
  await line.locator('.msg-thumb').click();
  await expect(docs.locator('#doc-name')).not.toHaveText('plan.md');
  expect(alice.context().pages().length).toBe(open);

  // The Written/Voice filter.
  await seg(chat, 'voice').click();
  await expect(chat.locator('#filter-empty')).toHaveText('No voice messages yet.');
  await expect(line).toBeHidden();
  await seg(chat, 'written').click();
  await expect(line).toBeVisible();

  // A hand raised there is in the room's queue.
  await chat.getByRole('button', { name: 'Raise my hand' }).click();
  await expect(row(bob, 'Alice').locator('.tag-hand:not(.blank)')).toHaveAttribute('aria-label', 'Hand raised, 1st');
  await expect(alice.getByRole('button', { name: 'Lower my hand' })).toBeVisible();

  // 031-AC11: one of Alice's tabs, its own heartbeat; unread lines lead its title.
  const [beat] = await Promise.all([
    chat.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith(`/api/rooms/${room.id}/presence`)),
    chat.evaluate(() => window.dispatchEvent(new Event('blur'))),
  ]);
  expect(beat.postDataJSON().tab).not.toBe(await alice.evaluate(() => tab));
  await chat.evaluate(() => { document.hasFocus = () => false; });
  await bob.locator('#body').fill('@Alice one');
  await bob.locator('#body').press('Enter');
  await expect(chat.locator('#messages')).toContainText('@Alice one');
  await expect(chat).toHaveTitle('(1 @) Chat · Docs room · grill-room');
  await chat.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(chat).toHaveTitle('Chat · Docs room · grill-room');
});

// 031-AC8: below 40rem the room menu has no chat window entry; the documents' stays.
test('no chat window entry on a phone', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 390, height: 844 });
  await alice.locator('#room-open').click();
  await expect(alice.locator('#room-menu').getByRole('link', { name: 'Open documents in a new window' })).toBeVisible();
  await expect(alice.locator('#chat-window-menu')).toBeHidden();
  await alice.setViewportSize({ width: 640, height: 844 });
  await expect(alice.locator('#chat-window-menu')).toBeHidden();
  await alice.setViewportSize({ width: 660, height: 844 });
  await expect(alice.locator('#chat-window-menu')).toBeVisible();
});
