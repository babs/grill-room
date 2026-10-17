import { request as pwRequest } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, menuItem, appointAdmin, signIn, noOverflow, rowAction } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const widths = [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }];

// gate holds page's requests to url (of method, when given) until the returned function runs: a
// test sees a button in flight without any timing.
async function gate(page, url, method) {
  let open;
  const opened = new Promise((resolve) => { open = resolve; });
  await page.route(url, async (route) => {
    if (method && route.request().method() !== method) return route.fallback();
    await opened;
    await route.fallback();
  });
  return async () => {
    open();
    await page.unroute(url);
  };
}

// share asks for page's shareable link of the room, as the popup does.
const share = (page, roomID) => page.evaluate((id) => fetch(`/api/rooms/${id}/invites/share`, { method: 'POST' }).then((r) => r.json()), roomID);

// joinAPI joins through url as name in ctx: the same context joining twice is one person with two seats.
async function joinAPI(ctx, url, name) {
  expect((await ctx.post('/api/join', { data: { invite: new URL(url).hash.slice(1), name } })).status()).toBe(201);
}

// chain builds a three-level invite chain: Alice, a room admin, joined through I0 and shares S_A;
// Bob joined through S_A and shares S_B; Carol joined through S_B; Dan joined through S_A and
// through I1, so he keeps access when S_A goes.
async function chain(browser, request, baseURL) {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const sA = await share(alice, room.id);
  const bob = await pwRequest.newContext({ baseURL });
  await joinAPI(bob, sA.url, 'Bob');
  const sB = await (await bob.post(`/api/rooms/${room.id}/invites/share`)).json();
  const carol = await pwRequest.newContext({ baseURL });
  await joinAPI(carol, sB.url, 'Carol');
  const dan = await pwRequest.newContext({ baseURL });
  await joinAPI(dan, sA.url, 'Dan');
  await joinAPI(dan, (await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json()).url, 'Dan');
  const ids = (await (await request.get(`/api/rooms/${room.id}/invites`, { headers: admin })).json()).map((i) => i.id).sort((a, b) => a - b);
  return { room, alice, i0: ids[0], sA: sA.id, sB: sB.id, contexts: [bob, carol, dan] };
}

// people reads a list of the revoke sheet as "name marker…" lines, in any order, once the sheet
// is open: it opens on the preview's answer.
async function people(sheet, id) {
  await expect(sheet.locator('#revoke-title')).toBeVisible();
  return (await sheet.locator(`#${id} li`).allTextContents()).map((t) => t.replace(/\s+/g, ' ').trim()).sort();
}

// 011-AC26, 011-AC27: the popup opens on the shareable link with Copy, the same link on every open
// and from the empty chat's card; no list and no "Your invites" heading (D11). The card shows
// while nobody else joined, messages or not, and goes once someone does (D3).
test('the popup and the empty chat card copy one shareable link', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  const card = alice.locator('#chat-empty');
  await expect(card).toBeVisible();
  await alice.locator('#body').fill('hello, anyone?');
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('#messages')).toContainText('hello, anyone?');
  await expect(card).toBeVisible();
  // A rename's system line reloads the room, which judges the card again with the message there.
  await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json())
    .then((d) => fetch(`/api/rooms/${id}/participants/${d.me.id}/name`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: 'Alicia' }) })), room.id);
  await expect(alice.locator('#me')).toHaveText('Alicia');
  await expect(card).toBeVisible();

  const shareURL = `**/api/rooms/${room.id}/invites/share`;
  const cardButton = card.getByRole('button', { name: 'Copy invite link' });
  let release = await gate(alice, shareURL);
  await cardButton.click();
  await expect(cardButton).toBeDisabled();
  await release();
  await expect(alice.locator('#chat-invite-status')).toHaveText('Copied. Paste it to the people you invite.');
  await expect(cardButton).toBeEnabled();
  const link = await alice.evaluate(() => navigator.clipboard.readText());
  expect(link).toMatch(/\/join#[A-Za-z0-9_-]+$/);

  const popup = alice.locator('#invites');
  const copy = popup.getByRole('button', { name: 'Copy', exact: true });
  release = await gate(alice, shareURL);
  await menuItem(alice, 'Invite people');
  await expect(copy).toBeDisabled();
  await release();
  await expect(alice.locator('#invite-link')).toHaveValue(link);
  await expect(copy).toBeEnabled();
  await expect(popup).toContainText('Anyone holding this link can join and read the room.');
  // Copy and Close only: no button that makes a link, no list.
  await expect(popup.getByRole('button')).toHaveCount(2);
  await expect(popup.locator('ul, ol, h3')).toHaveCount(0);
  await expect(alice.getByText('Your invites')).toHaveCount(0);
  await alice.evaluate(() => navigator.clipboard.writeText(''));
  await copy.click();
  await expect.poll(() => alice.evaluate(() => navigator.clipboard.readText())).toBe(link);
  await popup.getByRole('button', { name: 'Close' }).click();
  await menuItem(alice, 'Invite people');
  await expect(alice.locator('#invite-link')).toHaveValue(link);

  await popup.getByRole('link', { name: 'More links and revoking in Room settings › Invites' }).click();
  await expect(popup).toBeHidden();
  await expect(alice).toHaveURL(/#manage\/invites$/);
  await expect(alice.locator('#manage #panel-invites')).toBeVisible();
  await expect(alice.getByText('Your invites')).toHaveCount(0);
  await alice.keyboard.press('Escape');

  // A second seat of your own profile is not someone else: the card stays.
  const second = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  expect((await alice.request.post('/api/join', { data: { invite: new URL(second.url).hash.slice(1), name: 'Alicia' } })).status()).toBe(201);
  await expect.poll(() => alice.evaluate(() => participants.length)).toBe(2);
  await expect(card).toBeVisible();

  const bob = await (await browser.newContext()).newPage();
  await bob.goto(new URL(link).pathname + new URL(link).hash);
  await bob.getByLabel('Your name').fill('Bob');
  await bob.getByRole('button', { name: 'Join the room' }).click();
  await expect(bob.locator('#me')).toHaveText('Bob');
  await expect(card).toBeHidden();
  await expect(bob.locator('#chat-empty')).toBeHidden();
});

// 011-AC27: a plain member's Room settings › Invites lists their own invites only, says so when
// there is none, and shows a new link once, with Copy; the button waits for the answer (011-AC26).
test('a plain member manages their own invites in Room settings', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await share(alice, room.id);
  await bob.goto(`/r/${room.id}#manage/invites`);
  const panel = bob.locator('#manage #panel-invites');
  await expect(panel.locator('#invites-empty')).toHaveText('No invite links yet. New invite link makes one to share.');
  await expect(bob.getByText('Your invites')).toHaveCount(0);

  const make = panel.getByRole('button', { name: 'New invite link' });
  const release = await gate(bob, `**/api/rooms/${room.id}/invites`, 'POST');
  await make.click();
  await expect(make).toBeDisabled();
  await release();
  await expect(panel.locator('#invite-link-new')).toHaveValue(/\/join#[A-Za-z0-9_-]+$/);
  await expect(panel.getByRole('button', { name: 'Copy', exact: true })).toBeVisible();
  await expect(make).toBeEnabled();
  const rows = panel.locator('#invites-all .list-row:not(.head)');
  await expect(rows).toHaveCount(1);
  await expect(rows).not.toContainText('shareable');
  await bob.keyboard.press('Escape');

  await menuItem(bob, 'Invite people');
  await expect(bob.locator('#invite-link')).toHaveValue(/\/join#/);
  await bob.locator('#invite-manage').click();
  // His two, not Alice's; the new link is not shown again.
  await expect(rows).toHaveCount(2);
  await expect(rows.filter({ hasText: 'Your shareable link' })).toHaveCount(1);
  await expect(panel.locator('#invite-link-new')).toHaveCount(0);
});

// 011-AC20: a revoke from Room settings updates the room page at once: the people it cut leave
// People, and the empty-chat card comes back once nobody else is left.
test('a revoke from Room settings updates People and the empty chat card', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await appointAdmin(alice, request, room.id);
  const link = (await share(alice, room.id)).url;
  const bob = await (await browser.newContext()).newPage();
  await bob.goto(new URL(link).pathname + new URL(link).hash);
  await bob.getByLabel('Your name').fill('Bob');
  await bob.getByRole('button', { name: 'Join the room' }).click();
  const people = alice.locator('#participants li .name, #participants-away li .name');
  await expect(people).toHaveText(['Alice', 'Bob']);
  await expect(alice.locator('#chat-empty')).toBeHidden();

  await alice.goto(`/r/${room.id}#manage/invites`);
  await rowAction(alice.locator('#manage #invites-all .list-row:not(.head)').filter({ hasText: 'Your shareable link' }), /^Revoke/);
  await alice.locator('#revoke-sheet').getByLabel('Also remove the 1 person who joined through it').check();
  await alice.locator('#revoke-sheet').getByRole('button', { name: 'Revoke', exact: true }).click();
  await expect(alice.locator('#toasts .toast')).toHaveText('Revoked. Bob lost access.');
  await alice.locator('#manage-close').click();
  await expect(people).toHaveText(['Alice']);
  await expect(alice.locator('#chat-empty')).toBeVisible();
});

for (const vp of widths) {
  // 011-AC19, 011-AC20, 011-AC26, 016-AC4: Room settings › Invites asks before a revoke; the box,
  // unticked, offers to remove who joined, with who loses access, admin and you marked, who keeps
  // it, the chained count; ticked, the toast names who the revoke cut.
  test(`the revoke sheet in Room settings, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const { room, alice, i0, sA, sB, contexts } = await chain(browser, request, baseURL);
    await alice.setViewportSize(vp);
    await alice.goto(`/r/${room.id}#manage/invites`);
    const panel = alice.locator('#manage #room-panel');
    await expect(panel.locator(`#invite-${sA}`)).toContainText('Your shareable link');
    await expect(panel.locator(`#invite-${sB}`)).toContainText('shareable');
    await expect(panel.locator(`#invite-${sB}`)).not.toContainText('Your');

    const sheet = alice.locator('#revoke-sheet');
    await rowAction(panel.locator(`#invite-${i0}`), /^Revoke/);
    await expect(sheet.locator('#revoke-title')).toHaveText('Revoke this invite?');
    await expect(sheet.getByLabel('Also remove the 3 people who joined through it')).not.toBeChecked();
    await expect(sheet).toContainText('Ticked, these people lose access:');
    expect(await people(sheet, 'revoke-lose')).toEqual(['Alice admin you', 'Bob', 'Carol']);
    await expect(sheet).toContainText('Keep access through another invite:');
    expect(await people(sheet, 'revoke-keep')).toEqual(['Dan']);
    await expect(sheet.locator('#revoke-chain')).toHaveText('Ticked, it also revokes 2 invites made by people who joined through it.');
    if (vp.width < 400) {
      expect((await sheet.boundingBox()).width).toBe(vp.width);
      expect(await noOverflow(alice)).toEqual([]);
    }
    await sheet.getByRole('button', { name: 'Cancel' }).click();
    await expect(sheet).toHaveCount(0);
    await expect(panel.locator(`#invite-${i0}`)).toContainText('valid');

    await rowAction(panel.locator(`#invite-${sA}`), /^Revoke/);
    expect(await people(sheet, 'revoke-lose')).toEqual(['Bob', 'Carol']);
    expect(await people(sheet, 'revoke-keep')).toEqual(['Dan']);
    await expect(sheet.locator('#revoke-chain')).toHaveText('Ticked, it also revokes 1 invite made by people who joined through it.');
    await sheet.getByLabel('Also remove the 2 people who joined through it').check();
    const release = await gate(alice, new RegExp(`/api/rooms/${room.id}/invites/${sA}\\?cut=1$`), 'DELETE');
    await sheet.getByRole('button', { name: 'Revoke', exact: true }).click();
    await expect(sheet.getByRole('button', { name: 'Revoke', exact: true })).toBeDisabled();
    await expect(sheet.getByRole('button', { name: 'Cancel' })).toBeDisabled();
    await expect(sheet.getByRole('button', { name: 'Close' })).toBeDisabled();
    // In flight, Escape leaves the sheet open, pressed twice too: an error would have nowhere else to show.
    await alice.keyboard.press('Escape');
    await alice.keyboard.press('Escape');
    await expect(sheet).toBeVisible();
    await release();
    await expect(alice.locator('#toasts .toast')).toHaveText(/^Revoked\. (Bob and Carol|Carol and Bob) lost access\.$/);
    await expect(sheet).toHaveCount(0);
    await expect(panel.locator(`#invite-${sA}`)).toContainText('revoked');
    await expect(panel.locator(`#invite-${sB}`)).toContainText('revoked');
    for (const c of contexts) await c.dispose();
  });

  // 011-AC19, 011-AC20: /admin › room › Invites asks in the same sheet; the super-admin is nobody's "you".
  test(`the revoke sheet in /admin, ${vp.name}`, async ({ browser, request, baseURL }) => {
    const { room, i0, sA, sB, contexts } = await chain(browser, request, baseURL);
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page)).toBe(201);
    await page.goto(`/admin#rooms/${room.id}/invites`);
    const panel = page.locator('#room-panel');
    await expect(panel.locator(`#invite-${sA}`)).toContainText('shareable');
    await expect(panel.locator(`#invite-${sA}`)).not.toContainText('Your');

    const sheet = page.locator('#revoke-sheet');
    await rowAction(panel.locator(`#invite-${i0}`), /^Revoke/);
    expect(await people(sheet, 'revoke-lose')).toEqual(['Alice admin', 'Bob', 'Carol']);
    expect(await people(sheet, 'revoke-keep')).toEqual(['Dan']);
    await expect(sheet.locator('#revoke-chain')).toHaveText('Ticked, it also revokes 2 invites made by people who joined through it.');
    if (vp.width < 400) {
      expect((await sheet.boundingBox()).width).toBe(vp.width);
      expect(await noOverflow(page)).toEqual([]);
    }
    await sheet.getByLabel('Also remove the 3 people who joined through it').check();
    const release = await gate(page, new RegExp(`/api/admin/rooms/${room.id}/invites/${i0}\\?cut=1$`), 'DELETE');
    await sheet.getByRole('button', { name: 'Revoke', exact: true }).click();
    await expect(sheet.getByRole('button', { name: 'Revoke', exact: true })).toBeDisabled();
    await release();
    const toast = page.locator('.toast');
    await expect(toast).toHaveText(/^Revoked\. .+ lost access\.$/);
    for (const name of ['Alice', 'Bob', 'Carol']) await expect(toast).toContainText(name);
    await expect(toast).not.toContainText('Dan');
    await expect(sheet).toHaveCount(0);
    for (const id of [i0, sA, sB]) await expect(panel.locator(`#invite-${id}`)).toContainText('revoked');
    for (const c of contexts) await c.dispose();
  });
}

for (const vp of widths) {
  // 016-AC3 016-AC4: a revoke stops new joins only. A plain member's sheet has no box; their joiner's
  // page stays live and a new join through the link fails. A room admin's box, ticked, ends the
  // joiner's page.
  test(`a revoke keeps who joined unless the box is ticked, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
    await appointAdmin(alice, request, room.id);
    const joinPage = async (url, name) => {
      const page = await (await browser.newContext()).newPage();
      await page.goto(new URL(url).pathname + new URL(url).hash);
      await page.getByLabel('Your name').fill(name);
      await page.getByRole('button', { name: 'Join the room' }).click();
      await expect(page.locator('#me')).toHaveText(name);
      return page;
    };
    const carol = await joinPage((await share(bob, room.id)).url, 'Carol');
    const bobLink = (await share(bob, room.id)).url;

    await bob.setViewportSize(vp);
    await bob.goto(`/r/${room.id}#manage/invites`);
    await rowAction(bob.locator('#manage #invites-all .list-row:not(.head)'), /^Revoke/);
    const sheet = bob.locator('#revoke-sheet');
    await expect(sheet.locator('#revoke-stays')).toHaveText('No one can join through it any more. Who joined through it keeps their seat.');
    await expect(sheet.locator('#revoke-cut')).toHaveCount(0);
    await expect(sheet.locator('#revoke-lose')).toHaveCount(0);
    if (vp.width < 400) expect(await noOverflow(bob)).toEqual([]);
    await sheet.getByRole('button', { name: 'Revoke', exact: true }).click();
    await expect(bob.locator('#toasts .toast')).toHaveText('Revoked. Nobody lost access.');
    await carol.locator('#body').fill('still here');
    await carol.locator('#body').press('Enter');
    await expect(alice.locator('#messages')).toContainText('still here');
    expect((await request.post('/api/join', { data: { invite: new URL(bobLink).hash.slice(1), name: 'Mallory' } })).status()).toBe(401);

    const dan = await joinPage((await share(alice, room.id)).url, 'Dan');
    await alice.setViewportSize(vp);
    await alice.goto(`/r/${room.id}#manage/invites`);
    const mine = alice.locator('#manage #invites-all .list-row:not(.head)').filter({ hasText: 'Your shareable link' });
    await rowAction(mine, /^Revoke/);
    const box = alice.locator('#revoke-sheet').getByLabel('Also remove the 1 person who joined through it');
    await expect(box).not.toBeChecked();
    await box.check();
    await alice.locator('#revoke-sheet').getByRole('button', { name: 'Revoke', exact: true }).click();
    await expect(alice.locator('#toasts .toast')).toHaveText('Revoked. Dan lost access.');
    await expect(dan.locator('#banner')).toHaveText('Your access to this room has ended.');
    await expect(carol.locator('#messages')).toContainText('Alice revoked an invite; Dan was removed');
  });
}
