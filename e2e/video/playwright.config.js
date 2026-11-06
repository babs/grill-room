import { defineConfig } from '@playwright/test';
import { withFreshDB } from '../pgdb.js';

// The video stack (spec 030): video/run.sh starts LiveKit and exports its settings, then runs this
// config, which starts the e2e server.
for (const key of ['VIDEO_PORT', 'VIDEO_METRICS_PORT', 'VIDEO_LIVEKIT_PORT', 'LIVEKIT_API_KEY', 'LIVEKIT_API_SECRET']) {
  if (!process.env[key]) throw new Error(`${key} is not set: run video/run.sh, not this config`);
}
const port = process.env.VIDEO_PORT;
const baseURL = `http://127.0.0.1:${port}`;
export const adminToken = 'e2e-admin-token';

export default defineConfig({
  testDir: '.',
  workers: 1,
  timeout: 120_000,
  use: {
    baseURL,
    channel: 'chrome',
    trace: 'retain-on-failure',
    viewport: { width: 1440, height: 900 },
    launchOptions: {
      args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--auto-select-desktop-capture-source=Entire screen'],
    },
  },
  outputDir: '../test-results/video/results',
  webServer: {
    cwd: '..',
    command: withFreshDB(`e2e_${port}`, 'mkdir -p test-results && exec ../bin/grill-room-e2e'),
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${process.env.VIDEO_METRICS_PORT}`,
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      GRILL_ROOM_LIVEKIT_URL: `ws://127.0.0.1:${process.env.VIDEO_LIVEKIT_PORT}`,
      GRILL_ROOM_LIVEKIT_API_URL: `http://127.0.0.1:${process.env.VIDEO_LIVEKIT_PORT}`,
      GRILL_ROOM_LIVEKIT_API_KEY: process.env.LIVEKIT_API_KEY,
      GRILL_ROOM_LIVEKIT_API_SECRET: process.env.LIVEKIT_API_SECRET,
      LOG_FORMAT: 'json',
      LOG_LEVEL: process.env.VIDEO_LOG_LEVEL || 'info',
    },
  },
});
