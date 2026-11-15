import { test, expect } from './fixtures.js';
import { fileURLToPath } from 'node:url';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, noOverflow, upload, annotateSelection, menuItem, docAction, newRoomEditor, rowAction, unfoldTabs, fakeDaemon, agentState } from './helpers.js';

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
      // 009-AC36: the details card says it, no native title.
      await expect(line).not.toHaveAttribute('title');
      await line.focus();
      await expect(page.locator('#person-card')).toContainText('Written as Alice');
      await page.keyboard.press('Escape');
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
    await newRoomEditor(prof);
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

  // 005-AC21 005-AC15: a room admin renames Bob from the Members tab of Room settings, a wide sheet with
  // deep-linked tabs, full screen on a phone; Bob gets a notice. The super-admin renames him from
  // /admin. Both are audited.
  test(`admin rename and Room settings tabs, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await makeAdmin(request, alice, room);
    await alice.setViewportSize(vp);
    // Reloaded, the page reads Alice's new role.
    await alice.reload();
    await alice.goto(`/r/${room.id}#manage/admins`);
    const sheet = alice.locator('#manage');
    await expect(sheet).toBeVisible();
    await expect(sheet.locator('#tab-admins')).toHaveAttribute('aria-current', 'page');
    await expect(sheet.locator('#tab-general, #tab-invites, #tab-admins, #tab-links, #tab-documents, #tab-export')).toHaveCount(6);
    const box = await sheet.boundingBox();
    if (vp.width < 400) expect(box.width).toBe(vp.width);
    else expect(box.width).toBeGreaterThan(900);
    expect(await noOverflow(alice)).toEqual([]);

    // 040-AC11: the row's ⋯ is People's, Rename opening the same sheet.
    await rowAction(sheet.locator('#room-admins > li', { hasText: 'Bob' }), /^Rename$/);
    await expect(alice.locator('#rename-title')).toHaveText('Rename Bob');
    await alice.locator('#rename').getByLabel('Display name').fill('Robert');
    await alice.locator('#rename-form').getByRole('button', { name: 'Rename' }).click();
    await expect(bob.locator('#notice')).toHaveText(/Your name in this room is now Robert\./);
    await expect(bob.locator('#me')).toHaveText('Robert');
    await expect(bob.locator('.msg.system').last()).toContainText('Alice renamed Bob to Robert');

    // The tabs are links: the keyboard reaches them, and each sets the deep link.
    await unfoldTabs(sheet.locator('nav.tabs'));
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
    await rowAction(admins.locator('> li', { hasText: 'Robert' }), 'Rename Robert');
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
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await alice.setViewportSize(vp);
    // Each opens from a header menu: focus goes back to that menu's button.
    for (const [item, which, id] of [['Personas', 'room', '#ai'], ['Room settings', 'room', '#manage'], ['Change my name', 'me', '#rename']]) {
      const opener = alice.locator(which === 'me' ? '#me' : '#room-open');
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
    await bob.locator('#doc-more').click();
    await expect(bob.locator('#doc-menu').getByRole('button', { name: 'Rename' })).toBeVisible();
    await expect(bob.locator('#doc-delete')).toBeHidden();
    await bob.keyboard.press('Escape');
    await docAction(bob, 'Rename');
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
    await docAction(alice, 'Delete');
    await expect(alice.locator('#viewer')).toBeHidden();
    await expect(bob.locator('#documents li')).toHaveCount(0);
    await alice.locator('#toasts').getByRole('button', { name: 'Undo' }).click();
    await expect(bob.locator('#documents')).toHaveText('roadmap.md');
    await alice.locator('#documents').getByRole('button', { name: 'roadmap.md' }).click();
    await docAction(alice, 'Delete');
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
    await expect(chips.first()).toHaveAttribute('data-tip', 'Grill, persona');
    await expect(chips.nth(1)).toHaveClass(/\bme\b/);
    await expect(line).toHaveClass(/mentions-me/);
    // Its text sits clear of the bar, in line with the other messages.
    const pad = await line.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft));
    expect(pad).toBeGreaterThan(3);
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

// wholeTab: the bar's current tab shows whole, or, the bar folded, as the name on its button.
const wholeTab = (nav) => nav.evaluate((n) => {
  const cur = n.querySelector('[aria-current=page]');
  const b = 'menu' in n.dataset ? n.querySelector('.tabs-menu') : cur;
  const [r, box] = [b.getBoundingClientRect(), n.getBoundingClientRect()];
  return { name: (b.textContent || '').trim(), whole: r.left >= box.left - 0.5 && r.right <= box.right + 0.5 && b.scrollWidth <= b.clientWidth + 0.5 };
});

// 038-AC9 040-AC1: Room settings names its tabs Members and Deleted documents (/admin: Members and admins), their deep links
// unchanged; the blocked profiles show under the member list, no longer under Invites. The longer
// names fold the bar when it overflows: no sideways overflow, the current tab whole, at 390, 820 and 1440 px.
test('Room settings: Members, Deleted documents', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await makeAdmin(request, alice, room);
  const bobID = await bob.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.me.id), room.id);
  expect((await request.delete(`/api/admin/rooms/${room.id}/participants/${bobID}`, { headers: admin, data: { block: true } })).status()).toBe(204);
  const sheet = alice.locator('#manage');
  const nav = sheet.locator('nav.tabs');
  const shots = fileURLToPath(new URL('./test-results/038-captures/', import.meta.url));
  for (const width of [390, 820, 1440]) {
    await alice.setViewportSize({ width, height: 900 });
    await alice.goto('about:blank');
    await alice.goto(`/r/${room.id}#manage/admins`);
    await expect(sheet.locator('#tab-admins')).toHaveAttribute('aria-current', 'page');
    await expect(sheet.locator('#tab-admins')).toHaveText('Members');
    const [members, blocked] = [sheet.locator('#room-admins'), sheet.locator('#blocked')];
    await expect(blocked).toContainText('Bob');
    await expect(sheet.getByRole('heading', { name: 'Blocked from rejoining' })).toBeVisible();
    const [m, b] = [await members.boundingBox(), await blocked.boundingBox()];
    expect(b.y, `${width}: blocked under the members`).toBeGreaterThanOrEqual(m.y + m.height);
    await expect.poll(() => wholeTab(nav), `${width}: admins tab`).toEqual({ name: 'Members', whole: true });
    expect(await noOverflow(alice), `${width} px`).toEqual([]);
    if (width !== 820) await alice.screenshot({ path: `${shots}room-settings-members-${width}.png` });

    await alice.goto('about:blank');
    await alice.goto(`/r/${room.id}#manage/documents`);
    await expect(sheet.locator('#tab-documents')).toHaveAttribute('aria-current', 'page');
    await expect(sheet.locator('#tab-documents')).toHaveText('Deleted documents');
    await expect.poll(() => wholeTab(nav), `${width}: documents tab`).toEqual({ name: 'Deleted documents', whole: true });
    expect(await noOverflow(alice), `${width} px`).toEqual([]);
  }
  // Invites no longer mounts the blocked list: it would fetch the blocks in the same pass as the invites.
  const asked = [];
  const log = (r) => asked.push(new URL(r.url()).pathname);
  await alice.goto('about:blank');
  alice.on('request', log);
  await alice.goto(`/r/${room.id}#manage/invites`);
  await expect(sheet.locator('#panel-invites')).toBeVisible();
  await expect(sheet.locator('#invites-all')).toBeVisible();
  alice.off('request', log);
  expect(asked).toContain(`/api/rooms/${room.id}/invites`);
  expect(asked).not.toContain(`/api/rooms/${room.id}/blocks`);
  await expect(sheet.locator('#blocked')).toHaveCount(0);
  // /admin's room panel shares the tab and the section.
  const page = await (await browser.newContext()).newPage();
  expect(await signIn(page, undefined, `rooms/${room.id}/admins`)).toBe(201);
  await expect(page.locator('#room-panel #tab-admins')).toHaveText('Members and admins');
  await expect(page.locator('#room-panel #blocked')).toContainText('Bob');
});

// menuItems reads a menu's buttons as [label, checked, disabled, reason], '' for an absent attribute.
const menuItems = (menu) => menu.locator('button').evaluateAll((bs) => bs.map((b) => [(b.textContent || '').trim(),
  b.getAttribute('aria-pressed') || '', b.getAttribute('aria-disabled') || '', b.dataset.tip || '']));
// callItems are People's own, which Members does not show (040-AC11).
const callItems = ['Lower hand'];

// peopleItems opens name's ⋯ in People and returns its items and whether it holds a volume.
async function peopleItems(page, name) {
  await page.locator('#participants li, #participants-away li').filter({ has: page.locator('.name', { hasText: new RegExp(`^${name}$`) }) })
    .first().locator('.person-more').click();
  const menu = page.locator('#person-menu');
  await expect(menu).toBeVisible();
  const items = await menuItems(menu);
  const volume = await menu.getByRole('slider').count();
  await page.keyboard.press('Escape');
  await expect(menu).toBeHidden();
  return { items, volume };
}

// memberItems opens Room settings › Members and returns the items of name's row ⋯; the sheet closes.
async function memberItems(page, name) {
  await page.evaluate(() => { location.hash = '#manage/admins'; });
  const row = page.locator('#manage #room-admins > li').filter({ has: page.locator('.name', { hasText: new RegExp(`^${name}$`) }) });
  await row.locator('.actions .more').click();
  const menu = row.locator('.menu');
  await expect(menu).toBeVisible();
  const items = await menuItems(menu);
  await page.keyboard.press('Escape');
  await expect(menu).toBeHidden();
  await page.keyboard.press('Escape');
  await expect(page.locator('#manage')).toBeHidden();
  // Its close event, a task later, drops the hash: the next open must come after it.
  await expect.poll(() => page.evaluate(() => location.hash)).toBe('');
  return items;
}

// same reads name's membership actions in both menus and checks they match, as want.
async function same(page, name, want) {
  const people = await peopleItems(page, name);
  const membership = people.items.filter(([label]) => !callItems.includes(label));
  expect(membership, `People ⋯ of ${name}`).toEqual(want);
  expect(await memberItems(page, name), `Members ⋯ of ${name}`).toEqual(want);
  return people;
}

// 040-AC11 040-AC12: People's ⋯ and Room settings › Members' row ⋯ show one set of membership actions,
// labels, order, checked and disabled states and reasons alike: a room admin who is also the AI host,
// on another row and on their own as the last room admin, then with a second room admin; an AI host who
// is not a room admin and a plain member read People only. Lower hand and Volume stay in People.
test('membership actions: People and Members alike', async ({ browser, request, baseURL }) => {
  test.setTimeout(120_000);
  const { room, pages: [alice, bob, carol, dan] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol', 'Dan']);
  await makeAdmin(request, alice, room);
  await alice.reload();
  const stops = [await fakeDaemon(alice, baseURL, room.id, 'alice-laptop')];
  try {
    await expect.poll(async () => (await agentState(alice, room.id)).host_participant_id).toBeTruthy();
    await bob.getByRole('button', { name: 'Raise my hand' }).click();
    const off = (label) => [label, '', '', ''];
    const bobWant = [off('Rename'), off('Make room admin'), ['Can ask the AI', 'false', '', ''], off('Remove from room…')];
    await expect(alice.locator('#participants li', { hasText: 'Bob' }).locator('.tag-hand:not(.blank)')).toHaveText('1');
    const people = await same(alice, 'Bob', bobWant);
    expect(people.items[0][0], 'Lower hand, People only').toBe('Lower hand');
    // The last room admin: Step down off with its reason, Leave room… with 038-AC10's.
    await same(alice, 'Alice', [['Step down', '', 'true', 'Make someone else room admin first.'],
      ['Leave room…', '', 'true', 'Make someone else room admin, then step down.']]);

    // A toggle from Members: both menus show it ticked, and the focus is back on the row's ⋯.
    await alice.evaluate(() => { location.hash = '#manage/admins'; });
    const bobRow = alice.locator('#manage #room-admins > li', { hasText: 'Bob' });
    await rowAction(bobRow, 'Can ask the AI');
    await expect(bobRow).toContainText('can ask the AI');
    await expect(bobRow.locator('.actions .more')).toBeFocused();
    await alice.keyboard.press('Escape');
    bobWant[2] = ['Can ask the AI', 'true', '', ''];
    await same(alice, 'Bob', bobWant);

    // A second room admin: Carol's removal waits for her role, and Alice may step down.
    expect((await request.put(`/api/rooms/${room.id}/admins/${await carol.evaluate(() => myID)}`, { headers: admin })).status()).toBe(204);
    await expect(alice.locator('#participants li', { hasText: 'Carol' }).locator('.sigil')).toHaveText('@');
    await same(alice, 'Carol', [off('Rename'), off('Remove room admin'), ['Can ask the AI', 'false', '', ''],
      ['Remove from room…', '', 'true', 'Remove their room admin role first']]);
    await same(alice, 'Alice', [off('Step down'), ['Leave room…', '', 'true', 'Step down as room admin first']]);

    // Bob's app connects: Make AI host shows in both; Alice hands the role over from Members.
    stops.push(await fakeDaemon(bob, baseURL, room.id, 'bob-laptop'));
    bobWant.splice(3, 0, off('Make AI host'));
    await expect.poll(async () => (await peopleItems(alice, 'Bob')).items.map(([l]) => l)).toContain('Make AI host');
    await same(alice, 'Bob', bobWant);
    await alice.evaluate(() => { location.hash = '#manage/admins'; });
    await rowAction(bobRow, 'Make AI host');
    await expect(bobRow).toContainText('AI host');
    await alice.keyboard.press('Escape');

    // The AI host who is no room admin: People only, as before 040.
    await expect.poll(async () => (await peopleItems(bob, 'Alice')).items).toEqual([['Can ask the AI', 'false', '', ''], off('Make AI host')]);
    await bob.evaluate(() => { location.hash = '#manage'; });
    await expect(bob.locator('#manage #tab-invites')).toHaveAttribute('aria-current', 'page');
    await expect(bob.locator('#manage #tab-admins')).toHaveCount(0);
    await bob.keyboard.press('Escape');
    // A plain member: Leave room… on their own row, nothing on others'.
    expect((await peopleItems(dan, 'Dan')).items).toEqual([off('Leave room…')]);
    await expect(dan.locator('#participants li', { hasText: 'Alice' }).locator('.person-more')).toBeDisabled();
  } finally {
    for (const stop of stops) stop();
  }
});

// 040-AC10 040-AC13 040-AC17 003: Room settings › Members lists every seat, two seats of one profile as
// two rows, each with the invite it joined through, named or "Link <id>", and the badges; a plain member's
// room payload carries no invite and they get no Members tab. Blocked from rejoining shows when empty.
// /admin's Members and admins shows the same invites.
test('Members: one row per seat, with its invite', async ({ browser, request, baseURL }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Seats room' } })).json();
  const invite = async (name) => (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin, data: name ? { name } : {} })).json();
  const joinPage = async (name, inv) => {
    const page = await (await browser.newContext()).newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    return page;
  };
  const alice = await joinPage('Alice', await invite('Founders'));
  const team = await invite('Team link');
  const bob = await joinPage('Bob', team);
  const plain = await invite('');
  // Bob's second seat, same profile, through another invite.
  expect((await bob.request.post('/api/join', { data: { invite: new URL(plain.url).hash.slice(1), name: 'Bob' } })).status()).toBe(201);
  const carol = await joinPage('Carol', team);
  await makeAdmin(request, alice, room);
  const host = await fakeDaemon(alice, baseURL, room.id, 'alice-laptop');
  try {
    // Bob's seat through the team link, as the admin-only overview says.
    const seats = (await alice.evaluate((id) => fetch(`/api/rooms/${id}/overview`).then((r) => r.json()), room.id)).seats;
    const bobSeat = seats.find((x) => x.invite_id === team.id).participant_id;
    expect(await alice.evaluate(([id, p]) => fetch(`/api/rooms/${id}/participants/${p}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ can_trigger: true }) }).then((r) => r.status), [room.id, bobSeat])).toBe(204);
    // Reloaded, the page reads Alice's new role.
    await alice.goto('about:blank');
    await alice.goto(`/r/${room.id}#manage/admins`);
    const rows = alice.locator('#manage #room-admins > li');
    await expect(rows).toHaveCount(4);
    const cells = async () => (await rows.evaluateAll((ls) => ls.map((l) => [l.querySelector('.name').textContent,
      [...l.querySelectorAll('.main .badge')].map((b) => b.textContent).join(', '), l.querySelector('.cell.invite')?.textContent || ''])));
    await expect.poll(cells).toEqual([['Alice', 'room admin, AI host', 'Founders'], ['Bob', 'can ask the AI', 'Team link'],
      ['Bob', '', 'Link ' + plain.id], ['Carol', '', 'Team link']]);
    await expect(alice.locator('#manage #blocked-empty')).toHaveText('Nobody is blocked.');
    await expect(alice.locator('#manage').getByRole('heading', { name: 'Blocked from rejoining' })).toBeVisible();

    // 040-AC11: a focus moved while a Members action is in flight stays where the user put it.
    await alice.route(`**/api/rooms/${room.id}/admins/*`, async (route) => {
      if (route.request().method() === 'PUT') await new Promise((r) => setTimeout(r, 1500));
      await route.fallback();
    });
    await rowAction(rows.nth(1), 'Make room admin');
    const carolMore = rows.nth(3).locator('.actions .more');
    await carolMore.focus();
    await expect(rows.nth(1).locator('.badge.role')).toHaveText('room admin');
    await expect(carolMore).toBeFocused();
    await alice.unroute(`**/api/rooms/${room.id}/admins/*`);

    // Carol, a plain member: no invite in what the room tells her, no Members tab.
    const payload = await carol.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.text()), room.id);
    expect(payload).not.toMatch(/invite_(id|name)|Team link|Founders/);
    expect((await carol.request.get(`/api/rooms/${room.id}/overview`)).status()).toBe(403);
    await carol.goto('about:blank');
    await carol.goto(`/r/${room.id}#manage/admins`);
    await expect(carol.locator('#manage #tab-invites')).toHaveAttribute('aria-current', 'page');
    await expect(carol.locator('#manage #tab-admins, #manage #room-admins')).toHaveCount(0);

    const page = await (await browser.newContext()).newPage();
    expect(await signIn(page, undefined, `rooms/${room.id}/admins`)).toBe(201);
    await expect(page.locator('#room-panel #tab-admins')).toHaveText('Members and admins');
    await expect(page.locator('#room-panel #room-admins .cell.invite')).toHaveText(['Founders', 'Team link', 'Link ' + plain.id, 'Team link']);
    await expect(page.locator('#room-panel #blocked-empty')).toHaveText('Nobody is blocked.');
  } finally {
    host();
  }
});
