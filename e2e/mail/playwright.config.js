import { defineConfig } from '@playwright/test';
import { quietPush } from '../quiet-push.js';
import { withFreshDB } from '../pgdb.js';

// Magic links against a throwaway mailpit, with a short link lifetime so expiry fits in a test.
// With GRILL_ROOM_E2E_LAB=1, against the lab deployment and its mailpit instead: K8S_DOMAIN,
// GRILL_ROOM_ADMIN_TOKEN and K8S_MAILPIT_URL come from the repo's untracked .env.
const lab = process.env.GRILL_ROOM_E2E_LAB === '1';
if (lab) {
  process.loadEnvFile(new URL('../../.env', import.meta.url));
  for (const key of ['K8S_DOMAIN', 'GRILL_ROOM_ADMIN_TOKEN', 'K8S_MAILPIT_URL']) {
    if (!process.env[key]) throw new Error(`${key} is not set in .env`);
  }
}
const port = process.env.GRILL_ROOM_E2E_MAIL_PORT || '7693';
const baseURL = lab ? `https://grill-room.${process.env.K8S_DOMAIN}` : `http://127.0.0.1:${port}`;
const mailpitSMTP = '7695';
const mailpit = 'grill-room-e2e-mailpit';
export const mailpitAPI = lab ? process.env.K8S_MAILPIT_URL.replace(/\/$/, '') : 'http://127.0.0.1:7696';
export const adminToken = lab ? process.env.GRILL_ROOM_ADMIN_TOKEN : 'e2e-admin-token';
// Seconds; the lab runs the default 15 minutes.
export const magicLinkTTL = lab ? 900 : 4;
export { lab };

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  workers: 1,
  use: { baseURL, channel: 'chrome', storageState: quietPush(baseURL) },
  // Under e2e/test-results/, which git ignores: traces hold invite and recovery links.
  outputDir: '../test-results/mail',
  globalTeardown: lab ? undefined : './teardown.js',
  webServer: lab ? undefined : [
    {
      command: `docker rm -f ${mailpit} >/dev/null 2>&1; exec docker run --rm --name ${mailpit} -p 127.0.0.1:${mailpitSMTP}:1025 -p 127.0.0.1:7696:8025 axllent/mailpit:v1.31.4`,
      url: `${mailpitAPI}/readyz`,
      reuseExistingServer: false,
    },
    {
      // From e2e/, as the main config runs it.
      cwd: '..',
      command: withFreshDB(`e2e_${port}`, 'mkdir -p test-results && exec ../bin/grill-room'),
      url: `${baseURL}/healthz`,
      reuseExistingServer: false,
      env: {
        GRILL_ROOM_ADDR: `127.0.0.1:${port}`,
        GRILL_ROOM_METRICS_ADDR: `127.0.0.1:${Number(port) + 1}`,
        GRILL_ROOM_URL: baseURL,
        GRILL_ROOM_ADMIN_TOKEN: adminToken,
        GRILL_ROOM_SMTP_ADDR: `127.0.0.1:${mailpitSMTP}`,
        GRILL_ROOM_SMTP_FROM: 'grill-room <noreply@grill-room.invalid>',
        GRILL_ROOM_MAGIC_LINK_TTL: `${magicLinkTTL}s`,
        // Specs send X-Forwarded-For as clients behind a proxy on loopback (005-AC20).
        GRILL_ROOM_TRUSTED_PROXIES: '127.0.0.1',
        LOG_FORMAT: 'json',
      },
    },
  ],
});
