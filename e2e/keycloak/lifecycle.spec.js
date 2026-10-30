import { test, expect } from '../fixtures.js';
import { admin, signInButton, helpers } from '../oidc/scenarios.js';
import * as provider from './provider.js';
import { db, kcURL } from './playwright.config.js';
import { endSessions, disable, leaveGroup, signingKeys, rotateKeys } from './admin.js';

// What only a real provider plays: its session, and an account changed at Keycloak while the
// server runs. Each test has its own account (provider.js): none depends on another's changes.
const { users, signInAtProvider } = provider;
const { expire, invite, clickSignIn, joined, internal } = helpers(provider, db);

// The tests age a sign-in with the page off the room, which would otherwise start its own round
// meanwhile, then reopen it waiting for the commit only: the round navigates away before the load.

// loginForms counts the Keycloak pages page is shown: a silent sign-in only redirects.
function loginForms(page) {
  let n = 0;
  page.on('response', (r) => {
    if (r.request().isNavigationRequest() && r.url().startsWith(kcURL) && r.status() === 200) n++;
  });
  return () => n;
}

// 027-AC3, 021-AC8: past the revalidation, the page goes through prompt=none on the live Keycloak
// session and comes back without a login form; with the session ended at Keycloak, it lands on the
// login form, and signing in brings it back to the page it was on.
test('revalidation rides the Keycloak session, then its login form once the session ends', async ({ browser, request }) => {
  const u = users.revalidate;
  const { room, path } = await invite(request, 'Revalidate room');
  const page = await joined(browser, u, path);
  const roomURL = `/r/${room.id}#manage/invites`;
  await page.goto(roomURL);
  const forms = loginForms(page);
  await page.goto('about:blank');
  expire(u.sub);
  let login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
  await page.goto(roomURL, { waitUntil: 'commit' });
  await login;
  await expect(page).toHaveURL(new RegExp(`/r/${room.id}#manage/invites$`));
  await expect(page.locator('#me')).toHaveText(u.name);
  expect(forms()).toBe(0);

  // The guard holds a second round within a minute: this one is the next.
  await page.evaluate(() => sessionStorage.removeItem('grill_room_reauth_at'));
  await page.goto('about:blank');
  await endSessions(request, u);
  expire(u.sub);
  login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
  await signInAtProvider(page, u, async () => {
    await page.goto(roomURL, { waitUntil: 'commit' });
    await login;
  });
  expect(forms()).toBe(1);
  await expect(page).toHaveURL(new RegExp(`/r/${room.id}#manage/invites$`));
  await expect(page.locator('#me')).toHaveText(u.name);
  expect(await internal(page)).toBe(true);
});

// 027-AC4, 021-AC7, 021-AC8: an account disabled at Keycloak cannot sign in again; past the
// revalidation its API token and seat answer 401 reauth and its invite stops admitting.
test('an account disabled at Keycloak loses its token, seat and invite', async ({ browser, request }) => {
  const u = users.disabled;
  const { room, path } = await invite(request, 'Disabled room');
  const page = await joined(browser, u, path);
  const made = await page.evaluate(() => fetch('/api/profile/tokens', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'ci', scope: 'read' }) }).then((r) => r.json()));
  const memberInvite = () => page.evaluate((id) => fetch(`/api/rooms/${id}/invites`, { method: 'POST' }).then((r) => r.json()), room.id);
  const join = async (inv, name) => (await browser.newContext()).request.post('/api/join', { data: { invite: new URL(inv.url).hash.slice(1), name } });
  const [mine, probe] = [await memberInvite(), await memberInvite()];
  expect((await request.get('/api/profile', { headers: { Authorization: `Bearer ${made.token}` } })).status()).toBe(200);
  expect((await join(probe, 'Pia')).status()).toBe(201);

  await page.goto('about:blank');
  await disable(request, u);
  expire(u.sub);
  const token = await request.get('/api/profile', { headers: { Authorization: `Bearer ${made.token}` } });
  expect([token.status(), (await token.json()).reason]).toEqual([401, 'reauth']);
  const seat = await page.request.get(`/api/rooms/${room.id}`);
  expect([seat.status(), (await seat.json()).reason]).toEqual([401, 'reauth']);
  const fresh = await join(mine, 'Vic');
  expect([fresh.status(), (await fresh.json()).error]).toEqual([401, 'This invite no longer works']);

  // The page's sign-in round: Keycloak refuses the account, the server gets no sign-in.
  await signInAtProvider(page, u, () => page.goto(`/r/${room.id}`, { waitUntil: 'commit' }));
  await expect(page.getByText('Account is disabled')).toBeVisible();
  const profiles = await (await request.get('/api/admin/profiles', { headers: admin })).json();
  expect(profiles.find((p) => p.name === u.name).sign_in_expired).toBe(true);
});

// 027-AC5, 021-AC3, 021-AC11: out of the admin group at Keycloak, the next sign-in ends the
// provider admin session and /admin no longer opens; out of the member group, the next sign-in is
// refused.
test('groups removed at Keycloak take effect at the next sign-in', async ({ browser, request }) => {
  const u = users.groups;
  const ops = await (await browser.newContext()).newPage();
  await signInAtProvider(ops, u, () => ops.goto('/auth/login?next=/admin'));
  await expect(ops.locator('#logout')).toBeVisible();
  const cookie = (await ops.context().cookies()).find((c) => c.name === 'grill_room_admin');
  const asAdmin = () => request.get('/api/admin/sessions', { headers: { Cookie: `${cookie.name}=${cookie.value}` } });
  expect((await asAdmin()).status()).toBe(200);
  const sessionsOf = async () => (await (await request.get('/api/admin/sessions', { headers: admin })).json()).filter((x) => x.profile_name === u.name);
  expect(await sessionsOf()).toHaveLength(1);

  await leaveGroup(request, u, 'grill-admins');
  // The next sign-in, through the live Keycloak session.
  await ops.goto('/auth/login?next=/admin');
  await expect(ops.getByRole('button', { name: signInButton })).toBeVisible();
  expect((await asAdmin()).status()).toBe(401);
  expect(await sessionsOf()).toEqual([]);
  await ops.getByRole('button', { name: signInButton }).click();
  await expect(ops.locator('#not-admin')).toContainText('Your organisation account is not in the admin group.');

  await leaveGroup(request, u, 'grill-members');
  await ops.goto('/auth/login');
  await expect(ops.locator('main p').first()).toHaveText('Your account is not allowed on this instance. Ask for an invite to join a room as a guest.');
});

// 027-AC6: signing keys rotated at Keycloak with the server running (a new active key, the old one
// disabled): a new sign-in succeeds, the server fetching the new key it has not seen.
test('a sign-in after Keycloak rotates its signing keys', async ({ browser, request }) => {
  const u = users.keys;
  const signedIn = async () => {
    const page = await (await browser.newContext()).newPage();
    await page.goto('/');
    await signInAtProvider(page, u, clickSignIn(page));
    await expect(page).toHaveURL(/^http:\/\/127\.0\.0\.1:\d+\/$/);
    return internal(page);
  };
  // The server holds the old key first.
  expect(await signedIn()).toBe(true);
  const before = await signingKeys(request);
  await rotateKeys(request);
  const after = await signingKeys(request);
  expect(after).toHaveLength(1);
  expect(before).not.toContain(after[0]);
  expect(await signedIn()).toBe(true);
});
