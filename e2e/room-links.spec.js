import { randomUUID } from 'node:crypto';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { fakeDaemon, personMenu, rowAction, unfoldTabs, uniq } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// join opens an invite of room in a fresh browser as name, or in page when given.
async function join(browser, request, roomID, name, page) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  page ??= await (await browser.newContext()).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

// linksTab opens Room settings › Links through its deep link (023-AC1).
async function linksTab(page, roomID) {
  await page.goto(`/r/${roomID}#links`);
  const panel = page.locator('#manage');
  await expect(panel).toBeVisible();
  await unfoldTabs(panel.locator('nav.tabs'));
  await expect(panel.getByRole('link', { name: 'Links', exact: true, includeHidden: true })).toHaveAttribute('aria-current', 'page');
  return panel;
}

// 023-AC1 023-AC2 023-AC5 023-AC8: an admin of two rooms links both ways in one save and sees both
// rows in each room; a plain member sees the rows, no "Link a room" and no ⋯; the room menu names
// the links.
test('room links from Room settings', async ({ browser, request }) => {
  const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby' } })).json();
  const ana = await join(browser, request, lobby.id, 'Ana');
  const make = async (name) => (await (await ana.request.post('/api/profile/rooms', { data: { name, display_name: 'Ana' } })).json()).id;
  const pricing = await make('Pricing ' + uniq());
  const legal = await make('Legal ' + uniq());
  const names = await Promise.all([pricing, legal].map(async (id) => (await (await ana.request.get(`/api/rooms/${id}`)).json()).room.name));

  let panel = await linksTab(ana, pricing);
  await expect(panel.locator('#links-reads-empty')).toBeVisible();
  await panel.getByRole('button', { name: 'Link a room' }).click();
  const sheet = ana.locator('#link-sheet');
  await sheet.getByLabel('Room').selectOption(legal);
  await expect(sheet.getByLabel(`${names[0]} reads ${names[1]}`)).toBeChecked();
  await expect(sheet.getByLabel(`${names[1]} reads ${names[0]}`)).toBeChecked();
  await sheet.getByRole('button', { name: 'Save' }).click();
  await expect(sheet).toBeHidden();
  await expect(panel.locator('#links-reads .list-row:not(.head)')).toHaveText([new RegExp(names[1])]);
  await expect(panel.locator('#links-read-by .list-row:not(.head)')).toHaveText([new RegExp(names[1])]);
  // 023-AC3: both directions exist: the sheet shows them ticked and disabled.
  await panel.getByRole('button', { name: 'Link a room' }).click();
  await sheet.getByLabel('Room').selectOption(legal);
  await expect(sheet.getByLabel(`${names[0]} reads ${names[1]}`)).toBeDisabled();
  await expect(sheet.getByLabel(`${names[1]} reads ${names[0]}`)).toBeChecked();
  await expect(sheet.getByRole('button', { name: 'Save' })).toBeDisabled();
  await sheet.getByRole('button', { name: 'Cancel' }).click();

  panel = await linksTab(ana, legal);
  await expect(panel.locator('#links-reads .list-row:not(.head)')).toHaveText([new RegExp(names[0])]);
  await expect(panel.locator('#links-read-by .list-row:not(.head)')).toHaveText([new RegExp(names[0])]);
  // 023-AC8: the room menu counts linked rooms, a room linked both ways once.
  await ana.keyboard.press('Escape');
  await ana.locator('#room-open').click();
  await expect(ana.locator('#links-open')).toHaveText('Linked rooms (1)');
  await ana.keyboard.press('Escape');
  const notesName = 'Notes ' + uniq();
  const notes = await make(notesName);
  expect((await ana.request.post(`/api/rooms/${legal}/links`, { data: { room: notes, reads: true } })).status()).toBe(201);
  await ana.locator('#room-open').click();
  await expect(ana.locator('#links-open')).toHaveText('Linked rooms (2)');

  // 023-AC1: a plain member reads the list, with plain names for rooms they are not in.
  const bob = await join(browser, request, legal, 'Bob');
  panel = await linksTab(bob, legal);
  await expect(panel.locator('#links-reads .list-row:not(.head)')).toHaveText([new RegExp(names[0]), new RegExp(notesName)]);
  await expect(panel.getByRole('button', { name: 'Link a room' })).toHaveCount(0);
  await expect(panel.locator('.list-row .more')).toHaveCount(0);
  await expect(panel.locator('#links-reads a.name')).toHaveCount(0);
  await bob.keyboard.press('Escape');
  await bob.locator('#room-open').click();
  await bob.locator('#links-open').click();
  await expect(bob.locator('#manage')).toBeVisible();
  // The tab says so when the links cannot be fetched, rather than loading for ever.
  await bob.route('**/api/rooms/*/links', (r) => r.abort());
  // A hash-only goto keeps the page and its loaded links: leave it so the room loads afresh.
  await bob.goto('about:blank');
  panel = await linksTab(bob, legal);
  await expect(panel.locator('#links-error')).toBeVisible();
  await bob.unroute('**/api/rooms/*/links');

  // 023-AC5: Ana removes one direction from its ⋯, no confirm; the other stays.
  panel = await linksTab(ana, legal);
  await rowAction(panel.locator('#links-reads .list-row:not(.head)').first(), 'Remove link');
  await expect(panel.locator('#links-reads .list-row:not(.head)')).toHaveText([new RegExp(notesName)]);
  await expect(panel.locator('#links-read-by .list-row:not(.head)')).toHaveCount(1);

  // 023-AC2 023-AC3: picking another room ticks both boxes again; an existing link stays ticked and disabled.
  await panel.getByRole('button', { name: 'Link a room' }).click();
  await sheet.getByLabel('Room').selectOption(notes);
  await expect(sheet.getByLabel(`${names[1]} reads ${notesName}`)).toBeDisabled();
  await expect(sheet.getByLabel(`${names[1]} reads ${notesName}`)).toBeChecked();
  await sheet.getByLabel(`${notesName} reads ${names[1]}`).uncheck();
  await sheet.getByLabel('Room').selectOption(pricing);
  await sheet.getByLabel('Room').selectOption(notes);
  await expect(sheet.getByLabel(`${notesName} reads ${names[1]}`)).toBeChecked();
  await expect(sheet.getByLabel(`${names[1]} reads ${notesName}`)).toBeDisabled();
});

// 023-AC9 023-AC14 023-AC15: a turn of Pricing, which reads Legal, gets Legal's document from the
// server through a stub AI host; Legal's remove sheet says the link keeps reading, and names
// Pricing, where Bob still sits, to Ana who sits there too; Pricing, read by none, says nothing.
test('a turn reads a linked room; the remove sheet says links stay', async ({ browser, request, baseURL }) => {
  const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby' } })).json();
  const ana = await join(browser, request, lobby.id, 'Ana');
  const make = async (name) => (await (await ana.request.post('/api/profile/rooms', { data: { name, display_name: 'Ana' } })).json()).id;
  const pricingName = 'Pricing ' + uniq();
  const legalName = 'Legal ' + uniq();
  const pricing = await make(pricingName);
  const legal = await make(legalName);
  expect((await ana.request.post(`/api/rooms/${pricing}/links`, { data: { room: legal, reads: true } })).status()).toBe(201);
  const up = await ana.request.post(`/api/rooms/${legal}/documents`, { multipart: { file: { name: 'terms.md', mimeType: 'text/markdown', buffer: Buffer.from('# Terms\n\nNet 30.\n') } } });
  expect(up.status()).toBe(201);
  const docID = (await up.json()).id;

  await ana.goto(`/r/${pricing}`);
  const d = await fakeDaemon(ana, baseURL, pricing, 'ana-laptop');
  try {
    expect((await ana.request.post(`/api/rooms/${pricing}/turns`, { data: {} })).status()).toBe(201);
    await expect.poll(() => d.turns.length).toBe(1);
    const daemon = (p, init = {}) => fetch(`${baseURL}/api/rooms/${pricing}/daemon/turns/${d.turns[0]}${p}`,
      { ...init, headers: { Authorization: 'Bearer ' + d.token, 'Content-Type': 'application/json' } });
    const m = await (await daemon('/manifest')).json();
    const want = `linked/${legalName.toLowerCase().replace(/[^a-z0-9]+/g, '-')}/uploads/${docID}-terms.md`;
    expect(m.files.map((f) => f.path)).toContain(want);
    const tar = await daemon('/workspace', { method: 'POST', body: JSON.stringify({ paths: [want] }) });
    expect(tar.status).toBe(200);
    expect(Buffer.from(await tar.arrayBuffer()).toString()).toContain('Net 30.');
  } finally {
    d();
  }

  const bob = await join(browser, request, legal, 'Bob');
  // Bob's own profile takes a seat in Pricing too.
  const inv = await (await request.post(`/api/rooms/${pricing}/invites`, { headers: admin })).json();
  expect((await bob.request.post('/api/join', { data: { invite: inv.token, name: 'Bob' } })).status()).toBe(201);
  await ana.goto(`/r/${legal}`);
  await (await personMenu(ana, 'Bob')).getByRole('button', { name: 'Remove from room…' }).click();
  const sheet = ana.locator('dialog.confirm');
  await expect(sheet.locator('#confirm-text')).toHaveText('Their messages stay. Rooms that read this one keep their access; removing someone here does not cut it.'
    + ` Bob still sits in ${pricingName}.`);
  await sheet.getByRole('button', { name: 'Cancel' }).click();
  await bob.goto(`/r/${pricing}`);
  await expect(bob.locator('#me')).toHaveText('Bob');
  await ana.goto(`/r/${pricing}`);
  await (await personMenu(ana, 'Bob')).getByRole('button', { name: 'Remove from room…' }).click();
  await expect(sheet.locator('#confirm-text')).toHaveText('Their messages stay.');
});

// 023-AC16 to 023-AC20: a stub AI host's turn result carries room-new.json, as the daemon posts
// it; the card offers Create… to Bob, a plain member (no link boxes), and to Ana, a room admin,
// who creates the room with Bob and both links; the card then links to the room.
test('a persona proposes a room; a member creates it from the card', async ({ browser, request, baseURL }) => {
  const lobby = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby' } })).json();
  const ana = await join(browser, request, lobby.id, 'Ana');
  const pricingName = 'Pricing ' + uniq();
  const pricing = (await (await ana.request.post('/api/profile/rooms', { data: { name: pricingName, display_name: 'Ana' } })).json()).id;
  const bob = await join(browser, request, pricing, 'Bob');
  await ana.goto(`/r/${pricing}`);
  const d = await fakeDaemon(ana, baseURL, pricing, 'ana-laptop');
  // propose runs one turn whose result carries room_new, as the daemon posts it.
  const propose = async (name) => {
    const n = d.turns.length;
    expect((await ana.request.post(`/api/rooms/${pricing}/turns`, { data: {} })).status()).toBe(201);
    await expect.poll(() => d.turns.length).toBe(n + 1);
    const roomNew = JSON.stringify({ name, brief: '# Why\n\nThe contract needs **its own** review.' });
    const res = await fetch(`${baseURL}/api/rooms/${pricing}/daemon/turns/${d.turns[n]}`, { method: 'POST',
      headers: { Authorization: 'Bearer ' + d.token, 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: randomUUID(), session_offset: 0, synthesis: 'A side question.', room_new: roomNew }) });
    expect(res.status).toBe(200);
  };
  const legalName = 'Legal ' + uniq();
  try {
    await propose(legalName);
    const card = (page) => page.locator('#messages li.msg.system.proposal').first();
    await expect(card(ana).locator('.body')).toHaveText(`Grill proposes a room: ${legalName}`);

    // 023-AC17: Bob, a plain member, gets Create… with no link boxes and no Dismiss.
    await bob.goto(`/r/${pricing}`);
    await expect(card(bob).getByRole('button', { name: 'Dismiss' })).toHaveCount(0);
    await card(bob).getByRole('button', { name: 'Create…' }).click();
    let sheet = bob.locator('#proposal-sheet');
    await expect(sheet.locator('#proposal-brief strong')).toHaveText('its own');
    await expect(sheet.locator('#proposal-no-links')).toHaveText(`A room admin of ${pricingName} can link the rooms later.`);
    await expect(sheet.getByLabel('Bob (you)')).toBeDisabled();
    await sheet.getByRole('button', { name: 'Cancel' }).click();

    // 023-AC17 023-AC18: Ana ticks Bob, keeps both links ticked, and creates.
    await card(ana).getByRole('button', { name: 'Create…' }).click();
    sheet = ana.locator('#proposal-sheet');
    await expect(sheet.getByLabel('Name')).toHaveValue(legalName);
    await expect(sheet.getByLabel('Ana (you)')).toBeChecked();
    await expect(sheet.getByLabel('Bob', { exact: true })).not.toBeChecked();
    await sheet.getByLabel('Bob', { exact: true }).check();
    await expect(sheet.getByLabel(`${legalName} reads ${pricingName}`)).toBeChecked();
    await expect(sheet.getByLabel(`${pricingName} reads ${legalName}`)).toBeChecked();
    await sheet.getByRole('button', { name: 'Create', exact: true }).click();
    await expect(sheet).toBeHidden();
    // 023-AC18: the card says who created it and links there for its members.
    const link = card(ana).getByRole('link', { name: `Created by Ana: ${legalName}` });
    await expect(link).toBeVisible();
    await expect(card(ana).getByRole('button')).toHaveCount(0);
    // A second room created in the same page load links too, though the page fetched its rooms
    // before. No link: no line in this room makes the page reload its state meanwhile.
    const opsName = 'Ops ' + uniq();
    // The card's line reloads the page's state, which fetches the rooms again, before Ops exists.
    const fetched = ana.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
    await propose(opsName);
    await fetched;
    const second = ana.locator('#messages li.msg.system.proposal').nth(1);
    await second.getByRole('button', { name: 'Create…' }).click();
    await sheet.getByLabel(`${opsName} reads ${pricingName}`).uncheck();
    await sheet.getByLabel(`${pricingName} reads ${opsName}`).uncheck();
    await sheet.getByRole('button', { name: 'Create', exact: true }).click();
    await expect(second.getByRole('link', { name: `Created by Ana: ${opsName}` })).toBeVisible();
    await expect(link).toBeVisible();
    // A reload of the room's state fetches the rooms once: Bob, not in Ops, sends for them no twice.
    await expect(bob.locator('#messages li.msg.system.proposal.created')).toHaveCount(2);
    let fetches = 0;
    bob.on('request', (r) => { if (new URL(r.url()).pathname === '/api/profile') fetches++; });
    const fetched2 = bob.waitForResponse((r) => new URL(r.url()).pathname === '/api/profile');
    expect((await ana.request.patch(`/api/rooms/${pricing}`, { data: { name: pricingName + ' (old)' } })).status()).toBe(200);
    await fetched2;
    await bob.waitForTimeout(500); // a second fetch would follow the first at once
    expect(fetches).toBe(1);
    expect((await ana.request.patch(`/api/rooms/${pricing}`, { data: { name: pricingName } })).status()).toBe(200);
    await link.click();
    await expect(ana.locator('#me')).toHaveText('Ana');
    const legal = new URL(ana.url()).pathname.split('/').pop();
    const room = await (await ana.request.get(`/api/rooms/${legal}`)).json();
    expect(room.room.proposed_in).toEqual({ room: pricing, name: pricingName, member: true });
    expect(room.participants.map((p) => p.name).sort()).toEqual(['Ana', 'Bob']);
    const links = await (await ana.request.get(`/api/rooms/${legal}/links`)).json();
    expect(links.length).toBe(2);
    // 023-AC20: Room settings › General says where it was proposed.
    await ana.goto(`/r/${legal}#manage`);
    await expect(ana.locator('#proposed-in')).toHaveText(`Proposed in ${pricingName}.`);
    await expect(ana.locator('#proposed-in a')).toHaveAttribute('href', `/r/${pricing}`);
    // Bob sits in it, through Ana's shareable invite.
    await bob.goto(`/r/${legal}`);
    await expect(bob.locator('#me')).toHaveText('Bob');
  } finally {
    d();
  }
});
