import { defineConfig } from '@playwright/test';
import { quietPush } from './quiet-push.js';

const port = process.env.GRILL_ROOM_E2E_PORT || '7690';
const baseURL = `http://127.0.0.1:${port}`;

export const adminToken = 'e2e-admin-token';

const otherStacks = ['voice/**', 'agent-live/**', 'install/**', 'lab/**', 'mail/**', 'supervisor/**', 'crossinstance/**', 'scenario/**', 'push/**'];

// The specs whose daemons run agent turns, each in a Docker sandbox.
const sandboxSpecs = ['admin', 'agent', 'approvals', 'concurrency', 'egress', 'export-import', 'handoff', 'mentions', 'personas',
  'room-admins', 'skills', 'triggers'].map((n) => `${n}.spec.js`);

export default defineConfig({
  testDir: '.',
  // voice/, agent-live/ and install/ run against the compose stack, lab/ against the lab
  // deployment, supervisor/ and crossinstance/ against both, mail/ against its own server and
  // mailpit, scenario/ wherever grill-scenario points it, push/ against the lab, each with its own
  // config.
  testIgnore: otherStacks,
  timeout: 30_000,
  // Chrome fails the requests it is opening when the host's interfaces change, as Docker adds or
  // removes a sandbox's networks. The specs that start sandboxes run one at a time, and `make e2e`
  // runs them apart from the others (two invocations), so no other spec runs beside them.
  projects: [
    { name: 'parallel', testIgnore: [...otherStacks, ...sandboxSpecs], outputDir: 'test-results/parallel' },
    { name: 'sandbox', testMatch: sandboxSpecs, testIgnore: otherStacks, workers: 1, outputDir: 'test-results/sandbox' },
  ],
  // The installed Chrome: no browser download.
  // On CI a failed test keeps its trace: what a flake did is in the artifact.
  use: { baseURL, channel: 'chrome', trace: process.env.CI ? 'retain-on-failure' : 'off', storageState: quietPush(baseURL) },
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
      // The notes watcher answers 1 s after the last note, not 20 s (009-AC32).
      GRILL_ROOM_NOTES_WATCH_DELAY: '1s',
      LOG_FORMAT: 'json',
    },
  },
});
