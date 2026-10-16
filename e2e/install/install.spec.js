import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { createRoom, invite, join } from '../voice/helpers.js';
import { pairDaemon } from '../helpers.js';

const repo = resolve('..');
const out = resolve('test-results', 'install');
const box = 'grill-room-install-e2e';
// Above v0.0.3, the first release that runs as a worker without Claude credentials.
const versions = ['v0.0.4-e2e.1', 'v0.0.4-e2e.2', 'v0.0.4-e2e.3'];
const bin = 'grill-room-daemon-linux-amd64';

const run = (cmd, args, opts = {}) => execFileSync(cmd, args, { encoding: 'utf8', ...opts });
const inBox = (script) => run('docker', ['exec', box, 'sh', '-c', script]);
const sha256 = (b) => createHash('sha256').update(b).digest('hex');
const server = () => run('docker', ['compose', '-f', `${repo}/compose.yaml`, 'ps', '-q', 'grill-room']).trim();

// serve copies a release into the running room server and restarts it, as a deploy would.
async function serve(request, dir) {
  run('docker', ['cp', `${dir}/.`, `${server()}:/daemon/`]);
  run('docker', ['restart', server()]);
  await expect.poll(async () => (await request.get('/api/daemon/version').catch(() => null))?.status(), { timeout: 60_000 }).toBe(200);
}

// adminLink returns the first admin page link the daemon printed after the last line holding
// after (the daemon prints a fresh link each time one is used).
async function adminLink(after = '') {
  let link;
  await expect.poll(() => {
    const log = inBox('cat /root/daemon.log 2>/dev/null || true');
    const at = after ? log.lastIndexOf(after) : 0;
    link = at < 0 ? undefined : log.slice(at).match(/Admin page: (http:\/\/127\.0\.0\.1:\d+\/s\/\S+)/)?.[1];
    return !!link;
  }, { timeout: 60_000 }).toBe(true);
  return link;
}

const installed = () => inBox('sha256sum /root/.local/bin/grill-room-daemon').split(' ')[0];
const stored = () => inBox('ls -A /root/.local/state/grill-room/bin 2>/dev/null || true').trim();

test.beforeAll(() => {
  mkdirSync(out, { recursive: true });
  // A throwaway dev key with an empty password signs the releases, as `make dev-release` does.
  rmSync(`${out}/e2e-cosign.key`, { force: true });
  rmSync(`${out}/e2e-cosign.pub`, { force: true });
  const cosign = { env: { ...process.env, COSIGN_PASSWORD: '' }, stdio: 'ignore' };
  run('cosign', ['generate-key-pair', '--output-key-prefix', `${out}/e2e-cosign`], cosign);
  for (const v of versions) {
    run('make', ['-C', repo, 'dev-release', `DIST=${out}/${v}`, `VERSION=${v}`, `COSIGN_KEY=${out}/e2e-cosign.key`], cosign);
    // cosign writes bundles 0600: the room server, another user in its container, must read them.
    run('sh', ['-c', `chmod a+r "${out}/${v}"/*.sigstore.json`]);
  }
  // The third release is tampered after publication: its binary no longer matches its .sha256.
  const f = `${out}/${versions[2]}/${bin}`;
  writeFileSync(f, Buffer.concat([readFileSync(f), Buffer.from('tampered')]));
  run('docker', ['rm', '-f', box], { stdio: 'ignore' });
  // Host network: the container reaches the room through this machine's traefik, and the browser
  // here reaches the daemon's admin page on 127.0.0.1.
  run('docker', ['run', '-d', '--name', box, '--network', 'host', 'debian:stable-slim', 'sleep', 'infinity']);
  inBox('apt-get update -qq && apt-get install -y -qq --no-install-recommends curl ca-certificates >/dev/null');
});

test.afterAll(() => {
  try { run('docker', ['cp', `${box}:/root/daemon.log`, `${out}/daemon.log`]); } catch { /* the container may not have started */ }
  run('docker', ['rm', '-f', box], { stdio: 'ignore' });
  // Back to the release the image ships.
  const bind = run('docker', ['network', 'inspect', 'bridge', '-f', '{{(index .IPAM.Config 0).Gateway}}']).trim();
  run('docker', ['compose', '-f', `${repo}/compose.yaml`, 'up', '-d', '--force-recreate', 'grill-room'], { env: { ...process.env, GRILL_ROOM_BIND: bind }, stdio: 'ignore' });
});

// 001-AC36 001-AC37 003-AC2 003-AC4 003-AC5 003-AC7: the room's one-liner installs and starts the
// daemon on a clean Debian, and it pairs through its local page. A newer dev-signed release on
// the room server is refused for the worker until its key is trusted on the page, then runs from
// bin/<version>/. Following the instance, the supervisor offers it, applies it on approval and
// drops the copy it no longer needs. A tampered release is refused by the worker, by the update
// and by install.sh.
test('one-liner install, pairing, dev key, self-update, tampered release refused', async ({ browser, request, baseURL }) => {
  await serve(request, `${out}/${versions[0]}`);
  const room = await createRoom(request, 'Install e2e');
  const alice = await join(browser, await invite(request, room.id), 'Alice');
  try {
    const roomURL = `${baseURL}/r/${room.id}`;

    // The one-liner exactly as the room page shows it, under the container's sh (dash).
    await alice.locator('#host-agent summary').click();
    const oneLiner = await alice.locator('#host-oneliner').textContent();
    expect(oneLiner).toBe(`curl -fsSL ${baseURL}/install.sh | sh -s -- ${roomURL}`);
    run('docker', ['exec', '-d', box, 'sh', '-c', `${oneLiner} > /root/daemon.log 2>&1`]);
    const admin = await pairDaemon(alice, { link: adminLink() });
    const log = inBox('cat /root/daemon.log');
    expect(log).toContain(`Downloading ${bin}`);
    expect(log).toContain('Installed /root/.local/bin/grill-room-daemon');
    expect(installed()).toBe(sha256(readFileSync(`${out}/${versions[0]}/${bin}`)));
    await expect(admin.locator('#host')).toContainText(versions[0]);
    await expect(admin.locator('#docker')).toHaveText('Docker is not installed');
    await expect(admin.locator('#self-sig')).toHaveText('dev build');
    // 003b-AC1: the app's binary is in the server's details, behind its ⋯.
    await admin.locator('#instances .server-head button.more').click();
    await admin.getByRole('button', { name: 'App details' }).click();
    await expect(admin.locator('#instances')).toContainText("App binary: this app's own binary");
    await expect(admin.locator('#update')).toBeHidden();

    // A newer release on the room server: its worker restarts on it, and refuses an unknown key.
    await serve(request, `${out}/${versions[1]}`);
    await expect(admin.locator('#instances .server-head .status')).toHaveText('refused', { timeout: 120_000 });
    await expect(admin.locator('#instances .server-error')).toContainText('ignature refused');
    expect(stored()).toBe('');
    // Trusted on the page, the key lets it run from bin/<version>/.
    await admin.locator('#tab-update').click();
    await admin.locator('#key-name').fill('e2e');
    await admin.locator('#key-pem').fill(readFileSync(`${out}/e2e-cosign.pub`, 'utf8'));
    await admin.getByRole('button', { name: 'Add the key' }).click();
    await expect(admin.locator('#instances .server-head')).toContainText(versions[1], { timeout: 60_000 });
    await expect(admin.locator('#instances .server-head .status')).toHaveText('connected', { timeout: 60_000 });
    await expect(admin.locator('#instances')).toContainText('App binary: signed with the dev key e2e');
    await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.', { timeout: 30_000 });
    expect(stored()).toBe(versions[1]);
    expect(inBox(`sha256sum /root/.local/state/grill-room/bin/${versions[1]}/${bin}`).split(' ')[0]).toBe(sha256(readFileSync(`${out}/${versions[1]}/${bin}`)));

    // The supervisor follows the instance: the release is offered, applied on approval.
    await admin.locator('#channel').selectOption('instance');
    await admin.locator('#channel-form').getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#update-version')).toHaveText(versions[1], { timeout: 60_000 });
    await expect(admin.locator('#update-from')).toHaveText(baseURL);
    await admin.getByRole('button', { name: 'Update and restart' }).click();
    // 005-AC45: the same tab waits for the new version on its address and reconnects.
    await expect(admin.locator('#banner')).toContainText('Updating');
    await expect(admin.locator('#banner')).toHaveText(`Updated to ${versions[1]}.`, { timeout: 120_000 });
    const next = admin;
    await expect(next.locator('#host')).toContainText(versions[1]);
    await expect(next.locator('#self-sig')).toHaveText('signed with a dev key');
    await expect(next.locator('#pairing')).toHaveText('Paired. Connected.', { timeout: 30_000 });
    await expect(next.locator('#update')).toBeHidden();
    expect(installed()).toBe(sha256(readFileSync(`${out}/${versions[1]}/${bin}`)));
    // The worker now runs the supervisor's own binary: the stored copy is gone.
    await next.locator('#tab-rooms').click();
    if (!(await next.locator('.server-details').count())) {
      await next.locator('#instances .server-head button.more').click();
      await next.getByRole('button', { name: 'App details' }).click();
    }
    await expect(next.locator('#instances')).toContainText("App binary: this app's own binary");
    expect(stored()).toBe('');

    // A tampered release: the worker and the update refuse it; the current binary stays.
    await serve(request, `${out}/${versions[2]}`);
    await expect(next.locator('#instances')).toContainText('does not match the sha256', { timeout: 120_000 });
    await expect(next.locator('#update-version')).toHaveText(versions[2], { timeout: 60_000 });
    await next.locator('#tab-update').click();
    await next.getByRole('button', { name: 'Update and restart' }).click();
    await expect(next.locator('#update-status')).toContainText('does not match the sha256');
    expect(installed()).toBe(sha256(readFileSync(`${out}/${versions[1]}/${bin}`)));
    await expect(next.locator('#host')).toContainText(versions[1]);

    // install.sh refuses it too, here under bash, and installs nothing.
    let refused;
    try {
      inBox(`mkdir -p /root/clean && HOME=/root/clean bash -c 'curl -fsSL ${baseURL}/install.sh | bash -s -- ${roomURL}'`);
    } catch (err) {
      refused = err;
    }
    expect(refused?.status).toBe(1);
    expect(refused.stderr).toContain('sha256 mismatch');
    expect(inBox('ls -A /root/clean/.local/bin')).toBe('');
  } finally {
    await request.delete(`/api/rooms/${room.id}`, { headers: { Authorization: `Bearer ${process.env.GRILL_ROOM_ADMIN_TOKEN}` } });
  }
});
