import { defineConfig } from '@playwright/test';

const port = process.env.GRILL_ROOM_E2E_PORT || '7690';
const baseURL = `http://127.0.0.1:${port}`;

export const adminToken = 'e2e-admin-token';

export default defineConfig({
  testDir: '.',
  // voice/ runs against the compose stack, with its own config.
  testIgnore: 'voice/**',
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
      LOG_FORMAT: 'json',
    },
  },
});
