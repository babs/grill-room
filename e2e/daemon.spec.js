import { test, expect } from '@playwright/test';
import { hostname } from 'node:os';
import { roomWith, startDaemon, pairDaemon, stopDaemon, confirmSheet } from './helpers.js';

// 001-AC22: run the daemon with the room URL, open its local page, approve in the room session
// with one click: the daemon is paired and connected, and no token was shown or typed.
test('daemon pairs through the room approval', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await alice.context().newPage();
    await admin.goto(await daemon.link);
    await expect(admin).toHaveURL(/^http:\/\/127\.0\.0\.1:\d+\/$/);
    await expect(admin.locator('#pairing')).toHaveText('Waiting for your approval in the room.');
    const code = await admin.locator('#code').textContent();
    expect(code).toMatch(/^\d{3}-\d{3}$/);

    // A slow pairing request: Approve shows only once it answers, a click before would be lost.
    let shown = '';
    await alice.context().route('**/pairings/*', async (route) => {
      if (route.request().method() !== 'GET') return route.continue();
      const response = await route.fetch();
      await new Promise((r) => setTimeout(r, 1000)); // holds the answer, no assertion waits on it
      shown = (await response.json()).code;
      return route.fulfill({ response });
    });
    const [approval] = await Promise.all([alice.context().waitForEvent('page'), admin.getByRole('link', { name: 'approve in the room' }).click()]);
    await approval.getByRole('button', { name: 'Approve' }).click();
    expect(shown).toBe(code);
    await expect(approval.locator('#pairing-text')).toContainText('Daemon paired');

    await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.');
    // 003-AC12: the Claude account is the supervisor's CLAUDE_CONFIG_DIR, read only, with its status.
    await expect(admin.locator('#accounts li').first()).toContainText('default');
    await expect(admin.locator('#accounts li').first()).toContainText('valid');
    await expect(admin.locator('.alerts')).toHaveCount(0);
    await admin.locator('#tab-settings').click();
    await expect(admin.locator('#can-host')).toBeVisible();
    // The page reloads into the same session; the one-time link is spent.
    await admin.reload();
    await expect(admin.locator('#pairing')).toHaveText('Paired. Connected.');
    const spent = await alice.context().newPage();
    await spent.goto(await daemon.link);
    await expect(spent.locator('#banner')).toContainText('already used');
    for (const p of [admin, approval, alice]) await expect(p.locator('body')).not.toContainText('e2e-fake');

    await stopDaemon(admin);
    await expect(admin.locator('#banner')).toHaveText('The AI host app stopped. You can close this tab.');
    expect(await daemon.exited).toBe(0);
  } finally {
    daemon.cleanup();
  }
});

// 003-AC15, 003-AC17: the room says who holds the agent, never the machine. From the page the host
// cuts the worker off: the room sees the agent offline until the host lets it back.
test('daemon page cuts a worker off from Claude and lets it back', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' });
  try {
    const admin = await pairDaemon(alice, daemon);
    const led = alice.locator('button.persona[data-name="Grill"] .led');
    await expect(led).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    await expect(alice.locator('#messages')).toContainText('Alice holds the agent(s).');
    await expect(admin.getByLabel('Claude account of ' + baseURL, { exact: true })).toHaveValue('default');
    await admin.locator('#tab-instances').click();
    await admin.getByRole('button', { name: 'Cut it off from Claude' }).click();
    await confirmSheet(admin, 'Cut it off');
    await expect(admin.locator('#instances')).toContainText('cut off: its turns end and none starts');
    await expect(led).toHaveAttribute('aria-label', 'Grill offline');
    await admin.getByRole('button', { name: 'Let it use Claude again' }).click();
    await expect(led).toHaveAttribute('aria-label', 'Grill ready');
    await expect(alice.locator('body')).not.toContainText(hostname());
  } finally {
    daemon.cleanup();
  }
});

// 003-AC4, 003-AC5: the page says the supervisor is a dev build. Accepting unsigned binaries from
// an instance needs its host typed exactly; while on, a red banner and a badge show.
test('daemon page shows its signature and guards the unsigned toggle', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  // An unreachable GitHub: no update check leaves this machine.
  const daemon = startDaemon(`${baseURL}/r/${room.id}`, { GRILL_ROOM_GITHUB_URL: 'http://127.0.0.1:9' });
  try {
    const admin = await alice.context().newPage();
    // One status poll fails, as Chrome fails requests when Docker adds or removes a network
    // (ERR_NETWORK_CHANGED): the page says so and keeps polling.
    let failed = false;
    await admin.route('**/api/status', async (route) => {
      if (failed || !(await route.fetch().then((r) => r.json())).instances?.length) return route.continue();
      failed = true;
      return route.abort('failed');
    });
    await admin.goto(await daemon.link);
    await expect(admin.locator('#self-sig')).toHaveText('dev build');
    await expect.poll(() => failed).toBe(true);
    // The worker binary shows once the worker runs, which a loaded machine takes a while to reach.
    await expect(admin.locator('#instances')).toContainText("Worker binary: the supervisor's own binary", { timeout: 30_000 });
    await expect(admin.locator('#banner')).toBeHidden();
    await expect(admin.locator('#relax-banner')).toBeHidden();
    await expect(admin.locator('#checked')).toContainText('127.0.0.1:9');

    await admin.locator('#tab-instances').click();
    const card = admin.locator('#instances li.instance', { hasText: baseURL });
    const accept = card.getByRole('button', { name: 'Accept unsigned binaries' });
    const host = card.getByLabel('To accept its unsigned binaries, type its host');
    await host.fill('127.0.0.1');
    await accept.click();
    // 005-AC39: it asks first.
    await confirmSheet(admin, 'Accept');
    await expect(card.getByRole('status')).toContainText("Type the instance's host exactly");
    await expect(admin.locator('#relax-banner')).toBeHidden();
    await host.fill(new URL(baseURL).host);
    await accept.click();
    await confirmSheet(admin, 'Accept');
    await expect(admin.locator('#relax-banner')).toBeVisible();
    await expect(admin.locator('#relax-banner')).toContainText(baseURL);
    await expect(admin.locator('#instances .badge.danger')).toHaveText('accepts unsigned binaries');
    await admin.getByRole('button', { name: 'Stop accepting unsigned binaries' }).click();
    await expect(admin.locator('#relax-banner')).toBeHidden();
    await expect(admin.locator('#instances .badge.danger')).toHaveCount(0);

    await stopDaemon(admin);
    expect(await daemon.exited).toBe(0);
  } finally {
    daemon.cleanup();
  }
});
