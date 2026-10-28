import realm from './realm/grill-room.json' with { type: 'json' };
import { kcURL } from './playwright.config.js';

// Keycloak as e2e/oidc/scenarios.js drives a provider: its login form, the accounts of
// realm/grill-room.json. sub is the account's id, which Keycloak signs as the subject.
const user = (username) => {
  const u = realm.users.find((x) => x.username === username);
  if (!u) throw new Error(`realm/grill-room.json has no user ${username}`);
  return { sub: u.id, username, password: u.credentials[0].value, name: `${u.firstName} ${u.lastName}` };
};

// The roles of e2e/oidc/provider.js. unverified and later each sign in in one test only: the 026
// tests count the rooms of their profile.
export const users = {
  member: user('mia'),
  admin: user('ada'),
  outsider: user('otto'),
  unverified: user('una'),
  second: user('kai'),
  later: user('noa'),
  // lifecycle.spec.js's, one per test: each changes its account at Keycloak.
  revalidate: user('rey'),
  disabled: user('dora'),
  groups: user('gaby'),
  keys: user('kit'),
};

// signInAtProvider signs page in as u at the provider; start is the step that sends it there.
export async function signInAtProvider(page, u, start) {
  await start();
  await page.locator('#username').fill(u.username);
  await page.locator('#password').fill(u.password);
  await page.locator('#kc-login').click();
}

// useAccount makes the provider session of page's browser u's: it drops Keycloak's cookies and
// signs u in to the account console in another tab.
export async function useAccount(page, u) {
  const context = page.context();
  await context.clearCookies({ domain: 'localhost' });
  const tab = await context.newPage();
  await signInAtProvider(tab, u, () => tab.goto(`${kcURL}/realms/grill-room/account`));
  await tab.waitForURL((url) => url.pathname.startsWith('/realms/grill-room/account'));
  await tab.close();
}
