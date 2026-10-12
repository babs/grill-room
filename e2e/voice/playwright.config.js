import { defineConfig } from '@playwright/test';

// The compose stack behind the local traefik (`make dev-up`): LiveKit and the STT worker run there.
// With GRILL_ROOM_E2E_LAB=1, the lab deployment instead. DEV_DOMAIN (or K8S_DOMAIN) and
// GRILL_ROOM_ADMIN_TOKEN come from the repo's untracked .env.
process.loadEnvFile(new URL('../../.env', import.meta.url));
const domain = process.env.GRILL_ROOM_E2E_LAB === '1' ? 'K8S_DOMAIN' : 'DEV_DOMAIN';
for (const key of [domain, 'GRILL_ROOM_ADMIN_TOKEN']) {
  if (!process.env[key]) throw new Error(`${key} is not set in .env`);
}

export default defineConfig({
  testDir: '.',
  timeout: 180_000,
  workers: 1,
  use: { baseURL: `https://grill-room.${process.env[domain]}` },
});
