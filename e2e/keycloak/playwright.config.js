import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';
import { withFreshDB } from '../pgdb.js';
import { adminToken } from '../playwright.config.js';

// Restricted mode (specs 021, 026) against Keycloak, realm in realm/ (027-AC1). As in the oidc
// stack, the provider answers on localhost and the server on 127.0.0.1: two sites.
const port = Number(process.env.GRILL_ROOM_E2E_KEYCLOAK_PORT || '7740');
const baseURL = `http://127.0.0.1:${port}`;
const kcPort = port + 2;
export const kcURL = `http://localhost:${kcPort}`;
export const db = `e2e_keycloak_${port}`;
// The master realm's admin, for the admin API (admin.js). Test-only.
export const kcAdmin = { username: 'e2e-admin', password: 'e2e-admin-password' };

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  workers: 1,
  outputDir: '../test-results/keycloak',
  use: { baseURL, channel: 'chrome', trace: process.env.CI ? 'retain-on-failure' : 'off', storageState: quietPush(baseURL) },
  webServer: [{
    command: `exec ./keycloak.sh run ${kcPort} ${baseURL}`,
    url: `http://127.0.0.1:${kcPort}/realms/grill-room/.well-known/openid-configuration`,
    // An image pull, then keycloak.sh's 180 s for Keycloak to start.
    timeout: 420_000,
    reuseExistingServer: false,
    // SIGTERM lets keycloak.sh remove its container; the default SIGKILL would leave it running.
    gracefulShutdown: { signal: 'SIGTERM', timeout: 30_000 },
    env: { KC_BOOTSTRAP_ADMIN_USERNAME: kcAdmin.username, KC_BOOTSTRAP_ADMIN_PASSWORD: kcAdmin.password },
  }, {
    // From e2e/, as the main config runs it.
    cwd: '..',
    command: withFreshDB(db, 'mkdir -p test-results && exec ../bin/grill-room-e2e'),
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${port + 1}`,
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      GRILL_ROOM_OIDC_ISSUER: `${kcURL}/realms/grill-room`,
      GRILL_ROOM_OIDC_CLIENT_ID: 'grill-room-e2e',
      GRILL_ROOM_OIDC_CLIENT_SECRET: 'e2e-keycloak-secret',
      GRILL_ROOM_OIDC_MEMBER_GROUP: 'grill-members',
      GRILL_ROOM_OIDC_ADMIN_GROUP: 'grill-admins',
      LOG_FORMAT: 'json',
    },
  }],
});
