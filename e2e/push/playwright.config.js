import { existsSync } from 'node:fs';
import { defineConfig } from '@playwright/test';

// A real Web Push through the browser vendor's push service (005-AC24), against PUSH_BASE with
// PUSH_ADMIN_TOKEN, or the lab deployment (K8S_DOMAIN and GRILL_ROOM_ADMIN_TOKEN from the repo's
// untracked .env). Chrome
// must run headed, as `make e2e-push` does under xvfb in a container: headless Chrome has no push.
const env = new URL('../../.env', import.meta.url);
if (existsSync(env)) process.loadEnvFile(env);
const base = process.env.PUSH_BASE || (process.env.K8S_DOMAIN && `https://grill-room.${process.env.K8S_DOMAIN}`);
process.env.PUSH_ADMIN_TOKEN ||= process.env.PUSH_BASE ? '' : process.env.GRILL_ROOM_ADMIN_TOKEN;
if (!base || !process.env.PUSH_ADMIN_TOKEN) throw new Error('set PUSH_BASE and PUSH_ADMIN_TOKEN, or K8S_DOMAIN and GRILL_ROOM_ADMIN_TOKEN');

export default defineConfig({
  testDir: '.',
  timeout: 120_000,
  workers: 1,
  retries: 0,
  use: { baseURL: base, channel: 'chrome', headless: false },
});
