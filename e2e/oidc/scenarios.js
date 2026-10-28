import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { test, expect } from '../fixtures.js';
import { rowAction, confirmSheet, signIn } from '../helpers.js';
import { adminToken } from '../playwright.config.js';

export const admin = { Authorization: `Bearer ${adminToken}` };
const pgdb = fileURLToPath(new URL('../../bin/pgdb', import.meta.url));
export const signInButton = 'Sign in with your organisation account';

// helpers are the steps the restricted-mode tests share, against provider, an adapter as
// e2e/oidc/provider.js, and the stack's server on database db.
export function helpers(provider, db) {
  // expire ages sub's last sign-in past the revalidation: no test clock reaches the e2e server.
  const expire = (sub) => {
    const at = new Date(Date.now() - 200 * 3600_000).toISOString();
    execFileSync(pgdb, ['exec', process.env.GRILL_ROOM_TEST_DATABASE_URL, db, `UPDATE profiles SET oidc_signed_in_at = '${at}' WHERE oidc_subject = '${sub}'`]);
  };

  async function invite(request, name) {
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    return { room, path: new URL(inv.url).pathname + new URL(inv.url).hash, token: new URL(inv.url).hash.slice(1) };
  }

  const clickSignIn = (page) => () => page.getByRole('button', { name: signInButton }).click();

  // joined signs a new browser in as u through the provider and joins the room at path.
  async function joined(browser, u, path) {
    const page = await (await browser.newContext()).newPage();
    await page.goto(path);
    await provider.signInAtProvider(page, u, clickSignIn(page));
    await expect(page.getByRole('button', { name: signInButton })).toBeHidden();
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(u.name);
    return page;
  }

  const internal = (page) => page.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((p) => p.internal));

  return { expire, invite, clickSignIn, joined, internal };
}

// restricted declares the restricted-mode tests (021, 026) against provider and the stack's server
// on database db, with the member group set.
export function restricted(provider, db) {
  const { users, signInAtProvider } = provider;
  const { expire, invite, clickSignIn, joined, internal } = helpers(provider, db);

  // 021-AC12, 027-AC2: the join page offers the provider above the name form; signing in comes back
  // to the invite, its fragment kept, and joins as internal. Without an invite only the button shows.
  test('sign in from an invite lands in the room as internal', async ({ browser, request }) => {
    const { room, path } = await invite(request, 'Org room');
    const page = await (await browser.newContext()).newPage();
    await page.goto(path);
    const button = page.getByRole('button', { name: signInButton });
    await expect(button).toBeVisible();
    await expect(page.locator('#join')).toBeVisible();
    expect(await button.evaluate((b) => b.compareDocumentPosition(document.getElementById('join')) & Node.DOCUMENT_POSITION_FOLLOWING)).toBeTruthy();
    await signInAtProvider(page, users.member, () => button.click());
    // Back on the invite: the fragment came back from the tab, never through the provider.
    await expect(button).toBeHidden();
    await expect.poll(() => new URL(page.url()).hash).toBe(new URL(path, 'http://x').hash);
    await expect(page.getByLabel('Your name')).toHaveValue(users.member.name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page).toHaveURL(new RegExp(`/r/${room.id}$`));
    await expect(page.locator('#me')).toHaveText(users.member.name);
    expect(await internal(page)).toBe(true);

    const visitor = await (await browser.newContext()).newPage();
    await visitor.goto('/');
    await expect(visitor.getByRole('button', { name: signInButton })).toBeVisible();
    await expect(visitor.locator('#join')).toBeHidden();
    await expect(visitor.locator('#create')).toBeHidden();
    await expect(visitor.locator('#recover')).toBeHidden();
    await expect(visitor.locator('#status')).toHaveText('');

    // A guest's external profile, without an invite: still the button alone.
    const guest = await (await browser.newContext()).newPage();
    await guest.goto(path);
    await guest.getByLabel('Your name').fill('Gwen');
    await guest.getByRole('button', { name: 'Join the room' }).click();
    await expect(guest.locator('#me')).toHaveText('Gwen');
    await guest.goto('/');
    await expect(guest.getByRole('button', { name: signInButton })).toBeVisible();
    await expect(guest.locator('#your-profile')).toBeHidden();
    await expect(guest.locator('#join')).toBeHidden();
    await expect(guest.locator('#status')).toHaveText('');
  });

  // 021-AC3, 027-AC2: an account outside the member group is refused, and no profile is made for it.
  test('an account outside the member group is refused', async ({ browser, request }) => {
    const { path } = await invite(request, 'Members room');
    const page = await (await browser.newContext()).newPage();
    await page.goto(path);
    await signInAtProvider(page, users.outsider, clickSignIn(page));
    await expect(page.locator('main p').first()).toHaveText('Your account is not allowed on this instance. Ask for an invite to join a room as a guest.');
    const profiles = await (await request.get('/api/admin/profiles', { headers: admin })).json();
    expect(profiles.map((p) => p.name)).not.toContain(users.outsider.name);
  });

  // 021-AC8: an expired sign-in goes through the provider silently and comes back to the same page,
  // its fragment kept; without a provider session, login_required falls back to a normal sign-in.
  test('an expired sign-in comes back through the provider to the same page', async ({ browser, request }) => {
    // Sets the fake's session flag and reads its prompt log; e2e/keycloak/lifecycle.spec.js plays
    // it with a real session (027-AC3).
    test.skip(!provider.fake, 'needs the fake provider session switch and prompt log');
    const { room, path } = await invite(request, 'Reauth room');
    const page = await joined(browser, users.member, path);
    await page.goto(`/r/${room.id}#manage/invites`);
    await provider.fake.session(request, users.member, true);
    expire(users.member.sub);
    let login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
    await page.reload();
    await login;
    await expect(page).toHaveURL(new RegExp(`/r/${room.id}#manage/invites$`));
    await expect(page.locator('#me')).toHaveText(users.member.name);
    expect(await provider.fake.prompts(request)).toEqual(['none']);

    // No session at the provider: silent, refused, then the normal sign-in.
    await provider.fake.session(request, users.member, false);
    expire(users.member.sub);
    await page.evaluate(() => sessionStorage.removeItem('grill_room_reauth_at'));
    login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
    await page.reload();
    await login;
    await expect(page).toHaveURL(new RegExp(`/r/${room.id}#manage/invites$`));
    await expect(page.locator('#me')).toHaveText(users.member.name);
    expect(await provider.fake.prompts(request)).toEqual(['none', '']);
    expect(await internal(page)).toBe(true);
  });

  // 021-AC8, 027-AC2: a room page whose stream closes for reauth signs in again, silently through
  // the provider session the sign-in left, instead of saying the access ended; with the guard
  // holding, loadRoom says to sign in again.
  test('a stream closed for reauth signs in again, the guard stops a second round', async ({ browser, request }) => {
    const { room, path, token } = await invite(request, 'Stream room');
    const page = await joined(browser, users.member, path);
    expire(users.member.sub);
    // Any other call of the page would go to sign in too: the banner tells the stream's own answer.
    await page.evaluate(() => {
      const b = document.getElementById('banner');
      new MutationObserver(() => { if (!b.hidden) sessionStorage.setItem('e2e_banner', b.textContent); })
        .observe(b, { attributes: true, childList: true, characterData: true, subtree: true });
    });
    const login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
    // Another participant's line wakes every stream at once, ahead of any poll of the page.
    const guest = await (await request.post('/api/join', { data: { invite: token, name: 'Gus' } })).json();
    await request.post(`/api/rooms/${room.id}/messages`, { headers: { Authorization: `Bearer ${guest.token}` }, data: { body: 'hello' } });
    await login;
    await expect(page).toHaveURL(new RegExp(`/r/${room.id}$`));
    await expect(page.locator('#me')).toHaveText(users.member.name);
    await expect(page.locator('#banner')).toBeHidden();
    expect(await page.evaluate(() => sessionStorage.getItem('e2e_banner'))).toBeNull();

    // A round within the last minute: no redirect, the reason instead.
    expire(users.member.sub);
    await page.evaluate(() => sessionStorage.setItem('grill_room_reauth_at', String(Date.now())));
    let again = 0;
    page.on('request', (r) => { if (r.url().includes('/auth/login')) again++; });
    await page.reload();
    await expect(page.locator('#banner')).toHaveText('Your organisation sign-in expired. Sign in again from your profile page.');
    expect(again).toBe(0);
  });

  // 021-AC8, 027-AC2: a silent sign-in that lands in another account ends on the room's refusal, once.
  test('a sign-in into a second account does not loop', async ({ browser, request }) => {
    const { room, path } = await invite(request, 'Two accounts');
    const page = await joined(browser, users.member, path);
    await provider.useAccount(page, users.second);
    expire(users.member.sub);
    let logins = 0;
    page.on('request', (r) => { if (r.url().includes('/auth/login')) logins++; });
    await page.goto(`/r/${room.id}`);
    await expect(page.locator('#banner')).toContainText('You are not in this room');
    await page.waitForTimeout(2000);
    expect(logins).toBe(1);
    // Elsewhere a login form nobody fills would have kept the banner away.
    if (provider.fake) expect(await provider.fake.prompts(request)).toEqual(['none']);
  });

  // 021-AC11, 021-AC10, 027-AC2: a member of the admin group opens /admin without the token;
  // Profiles shows internal and external profiles, an expired sign-in, and Unbind makes one
  // external. Without the group the token form shows, and the token still works.
  test('admin through the provider group, and Unbind', async ({ browser, request }) => {
    const { path } = await invite(request, 'Admin room');
    const member = await joined(browser, users.member, path);
    const guest = await (await browser.newContext()).newPage();
    await guest.goto(path);
    await guest.getByLabel('Your name').fill('Gil');
    await guest.getByRole('button', { name: 'Join the room' }).click();
    await expect(guest.locator('#me')).toHaveText('Gil');
    // An internal outside the admin group gets the token form.
    await member.goto('/admin');
    await expect(member.locator('#token')).toBeVisible();
    // Its provider button says why rather than going round through the provider.
    let rounds = 0;
    member.on('request', (r) => { if (r.url().includes('/auth/login')) rounds++; });
    await member.getByRole('button', { name: signInButton }).click();
    const outside = member.locator('#not-admin');
    await expect(outside).toContainText('Your organisation account is not in the admin group.');
    await expect(outside.getByRole('link', { name: 'Sign in again' })).toHaveAttribute('href', '/auth/login?next=/admin');
    await member.waitForTimeout(1000);
    expect(rounds).toBe(0);
    await expect(member).toHaveURL(/\/admin$/);
    expire(users.member.sub);

    const ops = await (await browser.newContext()).newPage();
    await signInAtProvider(ops, users.admin, () => ops.goto('/auth/login?next=/admin'));
    await expect(ops.locator('#logout')).toBeVisible();
    await expect(ops.locator('#token')).toHaveCount(0);
    // Sign out sticks: no new session by itself, the provider's button instead, which signs in
    // again through the provider session.
    await ops.locator('#logout').click();
    await expect(ops.locator('#token')).toBeVisible();
    await ops.reload();
    await expect(ops.locator('#token')).toBeVisible();
    await ops.getByRole('button', { name: signInButton }).click();
    await expect(ops.locator('#logout')).toBeVisible();
    expect(await ops.evaluate(() => localStorage.getItem('grill_room_admin_signed_out'))).toBeNull();
    await ops.goto('/admin#profiles');
    const row = (name) => ops.locator('#profiles .list-row').filter({ hasText: name });
    const memberRow = row(users.member.name);
    await expect(row(users.admin.name).locator('.sign-in')).toContainText(/^Internal · /);
    await expect(memberRow.locator('.sign-in')).toHaveText('Internal · sign-in expired');
    await expect(row('Gil').locator('.sign-in')).toHaveText('External');
    await rowAction(memberRow, new RegExp(`^Unbind ${users.member.name}`));
    await confirmSheet(ops, 'Unbind');
    await expect(memberRow.locator('.sign-in')).toHaveText('External');
    await memberRow.locator('.actions .more').click();
    await expect(memberRow.locator('.menu').getByRole('button', { name: /^Unbind/ })).toHaveCount(0);
    await ops.keyboard.press('Escape');
    const audit = await (await request.get('/api/admin/audit?action=profile.oidc_unbind', { headers: admin })).json();
    expect(audit.entries.length).toBe(1);
    const logins = await (await request.get('/api/admin/audit?action=admin.login', { headers: admin })).json();
    expect(logins.entries.some((e) => e.details.via === 'oidc')).toBe(true);
    // Revoked from another browser, the provider session ends on the page's next call: the form
    // shows, nothing reopens it by itself.
    const sessions = await (await request.get('/api/admin/sessions', { headers: admin })).json();
    const mine = sessions.find((x) => x.profile_name === users.admin.name);
    expect((await request.delete(`/api/admin/sessions/${mine.id}`, { headers: admin })).status()).toBe(204);
    await ops.locator('#tab-sessions').click();
    await expect(ops.locator('#token')).toBeVisible();
    await expect(ops.locator('.login p[role=alert]')).toHaveText('Your session ended: sign in again.');
    await ops.getByRole('button', { name: signInButton }).click();
    await expect(ops.locator('#logout')).toBeVisible();
    // Unbound, the member's browser is an external, still signed in.
    await member.goto('/');
    expect(await internal(member)).toBe(false);
    // The admin token keeps working.
    expect(await signIn(member)).toBe(201);

    // An expired sign-in ends the provider session; the button, not a sign-out, brings it back.
    expire(users.admin.sub);
    await ops.goto('/admin');
    await expect(ops.locator('#token')).toBeVisible();
    await ops.getByRole('button', { name: signInButton }).click();
    await expect(ops.locator('#logout')).toBeVisible();
  });

  // guestThenSignIn joins the room of an invite as a guest named name, then signs in from the home
  // page as u; it returns the page on the attach offer.
  async function guestThenSignIn(browser, request, u, name) {
    const { room, path } = await invite(request, `${name}'s room`);
    const page = await (await browser.newContext()).newPage();
    await page.goto(path);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    await page.goto('/');
    await signInAtProvider(page, u, clickSignIn(page));
    await expect(page.getByRole('heading', { name: 'Attach your guest profile' })).toBeVisible();
    await expect(page.locator('main')).toContainText(`Attach your guest profile ${name} (1 room) to your organisation account, signed in as ${u.name}?`);
    await expect(page.locator('main li')).toHaveText([`Rooms: ${name}'s room`]);
    return { page, room };
  }

  const profileRooms = async (page) => {
    await page.goto('/profile');
    return page.locator('#rooms li .room-line a').allTextContents();
  };

  // 026-AC3, 026-AC4, 021-AC4, 027-AC2: a guest who signs in is offered to attach the guest profile;
  // Attach brings its room to the bound profile, audited as an oidc_attach merge. The account's
  // email is unverified: the profile does not take it.
  test('a guest signing in attaches the guest profile', async ({ browser, request }) => {
    const { page, room } = await guestThenSignIn(browser, request, users.unverified, 'Hana');
    await page.getByRole('button', { name: 'Attach' }).click();
    await expect(page).toHaveURL(/\/$/);
    expect(await internal(page)).toBe(true);
    // An unverified address is never taken: it would bind whichever profile holds it.
    expect(await page.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((p) => p.profile.email))).toBeUndefined();
    await expect.poll(() => profileRooms(page)).toEqual([room.name]);
    const audit = await (await request.get('/api/admin/audit?action=profile.merge', { headers: admin })).json();
    expect(audit.entries.some((e) => e.details.via === 'oidc_attach' && e.details.merge_name === 'Hana')).toBe(true);
  });

  // 026-AC4, 027-AC2: Not now keeps the guest profile apart: the bound profile has no room.
  test('Not now keeps the guest profile apart', async ({ browser, request }) => {
    const { page } = await guestThenSignIn(browser, request, users.later, 'Ivan');
    await page.getByRole('button', { name: 'Not now' }).click();
    await expect(page).toHaveURL(/\/$/);
    expect(await internal(page)).toBe(true);
    expect(await page.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((p) => p.profile.rooms.length))).toBe(0);
  });

  // 026-AC4, 027-AC2: the admin merge sheet warns when the result is an organisation account,
  // naming the guest side whose devices are signed out, in both directions.
  test('the merge sheet warns about a bound result', async ({ browser, request }) => {
    const { path } = await invite(request, 'Merge warning room');
    const member = await joined(browser, users.member, path);
    const guest = await (await browser.newContext()).newPage();
    await guest.goto(path);
    await guest.getByLabel('Your name').fill('Lena');
    await guest.getByRole('button', { name: 'Join the room' }).click();
    await expect(guest.locator('#me')).toHaveText('Lena');
    const idOf = async (page) => (await (await page.request.get('/api/profile')).json()).profile.id;
    const [kira, lena] = [await idOf(member), await idOf(guest)];
    const page = await (await browser.newContext()).newPage();
    expect(await signIn(page, adminToken, 'profiles')).toBe(201);
    await rowAction(page.locator('#profile-' + lena), /^Merge Lena into another profile/);
    const sheet = page.locator('#merge-sheet');
    const warning = 'The result is an organisation account: devices signed into Lena are signed out, its paired daemons removed and their running turn failed.';
    await sheet.locator('#merge-keep').selectOption(String(kira));
    await expect(sheet.locator('#merge-bound')).toHaveText(warning);
    await sheet.locator('#merge-swap').click();
    await expect(sheet.locator('#merge-keep')).toHaveValue(String(lena));
    await expect(sheet.locator('#merge-bound')).toHaveText(warning);
  });
}
