import { defineConfig } from '@playwright/test';

// The compose stack behind the local traefik (`make dev-up`), with two real daemons running real
// `claude -p --model haiku` turns: this suite spends Claude requests. DEV_DOMAIN and
// GRILL_ROOM_ADMIN_TOKEN come from the repo's untracked .env.
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['DEV_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set in .env`);
}

export default defineConfig({
  testDir: '.',
  timeout: 600_000,
  workers: 1,
  retries: 0,
  use: { baseURL: `https://grill-room.${process.env.DEV_DOMAIN}`, channel: 'chrome' },
});
