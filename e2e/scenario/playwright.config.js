import { defineConfig } from '@playwright/test';
import { readFileSync } from 'node:fs';

// The scenario driver (scenarios/, 004-AC16): `grill-scenario` writes the plan and runs this
// config. GRILL_ROOM_ADMIN_TOKEN comes from the repo's untracked .env. It runs real daemons with
// this machine's Claude Code login: it spends Claude requests, up to the scenario's budget.
process.loadEnvFile(new URL('../../.env', import.meta.url));
for (const key of ['GRILL_ROOM_ADMIN_TOKEN', 'GRILL_SCENARIO_PLAN']) {
  if (!process.env[key]) throw new Error(`${key} is not set`);
}
const plan = JSON.parse(readFileSync(process.env.GRILL_SCENARIO_PLAN, 'utf8'));

export default defineConfig({
  testDir: '.',
  outputDir: `${plan.run}/test-results`,
  timeout: 3_600_000,
  workers: 1,
  retries: 0,
  use: { baseURL: new URL(plan.url).origin, channel: 'chrome' },
});
