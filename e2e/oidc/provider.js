import { expect } from '@playwright/test';
import { idpURL } from './playwright.config.js';

// The fake provider (tools/e2eoidc) as e2e/oidc/scenarios.js drives a provider: it signs in, without
// a page, whoever /control names.
const user = (sub, name, groups, verified = true) => ({ sub, name, email: `${sub}@example.com`, email_verified: verified, groups });

// The accounts of e2e/keycloak/realm/grill-room.json, by role. unverified and later each sign in in
// one test only: the 026 tests count the rooms of their profile.
export const users = {
  member: user('mia', 'Mia Member', ['grill-members']),
  admin: user('ada', 'Ada Admin', ['grill-members', 'grill-admins']),
  outsider: user('otto', 'Otto Outsider', []),
  unverified: user('una', 'Una Unverified', ['grill-members'], false),
  second: user('kai', 'Kai Second', ['grill-members']),
  later: user('noa', 'Noa Later', ['grill-members']),
};

const control = async (request, data) => {
  expect((await request.post(`${idpURL}/control`, { data })).status()).toBe(204);
};

// signInAtProvider signs page in as u at the provider; start is the step that sends it there.
export async function signInAtProvider(page, u, start) {
  await control(page.request, { user: u, session: false });
  await start();
}

// useAccount makes the provider session of page's browser u's, as signing in there in another tab.
export const useAccount = (page, u) => control(page.request, { user: u, session: true });

// The fake's own switches: its session flag and the prompts it saw since the last control call.
export const fake = {
  session: (request, u, session) => control(request, { user: u, session }),
  prompts: async (request) => (await (await request.get(`${idpURL}/control`)).json()).prompts,
};
