import { defineConfig } from '@playwright/test';

const port = process.env.GRILL_ROOM_E2E_PORT || '7690';
const baseURL = `http://127.0.0.1:${port}`;

export const adminToken = 'e2e-admin-token';

export default defineConfig({
  testDir: '.',
  // voice/, agent-live/ and install/ run against the compose stack, lab/ against the lab
  // deployment, supervisor/ and crossinstance/ against both, mail/ against its own server and
  // mailpit, scenario/ wherever grill-scenario points it, each with its own config.
  testIgnore: ['voice/**', 'agent-live/**', 'install/**', 'lab/**', 'mail/**', 'supervisor/**', 'crossinstance/**', 'scenario/**'],
  timeout: 30_000,
  // The installed Chrome: no browser download.
  use: { baseURL, channel: 'chrome' },
  webServer: {
    command: 'mkdir -p test-results && rm -f test-results/e2e.db* && ../bin/grill-room',
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${Number(port) + 1}`,
      GRILL_ROOM_DB: 'test-results/e2e.db',
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      // `make e2e` writes a real release there first.
      GRILL_ROOM_DAEMON_DIR: '../bin/e2e-release',
      // Specs send X-Forwarded-For as clients behind a proxy on loopback (005-AC20).
      GRILL_ROOM_TRUSTED_PROXIES: '127.0.0.1',
      LOG_FORMAT: 'json',
    },
  },
});
