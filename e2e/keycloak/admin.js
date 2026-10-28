import { kcURL, kcAdmin } from './playwright.config.js';

// The Keycloak admin API calls the lifecycle tests make, as the master realm's admin. Each takes
// the test's APIRequestContext.
const realmURL = `${kcURL}/admin/realms/grill-room`;

async function call(request, method, path, data) {
  const t = await request.post(`${kcURL}/realms/master/protocol/openid-connect/token`, {
    form: { grant_type: 'password', client_id: 'admin-cli', username: kcAdmin.username, password: kcAdmin.password },
  });
  if (!t.ok()) throw new Error(`keycloak admin token: ${t.status()} ${await t.text()}`);
  const r = await request.fetch(`${realmURL}${path}`, { method, data, headers: { Authorization: `Bearer ${(await t.json()).access_token}` } });
  if (!r.ok()) throw new Error(`keycloak ${method} ${path}: ${r.status()} ${await r.text()}`);
  return (await r.text()) ? r.json() : null;
}

// endSessions logs u out of every Keycloak session.
export const endSessions = (request, u) => call(request, 'POST', `/users/${u.sub}/logout`);

export async function disable(request, u) {
  const rep = await call(request, 'GET', `/users/${u.sub}`);
  await call(request, 'PUT', `/users/${u.sub}`, { ...rep, enabled: false });
}

export async function leaveGroup(request, u, name) {
  const [g] = await call(request, 'GET', `/groups?search=${encodeURIComponent(name)}&exact=true`);
  await call(request, 'DELETE', `/users/${u.sub}/groups/${g.id}`);
}

// signingKeys are the kids of the realm's published RS256 signing keys.
export async function signingKeys(request) {
  const { keys } = await (await request.get(`${kcURL}/realms/grill-room/protocol/openid-connect/certs`)).json();
  return keys.filter((k) => k.use === 'sig' && k.alg === 'RS256').map((k) => k.kid);
}

// rotateKeys adds an RSA key provider above the realm's and disables the old ones.
export async function rotateKeys(request) {
  const realm = await call(request, 'GET', '');
  const old = (await call(request, 'GET', '/components?type=org.keycloak.keys.KeyProvider')).filter((c) => c.providerId === 'rsa-generated');
  await call(request, 'POST', '/components', {
    name: 'rsa-rotated', providerId: 'rsa-generated', providerType: 'org.keycloak.keys.KeyProvider', parentId: realm.id, config: { priority: ['200'] },
  });
  for (const c of old) await call(request, 'PUT', `/components/${c.id}`, { ...c, config: { ...c.config, enabled: ['false'], active: ['false'] } });
}
