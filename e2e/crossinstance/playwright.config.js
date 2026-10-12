import { defineConfig } from '@playwright/test';

// A room exported from the compose stack (`make dev-up`) and imported on the lab deployment
// (`make k8s-apply`), then the reverse, with one real haiku turn on each side: this suite spends
// Claude requests. DEV_DOMAIN, K8S_DOMAIN and GRILL_ROOM_ADMIN_TOKEN come from the repo's untracked
// .env.
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['DEV_DOMAIN', 'K8S_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set in .env`);
}

export default defineConfig({
  testDir: '.',
  timeout: 600_000,
  workers: 1,
  retries: 0,
  use: { channel: 'chrome' },
});
