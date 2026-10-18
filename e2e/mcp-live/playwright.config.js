import { defineConfig } from '@playwright/test';
import { withFreshDB } from '../pgdb.js';

// 012 Phase 2: a real Claude Code drives /mcp on a server of its own, here on loopback. It runs
// this machine's Claude Code login with haiku: at most six real Claude requests.
const port = process.env.GRILL_ROOM_E2E_MCP_PORT || '7795';
export const baseURL = `http://127.0.0.1:${port}`;
export const adminToken = 'e2e-mcp-admin-token';

export default defineConfig({
  testDir: '.',
  timeout: 300_000,
  workers: 1,
  retries: 0,
  use: { baseURL, channel: 'chrome' },
  webServer: {
    command: withFreshDB(`e2e_${port}`, 'exec ../../bin/grill-room'),
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    env: {
      GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
      GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${Number(port) + 1}`,
      GRILL_ROOM_URL: baseURL,
      GRILL_ROOM_ADMIN_TOKEN: adminToken,
      LOG_FORMAT: 'json',
    },
  },
});
