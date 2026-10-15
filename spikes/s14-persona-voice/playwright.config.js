import { defineConfig } from '@playwright/test';

// The demo runs against the loopback stack of spikes/S14-persona-voice.md; S14_* variables point at it.
export default defineConfig({
  testDir: '.',
  testMatch: 'demo.spec.js',
  outputDir: process.env.S14_OUT || 'test-results',
  workers: 1,
  retries: 0,
  use: { baseURL: process.env.S14_BASE, channel: 'chrome' },
});
