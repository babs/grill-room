import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';

// The lab deployment (`make k8s-apply`), reached through its public hostnames like any participant.
// K8S_DOMAIN and GRILL_ROOM_ADMIN_TOKEN come from the repo's untracked .env. The session test runs
// a real daemon with this machine's Claude Code login: it spends a few Claude requests (haiku).
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['K8S_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set in .env`);
}

export default defineConfig({
  testDir: '.',
  timeout: 600_000,
  workers: 1,
  retries: 0,
  use: { baseURL: `https://grill-room.${process.env.K8S_DOMAIN}`, channel: 'chrome', storageState: quietPush(`https://grill-room.${process.env.K8S_DOMAIN}`) },
});
