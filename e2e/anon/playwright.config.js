import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';
import { withFreshDB } from '../pgdb.js';
import { adminToken } from '../playwright.config.js';

// Anonymous room creation on (019-AC8), one per IP an hour so the second one hits the limit.
const port = Number(process.env.GRILL_ROOM_E2E_ANON_PORT || '7697');
const baseURL = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: '.',
  timeout: 30_000,
  workers: 1,
  outputDir: '../test-results/anon',
  use: { baseURL, channel: 'chrome', trace: process.env.CI ? 'retain-on-failure' : 'off', storageState: quietPush(baseURL) },
  webServer: [{
    // From e2e/, as the main config runs it.
    cwd: '..',
    command: withFreshDB(`e2e_anon_${port}`, 'mkdir -p test-results && exec ../bin/grill-room-e2e'),
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${port + 1}`,
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      GRILL_ROOM_DAEMON_DIR: '../bin/e2e-release',
      // Specs send X-Forwarded-For as clients behind a proxy on loopback (005-AC20).
      GRILL_ROOM_TRUSTED_PROXIES: '127.0.0.1',
      GRILL_ROOM_ANONYMOUS_ROOMS: 'true',
      GRILL_ROOM_ANONYMOUS_ROOMS_PER_HOUR: '1',
      LOG_FORMAT: 'json',
    },
  }],
});
