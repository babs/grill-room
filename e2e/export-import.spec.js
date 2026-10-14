import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { gunzipSync, gzipSync } from 'node:zlib';
import { adminToken } from './playwright.config.js';
import { roomWith, startDaemon, pairDaemon, removeSandbox, ask, signIn, noOverflow, menuItem } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const widths = [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }];

// exported fetches room's archive with the admin token.
async function exported(request, room) {
  const res = await request.get(`/api/rooms/${room}/export`, { headers: admin });
  expect(res.status()).toBe(200);
  return res.body();
}

for (const vp of widths) {
  // 004-AC11 004-AC12 004-AC14 004-AC15: /admin exports a room, previews an archive (name, counts,
  // sizes, versions, warnings) without creating anything, imports it as a new room, and refuses a
  // tampered archive with its reason.
  test(`export, preview and import from /admin, ${vp.name}`, async ({ browser, request }) => {
    const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
    const name = `Export ${vp.name} ${Date.now()}`;
    expect((await request.patch(`/api/rooms/${room.id}`, { headers: admin, data: { name } })).status()).toBe(200);
    // Other tests create rooms meanwhile: count the ones of this name.
    const named = async () => (await (await request.get('/api/rooms', { headers: admin })).json()).filter((r) => r.name === name).length;
    await alice.locator('#body').fill('Ship on Friday.');
    await alice.locator('#body').press('Enter');
    await expect(alice.locator('#messages')).toContainText('Ship on Friday.');
    const page = await (await browser.newContext({ viewport: vp })).newPage();
    expect(await signIn(page, adminToken, 'rooms')).toBe(201);
    await page.goto(`/admin#rooms/${room.id}/export`);
    const panel = page.locator('#room-panel');
    await expect(panel.locator('#export-warning')).toContainText('everything Claude read');
    const [dl] = await Promise.all([page.waitForEvent('download'), panel.locator('#export').click()]);
    expect(dl.suggestedFilename()).toMatch(new RegExp(`^grill-room-${room.id}-\\d{8}-\\d{6}\\.tar\\.gz$`));
    const file = await dl.path();
    const log = await (await request.get(`/api/admin/audit?action=room.export&target=${room.id}`, { headers: admin })).json();
    expect(log.entries[0].details.via).toBe('session');

    await page.getByRole('link', { name: '← All rooms' }).click();
    await page.getByRole('button', { name: 'Import a room' }).click();
    await page.locator('#import-file').setInputFiles(file);
    await page.getByRole('button', { name: 'Preview' }).click();
    const pv = page.locator('#import-preview');
    await expect(pv.locator('#import-name')).toHaveText(name);
    await expect(pv.locator('#import-version')).toContainText('this instance');
    await expect(pv.locator('#import-counts')).toContainText('Participants1');
    await expect(pv.locator('#import-counts')).toContainText('Personas1');
    await expect(pv).toContainText('Format3');
    if (vp.width < 400) {
      expect(await noOverflow(page)).toEqual([]);
    }
    expect(await named()).toBe(1);
    await page.getByRole('button', { name: 'Import as a new room' }).click();
    await expect(page.locator('#import-status')).toContainText('Imported as ' + name);
    const copy = (await page.locator('#import-status').textContent()).match(/\(([a-z0-9]+)\)\.$/)[1];
    await expect(page.locator('#room-' + copy)).toContainText(name);
    expect(await named()).toBe(2);
    // Alice's profile opens the copy, with her line.
    await alice.goto('/r/' + copy);
    await expect(alice.locator('#messages')).toContainText('Ship on Friday.');

    // A tampered archive: one byte changed inside, refused with the reason, nothing created.
    const tar = gunzipSync(readFileSync(file));
    tar[tar.indexOf('Ship on Friday.')] = 'X'.charCodeAt(0);
    await page.locator('#import-file').setInputFiles({ name: 'tampered.tar.gz', mimeType: 'application/gzip', buffer: gzipSync(tar) });
    await page.getByRole('button', { name: 'Preview' }).click();
    await expect(page.locator('#import-status')).toContainText('sha256 mismatch on messages.jsonl');
    await expect(pv).toHaveCount(0);
    expect(await named()).toBe(2);
  });
}

// 004-AC11 004-AC8: a room admin finds the export, with its warning, in the room page's panel.
test('the room page offers its admins the export', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const me = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  await alice.reload();
  await menuItem(alice, 'Room settings');
  const dialog = alice.locator('#manage');
  await dialog.getByRole('link', { name: 'Export', exact: true }).click();
  await expect(dialog.locator('#export-warning')).toContainText('everything Claude read');
  const [dl] = await Promise.all([alice.waitForEvent('download'), dialog.locator('#export').click()]);
  expect(dl.suggestedFilename()).toMatch(/\.tar\.gz$/);
});

// 004-AC12: an imported room's persona resumes its session: a new daemon serving the copy runs the
// next turn on the session the original's turn wrote, which still knows the code word. Fake claude:
// no Claude request.
test('the next turn resumes the imported session', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const first = startDaemon(`${baseURL}/r/${room.id}`);
  let second;
  try {
    await pairDaemon(alice, first);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 60_000 });
    await ask(alice, room.id, 'Remember: the code word is ZEBRA-42.');
    const res = await request.post('/api/rooms/import', { headers: { ...admin, 'Content-Type': 'application/gzip' }, data: await exported(request, room.id) });
    expect(res.status()).toBe(201);
    const copy = (await res.json()).room.id;

    // Alice, relinked by her profile, opens the copy and pairs another daemon for it.
    await alice.goto('/r/' + copy);
    second = startDaemon(`${baseURL}/r/${copy}`);
    await pairDaemon(alice, second);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 60_000 });
    expect(await ask(alice, copy, 'FAKE_RECALL')).toContain('ZEBRA-42');
  } finally {
    for (const d of [first, second].filter(Boolean)) {
      d.cleanup();
      await d.exited;
      removeSandbox(d.sandbox);
    }
  }
});
