import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';

// The compose stack behind the local traefik (`make dev-up`): a clean Debian container installs the
// daemon with the room's one-liner, then the suite bumps the release the room server hands out.
// It copies releases into the running grill-room container and restores it at the end. No Claude
// request. DEV_DOMAIN and GRILL_ROOM_ADMIN_TOKEN come from the repo's untracked .env.
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['DEV_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set in .env`);
}

export default defineConfig({
  testDir: '.',
  timeout: 600_000,
  workers: 1,
  retries: 0,
  use: { baseURL: `https://grill-room.${process.env.DEV_DOMAIN}`, channel: 'chrome', storageState: quietPush(`https://grill-room.${process.env.DEV_DOMAIN}`) },
});
