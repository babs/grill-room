import { defineConfig } from '@playwright/test';
import { withFreshDB } from '../pgdb.js';

// The persona voice stack (spec 013): persona-voice/run.sh starts LiveKit, the STT worker and the
// tts pod and exports their settings, then runs this config, which starts the e2e server.
for (const key of ['PERSONA_VOICE_PORT', 'PERSONA_VOICE_LIVEKIT_PORT', 'PERSONA_VOICE_STT_CONTAINER', 'LIVEKIT_API_KEY', 'TTS_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set: run persona-voice/run.sh, not this config`);
}
const port = process.env.PERSONA_VOICE_PORT;
const baseURL = `http://127.0.0.1:${port}`;
export const adminToken = 'e2e-admin-token';
export const sttMetrics = `http://127.0.0.1:${process.env.PERSONA_VOICE_STT_METRICS_PORT}/metrics`;
export const ttsMetrics = `http://127.0.0.1:${process.env.PERSONA_VOICE_TTS_METRICS_PORT}/metrics`;
export const sttContainer = process.env.PERSONA_VOICE_STT_CONTAINER;
export const engine = process.env.PERSONA_VOICE_ENGINE;

export default defineConfig({
  testDir: '.',
  workers: 1,
  timeout: 300_000,
  // ponytail: real TTS, STT and Chrome audio flake on shared CI runners; one retry until the causes are fixed.
  retries: process.env.CI ? 1 : 0,
  use: { baseURL, channel: 'chrome' },
  outputDir: '../test-results/persona-voice/results',
  webServer: {
    cwd: '..',
    command: withFreshDB(`e2e_${port}`, 'mkdir -p test-results && exec ../bin/grill-room-e2e'),
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${process.env.PERSONA_VOICE_METRICS_PORT}`,
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      GRILL_ROOM_LIVEKIT_URL: `ws://127.0.0.1:${process.env.PERSONA_VOICE_LIVEKIT_PORT}`,
      GRILL_ROOM_LIVEKIT_API_URL: `http://127.0.0.1:${process.env.PERSONA_VOICE_LIVEKIT_PORT}`,
      GRILL_ROOM_LIVEKIT_API_KEY: process.env.LIVEKIT_API_KEY,
      GRILL_ROOM_LIVEKIT_API_SECRET: process.env.LIVEKIT_API_SECRET,
      GRILL_ROOM_STT_TOKEN: process.env.STT_TOKEN,
      GRILL_ROOM_TTS_TOKEN: process.env.TTS_TOKEN,
      LOG_FORMAT: 'json',
      LOG_LEVEL: process.env.PERSONA_VOICE_LOG_LEVEL || 'info',
    },
  },
});
