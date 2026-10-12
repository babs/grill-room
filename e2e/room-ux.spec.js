import { test, expect } from '@playwright/test';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, noOverflow } from './helpers.js';

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

    await alice.getByRole('button', { name: 'Change your name' }).click();
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
    await expect(alice.locator('#notice')).toBeHidden();
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
    await bob.getByRole('button', { name: 'Change your name' }).click();
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
    await expect(sheet.locator('#tab-general, #tab-invites, #tab-admins, #tab-personas, #tab-skills, #tab-export, #tab-glossary')).toHaveCount(7);
    const box = await sheet.boundingBox();
    if (vp.width < 400) expect(box.width).toBe(vp.width);
    else expect(box.width).toBeGreaterThan(900);
    expect(await noOverflow(alice)).toEqual([]);

    await sheet.getByRole('button', { name: 'Rename Bob' }).click();
    await sheet.getByLabel('New name for Bob').fill('Robert');
    await sheet.getByRole('button', { name: 'Save' }).click();
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
    await admins.getByRole('button', { name: 'Save' }).click();
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
    for (const [button, id] of [['Personas', '#personas'], ['Manage', '#manage'], ['Change your name', '#rename']]) {
      const opener = alice.getByRole('button', { name: button, exact: true });
      const dialog = alice.locator(id);
      await opener.click();
      await expect(dialog).toBeVisible();
      await alice.keyboard.press('Escape');
      await expect(dialog).toBeHidden();
      await expect(opener).toBeFocused();

      await opener.click();
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
  await alice.locator('#notify summary').click();
  await alice.getByLabel('Count voice lines in the tab title').check();
  await voice();
  await expect(alice).toHaveTitle('(3) Docs room · grill-room');
  await alice.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(alice).toHaveTitle('Docs room · grill-room');
});
