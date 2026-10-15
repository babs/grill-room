import { defineConfig } from '@playwright/test';

// The device-voice stack (010 Phase 2): device-voice/run.sh starts LiveKit and the STT worker and
// exports their settings, then runs this config, which starts the e2e server against them.
for (const key of ['DEVICE_VOICE_PORT', 'DEVICE_VOICE_LIVEKIT_PORT', 'DEVICE_VOICE_STT_CONTAINER', 'LIVEKIT_API_KEY', 'GRILL_ROOM_E2E_MODEL_DIR']) {
  if (!process.env[key]) throw new Error(`${key} is not set: run device-voice/run.sh, not this config`);
}
const port = process.env.DEVICE_VOICE_PORT;
const baseURL = `http://127.0.0.1:${port}`;
export const adminToken = 'e2e-admin-token';
export const serverMetrics = `http://127.0.0.1:${process.env.DEVICE_VOICE_METRICS_PORT}/metrics`;
export const sttMetrics = `http://127.0.0.1:${process.env.DEVICE_VOICE_STT_METRICS_PORT}/metrics`;
export const sttContainer = process.env.DEVICE_VOICE_STT_CONTAINER;

export default defineConfig({
  testDir: '.',
  // One tab holds the real model, about 2.5 GB, beside the worker's 3 GB.
  workers: 1,
  timeout: 600_000,
  use: { baseURL, channel: 'chrome' },
  outputDir: '../test-results/device-voice/results',
  webServer: {
    // From e2e/, as the main config runs it.
    cwd: '..',
    command: 'mkdir -p test-results && rm -f test-results/e2e-device-voice.db* && exec ../bin/grill-room-e2e',
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${process.env.DEVICE_VOICE_METRICS_PORT}`,
      GRILL_ROOM_DB: 'test-results/e2e-device-voice.db',
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      GRILL_ROOM_LIVEKIT_URL: `ws://127.0.0.1:${process.env.DEVICE_VOICE_LIVEKIT_PORT}`,
      GRILL_ROOM_LIVEKIT_API_URL: `http://127.0.0.1:${process.env.DEVICE_VOICE_LIVEKIT_PORT}`,
      GRILL_ROOM_LIVEKIT_API_KEY: process.env.LIVEKIT_API_KEY,
      GRILL_ROOM_LIVEKIT_API_SECRET: process.env.LIVEKIT_API_SECRET,
      GRILL_ROOM_STT_TOKEN: process.env.STT_TOKEN,
      GRILL_ROOM_DEVICE_STT_MODEL_DIR: process.env.GRILL_ROOM_E2E_MODEL_DIR,
      LOG_FORMAT: 'json',
    },
  },
});
