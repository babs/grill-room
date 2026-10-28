import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';
import { withFreshDB } from '../pgdb.js';
import { adminToken } from '../playwright.config.js';

// Restricted mode (spec 021) against the fake provider of tools/e2eoidc. The provider answers on
// localhost and the server on 127.0.0.1: two sites, so the provider's redirects are cross-site.
const port = Number(process.env.GRILL_ROOM_E2E_OIDC_PORT || '7710');
const baseURL = `http://127.0.0.1:${port}`;
const idpPort = port + 2;
export const idpURL = `http://127.0.0.1:${idpPort}`;
export const db = `e2e_oidc_${port}`;
const client = { id: 'grill-room-e2e', secret: 'e2e-secret' };

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  workers: 1,
  outputDir: '../test-results/oidc',
  use: { baseURL, channel: 'chrome', trace: process.env.CI ? 'retain-on-failure' : 'off', storageState: quietPush(baseURL) },
  webServer: [{
    cwd: '..',
    command: `exec ../bin/e2eoidc -addr 127.0.0.1:${idpPort} -issuer http://localhost:${idpPort} -client-id ${client.id} -client-secret ${client.secret}`,
    url: `${idpURL}/.well-known/openid-configuration`,
    reuseExistingServer: false,
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
      GRILL_ROOM_OIDC_ISSUER: `http://localhost:${idpPort}`,
      GRILL_ROOM_OIDC_CLIENT_ID: client.id,
      GRILL_ROOM_OIDC_CLIENT_SECRET: client.secret,
      GRILL_ROOM_OIDC_MEMBER_GROUP: 'grill-members',
      GRILL_ROOM_OIDC_ADMIN_GROUP: 'grill-admins',
      LOG_FORMAT: 'json',
    },
  }],
});
