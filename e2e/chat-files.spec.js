import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, dropFiles, smallTargets, messageAction } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const sizes = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } };
// A 2 × 1 PNG: the server takes images by their content.
const png = 'iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAYAAAD0In+KAAAAEUlEQVR42mNk+M9Qz8DAwAAADjQB/5tYdNkAAAAASUVORK5CYII=';

// pasteImage pastes an image (a PNG unless type says) into the composer as a capture does: a
// ClipboardEvent carrying the file, and text beside it when given. It answers the stems a capture
// may take, the clock's second before and after the paste.
const pasteImage = (page, { type = 'image/png', text = '' } = {}) => page.evaluate(([b64, type, text]) => {
  const p2 = (n) => String(n).padStart(2, '0');
  const stamp = (t) => 'capture-' + t.getFullYear() + p2(t.getMonth() + 1) + p2(t.getDate()) + '-' + p2(t.getHours()) + p2(t.getMinutes()) + p2(t.getSeconds());
  const dt = new DataTransfer();
  if (text) dt.setData('text/plain', text);
  dt.items.add(new File([Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))], 'image', { type }));
  const before = stamp(new Date());
  document.getElementById('body').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  return [before, stamp(new Date())];
}, [png, type, text]);

// dropBytes drops files of n zero bytes each on target.
const dropBytes = (page, target, files) => page.evaluate(([target, files]) => {
  const dt = new DataTransfer();
  for (const f of files) dt.items.add(new File([new Uint8Array(f.size)], f.name, { type: f.type }));
  for (const type of ['dragenter', 'dragover', 'drop']) document.querySelector(target).dispatchEvent(new DragEvent(type, { dataTransfer: dt, bubbles: true, cancelable: true }));
}, [target, files]);

// showChat brings the chat back after a document opened: a phone shows one pane at a time.
async function showChat(page, phone) {
  await page.locator('#doc-close').click();
  if (phone) await page.locator('#tabbar').getByRole('button', { name: /Chat/ }).click();
}

const pending = (page) => page.getByRole('list', { name: 'Files to send' }).locator('li');

for (const [size, viewport] of Object.entries(sizes)) {
  const phone = size === 'phone';
  // 029-AC1 029-AC2 029-AC3 029-AC6 029-AC7 029-AC9: a pasted capture and two dropped files go
  // with one message; the other participant sees the thumbnail and the chips live and opens them,
  // and the Documents list holds them in its Chat folder, folded until unfolded.
  test(`files sent with a message, at ${size} width`, async ({ browser, request }) => {
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await alice.setViewportSize(viewport);
    await bob.setViewportSize(viewport);
    await expect(alice.locator('#messages')).toContainText('Bob joined');

    // 029-AC1: the paperclip names itself and opens a picker of several files.
    const attach = alice.getByRole('button', { name: 'Attach files' });
    await expect(attach).toBeVisible();
    const box = await attach.boundingBox();
    if (phone) expect(Math.min(box.width, box.height)).toBeGreaterThanOrEqual(44);
    const [chooser] = await Promise.all([alice.waitForEvent('filechooser'), attach.click()]);
    expect(chooser.isMultiple()).toBe(true);
    await chooser.setFiles([]);

    // 029-AC2: text pastes as before, and adds no file.
    await alice.context().grantPermissions(['clipboard-read', 'clipboard-write']);
    await alice.evaluate(() => navigator.clipboard.writeText('Here are '));
    await alice.locator('#body').focus();
    await alice.keyboard.press('ControlOrMeta+V');
    await expect(alice.locator('#body')).toHaveValue('Here are ');
    await expect(pending(alice)).toHaveCount(0);

    // An image pasted is a capture named by the local clock, with its preview.
    const names = await pasteImage(alice);
    await expect(pending(alice)).toHaveCount(1);
    const capture = await pending(alice).locator('.file-name').textContent();
    expect(names.map((n) => n + '.png')).toContain(capture);
    // The preview loads: the page's CSP lets blob: images through.
    await expect.poll(() => pending(alice).first().locator('img').evaluate((i) => i.naturalWidth)).toBeGreaterThan(0);

    // Files dropped on the chat pane join it, with the drop state while they hover.
    if (!phone) {
      await alice.locator('.chat').dispatchEvent('dragenter', { dataTransfer: await alice.evaluateHandle(() => { const d = new DataTransfer(); d.items.add(new File(['x'], 'x.md')); return d; }) });
      await expect(alice.locator('.chat .drop-overlay')).toBeVisible();
      await expect(alice.locator('#docs-pane .drop-overlay')).toBeHidden();
    }
    await dropFiles(alice, '.chat', [
      { name: 'brief.md', type: 'text/markdown', body: '# Brief\n' },
      { name: 'a-very-long-name-for-the-plan-of-the-quarter.md', type: 'text/markdown', body: '# Plan\n' },
    ]);
    await expect(alice.locator('.chat .drop-overlay')).toBeHidden();
    await expect(pending(alice)).toHaveCount(3);
    const long = pending(alice).filter({ hasText: 'a-very-long' }).locator('.file-name');
    await expect(long).toHaveAttribute('data-tip', 'a-very-long-name-for-the-plan-of-the-quarter.md');
    expect(await long.evaluate((e) => e.scrollWidth > e.clientWidth)).toBe(true);
    // The Documents pane got nothing: the files wait for the message.
    await expect(alice.locator('#uploads li')).toHaveCount(0);
    if (phone) expect(await smallTargets(alice), 'composer with files').toEqual([]);

    await alice.locator('#body').fill('Here are the files');
    await alice.getByRole('button', { name: 'Send' }).click();
    await expect(pending(alice)).toHaveCount(0);
    await expect(alice.locator('#body')).toHaveValue('');

    // 029-AC7: live on Bob's page, the image as a thumbnail, the others as chips.
    const line = bob.locator('.msg.text', { hasText: 'Here are the files' });
    const thumb = line.getByRole('button', { name: capture });
    await expect(thumb.locator('img')).toBeVisible();
    await expect.poll(() => thumb.locator('img').evaluate((i) => i.naturalWidth)).toBe(2);
    const shown = await thumb.locator('img').boundingBox();
    expect(shown.width).toBeLessThanOrEqual(240);
    expect(shown.height).toBeLessThanOrEqual(180);
    const chip = line.getByRole('button', { name: /brief\.md/ });
    await expect(chip).toContainText('8 B');
    await expect(chip.locator('svg')).toBeVisible();
    if (phone) expect(await smallTargets(bob), 'message with files').toEqual([]);

    // A click opens each in the viewer; the keyboard reaches them.
    await thumb.click();
    await expect(bob.locator('#doc-name')).toHaveText(capture);
    await showChat(bob, phone);
    await chip.focus();
    await bob.keyboard.press('Enter');
    await expect(bob.locator('#doc-name')).toHaveText('brief.md');
    await showChat(bob, phone);

    // 029-AC9: a Chat folder, folded, its files in the list's order; the root keeps the rest.
    if (phone) await bob.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
    const folder = bob.locator('#documents').getByRole('button', { name: 'Chat' });
    await expect(folder).toHaveAttribute('aria-expanded', 'false');
    await expect(bob.locator('#chat-docs')).toBeHidden();
    await folder.click();
    await expect(folder).toHaveAttribute('aria-expanded', 'true');
    const order = await bob.evaluate((id) => fetch(`/api/rooms/${id}/documents`).then((r) => r.json()), room.id);
    await expect(bob.locator('#chat-docs button.doc')).toHaveText(order.filter((d) => d.folder === 'chat').map((d) => d.name));
    expect(order.filter((d) => d.folder === 'chat')).toHaveLength(3);
    if (phone) expect(await smallTargets(bob), 'chat folder').toEqual([]);
    // Its state is kept in this browser, the docs window's too.
    await bob.reload();
    if (phone) await bob.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
    await expect(bob.locator('#documents').getByRole('button', { name: 'Chat' })).toHaveAttribute('aria-expanded', 'true');
    const docs = await bob.context().newPage();
    await docs.setViewportSize(viewport);
    await docs.goto(`/r/${room.id}/docs`);
    await expect(docs.locator('#chat-docs button.doc')).toHaveCount(3);
    await docs.locator('#chat-docs').getByRole('button', { name: 'brief.md' }).click();
    await expect(docs.locator('#doc-name')).toHaveText('brief.md');
    // Alice never unfolded it: folded there.
    if (phone) await alice.locator('#tabbar').getByRole('button', { name: /Docs/ }).click();
    await expect(alice.locator('#documents').getByRole('button', { name: 'Chat' })).toHaveAttribute('aria-expanded', 'false');
  });
}

// 029-AC3: a fifth file, an oversize one and an unaccepted one are refused on the spot, in the
// server's words, before any request.
test('files refused on the spot', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(sizes.desktop);
  const posts = [];
  alice.on('request', (r) => { if (r.method() === 'POST' && r.url().endsWith('/messages')) posts.push(r.url()); });
  const five = [1, 2, 3, 4, 5].map((n) => ({ name: `f${n}.md`, mimeType: 'text/markdown', buffer: Buffer.from('# ' + n) }));
  await alice.locator('#attach-input').setInputFiles(five);
  await expect(pending(alice)).toHaveCount(4);
  await expect(alice.locator('#send-error')).toHaveText('At most 4 files per message');
  // One more on a full composer: refused too.
  await dropFiles(alice, '.chat', [{ name: 'f6.md', type: 'text/markdown', body: '# 6' }]);
  await expect(pending(alice)).toHaveCount(4);
  await expect(alice.locator('#send-error')).toHaveText('At most 4 files per message');

  await pending(alice).first().getByRole('button', { name: 'Remove f1.md' }).click();
  await expect(pending(alice)).toHaveCount(3);
  const rules = await alice.evaluate(() => window.grillRoom.upload);
  await dropBytes(alice, '.chat', [{ name: 'big.png', type: 'image/png', size: rules.max_bytes + 1 }]);
  await expect(alice.locator('#send-error')).toHaveText('big.png: ' + rules.too_large);
  expect(rules.too_large).toMatch(/^file is over the \d+ MB limit$/);
  await dropFiles(alice, '.chat', [{ name: 'tool.exe', type: 'application/octet-stream', body: 'MZ' }]);
  await expect(alice.locator('#send-error')).toHaveText('tool.exe: ' + rules.unsupported);
  expect(rules.unsupported).toMatch(/^unsupported file type, accepted: /);
  await expect(pending(alice)).toHaveCount(3);
  expect(posts).toEqual([]);

  // Before the room has loaded, a file is not dropped silently: the composer says to add it again.
  await alice.evaluate(() => { uploadRules = null; });
  await dropFiles(alice, '.chat', [{ name: 'early.md', type: 'text/markdown', body: '# Early' }]);
  await expect(alice.locator('#send-error')).toHaveText('The room is still loading, try again in a moment');
  await expect(pending(alice)).toHaveCount(3);

  // Access ending mid-drag leaves no drop state behind.
  await alice.locator('.chat').dispatchEvent('dragenter', { dataTransfer: await alice.evaluateHandle(() => { const d = new DataTransfer(); d.items.add(new File(['x'], 'x.md')); return d; }) });
  await expect(alice.locator('body.drag-files .chat.dropping')).toHaveCount(1);
  await alice.evaluate(() => end('Your access to this room has ended.'));
  await expect(alice.locator('.dropping')).toHaveCount(0);
  await expect(alice.locator('body.drag-files')).toHaveCount(0);
});

// 029-AC6: while the request runs the composer shows its progress and sends nothing else; a
// failure keeps the text and the files with the server's error, and a retry sends them.
test('a failed send keeps the text and the files', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await alice.setViewportSize(sizes.desktop);
  let release;
  const held = new Promise((r) => { release = r; });
  let posts = 0;
  await alice.route('**/api/rooms/*/messages', async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    posts++;
    await held;
    await route.fulfill({ status: 413, contentType: 'application/json', body: JSON.stringify({ error: 'brief.md: this upload would take the room over its 200 MB limit' }) });
  });
  await dropFiles(alice, '.chat', [{ name: 'brief.md', type: 'text/markdown', body: '# Brief\n' }]);
  await alice.locator('#body').fill('Retry me');
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('#send-progress')).toBeVisible();
  await expect(alice.getByRole('button', { name: 'Send' })).toBeDisabled();
  await expect.poll(() => posts).toBe(1);
  await alice.locator('#body').press('Enter');
  // The files on their way cannot be removed meanwhile; one added since can.
  await expect(pending(alice).getByRole('button', { name: 'Remove brief.md' })).toBeDisabled();
  await dropFiles(alice, '.chat', [{ name: 'extra.md', type: 'text/markdown', body: '# Extra\n' }]);
  await expect(pending(alice)).toHaveCount(2);
  await pending(alice).getByRole('button', { name: 'Remove extra.md' }).click();
  await expect(pending(alice)).toHaveCount(1);
  release();
  await expect(alice.locator('#send-error')).toHaveText('brief.md: this upload would take the room over its 200 MB limit');
  await expect(alice.locator('#send-progress')).toBeHidden();
  await expect(alice.locator('#body')).toHaveValue('Retry me');
  await expect(pending(alice)).toHaveCount(1);
  expect(posts).toBe(1);

  await alice.unrouteAll();
  await alice.getByRole('button', { name: 'Send' }).click();
  await expect(bob.locator('.msg.text', { hasText: 'Retry me' }).getByRole('button', { name: /brief\.md/ })).toBeVisible();
  await expect(alice.locator('#send-error')).toBeEmpty();
  await expect(pending(alice)).toHaveCount(0);
});

// 029-AC8: a renamed document shows its new name in the message; a deleted one is a greyed
// "Deleted document" chip, no control, and its thumbnail is never asked for. An earlier version of
// a line sent with files only says so.
test('renamed and deleted chat documents', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await alice.setViewportSize(sizes.desktop);
  await bob.setViewportSize(sizes.desktop);
  await pasteImage(alice);
  await dropFiles(alice, '.chat', [{ name: 'brief.md', type: 'text/markdown', body: '# Brief\n' }]);
  await expect(pending(alice)).toHaveCount(2);
  await alice.locator('#body').press('Enter');
  const line = bob.locator('.msg.text').filter({ has: bob.locator('.msg-docs') });
  await expect(line.locator('.msg-thumb img')).toBeVisible();
  const docs = await bob.evaluate((id) => fetch(`/api/rooms/${id}/documents`).then((r) => r.json()), room.id);
  const img = docs.find((d) => d.format === 'image');
  const md = docs.find((d) => d.name === 'brief.md');

  expect((await alice.evaluate(([id, doc]) => fetch(`/api/rooms/${id}/documents/${doc}`, {
    method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: 'summary.md' }),
  }).then((r) => r.status), [room.id, md.id]))).toBe(204);
  await expect(line.getByRole('button', { name: /summary\.md/ })).toBeVisible();

  expect((await request.delete(`/api/rooms/${room.id}/documents/${img.id}`, { headers: admin })).status()).toBeLessThan(300);
  const gone = line.locator('.msg-doc.deleted');
  await expect(gone).toHaveText('Deleted document');
  await expect(line.locator('.msg-thumb')).toHaveCount(0);
  await expect(gone).not.toHaveAttribute('tabindex');
  expect(await gone.evaluate((e) => e.tagName)).toBe('SPAN');

  // Loaded again, the line asks nothing of the deleted file.
  const asked = [];
  bob.on('request', (r) => { if (r.url().includes(`/documents/${img.id}/`)) asked.push(r.url()); });
  await bob.reload();
  await expect(line.locator('.msg-doc.deleted')).toHaveText('Deleted document');
  await expect(line.getByRole('button', { name: /summary\.md/ })).toBeVisible();
  expect(asked).toEqual([]);

  // The line had no text: its earlier version says so.
  await messageAction(alice.locator('.msg.text').filter({ has: alice.locator('.msg-docs') }), 'Edit');
  await alice.getByRole('textbox', { name: 'New text' }).fill('Now with words');
  await alice.getByRole('textbox', { name: 'New text' }).press('Enter');
  const edited = bob.locator('.msg.text', { hasText: 'Now with words' });
  await edited.getByRole('button', { name: 'Show earlier versions' }).click();
  await expect(edited.getByRole('list', { name: 'Earlier versions' })).toContainText('(files only)');
});

// 029-AC2 029-AC9: the Documents pane's drop still adds at the root, beside the Chat folder.
test('a drop on the Documents pane still adds at the root', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(sizes.desktop);
  await dropFiles(alice, '.chat', [{ name: 'chat.md', type: 'text/markdown', body: '# Chat\n' }]);
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('#documents').getByRole('button', { name: 'Chat' })).toBeVisible();
  await dropFiles(alice, '#docs-pane', [{ name: 'root.md', type: 'text/markdown', body: '# Root\n' }]);
  await expect(alice.locator('#documents > li > button.doc', { hasText: 'root.md' })).toBeVisible();
  await expect(alice.locator('#chat-docs')).not.toContainText('root.md');
  await expect(pending(alice)).toHaveCount(0);
  await expect(alice.locator('#docs-pane .drop-overlay')).toBeHidden();
});

// 029-AC2: a pasted image takes its own type's extension; a type the room refuses is refused on the
// spot; a clipboard that also holds text pastes the text alone.
test('pasted images and their names', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(sizes.desktop);
  await expect.poll(() => alice.evaluate(() => !!window.grillRoom)).toBe(true);
  await pasteImage(alice, { text: 'A1\tB1' });
  await expect(pending(alice)).toHaveCount(0);

  const stems = await pasteImage(alice, { type: 'image/jpeg' });
  await expect(pending(alice)).toHaveCount(1);
  expect(stems.map((n) => n + '.jpg')).toContain(await pending(alice).locator('.file-name').textContent());

  const svg = await pasteImage(alice, { type: 'image/svg+xml' });
  await expect(pending(alice)).toHaveCount(2);
  expect(svg.map((n) => n + '.svg')).toContain(await pending(alice).nth(1).locator('.file-name').textContent());

  const rules = await alice.evaluate(() => window.grillRoom.upload);
  const bmp = await pasteImage(alice, { type: 'image/bmp' });
  await expect(pending(alice)).toHaveCount(2);
  expect(bmp.map((n) => n + ': ' + rules.unsupported)).toContain(await alice.locator('#send-error').textContent());

  // A text sent alone clears the refusal shown.
  await pending(alice).first().getByRole('button', { name: /^Remove / }).click();
  await expect(pending(alice)).toHaveCount(1);
  await pending(alice).first().getByRole('button', { name: /^Remove / }).click();
  await expect(pending(alice)).toHaveCount(0);
  await expect(alice.locator('#send-error')).not.toBeEmpty();
  await alice.locator('#body').fill('Just words');
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('.msg.text', { hasText: 'Just words' })).toBeVisible();
  await expect(alice.locator('#send-error')).toBeEmpty();

  // Every name a capture takes is one the Documents pane accepts.
  const exts = await alice.evaluate(() => ({ named: Object.values(captureExt), accepted: document.getElementById('upload-input').accept.split(',') }));
  expect(exts.named.length).toBeGreaterThan(0);
  for (const ext of exts.named) expect(exts.accepted).toContain(ext);
});

// 029-AC3: removing a file twice leaves the others: an entry no longer pending is ignored.
test('a file removed twice drops no other', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await expect.poll(() => alice.evaluate(() => !!window.grillRoom)).toBe(true);
  await dropFiles(alice, '.chat', [{ name: 'a.md', type: 'text/markdown', body: '# A' }, { name: 'b.md', type: 'text/markdown', body: '# B' }]);
  await expect(pending(alice)).toHaveCount(2);
  // What a send that ends after a removal replays: the sent entry dropped again.
  expect(await alice.evaluate(() => {
    const first = pending[0];
    dropPending(first);
    dropPending(first);
    return pending.map((p) => p.file.name);
  })).toEqual(['b.md']);
});

// 029-AC6: a request that never reaches the server ends the send: the text and the files stay,
// with "Not sent, retry", and the next send goes.
test('an aborted send keeps the text and the files', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(sizes.desktop);
  await alice.route('**/api/rooms/*/messages', (route) => (route.request().method() === 'POST' ? route.abort('aborted') : route.continue()));
  await dropFiles(alice, '.chat', [{ name: 'brief.md', type: 'text/markdown', body: '# Brief\n' }]);
  await alice.locator('#body').fill('Lost on the way');
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('#send-error')).toHaveText('Not sent, retry');
  await expect(alice.locator('#send-progress')).toBeHidden();
  await expect(alice.getByRole('button', { name: 'Send' })).toBeEnabled();
  await expect(alice.locator('#body')).toHaveValue('Lost on the way');
  await expect(pending(alice)).toHaveCount(1);
  await expect(pending(alice).getByRole('button', { name: 'Remove brief.md' })).toBeEnabled();
  await alice.unrouteAll();
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('.msg.text', { hasText: 'Lost on the way' }).getByRole('button', { name: /brief\.md/ })).toBeVisible();
});

// 029-AC8: a list asked for before a file was sent, answered after, is dropped: the file's chip,
// the Chat folder and a document opened from the chip stay as they are.
test('a stale documents list leaves a new chip alone', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(sizes.desktop);
  await expect.poll(() => alice.evaluate(() => !!window.grillRoom)).toBe(true);
  let release;
  const held = new Promise((r) => { release = r; });
  let caught;
  const asked = new Promise((r) => { caught = r; });
  await alice.route('**/api/rooms/*/documents', async (route) => {
    // The answer as it stands now, delivered later.
    const resp = await route.fetch();
    caught();
    await held;
    await route.fulfill({ response: resp });
  }, { times: 1 });
  await alice.evaluate(() => { loadDocuments(); });
  await asked;
  await dropFiles(alice, '.chat', [{ name: 'fresh.md', type: 'text/markdown', body: '# Fresh\n' }]);
  await alice.locator('#body').press('Enter');
  const chip = alice.locator('.msg-docs').getByRole('button', { name: /fresh\.md/ });
  await expect(chip).toBeVisible();
  // A later list, asked for once the file was stored, has drawn the folder.
  await expect(alice.locator('#chat-docs button.doc')).toHaveText(['fresh.md']);
  await chip.click();
  await expect(alice.locator('#doc-name')).toHaveText('fresh.md');
  // The stale answer, which knows no chat document yet, lands and its body is read.
  const landed = alice.waitForResponse((r) => r.url().endsWith('/documents') && r.request().method() === 'GET');
  release();
  await (await landed).finished();
  await alice.evaluate(() => new Promise((r) => setTimeout(r, 200)));
  await expect(alice.locator('#chat-docs button.doc')).toHaveText(['fresh.md']);
  await expect(alice.locator('#viewer')).toBeVisible();
  await expect(alice.locator('#doc-name')).toHaveText('fresh.md');
  await expect(chip).toBeVisible();
  await expect(alice.locator('.msg-doc.deleted')).toHaveCount(0);
});

// 029-AC6 021-AC8: an upload answered 401 "reauth" sends the page through the organisation sign-in,
// as a fetch does, from the chat composer and from the Documents pane alike.
for (const [where, target] of [['the chat', '.chat'], ['the Documents pane', '#docs-pane']]) {
  test(`an expired sign-in on an upload from ${where} signs in again`, async ({ browser, request }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(sizes.desktop);
    await expect.poll(() => alice.evaluate(() => !!window.grillRoom)).toBe(true);
    const path = target === '.chat' ? 'messages' : 'documents';
    await alice.route(`**/api/rooms/*/${path}`, (route) => (route.request().method() === 'POST'
      ? route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: 'sign in again', reason: 'reauth' }) })
      : route.continue()));
    await alice.route('**/auth/login*', (route) => route.fulfill({ status: 200, contentType: 'text/html', body: '<p>provider</p>' }));
    await dropFiles(alice, target, [{ name: 'brief.md', type: 'text/markdown', body: '# Brief\n' }]);
    if (target === '.chat') await alice.locator('#body').press('Enter');
    await alice.waitForURL((u) => u.pathname === '/auth/login' && u.searchParams.get('prompt') === 'none' && u.searchParams.get('next') === `/r/${room.id}`);
  });
}
