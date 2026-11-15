import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, fakeDaemon, appointAdmin, agentState, patchPersona, noOverflow, smallTargets } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const lines = {
  member: 'You are in this room: you read, write, add documents and notes, and talk to the personas by @name.',
  noHost: 'No one runs the AI for this room yet.',
  admin: 'You are a room admin: you rename the room, manage members and invites, and delete the room.',
  runs: 'You run the AI for this room: you approve its personas, skills and websites.',
  email: 'Your profile has no email: add one to keep your rooms across devices.',
  lastAdmin: 'You are the last room admin: make someone else room admin before you leave.',
};
const whoever = 'Whoever runs the AI decides who can ask the personas.';
const decides = (name) => name + ' decides who can ask the personas. You can still mention them in a message.';
const words = ['Room', 'Persona', 'AI host', 'AI host app', 'Call', 'Voice-to-text', 'Note', 'Starter', 'Skill', 'Plugin', 'Linked room', 'Room admin', 'Server admin'];

const sheet = (page) => page.locator('#help');
const row = (page, id) => page.locator(`#help-can [data-row="${id}"]`);
// can lists the "What you can do here" rows: [id, line, link label, link target].
const can = (page) => page.locator('#help-can [data-row]').evaluateAll((els) => els.map((e) => {
  const a = e.querySelector('a');
  return [e.dataset.row, e.querySelector('.name').textContent, ...(a ? [a.textContent, a.getAttribute('href')] : [])];
}));

async function openHelp(page) {
  await page.locator('#me').click();
  await page.locator('#me-menu').getByRole('link', { name: 'Help', exact: true }).click();
  await expect(sheet(page)).toBeVisible();
  await expect(page).toHaveURL(/#help$/);
  await expect(sheet(page).getByRole('heading', { name: 'Help', exact: true })).toBeVisible();
}

const idOf = (page) => page.evaluate(() => Number(myID));

// 042-AC8 042-AC9 042-AC10: a plain member sees the always lines and the no-host one; the lines
// follow a host arriving, the host letting them ask, and the host role handed to them, while the
// sheet stays open. The words follow, in order.
test('Help: a plain member, live through the host\'s changes', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  // Help sits before "My rooms and profile".
  expect(await bob.locator('#me-menu > :is(a, button)').evaluateAll((els) => els.map((e) => e.textContent.trim()))).toEqual(
    ['Change my name', 'Notifications', 'Text size', 'Use on another device', 'Diagnostics', 'Help', 'My rooms and profile', 'Leave room…']);
  await openHelp(bob);
  await expect(sheet(bob).getByRole('heading', { name: 'What you can do here' })).toBeVisible();
  await expect(sheet(bob).getByRole('heading', { name: 'Words' })).toBeVisible();
  // The list names itself by its heading; only Words, a <dl>, keeps a landmark for it.
  await expect(sheet(bob).getByRole('region')).toHaveCount(1);
  await expect(sheet(bob).getByRole('region', { name: 'Words' })).toBeVisible();
  await expect(sheet(bob).getByRole('list', { name: 'What you can do here' })).toBeVisible();
  expect(await can(bob)).toEqual([['member', lines.member], ['ask', whoever], ['host', lines.noHost, 'What\'s missing?', '#ai']]);
  expect(await sheet(bob).locator('#help-words dt').allTextContents()).toEqual(words);
  await expect(sheet(bob).locator('#help-words dd').first()).toHaveText('A place where people and personas talk, with its chat, documents and call.');

  const aliceHost = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  let bobHost;
  try {
    await expect.poll(() => can(bob)).toEqual([['member', lines.member], ['ask', decides('Alice')]]);
    const bobID = await idOf(bob);
    expect(await alice.evaluate(([r, p]) => fetch(`/api/rooms/${r}/participants/${p}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ can_trigger: true }) }).then((x) => x.status), [room.id, bobID])).toBeLessThan(300);
    await expect(row(bob, 'ask').locator('.name')).toHaveText('You can ask the personas.');
    // A host transfer while Help is open.
    bobHost = await fakeDaemon(bob, baseURL, room.id, 'Bob laptop');
    expect(await alice.evaluate(([r, p]) => fetch(`/api/rooms/${r}/host`, { method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ participant_id: p }) }).then((x) => x.status), [room.id, bobID])).toBeLessThan(300);
    await expect.poll(() => can(bob)).toEqual([['member', lines.member], ['ask', 'You can ask the personas.'], ['runs', lines.runs, 'AI in this room', '#ai']]);
    await openHelp(alice);
    await expect(row(alice, 'runs')).toHaveCount(0);
    await expect(row(alice, 'ask').locator('.name')).toHaveText(decides('Bob'));
  } finally {
    aliceHost();
    bobHost?.();
  }
});

// 042-AC9 042-AC7: the room admin and last room admin lines and their links; each link opens its
// target as a new entry, closing it returns to Help with the focus on the row, Forward reopens it.
// The last-admin line goes once someone else is room admin. Closed, Help gives the focus back to
// the avatar menu's button. An ended room hides the entry and opens nothing.
test('Help: room admin, last room admin, and the way back', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await appointAdmin(alice, request, room.id);
  await openHelp(alice);
  expect(await can(alice)).toEqual([['member', lines.member], ['ask', whoever], ['host', lines.noHost, 'What\'s missing?', '#ai'],
    ['admin', lines.admin, 'Room settings', '#manage'], ['last-admin', lines.lastAdmin, 'Room settings › Members and admins', '#manage/admins']]);

  const members = row(alice, 'last-admin').getByRole('link', { name: 'Room settings › Members and admins' });
  await members.click();
  await expect(alice.locator('#manage')).toBeVisible();
  await expect(alice).toHaveURL(/#manage\/admins$/);
  await expect(sheet(alice)).toBeHidden();
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeVisible();
  await expect(members).toBeFocused();
  await alice.goForward();
  await expect(alice.locator('#manage')).toBeVisible();
  await alice.goBack();
  await expect(sheet(alice)).toBeVisible();
  await expect(members).toBeFocused();

  const missing = row(alice, 'host').getByRole('link', { name: 'What\'s missing?' });
  await missing.click();
  await expect(alice.locator('#ai')).toBeVisible();
  await expect(sheet(alice)).toBeHidden();
  await alice.locator('#ai .close').click();
  await expect(sheet(alice)).toBeVisible();
  await expect(alice).toHaveURL(/#help$/);
  await expect(missing).toBeFocused();
  await alice.goForward();
  await expect(alice.locator('#ai')).toBeVisible();
  await alice.goBack();
  await expect(sheet(alice)).toBeVisible();
  await expect(missing).toBeFocused();

  // Another room admin: Alice is no longer the last.
  const bobID = await idOf(bob);
  expect((await request.put(`/api/admin/rooms/${room.id}/admins/${bobID}`, { headers: admin })).status()).toBe(204);
  await expect(row(alice, 'last-admin')).toHaveCount(0);
  await expect(row(alice, 'admin')).toHaveCount(1);
  await expect(missing).toBeFocused();

  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeHidden();
  await expect(alice).not.toHaveURL(/#/);
  await expect(alice.locator('#me')).toBeFocused();

  await alice.evaluate(() => end('This room was deleted.'));
  await expect(alice.locator('#help-open')).toBeHidden();
  await alice.evaluate(() => { location.hash = '#help'; });
  await expect(alice).not.toHaveURL(/#help/);
  await expect(sheet(alice)).toBeHidden();
});

// 042-AC9 042-AC7: a profile without an email gets the line and a plain link to /profile#email in
// the same tab; Back reopens Help with the focus on that row.
test('Help: a profile without an email', async ({ browser, request }) => {
  const { pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await bob.route('**/api/profile', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const res = await route.fetch();
    await route.fulfill({ response: res, json: { ...(await res.json()), email_enabled: true } });
  });
  await bob.reload();
  await openHelp(bob);
  const add = row(bob, 'email').getByRole('link', { name: 'Add an email' });
  await expect(row(bob, 'email').locator('.name')).toHaveText(lines.email);
  await expect(add).toHaveAttribute('href', '/profile#email');
  await expect(add).not.toHaveAttribute('target', /.+/);
  await add.click();
  // /profile#email lands on the Account tab's email field, rewriting its own hash to #account.
  await expect(bob).toHaveURL(/\/profile#account$/);
  await expect(bob.locator('#bind-email')).toBeFocused();
  await bob.goBack();
  await expect(sheet(bob)).toBeVisible();
  await expect(add).toBeFocused();
});

// 042-AC9, 003: a seat of the AI host's profile that is not the host's seat reads 039's own line
// for it and the AI host line; a third person reads neither, nor anything tying the two seats.
test('Help: two seats of one profile, seen from a third', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop');
  try {
    const profileOf = async (page) => (await (await page.request.get('/api/profile')).json()).profile.id;
    const [keep, merge] = [await profileOf(alice), await profileOf(bob)];
    expect((await request.post('/api/admin/profiles/merge', { headers: admin, data: { keep, merge } })).status()).toBe(200);
    await bob.reload();
    await expect.poll(async () => (await agentState(bob, room.id)).you_host).toBe(true);
    await openHelp(bob);
    expect(await can(bob)).toEqual([['member', lines.member],
      ['ask', 'You run the AI as Alice: open the room as Alice and tick "Can ask the AI" next to your name in People.'],
      ['runs', lines.runs, 'AI in this room', '#ai']]);
    expect((await agentState(carol, room.id)).you_host).toBeFalsy();
    await openHelp(carol);
    expect(await can(carol)).toEqual([['member', lines.member], ['ask', decides('Alice')]]);
    await expect(sheet(carol)).not.toContainText('Bob');
    await expect(sheet(carol)).not.toContainText('as Alice');
  } finally {
    host();
  }
});

// 042-AC7 039-AC5: Help › AI in this room › "Open the approvals" hands over to the approval strip:
// Help does not come back over it.
test('Help: the AI host opens the approvals', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const host = await fakeDaemon(alice, baseURL, room.id, 'Alice laptop', { approve_all: true });
  try {
    const grill = (await agentState(bob, room.id)).personas[0].id;
    expect(await patchPersona(bob, room.id, grill, { name: 'Griller' })).toBe(202);
    await openHelp(alice);
    await row(alice, 'runs').getByRole('link', { name: 'AI in this room' }).click();
    await expect(alice.locator('#ai')).toBeVisible();
    await alice.locator('#ai-rows [data-row="waiting"]').getByRole('button', { name: 'Open the approvals' }).click();
    // A return to Help would leave #help in the address bar: the strip's entry has no hash, no stamp.
    await expect.poll(() => alice.evaluate(() => [location.hash, history.state])).toEqual(['', null]);
    await expect(alice.locator('#ai')).toBeHidden();
    await expect(sheet(alice)).toBeHidden();
    await expect(alice.locator('#approvals')).toBeVisible();
    await expect(alice.locator('#approvals')).toBeFocused();
  } finally {
    host();
  }
});

// 042-AC8 042-AC7: Help opens in the docs and chat windows too, and its link to the AI sheet comes
// back there. On a phone it is full screen with nothing sticking out.
test('Help: docs and chat windows, and a phone', async ({ browser, request }) => {
  const { room, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  for (const path of [`/r/${room.id}/docs`, `/r/${room.id}/chat`]) {
    await bob.goto(path);
    await openHelp(bob);
    const missing = row(bob, 'host').getByRole('link', { name: 'What\'s missing?' });
    await missing.click();
    await expect(bob.locator('#ai')).toBeVisible();
    await bob.keyboard.press('Escape');
    await expect(sheet(bob)).toBeVisible();
    await expect(missing).toBeFocused();
    await bob.keyboard.press('Escape');
    await expect(sheet(bob)).toBeHidden();
  }
  await bob.goto(`/r/${room.id}`);
  await bob.setViewportSize({ width: 390, height: 844 });
  await openHelp(bob);
  expect((await sheet(bob).boundingBox()).width).toBeGreaterThanOrEqual(389);
  expect(await noOverflow(bob)).toEqual([]);
  expect(await smallTargets(bob)).toEqual([]);
});
