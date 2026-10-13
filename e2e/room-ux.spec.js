import { test, expect } from '@playwright/test';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, noOverflow, upload, annotateSelection, menuItem } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const widths = [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }];

// makeAdmin appoints page's participant a room admin of room, through the super-admin.
async function makeAdmin(request, page, room) {
  const me = await page.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  return me.me.id;
}

for (const vp of widths) {
  // 005-AC1: Alice renames herself from the room page, with the validation of a join, and makes it
  // her profile's name. Everyone reads "<old> is now <new>"; her past line shows the new name, the
  // old one on hover.
  test(`self-rename, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await alice.setViewportSize(vp);
    await alice.locator('#body').fill('Before the rename.');
    await alice.locator('#body').press('Enter');
    await expect(bob.locator('#messages')).toContainText('Before the rename.');

    await menuItem(alice, 'Change my name', 'me');
    const dialog = alice.locator('#rename');
    await expect(dialog.getByLabel('Display name')).toHaveValue('Alice');
    await dialog.getByLabel('Display name').fill('x'.repeat(40) + 'y');
    await expect(dialog.getByLabel('Display name')).toHaveValue('x'.repeat(40));
    await dialog.getByLabel('Display name').fill('   ');
    await dialog.getByRole('button', { name: 'Rename' }).click();
    await expect(dialog.locator('#rename-status')).toHaveText('display name must be 1 to 40 characters');
    await dialog.getByLabel('Display name').fill('Ali');
    await dialog.getByLabel('Also use it for my profile').check();
    await dialog.getByRole('button', { name: 'Rename' }).click();
    await expect(dialog).toBeHidden();
    await expect(alice.locator('#me')).toHaveText('Ali');
    // Her own rename raises no "your name is now" notice.
    await expect(alice.locator('#notice-text')).not.toContainText('Your name in this room');
    for (const page of [alice, bob]) {
      await expect(page.locator('.msg.system').last()).toContainText('Alice is now Ali');
      const line = page.locator('.msg.text', { hasText: 'Before the rename.' }).locator('.sender');
      await expect(line).toHaveText('Ali');
      await expect(line).toHaveAttribute('title', 'Written as Alice');
      await expect(page.locator('#participants')).toContainText('Ali');
    }
    // A later join under another name leaves the profile's name alone.
    const other = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Other room' } })).json();
    const inv = await (await request.post(`/api/rooms/${other.id}/invites`, { headers: admin })).json();
    const prof = await alice.context().newPage();
    await prof.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await prof.getByLabel('Your name').fill('Alice again');
    await prof.getByRole('button', { name: 'Join the room' }).click();
    await expect(prof.locator('#me')).toHaveText('Alice again');
    await prof.goto('/profile');
    await prof.getByRole('button', { name: 'New room' }).click();
    await expect(prof.getByLabel('Your name in the room')).toHaveValue('Ali');
    await prof.close();
    expect(await noOverflow(alice)).toEqual([]);

    // Without a profile, the option is not offered.
    const profiles = await (await request.get('/api/admin/profiles', { headers: admin })).json();
    const bobs = profiles.filter((p) => p.rooms.some((r) => r.name === 'Bob' && r.room_id === room.id));
    expect(bobs).toHaveLength(1);
    expect((await request.delete(`/api/admin/profiles/${bobs[0].id}`, { headers: admin })).status()).toBe(204);
    await bob.reload();
    await menuItem(bob, 'Change my name', 'me');
    await expect(bob.locator('#rename').getByLabel('Display name')).toHaveValue('Bob');
    await expect(bob.locator('#rename-profile-label')).toBeHidden();
  });

  // 005-AC21 005-AC15: a room admin renames Bob from the Admins tab of Manage, a wide sheet with
  // deep-linked tabs, full screen on a phone; Bob gets a notice. The super-admin renames him from
  // /admin. Both are audited.
  test(`admin rename and Manage tabs, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await makeAdmin(request, alice, room);
    await alice.setViewportSize(vp);
    await alice.goto(`/r/${room.id}#manage/admins`);
    const sheet = alice.locator('#manage');
    await expect(sheet).toBeVisible();
    await expect(sheet.getByRole('link', { name: 'Admins', exact: true })).toHaveAttribute('aria-current', 'page');
    await expect(sheet.locator('#tab-general, #tab-invites, #tab-admins, #tab-personas, #tab-skills, #tab-documents, #tab-export, #tab-glossary')).toHaveCount(8);
    const box = await sheet.boundingBox();
    if (vp.width < 400) expect(box.width).toBe(vp.width);
    else expect(box.width).toBeGreaterThan(900);
    expect(await noOverflow(alice)).toEqual([]);

    await sheet.getByRole('button', { name: 'Rename Bob' }).click();
    await sheet.getByLabel('New name for Bob').fill('Robert');
    await sheet.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(bob.locator('#notice')).toHaveText(/Your name in this room is now Robert\./);
    await expect(bob.locator('#me')).toHaveText('Robert');
    await expect(bob.locator('.msg.system').last()).toContainText('Alice renamed Bob to Robert');

    // The tabs are links: the keyboard reaches them, and each sets the deep link.
    await sheet.getByRole('link', { name: 'Invites', exact: true }).focus();
    await alice.keyboard.press('Enter');
    await expect(alice).toHaveURL(/#manage\/invites$/);
    await expect(sheet.locator('#invites-all')).toBeVisible();
    await alice.keyboard.press('Escape');
    await expect(sheet).toBeHidden();
    await expect(alice).toHaveURL(new RegExp(`/r/${room.id}$`));

    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, `rooms/${room.id}/admins`)).toBe(201);
    const admins = page.locator('#room-panel #room-admins');
    await admins.getByRole('button', { name: 'Rename Robert' }).click();
    await admins.getByLabel('New name for Robert').fill('Bobby');
    await admins.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(bob.locator('#me')).toHaveText('Bobby');
    await expect(bob.locator('.msg.system').last()).toContainText('The admin renamed Robert to Bobby');
    expect(await noOverflow(page)).toEqual([]);
    const log = await (await request.get(`/api/admin/audit?action=participant.rename&target=${room.id}`, { headers: admin })).json();
    expect(log.entries.map((e) => [e.details.from, e.details.to])).toEqual([['Robert', 'Bobby'], ['Bob', 'Robert']]);
  });

  // 005-AC13: every modal closes on a click outside it and on Escape, and gives focus back to what
  // opened it. A press that starts inside and ends outside does not close it.
  test(`modal dismissal, ${vp.name}`, async ({ browser, request }) => {
    const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(vp);
    // Each opens from a header menu: focus goes back to that menu's button.
    for (const [item, which, id] of [['Personas', 'more', '#personas'], ['Room settings', 'more', '#manage'], ['Change my name', 'me', '#rename']]) {
      const opener = alice.locator(which === 'me' ? '#me' : '#more-open');
      const dialog = alice.locator(id);
      await menuItem(alice, item, which);
      await expect(dialog).toBeVisible();
      await alice.keyboard.press('Escape');
      await expect(dialog).toBeHidden();
      await expect(opener).toBeFocused();

      await menuItem(alice, item, which);
      await expect(dialog).toBeVisible();
      const b = await dialog.boundingBox();
      // A drag from inside to outside keeps it open.
      await alice.mouse.move(b.x + 20, b.y + 20);
      await alice.mouse.down();
      await alice.mouse.move(2, vp.height - 2);
      await alice.mouse.up();
      if (b.width < vp.width) {
        await expect(dialog).toBeVisible();
        await alice.mouse.click(2, vp.height - 2);
        await expect(dialog).toBeHidden();
        await expect(opener).toBeFocused();
      } else {
        // Full screen on a phone: no outside to click, Escape stays.
        await alice.keyboard.press('Escape');
        await expect(dialog).toBeHidden();
      }
    }
  });
}

// 005-AC12: in a background tab the title counts others' lines and syntheses, voice lines only once
// opted in, and resets on focus.
test('unread count in the title', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await expect(alice).toHaveTitle('Docs room · grill-room');
  // Playwright keeps every page focused: say this one is not.
  await alice.evaluate(() => { document.hasFocus = () => false; });
  for (const text of ['one', 'two']) {
    await bob.locator('#body').fill(text);
    await bob.locator('#body').press('Enter');
    // The box clears once the line is sent: typing the next one before would lose it.
    await expect(bob.locator('#body')).toHaveValue('');
    await expect(alice.locator('#messages')).toContainText(text);
  }
  await expect(alice).toHaveTitle('(2) Docs room · grill-room');
  // Its own lines do not count.
  await alice.evaluate((id) => fetch(`/api/rooms/${id}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"body":"mine"}' }), room.id);
  await expect(alice.locator('#messages')).toContainText('mine');
  await expect(alice).toHaveTitle('(2) Docs room · grill-room');
  // Voice lines: the e2e server has no STT worker, so feed the counter one as the stream would.
  const voice = () => alice.evaluate(() => countUnread({ kind: 'voice', ts: new Date().toISOString(), participant_id: -1 }));
  await voice();
  await expect(alice).toHaveTitle('(2) Docs room · grill-room');
  await menuItem(alice, 'Notifications', 'me');
  await alice.getByLabel('Count voice lines in the tab title').check();
  await voice();
  await expect(alice).toHaveTitle('(3) Docs room · grill-room');
  // 005-AC27: a line naming her marks the count.
  await bob.locator('#body').fill('@Alice look');
  await bob.locator('#body').press('Enter');
  await expect(alice).toHaveTitle('(4 @) Docs room · grill-room');
  await alice.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(alice).toHaveTitle('Docs room · grill-room');
});

// 005-AC11: "Open in a new window" opens the whole document manager at /r/<room>/docs, live: a doc
// uploaded in the room shows there, an annotation made there shows in the room. Its address
// follows the document shown, and /r/<room>/docs/<doc> opens one document, at desktop and phone
// width.
test('document manager in its own window', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await upload(alice, 'plan.md', 'text/markdown', '# Rollout plan\n\nThe rollback step comes first.\n');
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  const [docs] = await Promise.all([alice.context().waitForEvent('page'), alice.locator('#docs-pane').getByRole('link', { name: 'Open documents in a new window' }).click()]);
  await expect(docs).toHaveURL(new RegExp(`/r/${room.id}/docs$`));
  await expect(docs).toHaveTitle('Documents · Docs room · grill-room');
  await expect(docs.locator('#messages')).toBeHidden();
  await expect(docs.locator('#voice')).toBeHidden();
  await docs.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(docs.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  const id = new URL(docs.url()).pathname.split('/')[4];
  expect(docs.url()).toMatch(new RegExp(`/r/${room.id}/docs/${id}$`));

  await annotateSelection(docs, 'rollback step', 'Who runs it?');
  await bob.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(bob.locator('#annot-list')).toContainText('Who runs it?');
  await upload(bob, 'notes.md', 'text/markdown', '# Notes\n');
  await expect(docs.locator('#documents')).toContainText('notes.md');
  // The viewer's own link opens the document shown.
  await expect(bob.locator('#doc-window')).toHaveAttribute('href', new RegExp(`/r/${room.id}/docs/[^/]+$`));

  // A deep link on a phone: the document first, the list once it is closed.
  const phone = await bob.context().newPage();
  await phone.setViewportSize({ width: 390, height: 844 });
  await phone.goto(`/r/${room.id}/docs/${id}`);
  await expect(phone.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  await expect(phone.locator('#annot-list')).toContainText('Who runs it?');
  expect(await noOverflow(phone)).toEqual([]);
  await phone.getByRole('button', { name: 'Close document' }).click();
  await expect(phone).toHaveURL(new RegExp(`/r/${room.id}/docs$`));
  await expect(phone.locator('#documents')).toBeVisible();
  expect(await noOverflow(phone)).toEqual([]);
});

// 005-AC24: the Notifications section offers browser notifications, and on an iPhone outside the
// home screen says they need the room added there. A real push runs in push/ (make e2e-push):
// Chrome has no Push API in Playwright's contexts.
test('notification controls, desktop and iPhone', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await menuItem(alice, 'Notifications', 'me');
  await expect(alice.getByRole('button', { name: 'Turn on notifications' })).toBeVisible();
  await expect(alice.locator('#push-hint')).toBeHidden();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ua = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';
  const phone = await (await browser.newContext({ userAgent: ua, viewport: { width: 390, height: 844 } })).newPage();
  await phone.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await phone.getByLabel('Your name').fill('Ian');
  await phone.getByRole('button', { name: 'Join the room' }).click();
  await menuItem(phone, 'Notifications', 'me');
  await expect(phone.locator('#push-hint')).toHaveText(/add.*home screen|Add to Home Screen/i);
  expect(await noOverflow(phone)).toEqual([]);
});

// 005-AC24: a notification's link, /r/<room>#m<id>, opens the chat scrolled to that line.
test('a deep link to a line scrolls the chat there', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const bob = await (await request.post('/api/join', { data: { invite: inv.token, name: 'Bob' } })).json();
  const post = async (body) => (await (await request.post(`/api/rooms/${room.id}/messages`, { headers: { Authorization: 'Bearer ' + bob.token }, data: { body } })).json()).id;
  const target = await post('the line to find');
  for (let i = 0; i < 60; i++) await post('filler ' + i);
  const page = await alice.context().newPage();
  await page.goto(`/r/${room.id}#m${target}`);
  await expect(page.locator('#messages li', { hasText: 'filler 59' })).toHaveCount(1);
  await expect(page.locator(`#messages li[data-id="${target}"]`)).toBeInViewport();
  await expect(page.locator('#messages li', { hasText: 'filler 59' })).not.toBeInViewport();
});

for (const vp of widths) {
  // 005-AC25 005-AC26: Bob renames a document from the viewer and everyone sees the new name and
  // the chat line; Alice, room admin, deletes it, and Bob's viewer closes; Bob has no Delete. Alice
  // restores it from Manage, Documents.
  test(`document rename, delete and restore, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await makeAdmin(request, alice, room);
    await alice.reload();
    await bob.setViewportSize(vp);
    await upload(bob, 'plan.md', 'text/markdown', '# Plan\n');
    await expect(bob.locator('#doc-name')).toHaveText('plan.md');
    await expect(bob.locator('#doc-delete')).toBeHidden();
    await bob.getByRole('button', { name: 'Rename', exact: true }).click();
    await bob.getByLabel('New name').fill('roadmap.txt');
    await bob.locator('#doc-rename-form').getByRole('button', { name: 'Save', exact: true }).click();
    await expect(bob.locator('#doc-rename-status')).toHaveText('the name must keep the extension .md');
    await bob.getByLabel('New name').fill('roadmap.md');
    await bob.locator('#doc-rename-form').getByRole('button', { name: 'Save', exact: true }).click();
    await expect(bob.locator('#doc-name')).toHaveText('roadmap.md');
    expect(await noOverflow(bob)).toEqual([]);
    await expect(alice.locator('#documents')).toHaveText('roadmap.md');
    await expect(alice.locator('.msg.system').last()).toContainText('Bob renamed `plan.md` → `roadmap.md`');

    await alice.locator('#documents').getByRole('button', { name: 'roadmap.md' }).click();
    // 005-AC39: no native dialog, an Undo toast instead; Undo restores, a second delete stays.
    await alice.locator('#doc-delete').click();
    await expect(alice.locator('#viewer')).toBeHidden();
    await expect(bob.locator('#documents li')).toHaveCount(0);
    await alice.locator('#toasts').getByRole('button', { name: 'Undo' }).click();
    await expect(bob.locator('#documents')).toHaveText('roadmap.md');
    await alice.locator('#documents').getByRole('button', { name: 'roadmap.md' }).click();
    await alice.locator('#doc-delete').click();
    await expect(alice.locator('#viewer')).toBeHidden();
    await expect(bob.locator('#viewer')).toBeHidden();
    await expect(bob.locator('#documents li')).toHaveCount(0);
    await expect(bob.locator('.msg.system').last()).toContainText('Alice deleted the document `roadmap.md`');

    await alice.goto(`/r/${room.id}#manage/documents`);
    const sheet = alice.locator('#manage');
    await sheet.getByRole('button', { name: 'Restore roadmap.md' }).click();
    await expect(sheet.locator('#deleted-documents')).toHaveCount(0);
    await expect(bob.locator('#documents')).toHaveText('roadmap.md');
  });
}

for (const vp of widths) {
  // 005-AC27: @Grill draws a persona chip with its badge and colour, @Ana_María a person chip,
  // @nobody stays text, and code stays code. Bob's line names Ana: her page highlights it, and its
  // text still reads "@Ana_María". Chips take the keyboard focus.
  test(`mention chips and the self-mention highlight, ${vp.name}`, async ({ browser, request }) => {
    const { pages: [ana, bob] } = await roomWith(browser, request, ['Ana María', 'Bob']);
    await ana.setViewportSize(vp);
    await bob.locator('#body').fill('@grill ask @Ana_María, not @nobody or `@Grill` <img src=x onerror=alert(1)>');
    await bob.locator('#body').press('Enter');
    const line = ana.locator('#messages li.msg.text', { hasText: 'ask' });
    const chips = line.locator('mark.mention-chip');
    await expect(chips).toHaveText(['@grill', '@Ana_María']);
    await expect(chips.first()).toHaveClass(/persona/);
    await expect(chips.first()).toHaveAttribute('data-badge', '🔥');
    await expect(chips.first()).toHaveAttribute('title', 'Grill, persona');
    await expect(chips.nth(1)).toHaveClass(/\bme\b/);
    await expect(line).toHaveClass(/mentions-me/);
    await expect(line.locator('code')).toHaveText('@Grill');
    await expect(line.locator('.body')).toContainText('@grill ask @Ana_María, not @nobody or @Grill');
    await expect(line.locator('img')).toHaveCount(0);
    await chips.first().focus();
    await expect(chips.first()).toBeFocused();
    // Bob is not named: no highlight on his page, Ana's chip is a person's.
    const his = bob.locator('#messages li.msg.text', { hasText: 'ask' });
    await expect(his).not.toHaveClass(/mentions-me/);
    await expect(his.locator('mark.mention-chip').nth(1)).toHaveClass(/person/);
    expect(await noOverflow(ana)).toEqual([]);
  });
}
