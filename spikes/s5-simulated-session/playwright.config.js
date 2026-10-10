import { defineConfig } from '@playwright/test';

// Against the lab deployment, like e2e/lab. node_modules here links to e2e/node_modules, so the spec
// and e2e's helpers load one Playwright. K8S_DOMAIN and GRILL_ROOM_ADMIN_TOKEN come from .env.
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['K8S_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set in .env`);
}

export default defineConfig({
  testDir: '.',
  outputDir: new URL('../../.scratchpad/s5/test-results', import.meta.url).pathname,
  timeout: 1_800_000,
  workers: 1,
  retries: 0,
  use: { baseURL: `https://grill-room.${process.env.K8S_DOMAIN}`, channel: 'chrome' },
});
