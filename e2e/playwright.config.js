import { defineConfig } from '@playwright/test';
import { quietPush } from './quiet-push.js';

const port = process.env.GRILL_ROOM_E2E_PORT || '7690';
const baseURL = `http://127.0.0.1:${port}`;
export const gitPort = String(Number(port) + 2);

export const adminToken = 'e2e-admin-token';

const otherStacks = ['voice/**', 'agent-live/**', 'mcp-live/**', 'install/**', 'lab/**', 'mail/**', 'supervisor/**', 'crossinstance/**', 'scenario/**', 'push/**', 'device-voice/**'];

const deviceSpec = 'device-stt.spec.js';

// The specs that also run in Firefox.
const firefoxSpecs = ['doc-frames.spec.js', 'documents.spec.js', 'ui-guards.spec.js', 'isolation.spec.js'];

// The specs whose daemons run agent turns, each in a Docker sandbox.
const sandboxSpecs = ['admin', 'agent', 'approvals', 'concurrency', 'egress', 'export-import', 'handoff', 'mentions', 'personas',
  'plugins', 'room-admins', 'skills', 'triggers'].map((n) => `${n}.spec.js`);

export default defineConfig({
  testDir: '.',
  // voice/, agent-live/ and install/ run against the compose stack, lab/ against the lab
  // deployment, supervisor/ and crossinstance/ against both, mail/ against its own server and
  // mailpit, scenario/ wherever grill-scenario points it, push/ against the lab, device-voice/ against
  // LiveKit and the STT worker in Docker, each with its own config.
  testIgnore: otherStacks,
  timeout: 30_000,
  // Chrome fails the requests it is opening when the host's interfaces change, as Docker adds or
  // removes a sandbox's networks. The specs that start sandboxes run one at a time, and `make e2e`
  // runs them apart from the others (two invocations), so no other spec runs beside them.
  // device-stt.spec.js loads the real model, 430 MB into one tab: it runs alone too, never beside
  // other specs' browsers.
  projects: [
    { name: 'parallel', testIgnore: [...otherStacks, ...sandboxSpecs, deviceSpec], outputDir: 'test-results/parallel' },
    { name: 'device', testMatch: deviceSpec, testIgnore: otherStacks, workers: 1, outputDir: 'test-results/device' },
    { name: 'sandbox', testMatch: sandboxSpecs, testIgnore: otherStacks, workers: 1, outputDir: 'test-results/sandbox' },
    // Firefox enforces COEP on the sandboxed document frame where Chrome does not. Playwright's
    // Firefox ships with its PDF viewer off.
    { name: 'firefox', testMatch: firefoxSpecs, testIgnore: otherStacks, outputDir: 'test-results/firefox',
      use: { browserName: 'firefox', channel: '', launchOptions: { firefoxUserPrefs: { 'pdfjs.disabled': false } } } },
  ],
  // The installed Chrome: no browser download.
  // On CI a failed test keeps its trace: what a flake did is in the artifact.
  use: { baseURL, channel: 'chrome', trace: process.env.CI ? 'retain-on-failure' : 'off', storageState: quietPush(baseURL) },
  webServer: [{
    // The plugins' git server (tools/e2egit): the e2e server build fetches from it as example.com.
    command: `rm -rf test-results/git test-results/git-ca.pem && exec ../bin/e2egit -addr 127.0.0.1:${gitPort} -root test-results/git -ca test-results/git-ca.pem`,
    port: Number(gitPort),
    reuseExistingServer: false,
  }, {
    command: 'mkdir -p test-results && rm -f test-results/e2e.db* && ../bin/grill-room-e2e',
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
      GRILL_ROOM_TEST_GIT_ALLOW_LOOPBACK: '1',
      GRILL_ROOM_TEST_GIT_CA: 'test-results/git-ca.pem',
      LOG_FORMAT: 'json',
      // The real model files, when there: device-stt.spec.js self-tests on them (010-AC2).
      ...(process.env.GRILL_ROOM_E2E_MODEL_DIR && { GRILL_ROOM_DEVICE_STT_MODEL_DIR: process.env.GRILL_ROOM_E2E_MODEL_DIR }),
    },
  }],
});
