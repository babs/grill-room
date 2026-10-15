import { mkdirSync } from 'node:fs';
import { test, expect } from './fixtures.js';
import { roomWith, appointAdmin, smallTargets } from './helpers.js';

// The /profile rework (specs/011b-profile-rework.md). The e2e server has no SMTP: the Email states
// come from GET /api/profile answered with email_enabled and the state patched in.
const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
const address = 'pat@example.com';
// SHOTS=<dir> saves full-page screenshots for the visual check.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png`, fullPage: true }) : null);
const states = {
  none: {},
  pending: { pending_email: address, pending_ttl_s: 14 * 60, pending_age_s: 90 },
  bound: { email: address },
};

// profilePage opens /profile in a new tab of page's browser, its GET /api/profile patched by
// patch(profile) with email on. A new tab: the joining tab's recovery link would change the text.
async function profilePage(page, patch = () => {}) {
  const tab = await page.context().newPage();
  await tab.route('**/api/profile', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const res = await route.fetch();
    const body = await res.json();
    body.email_enabled = true;
    if (body.profile) patch(body.profile);
    await route.fulfill({ response: res, json: body });
  });
  return tab;
}

// rect is an element's box; overlap says whether two boxes share any pixel.
const rect = (loc) => loc.evaluate((e) => e.getBoundingClientRect().toJSON());
const overlap = (a, b) => a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;

// filled lists the visible buttons drawn in the primary fill (docs/design.md §5: one per view).
const filled = (page) => page.evaluate(() => {
  const probe = document.createElement('span');
  probe.style.color = 'var(--primary)';
  document.body.append(probe);
  const primary = getComputedStyle(probe).color;
  probe.remove();
  return [...document.querySelectorAll('button, a.btn')].filter((b) => b.getClientRects().length && !b.closest('[hidden], dialog:not([open])')
    && getComputedStyle(b).backgroundColor === primary).map((b) => b.textContent.trim());
});

// 011b-AC1: "Other devices" follows the Email state.
test('other devices text follows the email state', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const said = {
    none: 'Add an email, then sign in with it on the other device.',
    pending: 'Confirm the link we sent, then sign in on the other device.',
    bound: `Sign in with ${address} on the other device.`,
  };
  for (const [state, patch] of Object.entries(states)) {
    const tab = await profilePage(pat, (p) => Object.assign(p, patch));
    await tab.goto('/profile');
    await expect(tab.locator('#device'), state).toHaveText(said[state]);
    await shot(tab, 'state-' + state);
    await tab.close();
  }
});

// 011b-AC2 011b-AC3 011b-AC4 011b-AC9: the admin badge sits in the meta line, clear of a long
// name; sub-headings rank under their section; a visible title right under the header; hints at
// 70ch at most.
test('profile hierarchy and long names', async ({ browser, request }) => {
  const { room, pages: [pat] } = await roomWith(browser, request, ['Pat']);
  await appointAdmin(pat, request, room.id);
  const long = 'Weekly retro for the platform team and friends, autumn editi';
  const tab = await profilePage(pat, (p) => {
    p.rooms[0].room_name = long;
    p.rooms[0].name = 'Patricia Review-Longname';
  });
  for (const size of [desktop, phone]) {
    await tab.setViewportSize(size);
    await tab.goto('/profile');
    const card = tab.locator('#rooms li.card').first();
    await expect(card.locator('.room-line')).toContainText(long);
    const badge = card.locator('.badge');
    await expect(badge).toHaveText('Admin');
    expect(overlap(await rect(badge), await rect(card.locator('.room-line'))), `badge over the name at ${size.width}`).toBe(false);

    const title = tab.getByRole('heading', { level: 1, name: 'Your profile' });
    await expect(title).toBeVisible();
    const header = await rect(tab.locator('.app-header'));
    expect((await rect(title)).top - header.bottom).toBeLessThanOrEqual(24);
  }
  const px = (loc) => loc.evaluate((e) => [parseFloat(getComputedStyle(e).fontSize), getComputedStyle(e).fontWeight]);
  const [h2] = await px(tab.locator('#starters-title'));
  const [h3, weight] = await px(tab.locator('#starters h3').first());
  const [hint] = await px(tab.locator('#starters .hint').first());
  expect(h3).toBeLessThan(h2);
  expect(h3).toBeGreaterThan(hint);
  expect(weight).toBe('600');
  await tab.setViewportSize(desktop);
  const wide = await tab.locator('main .hint').evaluateAll((els) => els.filter((e) => e.getClientRects().length)
    .map((e) => e.getBoundingClientRect().width / parseFloat(getComputedStyle(e).fontSize)));
  // 70ch of a proportional font is about 35 to 40 em.
  for (const em of wide) expect(em).toBeLessThanOrEqual(42);
});

// 011b-AC5 011b-AC6 011b-AC7: one filled button; the empty skills and shared starters say what
// goes there; the skill file picker is a button that names the chosen file.
test('one primary, empty states and the skill file picker', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const tab = await profilePage(pat);
  await tab.goto('/profile');
  await tab.locator('#skills > summary').click();
  await expect(tab.locator('#bind')).toBeVisible();
  expect(await filled(tab)).toEqual(['+ New room']);
  const empty = tab.locator('#skill-list .empty');
  await expect(empty).toContainText('No skills yet.');
  await expect(empty.getByRole('button')).toHaveText(['Add a skill']);
  await expect(tab.locator('#starters-instance')).toHaveCount(0);
  await expect(tab.locator('#starters .empty').last()).toHaveText(/^Nobody has shared a starter yet\./);

  await tab.setViewportSize(phone);
  await expect(tab.locator('input[type=file]:visible')).toHaveCount(0);
  expect((await rect(tab.locator('#skill-pick'))).height).toBeGreaterThanOrEqual(44);
  await expect(tab.locator('#skill-file-name')).toHaveText('No file chosen');
  await tab.locator('#skill-file').setInputFiles({ name: 'meeting-notes.zip', mimeType: 'application/zip', buffer: Buffer.from('PK') });
  await expect(tab.locator('#skill-file-name')).toHaveText('meeting-notes.zip');
  expect(await smallTargets(tab), 'profile').toEqual([]);
  // While the upload runs, its own button is the one held, not the picker.
  let answer;
  const held = new Promise((r) => { answer = r; });
  await tab.route('**/api/profile/skills?*', async (route) => { await held; await route.fulfill({ status: 400, json: { error: 'Not a zip.' } }); });
  await tab.getByRole('button', { name: 'Add skill', exact: true }).click();
  await expect(tab.getByRole('button', { name: 'Add skill', exact: true })).toBeDisabled();
  await expect(tab.locator('#skill-pick')).toBeEnabled();
  answer();
  await expect(tab.locator('#skill-status')).toHaveText('Not a zip.');
  await expect(tab.getByRole('button', { name: 'Add skill', exact: true })).toBeEnabled();
});

// 011b-AC8 011b-AC10: the old wording is gone; a browser with no profile gets the 28rem card.
test('profile wording and the signed-out card', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const tab = await profilePage(pat);
  await tab.goto('/profile');
  await tab.locator('#skills > summary').click();
  await expect(tab.locator('#starters-title')).toBeVisible();
  const text = await tab.locator('main').innerText();
  for (const old of ['for advanced users', 'Offer it', 'shared library', 'Upload', 'This profile', 'Your rooms', 'Profile without email',
    'Add an email above', 'Use on another device', 'No email on this profile yet', 'None yet.', 'as Pat']) {
    expect(text, old).not.toContain(old);
  }

  const stranger = await (await browser.newContext({ viewport: phone })).newPage();
  for (const size of [desktop, phone]) {
    await stranger.setViewportSize(size);
    await stranger.goto('/profile');
    const card = stranger.locator('#signed-out');
    await expect(card.getByRole('heading', { name: 'Your profile' })).toBeVisible();
    await expect(card).toContainText("You're not signed in on this browser.");
    expect((await rect(card)).width).toBeLessThanOrEqual(28 * 16 + 0.5);
  }
});
