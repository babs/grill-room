import { test, expect } from '../fixtures.js';
import { admin, nextMail, joinRoom, linkPath, ownProfile, newContext, durationText } from './helpers.js';
import { magicLinkTTL } from './playwright.config.js';
import { uniq } from '../helpers.js';

// 011-AC22 011-AC24: the page names the lifetime the server's mail names.
const lifetime = durationText(magicLinkTTL);
const neutral = 'If a profile uses this address, a link is on its way';
const anonText = 'Profile without email: add one to find it on another device';

// Every spec here makes its own room and profiles; they go at the end, as on the lab they would pile up.
const made = { rooms: [], profiles: [] };
test.afterAll(async ({ request }) => {
  for (const id of made.rooms) await request.delete(`/api/rooms/${id}`, { headers: admin });
  for (const id of made.profiles) await request.delete(`/api/profiles/${id}`, { headers: admin });
});

async function newRoom(request, name) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: `${name} ${uniq()}` } })).json();
  made.rooms.push(room.id);
  return room;
}

// member joins a new room as name and returns its page, with its profile kept for the cleanup.
async function member(browser, request, room, name) {
  const page = await joinRoom(browser, request, room.id, name);
  made.profiles.push((await ownProfile(page)).id);
  return page;
}

// bindEmail adds email to page's profile through the mailed link, n being that mail's number.
async function bindEmail(page, request, email, n = 1) {
  expect((await page.request.post('/api/profile/email', { data: { email } })).status()).toBe(202);
  const mail = await nextMail(request, email, n);
  const token = new URL(mail.link).hash.split('=')[1];
  expect((await page.request.post('/api/profile/magic', { data: { token, purpose: 'bind' } })).status()).toBe(200);
}

const invitePath = async (request, roomID) => linkPath((await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json()).url);

// 011-AC4 011-AC5 011-AC22 011-AC25: a signed-out join page offers the email recovery; the mailed link
// signs the browser in, and a join tab left open shows the profile once focused, then joins as it.
test('recover a profile by email from the join page', async ({ browser, request }) => {
  const email = `recover-${uniq()}@example.com`;
  const room = await newRoom(request, 'Recover');
  const alice = await member(browser, request, room, 'Alice');
  await bindEmail(alice, request, email);
  const id = (await ownProfile(alice)).id;

  // A browser holding a profile sees its name, never the recovery link.
  await alice.goto(await invitePath(request, room.id));
  await expect(alice.locator('#your-profile')).toBeVisible();
  await expect(alice.getByRole('link', { name: 'Your profile Alice' })).toHaveAttribute('href', '/profile');
  await expect(alice.locator('#recover')).toBeHidden();

  const ctx = await newContext(browser);
  const join = await ctx.newPage();
  const second = await room2Path(request);
  await join.goto(second);
  await expect(join.locator('#your-profile')).toBeHidden();
  await join.getByRole('button', { name: 'Recover my profile by email' }).click();
  await expect(join.locator('#recover-email')).toBeFocused();
  // The same answer for an address nobody holds.
  await join.locator('#recover-email').fill(`nobody-${uniq()}@example.com`);
  await join.getByRole('button', { name: 'Email me a sign-in link' }).click();
  await expect(join.locator('#recover .link-sent h3')).toHaveText(neutral);
  await expect(join.locator('#recover .link-sent')).toContainText(`The link works once, within ${lifetime}.`);
  const resend = join.locator('#recover .link-sent').getByRole('button', { name: /^Resend/ });
  await expect(resend).toHaveText(/^Resend \(\d+ s\)$/);
  await expect(resend).toBeDisabled();

  // The page's cooldown is the page's; the address the person meant goes through the API here.
  expect((await ctx.request.post('/api/profile/signin', { data: { email } })).status()).toBe(202);
  const mail = await nextMail(request, email, 2);
  expect(mail.subject).toBe('Sign in to grill-room');
  // 011-AC24: the mail says what the link does.
  expect(mail.text).toContain('signs you in to your grill-room profile');
  expect(mail.text).toContain(`within ${lifetime}.`);

  const tab = await ctx.newPage();
  await tab.goto(linkPath(mail.link));
  await tab.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(tab.locator('#status')).toHaveText('Signed in as Alice on this device.');
  await expect(tab.locator('#rooms li .room-line')).toContainText([room.name]);

  await join.bringToFront();
  await join.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(join.getByRole('link', { name: 'Your profile Alice' })).toBeVisible();
  await expect(join.locator('#recover')).toBeHidden();
  await join.getByLabel('Your name').fill('Alice again');
  await join.getByRole('button', { name: 'Join the room' }).click();
  await expect(join.locator('#me')).toHaveText('Alice again');
  expect((await ownProfile(join)).id).toBe(id);
});

// room2Path is an invite into a second room, so the join after the recovery makes a new seat.
async function room2Path(request) {
  const other = await newRoom(request, 'Recover second');
  return invitePath(request, other.id);
}

// 011-AC21 011-AC9: the Email section's states, none, pending (through a reload), another
// address, bound with Change, and a changed address once its link is used.
test('email section states', async ({ browser, request }) => {
  const stamp = uniq();
  const [first, other, changed] = ['first', 'other', 'changed'].map((k) => `${k}-${stamp}@example.com`);
  const room = await newRoom(request, 'States');
  const page = await member(browser, request, room, 'Sam');
  await page.goto('/profile#account');
  await expect(page.locator('#email-state')).toHaveText('No email yet. Add one to sign in on other devices.');
  await expect(page.locator('#profile-anon')).toBeVisible();
  await page.getByLabel('Add an email').fill(first);
  await page.getByRole('button', { name: 'Send me a link' }).click();
  const sent = page.locator('#email .link-sent');
  await expect(sent.locator('h3')).toHaveText('Link sent to ' + first);
  await expect(sent.getByRole('button', { name: /^Resend/ })).toHaveText(/^Resend \(\d+ s\)$/);
  await expect(sent.getByRole('button', { name: /^Resend/ })).toBeDisabled();
  await expect(page.locator('#bind')).toBeHidden();
  await nextMail(request, first, 1);
  // The reload reads the pending link with dates set here: the config's 4 s links could expire
  // before a slow reload answers.
  await page.route('**/api/profile', async (route) => {
    const body = await (await route.fetch()).json();
    Object.assign(body.profile, { pending_email: first, pending_age_s: 0, pending_ttl_s: 10 * 60 });
    await route.fulfill({ json: body });
  });
  await page.reload();
  await expect(sent.locator('h3')).toHaveText('Link sent to ' + first);
  await page.unroute('**/api/profile');

  await page.getByRole('button', { name: 'Use another address' }).click();
  await expect(page.locator('#bind-email')).toBeFocused();
  await expect(page.locator('#bind-email')).toHaveValue(first);
  await page.locator('#bind-email').fill(other);
  await page.getByRole('button', { name: 'Send me a link' }).click();
  await expect(sent.locator('h3')).toHaveText('Link sent to ' + other);
  const bind = await nextMail(request, other, 1);
  expect(bind.text).toContain(`This link adds ${other} to a grill-room profile`);
  expect(bind.text).toContain(`within ${lifetime}.`);
  await page.goto(linkPath(bind.link));
  await expect(page.locator('#status')).toHaveText(`Email added to your profile: ${other}.`);
  await expect(page.locator('#email-state')).toHaveText(other);
  await expect(page.locator('#profile-anon')).toBeHidden();

  await page.locator('#email-change').click();
  await page.getByLabel('New address').fill(changed);
  await page.getByRole('button', { name: 'Send me a link' }).click();
  await expect(page.locator('#email .link-sent h3')).toHaveText('Link sent to ' + changed);
  const change = await nextMail(request, changed, 1);
  await page.goto(linkPath(change.link));
  await expect(page.locator('#email-state')).toHaveText(changed);
});

// 011-AC23: a bind link opened in another browser binds after a confirmation naming the profile,
// leaving that browser as it was; a used sign-in link offers the email sign-in; an address another
// profile took meanwhile says so, and that the admin can merge the two.
test('link outcomes: bind elsewhere, used, taken', async ({ browser, request }) => {
  const stamp = uniq();
  const room = await newRoom(request, 'Outcomes');
  const bob = await member(browser, request, room, 'Bob');
  const elsewhere = `elsewhere-${stamp}@example.com`;
  expect((await bob.request.post('/api/profile/email', { data: { email: elsewhere } })).status()).toBe(202);
  const bind = await nextMail(request, elsewhere, 1);
  const phone = await (await newContext(browser)).newPage();
  await phone.goto(linkPath(bind.link));
  await expect(phone.locator('#confirm-text')).toHaveText(`This link adds ${elsewhere} to Bob's profile. This browser stays as it is. Only continue if you asked for this link yourself.`);
  await phone.getByRole('button', { name: 'Add the email' }).click();
  await expect(phone.locator('#status')).toHaveText("Email added to Bob's profile.");
  await expect(phone.locator('#signed-out')).toBeVisible();
  expect((await ownProfile(phone)) || null).toBeNull();
  expect((await ownProfile(bob)).email).toBe(elsewhere);

  // Used: a sign-in link opened twice, here by a browser with no profile.
  expect((await phone.request.post('/api/profile/signin', { data: { email: elsewhere } })).status()).toBe(202);
  const signin = await nextMail(request, elsewhere, 2);
  const laptop = await (await newContext(browser)).newPage();
  await laptop.goto(linkPath(signin.link));
  await laptop.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(laptop.locator('#status')).toHaveText('Signed in as Bob on this device.');
  await phone.goto(linkPath(signin.link));
  await phone.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(phone.locator('#status')).toHaveText('This link was already used.');
  await phone.getByRole('button', { name: 'Sign in by email' }).click();
  await expect(phone.locator('#signin-email')).toBeFocused();

  // Taken: two profiles ask for one address; the second link finds it gone.
  const taken = `taken-${stamp}@example.com`;
  const carol = await member(browser, request, room, 'Carol');
  const dan = await member(browser, request, room, 'Dan');
  expect((await carol.request.post('/api/profile/email', { data: { email: taken } })).status()).toBe(202);
  const forCarol = await nextMail(request, taken, 1);
  expect((await dan.request.post('/api/profile/email', { data: { email: taken } })).status()).toBe(202);
  const forDan = await nextMail(request, taken, 2);
  await carol.goto(linkPath(forCarol.link));
  await expect(carol.locator('#status')).toHaveText(`Email added to your profile: ${taken}.`);
  await dan.goto(linkPath(forDan.link));
  await expect(dan.locator('#status')).toHaveText('This address is already on another profile. Sign in with it to open that profile, or ask the admin to merge the two profiles.');
  expect((await ownProfile(dan)).email).toBeUndefined();
});

// 011-AC6 011-AC7 011-AC8 011-AC9: the marker shows on your own avatar and row only, its tip on
// hover and focus, your own card carries it, a click opens the email form; the nudge comes after
// your first message, "Later" snoozes it on every device of the profile; a bound email takes both away.
test('no-email marker and nudge', async ({ browser, request }) => {
  const room = await newRoom(request, 'Marker');
  const alice = await member(browser, request, room, 'Alice');
  const bob = await member(browser, request, room, 'Bob');
  await alice.reload();
  await expect(alice.locator('#me-anon')).toBeVisible();
  const aliceRow = (page) => page.locator('#participants li', { has: page.locator('.name', { hasText: /^Alice$/ }) });
  const bobRow = (page) => page.locator('#participants li', { has: page.locator('.name', { hasText: /^Bob$/ }) });
  const mark = aliceRow(alice).locator('.anon-mark');
  await expect(mark).toBeVisible();
  await expect(mark).toHaveAttribute('aria-label', anonText);
  await expect(bobRow(alice).locator('.anon-mark')).toHaveCount(0);
  // Bob's own page marks Bob, never Alice.
  await expect(bob.locator('#me-anon')).toBeVisible();
  await expect(aliceRow(bob)).toBeVisible();
  await expect(aliceRow(bob).locator('.anon-mark')).toHaveCount(0);

  await alice.locator('#me-anon').hover();
  await expect(alice.locator('#person-tip')).toHaveText(anonText);
  await alice.mouse.move(0, 0);
  await expect(alice.locator('#person-tip')).toBeHidden();
  await mark.focus();
  await expect(alice.locator('#person-tip')).toHaveText(anonText);
  await alice.keyboard.press('Escape');

  await aliceRow(alice).locator('.name').click();
  await expect(alice.locator('#person-card').getByRole('link', { name: anonText })).toHaveAttribute('href', '/profile#email');
  await aliceRow(bob).locator('.name').click();
  await expect(bob.locator('#person-card')).toBeVisible();
  await expect(bob.locator('#person-card')).not.toContainText(anonText);
  await bob.keyboard.press('Escape');

  // The banner, after the first message, in place of the recovery link notice.
  await expect(alice.locator('#email-nudge')).toBeHidden();
  await alice.locator('#body').fill('hello');
  await alice.locator('#body').press('Enter');
  await expect(alice.locator('#email-nudge')).toBeVisible();
  await expect(alice.locator('#notice')).toBeHidden();
  await alice.getByRole('button', { name: 'Later' }).click();
  await expect(alice.locator('#email-nudge')).toBeHidden();
  expect((await ownProfile(alice)).email_nudge_until).toBeTruthy();

  // A second device of the same profile, signed in with an admin recovery link: snoozed there too.
  const link = await (await request.post(`/api/admin/profiles/${(await ownProfile(alice)).id}/recovery-link`, { headers: admin })).json();
  const tablet = await (await newContext(browser, { hasTouch: true })).newPage();
  await tablet.goto(linkPath(link.url));
  await tablet.getByRole('button', { name: 'Sign in to this profile' }).click();
  await expect(tablet.locator('#status')).toHaveText('Signed in on this device.');
  await tablet.getByRole('link', { name: room.name }).click();
  await expect(tablet.locator('#me-anon')).toBeVisible();
  // 011-AC7: touch has no hover, so the first tap shows the tip and stays on the room.
  const roomURL = tablet.url();
  await tablet.locator('#me-anon').tap();
  await expect(tablet.locator('#person-tip')).toHaveText(anonText);
  expect(tablet.url()).toBe(roomURL);
  await tablet.locator('#body').fill('from the tablet');
  await tablet.locator('#body').press('Enter');
  await expect(tablet.locator('.msg.text').last()).toContainText('from the tablet');
  // The send's own nudge may still be on its way: run it here, on the state the page loaded.
  expect(await tablet.evaluate(async () => { await nudgeRecovery(); return document.getElementById('email-nudge').hidden; })).toBe(true);

  // The marker opens the email form.
  await alice.locator('#me-anon').click();
  await expect(alice).toHaveURL(/\/profile#account$/);
  await expect(alice.locator('#bind-email')).toBeFocused();

  await bindEmail(alice, request, `marked-${uniq()}@example.com`);
  await tablet.reload();
  await expect(aliceRow(tablet)).toBeVisible();
  // The page has read the profile once own holds the address: the marker's absence then means something.
  await expect.poll(() => tablet.evaluate(() => own.profile && own.profile.email)).toBeTruthy();
  await expect(tablet.locator('#me-anon')).toBeHidden();
  await expect(aliceRow(tablet).locator('.anon-mark')).toHaveCount(0);
  expect(await tablet.evaluate(async () => { await nudgeRecovery(); return document.getElementById('email-nudge').hidden; })).toBe(true);
  await tablet.goto('/profile#account');
  await expect(tablet.locator('#email-state')).toHaveText(/^marked-\d+@example\.com$/);
  await expect(tablet.locator('#email-change')).toBeVisible();
});
