import { test, expect } from './fixtures.js';
import { createHash } from 'node:crypto';
import { roomWith, menuItem } from './helpers.js';

// 001-AC36: the room offers the daemon for the visitor's platform, lets them pick another, and
// shows the install one-liner carrying this room's URL. The binary served matches its sha256.
test('host the agent: download and one-liner', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const section = alice.locator('#host-agent');
  await menuItem(alice, 'Run the AI on my computer');
  await expect(section).toBeVisible();
  await section.locator('summary').click();

  const origin = new URL(alice.url()).origin;
  await expect(alice.locator('#host-oneliner')).toHaveText(`curl -fsSL ${origin}/install.sh | sh -s -- ${origin}/r/${room.id}`);
  // This suite runs Chrome on linux/amd64, which userAgentData reports.
  await expect(alice.locator('#host-os')).toHaveValue('linux');
  await expect(alice.locator('#host-arch')).toHaveValue('amd64');
  const link = alice.locator('#host-download');
  await expect(link).toHaveAttribute('href', '/download/grill-room-daemon-linux-amd64');

  await alice.locator('#host-os').selectOption('darwin');
  await alice.locator('#host-arch').selectOption('arm64');
  await expect(link).toHaveAttribute('href', '/download/grill-room-daemon-darwin-arm64');
  await expect(alice.locator('#host-wsl')).toBeHidden();
  const shown = (await alice.locator('#host-sha').textContent()).replace('sha256 ', '');
  const bin = await (await request.get('/download/grill-room-daemon-darwin-arm64')).body();
  expect(createHash('sha256').update(bin).digest('hex')).toBe(shown);

  await alice.locator('#host-os').selectOption('wsl');
  await expect(link).toHaveAttribute('href', '/download/grill-room-daemon-linux-arm64');
  await expect(alice.locator('#host-wsl')).toBeVisible();
});
