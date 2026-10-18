import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';
import { withFreshDB, withExistingDB } from '../pgdb.js';
import { adminToken } from '../playwright.config.js';

// Two servers on one database, as two pods behind one Service (017-AC10, 017-AC11): pod A makes the
// database, pod B joins it once A has migrated it.
const port = Number(process.env.GRILL_ROOM_E2E_PORT || '7700');
export const podA = `http://127.0.0.1:${port}`;
export const podB = `http://127.0.0.1:${port + 10}`;
const db = `e2e_2pod_${port}`;

const pod = (url, metrics) => ({
  url: `${url}/healthz`,
  reuseExistingServer: false,
  // From e2e/, as the main config runs it.
  cwd: '..',
  env: {
    GRILL_ROOM_ADDR: url.replace('http://', ''),
    GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${metrics}`,
    GRILL_ROOM_URL: url,
    GRILL_ROOM_ADMIN_TOKEN: adminToken,
    GRILL_ROOM_DAEMON_DIR: '../bin/e2e-release',
    LOG_FORMAT: 'json',
  },
});

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  workers: 1,
  outputDir: '../test-results/twopod',
  use: { baseURL: podA, channel: 'chrome', trace: process.env.CI ? 'retain-on-failure' : 'off', storageState: quietPush(podA, podB) },
  webServer: [
    { ...pod(podA, port + 1), command: withFreshDB(db, 'mkdir -p test-results && exec ../bin/grill-room-e2e') },
    // Playwright's webServer timeout bounds the wait.
    { ...pod(podB, port + 11), command: `until curl -sf ${podA}/healthz >/dev/null; do sleep 0.2; done && ${withExistingDB(db, 'exec ../bin/grill-room-e2e')}` },
  ],
});
