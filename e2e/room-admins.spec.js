import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, startDaemon, pairDaemon, ask, skillZip, signIn, noOverflow, menuItem, confirmSheet, playSTT, stopSTT, newRoomEditor, personaSheet, closePersonas, rowAction, personMenu, appointAdmin, unfoldTabs } from './helpers.js';

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
  await unfoldTabs(panel.locator('nav.tabs'));
  await panel.getByRole('link', { name, exact: true }).click();
  await expect(panel.getByRole('link', { name, exact: true, includeHidden: true })).toHaveAttribute('aria-current', 'page');
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
    // One settings group, one Save that counts what it would save (004b-AC8).
    await expect(form.locator('.dirty')).toHaveText('1 unsaved change');
    await form.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(adminPage.locator('.toast')).toHaveText('Saved');
    await expect(form.locator('.dirty')).toHaveText('No unsaved changes');
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
  const row = adminPage.locator('#profiles .list-row', { hasText: qn });
  await expect(row).toContainText("10 of 10, the server's");
  // The row's ⋯ opens the limit in a sheet (004b-AC2).
  await rowAction(row, `Set the room limit of ${qn}`);
  const limit = adminPage.locator('#limit-sheet');
  await limit.getByLabel(`Rooms ${qn} may create`).fill('12');
  await limit.getByRole('button', { name: 'Set', exact: true }).click();
  await expect(limit).toBeHidden();
  await expect(row).toContainText('10 of 12');
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
  // sees no admin tab and the API refuses them; Alice appoints Bob, who removes her; the last
  // admin cannot step down; Bob renames, then deletes the room once its name is typed. Every
  // action is audited under the acting profile.
  test(`room admin delegation, ${vp.name}`, async ({ browser, request }) => {
    const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby ' + vp.name } })).json();
    const alice = await join(browser, request, lobby.id, 'Alice', vp);
    const id = await newRoom(alice, `Delegation ${vp.name}`);
    const bob = await join(browser, request, id, 'Bob', vp);
    const carol = await join(browser, request, id, 'Carol', vp);
    const api = `/api/rooms/${id}`;

    // Plain members: only the plugins (009-AC5) and the transcription glossary in Manage (005-AC23),
    // while a worker reads it (007-AC14), and the API says no.
    await playSTT(bob, id, { engines: ['whisper'], glossary: true });
    await menuItem(bob, 'Room settings');
    await unfoldTabs(bob.locator('#manage nav.tabs'));
    await bob.locator('#manage #tab-glossary').click();
    await expect(bob.locator('#manage #stt-glossary')).toBeVisible();
    await expect(bob.locator('#manage #room-panel')).toHaveCount(0);
    await bob.locator('#manage-close').click();
    await stopSTT(bob);
    for (const [method, path] of [['patch', api], ['delete', api], ['get', api + '/overview'], ['put', `${api}/admins/1`]]) {
      expect((await carol.request[method](path, { data: { name: 'x' } })).status(), `${method} ${path}`).toBe(403);
    }

    // The popup opens on her shareable link, made with the room (011-AC26); Room settings lists
    // every invite.
    await menuItem(alice, 'Invite people');
    await expect(alice.locator('#invite-link')).toHaveValue(/\/join#/);
    await alice.keyboard.press('Escape');
    const panel = await openManage(alice);
    await expect(panel.locator('#volume')).toContainText('Transcripts');
    await expect(panel).toContainText('Only the server admin changes the limit.');
    await expect(panel.locator('#quota')).toHaveCount(0);
    await tab(panel, 'Export');
    await expect(alice).toHaveURL(/#manage\/export$/);
    await expect(panel.locator('#export')).toHaveAttribute('href', `/api/rooms/${id}/export`);
    await expect(panel.locator('#export-warning')).toContainText('everything Claude read');
    await tab(panel, 'Invites');
    // Her seat's invite, her shareable link made with the room (011-AC26), Bob's and Carol's.
    await expect(panel.locator('#invites-all .list-row:not(.head)')).toHaveCount(4);
    await expect(panel.locator('#invites-all .list-row', { hasText: 'Your shareable link' })).toHaveCount(1);
    await expect(panel.locator('#invites-all .list-row:not(.head)').first()).toContainText('Alice');
    await fits(alice);
    await tab(panel, 'Admins');
    await panel.getByRole('button', { name: 'Make Bob a room admin' }).click();
    await expect(panel.locator('#room-admins > li', { hasText: 'Bob' }).locator('.badge.role')).toHaveText('admin');
    await expect(carol.locator('#messages')).toContainText('Alice made Bob a room admin.');

    // Bob, now an admin, removes Alice; she loses her panel.
    await expect(bob.locator('#manage-open')).not.toHaveAttribute('hidden');
    const bobPanel = await openManage(bob);
    await tab(bobPanel, 'Admins');
    await bobPanel.getByRole('button', { name: 'Remove Alice from the room admins' }).click();
    await expect(alice.locator('#manage #room-panel')).toHaveCount(0);
    await bobPanel.getByRole('button', { name: 'Step down as a room admin' }).click();
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
  await expect(bob.locator('#rooms li .room-line', { hasText: evil })).toHaveText(evil);
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
  await unfoldTabs(bobProfile.locator('#profile-tabs'));
  await bobProfile.locator('#tab-skills').click();
  await bobProfile.locator('#panel-skills').getByRole('button', { name: 'Add a skill' }).click();
  await bobProfile.locator('#skill-file').setInputFiles(skillZip('bobs', 'BOB-v1'));
  await bobProfile.locator('#skill-sheet').getByRole('button', { name: 'Add skill', exact: true }).click();
  await expect(bobProfile.locator('#skill-status')).toHaveText('Added bobs v1.');

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

  const grill = await personaSheet(alice, 'Grill', 'Capabilities');
  await grill.getByLabel('Skill to turn on for Grill').selectOption({ label: 'bobs v1 (room)' });
  await grill.getByRole('button', { name: 'Turn on the chosen skill for Grill' }).click();
  await alice.locator('#skills-disclaimer').getByRole('button', { name: 'I understand, turn it on' }).click();
  await expect(alice.locator('#persona-status')).toHaveText('bobs v1 is on for Grill.');
  await expect(grill.locator('.persona-skills li[data-skill]')).toHaveText([/^bobs v1 \(room\)/]);
  await closePersonas(alice);

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    expect((await ask(alice, room.id, 'FAKE_SKILLS with Bob')).trim()).toBe('Skills used: bobs: BOB-v1.');
    // Bob leaves: the admin revokes the invite he joined through, removing him, and he
    // removes his skill.
    const invites = await (await request.get(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    expect((await request.delete(`/api/rooms/${room.id}/invites/${invites[1].id}?cut=1`, { headers: admin })).status()).toBe(200);
    await expect(bob.locator('#banner')).toHaveText('Your access to this room has ended.');
    await bobProfile.reload();
    await unfoldTabs(bobProfile.locator('#profile-tabs'));
    await bobProfile.locator('#tab-skills').click();
    await bobProfile.getByRole('button', { name: 'More actions for bobs' }).click();
    await bobProfile.getByRole('menuitem', { name: 'Remove' }).click();
    await confirmSheet(bobProfile, 'Remove');
    await expect(bobProfile.locator('#skill-status')).toHaveText('bobs removed.');
    expect((await ask(alice, room.id, 'FAKE_SKILLS without Bob')).trim()).toBe('Skills used: bobs: BOB-v1.');
  } finally {
    daemon.cleanup();
  }
});

for (const vp of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'phone', ...phone }]) {
  // 016-AC6 016-AC8 016-AC11 016-AC12 016-AC13 016-AC14: a room admin removes Bob with a block from
  // People › ⋯; Bob's page ends, his line reads "(removed)", his rejoin is refused. Unblocked from
  // Room settings › Invites, he rejoins and the mark goes. A room admin's row offers no removal,
  // and a room admin cannot leave; Carol leaves, and the room leaves her profile.
  test(`remove, block, unblock and leave, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
    await appointAdmin(alice, request, room.id);
    await alice.setViewportSize(vp);
    await bob.locator('#body').fill('a line from bob');
    await bob.locator('#body').press('Enter');
    const bobLine = alice.locator('#messages li.msg', { hasText: 'a line from bob' }).locator('.sender');
    await expect(bobLine).toHaveText('Bob');

    const peopleOf = async (page) => {
      if (page.viewportSize().width < 600) await page.locator('#tabbar').getByRole('button', { name: /People/ }).click();
    };
    await peopleOf(alice);
    const menu = await personMenu(alice, 'Bob');
    await menu.getByRole('button', { name: 'Remove from room…' }).click();
    const sheet = alice.locator('dialog.confirm');
    await expect(sheet.locator('#confirm-title')).toHaveText('Remove Bob from the room?');
    await expect(sheet.locator('#confirm-text')).toHaveText('Their messages stay.');
    await sheet.getByLabel('Block from rejoining').check();
    await confirmSheet(alice, 'Remove');
    await expect(bob.locator('#banner')).toHaveText('Your access to this room has ended.');
    expect((await bob.request.get(`/api/rooms/${room.id}`)).status()).toBe(401);
    await expect(alice.locator('#messages')).toContainText('Alice removed Bob from the room and blocked them from rejoining');
    await expect(bobLine).toHaveText('Bob (removed)');
    await expect(alice.locator('#participants li .name, #participants-away li .name')).toHaveText(['Alice', 'Carol']);

    const link = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    await bob.goto(new URL(link.url).pathname + new URL(link.url).hash);
    await bob.getByLabel('Your name').fill('Bob');
    await bob.getByRole('button', { name: 'Join the room' }).click();
    await expect(bob.locator('#status')).toHaveText('A room admin blocked you from rejoining this room.');

    await alice.goto(`/r/${room.id}#manage/invites`);
    const blocked = alice.locator('#manage #blocked');
    await expect(blocked.locator('.list-row:not(.head)')).toHaveCount(1);
    await expect(blocked).toContainText('Bob');
    if (vp.width < 400) await fits(alice);
    await blocked.getByRole('button', { name: 'Unblock Bob' }).click();
    await expect(alice.locator('#manage #blocked')).toHaveCount(0);
    await alice.keyboard.press('Escape');
    await expect(alice.locator('#messages')).toContainText('Alice unblocked Bob');

    await bob.getByRole('button', { name: 'Join the room' }).click();
    await expect(bob.locator('#me')).toHaveText('Bob');
    await expect(bobLine).toHaveText('Bob');

    // A room admin's row: no removal while they hold the role, and no leaving for oneself.
    await peopleOf(alice);
    const own = await personMenu(alice, 'Alice');
    await expect(own.getByRole('button', { name: 'Leave room…' })).toHaveAttribute('aria-disabled', 'true');
    await expect(own.getByRole('button', { name: 'Leave room…' })).toHaveAttribute('data-tip', 'Step down as room admin first');
    await alice.keyboard.press('Escape');

    await carol.locator('#body').fill('a line from carol');
    await carol.locator('#body').press('Enter');
    await peopleOf(carol);
    await (await personMenu(carol, 'Carol')).getByRole('button', { name: 'Leave room…' }).click();
    await confirmSheet(carol, 'Leave');
    await expect(carol.locator('#banner')).toHaveText('Your access to this room has ended.');
    await expect(alice.locator('#messages')).toContainText('Carol left the room');
    await expect(alice.locator('#messages li.msg', { hasText: 'a line from carol' }).locator('.sender')).toHaveText('Carol (left)');
    await carol.goto('/profile');
    await expect(carol.locator('main')).toBeVisible();
    await expect(carol.getByText('Docs room')).toHaveCount(0);
  });
}
