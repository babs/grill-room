import { defineConfig } from '@playwright/test';

// One supervisor on this machine serving a room on the compose stack (`make dev-up`) and a room
// on the lab deployment (`make k8s-apply`) at once, with two real haiku turns: this suite spends
// Claude requests. DEV_DOMAIN, K8S_DOMAIN and GRILL_ROOM_ADMIN_TOKEN come from the repo's untracked
// .env; `make e2e-supervisor` passes the setup token file and the dev key.
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['DEV_DOMAIN', 'K8S_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN', 'GRILL_ROOM_E2E_SETUP_TOKEN_FILE', 'GRILL_ROOM_E2E_DEV_KEY']) {
  if (!process.env[key]) throw new Error(`${key} is not set`);
}

export default defineConfig({
  testDir: '.',
  timeout: 600_000,
  workers: 1,
  retries: 0,
  use: { channel: 'chrome' },
});
