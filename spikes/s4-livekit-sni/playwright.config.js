import { defineConfig } from '@playwright/test';

// Browsers are launched per case in s4.spec.js: launch args differ between cases.
export default defineConfig({
  testDir: '.',
  timeout: 180_000,
  workers: 1,
});
