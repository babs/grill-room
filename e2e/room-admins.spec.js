import { test, expect } from '@playwright/test';
import { adminToken } from './playwright.config.js';
import { roomWith, startDaemon, pairDaemon, ask, skillZip, signIn, noOverflow, menuItem, confirmSheet, newRoomEditor } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const phone = { width: 390, height: 844 };

// This file is the only one creating rooms from profiles: the creation mode it switches is
// instance-wide, and its tests run in order.
test.describe.configure({ mode: 'serial' });

async function fits(page) {
  expect(await noOverflow(page)).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize().width);
}

// join opens an invite of room in a fresh browser as name; viewport defaults to a desktop's.
async function join(browser, request, roomID, name, viewport) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext(viewport ? { viewport } : {})).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

// newRoom creates a room from page's profile page, as its room admin, and opens it.
async function newRoom(page, name) {
  await page.goto('/profile');
  await newRoomEditor(page);
  await page.getByLabel('Room name').fill(name);
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-name')).toHaveText(name);
  // A new room opens on its invite sheet (005-AC34).
  await page.locator('#invites').getByRole('button', { name: 'Close' }).click();
  return decodeURIComponent(new URL(page.url()).pathname.split('/')[2]);
}

// openManage opens the Manage sheet on its General tab (005-AC15).
async function openManage(page) {
  await menuItem(page, 'Room settings');
  const panel = page.locator('#manage #room-panel');
  await expect(panel).toBeVisible();
  await expect(panel.locator('#panel-general')).toBeVisible();
  return panel;
}

// tab opens a tab of the Manage sheet.
async function tab(panel, name) {
  await panel.getByRole('link', { name, exact: true }).click();
  await expect(panel.getByRole('link', { name, exact: true })).toHaveAttribute('aria-current', 'page');
}

// 004-AC7 004-AC19: a profile creates rooms from its profile page, at phone width, and becomes their
// room admin; the 11th is refused with its reason, and so is any once the instance admin switches
// creation to themselves in /admin. A profile the instance admin allows 12 creates its 11th.
test('room creation from the profile page, within the limits', async ({ browser, request }) => {
  const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby' } })).json();
  const page = await join(browser, request, lobby.id, 'Pia', phone);
  const id = await newRoom(page, 'Pia room 1');
  await expect(page.locator('#me')).toHaveText('Pia');
  await expect(page.locator('#manage-open')).not.toHaveAttribute('hidden');
  await fits(page);
  for (let i = 2; i <= 10; i++) {
    expect((await page.request.post('/api/profile/rooms', { data: { name: `Pia room ${i}`, display_name: 'Pia' } })).status()).toBe(201);
  }
  await page.goto('/profile');
  await expect(page.locator('#rooms li')).toHaveCount(11);
  await newRoomEditor(page);
  await expect(page.getByLabel('Your name in the room')).toHaveValue('Pia');
  await page.getByLabel('Room name').fill('Pia room 11');
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-create-status')).toHaveText('room creation refused: you have created 10 rooms, the most this instance allows per profile');
  await fits(page);
  // Deleting one frees its place.
  expect((await page.request.delete(`/api/rooms/${id}`)).status()).toBe(204);

  const adminPage = await (await browser.newContext({ viewport: phone })).newPage();
  expect(await signIn(adminPage, adminToken, 'instance')).toBe(201);
  const form = adminPage.locator('#room-creation');
  await expect(form.getByLabel('Rooms per profile')).toHaveValue('10');
  try {
    await form.getByLabel('Who creates rooms').selectOption('super-admin');
    await form.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(form.getByRole('status')).toHaveText('Saved.');
    await fits(adminPage);
    await page.getByRole('button', { name: 'Create room' }).click();
    await expect(page.locator('#room-create-status')).toHaveText('room creation refused: only the super-admin creates rooms on this instance');
  } finally {
    expect((await request.put('/api/admin/settings', { headers: admin, data: { rooms_per_profile: 10, storage_quota_bytes: 0, room_creation: 'anyone' } })).status()).toBe(200);
  }
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-name')).toHaveText('Pia room 11');
  const log = await (await request.get('/api/admin/audit?action=instance.settings', { headers: admin })).json();
  expect(log.entries[1].details.room_creation).toBe('super-admin');

  // Quinn gets a limit of 12 from the profiles view: his 11th room goes through, where Pia's was
  // refused at the instance's 10.
  // Unique: the profiles view lists every run's profiles, as --repeat-each reuses the server.
  const qn = 'Quinn ' + Date.now().toString(36);
  const quinn = await join(browser, request, lobby.id, qn, phone);
  for (let i = 1; i <= 10; i++) {
    expect((await quinn.request.post('/api/profile/rooms', { data: { name: `Quinn room ${i}`, display_name: qn } })).status()).toBe(201);
  }
  await adminPage.goto('/admin#profiles');
  const row = adminPage.locator('#profiles tbody tr', { hasText: qn });
  await expect(row).toContainText("10 created, limit instance's (10)");
  await row.getByLabel(`Rooms ${qn} may create`).fill('12');
  await row.getByRole('button', { name: 'Set' }).click();
  await expect(row).toContainText('10 created, limit 12');
  await fits(adminPage);
  await newRoom(quinn, 'Quinn room 11');
  await page.goto('/profile');
  await newRoomEditor(page);
  await page.getByLabel('Room name').fill('Pia room 12');
  await page.getByRole('button', { name: 'Create room' }).click();
  await expect(page.locator('#room-create-status')).toHaveText('room creation refused: you have created 10 rooms, the most this instance allows per profile');
  const set = await (await request.get('/api/admin/audit?action=profile.max_rooms', { headers: admin })).json();
  expect(set.entries[0]).toMatchObject({ target_kind: 'profile', target_name: qn, details: { max_rooms: 12, via: 'session' } });
});

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', ...phone }]) {
  // 004-AC8 004-AC19: the room admin panel, in Preact, at desktop and phone width. A plain member
  // sees no Manage button and the API refuses them; Alice appoints Bob, who removes her; the last
  // admin cannot step down; Bob renames, then deletes the room once its name is typed. Every
  // action is audited under the acting profile.
  test(`room admin delegation, ${vp.name}`, async ({ browser, request }) => {
    const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby ' + vp.name } })).json();
    const alice = await join(browser, request, lobby.id, 'Alice', vp);
    const id = await newRoom(alice, `Delegation ${vp.name}`);
    const bob = await join(browser, request, id, 'Bob', vp);
    const carol = await join(browser, request, id, 'Carol', vp);
    const api = `/api/rooms/${id}`;

    // Plain members: only the transcription glossary in Manage (005-AC23), and the API says no.
    await menuItem(bob, 'Room settings');
    await expect(bob.locator('#manage #stt-glossary')).toBeVisible();
    await expect(bob.locator('#manage #room-panel')).toHaveCount(0);
    await bob.locator('#manage-close').click();
    for (const [method, path] of [['patch', api], ['delete', api], ['get', api + '/overview'], ['put', `${api}/admins/1`]]) {
      expect((await carol.request[method](path, { data: { name: 'x' } })).status(), `${method} ${path}`).toBe(403);
    }

    // "Your invites" keeps to her own, the seat the room gave her; Manage lists them all. A phone
    // hides that sidebar.
    if (vp.width > 400) {
      await menuItem(alice, 'Invite people');
      await expect(alice.locator('#invite-list li')).toHaveCount(1);
      await alice.keyboard.press('Escape');
    }
    const panel = await openManage(alice);
    await expect(panel.locator('#volume')).toContainText('Transcripts');
    await expect(panel).toContainText('Only the instance admin changes the limit.');
    await expect(panel.locator('#quota')).toHaveCount(0);
    await tab(panel, 'Export');
    await expect(alice).toHaveURL(/#manage\/export$/);
    await expect(panel.locator('#export')).toHaveAttribute('href', `/api/rooms/${id}/export`);
    await expect(panel.locator('#export-warning')).toContainText('everything Claude read');
    await tab(panel, 'Invites');
    await expect(panel.locator('#invites-all tbody tr')).toHaveCount(3);
    await expect(panel.locator('#invites-all tbody tr').first()).toContainText('Alice');
    await fits(alice);
    await tab(panel, 'Admins');
    await panel.getByRole('button', { name: 'Make Bob a room admin' }).click();
    await expect(panel.locator('#room-admins li', { hasText: 'Bob' }).locator('strong')).toHaveText('admin');
    await expect(carol.locator('#messages')).toContainText('Alice made Bob a room admin.');

    // Bob, now an admin, removes Alice; she loses her panel.
    await expect(bob.locator('#manage-open')).not.toHaveAttribute('hidden');
    const bobPanel = await openManage(bob);
    await tab(bobPanel, 'Admins');
    await bobPanel.locator('#room-admins li', { hasText: 'Alice' }).getByRole('button', { name: 'Remove admin' }).click();
    await expect(alice.locator('#manage #room-panel')).toHaveCount(0);
    await bobPanel.locator('#room-admins li', { hasText: 'Bob' }).getByRole('button', { name: 'Step down' }).click();
    await expect(bob.locator('#manage [role="alert"]')).toHaveText("you are the room's last admin: appoint another one first");
    await tab(bobPanel, 'General');

    await bobPanel.locator('#room-name-input').fill(`Delegated ${vp.name}`);
    await bobPanel.getByRole('button', { name: 'Rename' }).click();
    await expect(carol.locator('#room-name')).toHaveText(`Delegated ${vp.name}`);
    await fits(bob);
    const del = bobPanel.getByRole('button', { name: 'Delete the room' });
    await bobPanel.locator('#delete-confirm').fill(`Delegation ${vp.name}`);
    await expect(del).toBeDisabled();
    await bobPanel.locator('#delete-confirm').fill(`Delegated ${vp.name}`);
    await del.click();
    await expect(bob).toHaveURL('/profile');
    await expect(carol.locator('#banner')).toHaveText('Your access to this room has ended.');

    const log = await (await request.get(`/api/admin/audit?target=${id}`, { headers: admin })).json();
    const seen = log.entries.filter((e) => e.actor !== 'super-admin').map((e) => `${e.details.as} ${e.action}`);
    expect(seen).toEqual(['Bob room.delete', 'Bob room.rename', 'Bob room_admin.remove', 'Alice room_admin.add', 'Alice room.create']);
    expect(log.entries.every((e) => e.actor === 'super-admin' || /^profile:[0-9a-f-]{36}$/.test(e.actor))).toBe(true);
  });
}

// 002-AC6 004-AC8: a room admin renames the room; every page shows the new name as text and the
// system line.
test('rename a room, with a name that is markup', async ({ browser, request }) => {
  const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby rename' } })).json();
  const alice = await join(browser, request, lobby.id, 'Alice');
  const id = await newRoom(alice, 'Plain name');
  const bob = await join(browser, request, id, 'Bob');
  const evil = '<img src=x onerror="window.__xss=1"><script>window.__xss=2</script>';
  const panel = await openManage(alice);
  await panel.locator('#room-name-input').fill(evil);
  await panel.getByRole('button', { name: 'Rename' }).click();
  for (const page of [alice, bob]) {
    await expect(page.locator('#room-name')).toHaveText(evil);
    await expect(page.locator('#messages')).toContainText(`Alice renamed the room from "Plain name" to "${evil.replaceAll('"', '\\"')}"`);
    await expect(page.locator('#room-name img, #room-name script, #messages img, #messages script, #manage h2 img')).toHaveCount(0);
    await expect(page).toHaveTitle(evil + ' · grill-room');
    expect(await page.evaluate(() => window.__xss)).toBeUndefined();
  }
  await menuItem(bob, 'My rooms and profile', 'me');
  await expect(bob.locator('#rooms li .room-line', { hasText: 'as Bob' })).toHaveText(`${evil} as Bob`);
  await expect(bob.locator('#rooms img, #rooms script')).toHaveCount(0);
  expect(await bob.evaluate(() => window.__xss)).toBeUndefined();
});

// 004-AC10: Bob copies a personal skill into the room from the Personas panel (phone width
// checked); the copy is a chat line; Alice turns it on and a turn (real daemon and sandbox, fake
// claude) runs it; Bob leaves the room and removes his own skill, and the next turn still runs the
// room's copy. A personal skill would have dropped out.
test('a room skill survives its author leaving', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const bobProfile = await bob.context().newPage();
  await bobProfile.goto('/profile');
  await bobProfile.locator('#skill-file').setInputFiles(skillZip('bobs', 'BOB-v1'));
  await bobProfile.locator('#skills summary').click();
  await bobProfile.getByRole('button', { name: 'Upload', exact: true }).click();
  await expect(bobProfile.locator('#skill-status')).toHaveText('Uploaded bobs v1.');

  await bob.setViewportSize(phone);
  await menuItem(bob, 'Personas');
  const copy = bob.locator('#room-skill-copy');
  await expect(copy.getByLabel('Copy one of your personal skills into the room').locator('option')).toHaveText(['bobs v1']);
  await copy.getByRole('button', { name: 'Copy' }).scrollIntoViewIfNeeded();
  expect(await bob.locator('#personas').evaluate((d) => d.scrollWidth <= d.clientWidth)).toBe(true);
  await copy.getByRole('button', { name: 'Copy' }).click();
  await expect(bob.locator('#personas-status')).toHaveText('bobs v1 copied into the room.');
  await expect(bob.locator('#room-skill-list li')).toHaveText(['bobs v1, copied by Bob']);
  await expect(copy).toBeHidden();
  await expect(alice.locator('#messages')).toContainText('Bob copied the skill bobs v1 into the room.');
  await bob.getByRole('button', { name: 'Close' }).click();

  await menuItem(alice, 'Personas');
  const grill = alice.locator('#personas .persona-form', { hasText: 'Grill' }).first();
  await grill.getByLabel('Skill to turn on for Grill').selectOption({ label: 'bobs v1 (room)' });
  await grill.getByRole('button', { name: 'Turn on the chosen skill for Grill' }).click();
  await alice.locator('#skills-disclaimer').getByRole('button', { name: 'I understand, turn it on' }).click();
  await expect(alice.locator('#personas-status')).toHaveText('bobs v1 is on for Grill.');
  await expect(grill.locator('.persona-skills li[data-skill]')).toHaveText([/^bobs v1 \(room\)/]);
  await alice.getByRole('button', { name: 'Close' }).click();

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    expect((await ask(alice, room.id, 'FAKE_SKILLS with Bob')).trim()).toBe('Skills used: bobs: BOB-v1.');
    // Bob leaves: the admin revokes the invite he joined through, the second roomWith made, and he
    // removes his skill.
    const invites = await (await request.get(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    expect((await request.delete(`/api/rooms/${room.id}/invites/${invites[1].id}`, { headers: admin })).status()).toBe(204);
    await expect(bob.locator('#banner')).toHaveText('Your access to this room has ended.');
    await bobProfile.reload();
    await bobProfile.locator('#skills summary').click();
    await bobProfile.getByRole('button', { name: 'Remove skill bobs' }).click();
    await confirmSheet(bobProfile, 'Remove');
    await expect(bobProfile.locator('#skill-status')).toHaveText('bobs removed.');
    expect((await ask(alice, room.id, 'FAKE_SKILLS without Bob')).trim()).toBe('Skills used: bobs: BOB-v1.');
  } finally {
    daemon.cleanup();
  }
});
