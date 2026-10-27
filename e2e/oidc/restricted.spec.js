import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { test, expect } from '../fixtures.js';
import { rowAction, confirmSheet, signIn } from '../helpers.js';
import { adminToken } from '../playwright.config.js';
import { idpURL, db, adminGroup } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const pgdb = fileURLToPath(new URL('../../bin/pgdb', import.meta.url));
const signInButton = 'Sign in with your organisation account';

// provider sets the user the fake provider signs in next and whether it holds a session, and
// clears the prompts it saw.
const provider = async (request, user, session) => {
  expect((await request.post(`${idpURL}/control`, { data: { user, session } })).status()).toBe(204);
};
const prompts = async (request) => (await (await request.get(`${idpURL}/control`)).json()).prompts;

// expire ages sub's last sign-in past the revalidation: no test clock reaches the e2e server.
const expire = (sub) => {
  const at = new Date(Date.now() - 200 * 3600_000).toISOString();
  execFileSync(pgdb, ['exec', process.env.GRILL_ROOM_TEST_DATABASE_URL, db, `UPDATE profiles SET oidc_signed_in_at = '${at}' WHERE oidc_subject = '${sub}'`]);
};

const user = (sub, name, groups = []) => ({ sub, name, email: `${sub}@example.com`, groups });

async function invite(request, name) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  return { room, path: new URL(inv.url).pathname + new URL(inv.url).hash, token: new URL(inv.url).hash.slice(1) };
}

// joined signs a new browser in as sub through the provider and joins the room at path as name.
async function joined(browser, request, sub, name, path) {
  await provider(request, user(sub, name), false);
  const page = await (await browser.newContext()).newPage();
  await page.goto(path);
  await page.getByRole('button', { name: signInButton }).click();
  await expect(page.getByRole('button', { name: signInButton })).toBeHidden();
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

const internal = (page) => page.evaluate(() => fetch('/api/profile').then((r) => r.json()).then((p) => p.internal));

// 021-AC12: the join page offers the provider above the name form; signing in comes back to the
// invite, its fragment kept, and joins as internal. Without an invite only the button shows.
test('sign in from an invite lands in the room as internal', async ({ browser, request }) => {
  const { room, path } = await invite(request, 'Org room');
  await provider(request, user('ana', 'Ana'), false);
  const page = await (await browser.newContext()).newPage();
  await page.goto(path);
  const button = page.getByRole('button', { name: signInButton });
  await expect(button).toBeVisible();
  await expect(page.locator('#join')).toBeVisible();
  expect(await button.evaluate((b) => b.compareDocumentPosition(document.getElementById('join')) & Node.DOCUMENT_POSITION_FOLLOWING)).toBeTruthy();
  await button.click();
  // Back on the invite: the fragment came back from the tab, never through the provider.
  await expect(button).toBeHidden();
  expect(new URL(page.url()).hash).toBe(new URL(path, 'http://x').hash);
  await expect(page.getByLabel('Your name')).toHaveValue('Ana');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page).toHaveURL(new RegExp(`/r/${room.id}$`));
  await expect(page.locator('#me')).toHaveText('Ana');
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

// 021-AC8: an expired sign-in goes through the provider silently and comes back to the same page,
// its fragment kept; without a provider session, login_required falls back to a normal sign-in.
test('an expired sign-in comes back through the provider to the same page', async ({ browser, request }) => {
  const { room, path } = await invite(request, 'Reauth room');
  const page = await joined(browser, request, 'bea', 'Bea', path);
  await page.goto(`/r/${room.id}#manage/invites`);
  await provider(request, user('bea', 'Bea'), true);
  expire('bea');
  let login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
  await page.reload();
  await login;
  await expect(page).toHaveURL(new RegExp(`/r/${room.id}#manage/invites$`));
  await expect(page.locator('#me')).toHaveText('Bea');
  expect(await prompts(request)).toEqual(['none']);

  // No session at the provider: silent, refused, then the normal sign-in.
  await provider(request, user('bea', 'Bea'), false);
  expire('bea');
  await page.evaluate(() => sessionStorage.removeItem('grill_room_reauth_at'));
  login = page.waitForRequest((r) => r.url().includes('/auth/login?prompt=none'));
  await page.reload();
  await login;
  await expect(page).toHaveURL(new RegExp(`/r/${room.id}#manage/invites$`));
  await expect(page.locator('#me')).toHaveText('Bea');
  expect(await prompts(request)).toEqual(['none', '']);
  expect(await internal(page)).toBe(true);
});

// 021-AC8: a room page whose stream closes for reauth signs in again instead of saying the access
// ended; with the guard holding, loadRoom says to sign in again.
test('a stream closed for reauth signs in again, the guard stops a second round', async ({ browser, request }) => {
  const { room, path, token } = await invite(request, 'Stream room');
  const page = await joined(browser, request, 'cyd', 'Cyd', path);
  await provider(request, user('cyd', 'Cyd'), true);
  expire('cyd');
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
  await expect(page.locator('#me')).toHaveText('Cyd');
  await expect(page.locator('#banner')).toBeHidden();
  expect(await page.evaluate(() => sessionStorage.getItem('e2e_banner'))).toBeNull();

  // A round within the last minute: no redirect, the reason instead.
  expire('cyd');
  await page.evaluate(() => sessionStorage.setItem('grill_room_reauth_at', String(Date.now())));
  let again = 0;
  page.on('request', (r) => { if (r.url().includes('/auth/login')) again++; });
  await page.reload();
  await expect(page.locator('#banner')).toHaveText('Your organisation sign-in expired. Sign in again from your profile page.');
  expect(again).toBe(0);
});

// 021-AC8: a silent sign-in that lands in another account ends on the room's refusal, once.
test('a sign-in into a second account does not loop', async ({ browser, request }) => {
  const { room, path } = await invite(request, 'Two accounts');
  const page = await joined(browser, request, 'dan', 'Dan', path);
  await provider(request, user('eve', 'Eve'), true);
  expire('dan');
  let logins = 0;
  page.on('request', (r) => { if (r.url().includes('/auth/login')) logins++; });
  await page.goto(`/r/${room.id}`);
  await expect(page.locator('#banner')).toContainText('You are not in this room');
  await page.waitForTimeout(2000);
  expect(logins).toBe(1);
  expect(await prompts(request)).toEqual(['none']);
});

// 021-AC11, 021-AC10: a member of the admin group opens /admin without the token; Profiles shows
// internal and external profiles, an expired sign-in, and Unbind makes one external. Without the
// group the token form shows, and the token still works.
test('admin through the provider group, and Unbind', async ({ browser, request }) => {
  const { path } = await invite(request, 'Admin room');
  const member = await joined(browser, request, 'fay', 'Fay', path);
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
  expire('fay');

  await provider(request, user('ops', 'Olga', [adminGroup]), false);
  const ops = await (await browser.newContext()).newPage();
  await ops.goto('/auth/login?next=/admin');
  await expect(ops.locator('#logout')).toBeVisible();
  await expect(ops.locator('#token')).toHaveCount(0);
  // Sign out sticks: no new session by itself, the provider's button instead, which signs in again.
  await ops.locator('#logout').click();
  await expect(ops.locator('#token')).toBeVisible();
  await ops.reload();
  await expect(ops.locator('#token')).toBeVisible();
  await ops.getByRole('button', { name: signInButton }).click();
  await expect(ops.locator('#logout')).toBeVisible();
  expect(await ops.evaluate(() => localStorage.getItem('grill_room_admin_signed_out'))).toBeNull();
  await ops.goto('/admin#profiles');
  const row = (name) => ops.locator('#profiles .list-row').filter({ hasText: name });
  await expect(row('Olga').locator('.sign-in')).toContainText(/^Internal · /);
  await expect(row('Fay').locator('.sign-in')).toHaveText('Internal · sign-in expired');
  await expect(row('Gil').locator('.sign-in')).toHaveText('External');
  await rowAction(row('Fay'), /^Unbind Fay/);
  await confirmSheet(ops, 'Unbind');
  await expect(row('Fay').locator('.sign-in')).toHaveText('External');
  await row('Fay').locator('.actions .more').click();
  await expect(row('Fay').locator('.menu').getByRole('button', { name: /^Unbind/ })).toHaveCount(0);
  await ops.keyboard.press('Escape');
  const audit = await (await request.get('/api/admin/audit?action=profile.oidc_unbind', { headers: admin })).json();
  expect(audit.entries.length).toBe(1);
  const logins = await (await request.get('/api/admin/audit?action=admin.login', { headers: admin })).json();
  expect(logins.entries.some((e) => e.details.via === 'oidc')).toBe(true);
  // Revoked from another browser, the provider session ends on the page's next call: the form
  // shows, nothing reopens it by itself.
  const sessions = await (await request.get('/api/admin/sessions', { headers: admin })).json();
  const mine = sessions.find((x) => x.profile_name === 'Olga');
  expect((await request.delete(`/api/admin/sessions/${mine.id}`, { headers: admin })).status()).toBe(204);
  await ops.locator('#tab-sessions').click();
  await expect(ops.locator('#token')).toBeVisible();
  await expect(ops.locator('.login p[role=alert]')).toHaveText('Your session ended: sign in again.');
  await ops.getByRole('button', { name: signInButton }).click();
  await expect(ops.locator('#logout')).toBeVisible();
  // Unbound, Fay's browser is an external, still signed in.
  await member.goto('/');
  expect(await internal(member)).toBe(false);
  // The admin token keeps working.
  expect(await signIn(member)).toBe(201);

  // An expired sign-in ends the provider session; the button, not a sign-out, brings it back.
  expire('ops');
  await ops.goto('/admin');
  await expect(ops.locator('#token')).toBeVisible();
  await ops.getByRole('button', { name: signInButton }).click();
  await expect(ops.locator('#logout')).toBeVisible();
});
