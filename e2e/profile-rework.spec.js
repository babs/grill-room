import { mkdirSync } from 'node:fs';
import { test, expect } from './fixtures.js';
import { roomWith, appointAdmin, smallTargets, noOverflow, skillZip, confirmSheet, unfoldTabs } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// The /profile rework (specs/011b-profile-rework.md). The e2e server has no SMTP: the Email states
// come from GET /api/profile answered with email_enabled and the state patched in.
const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
const address = 'pat@example.com';
const states = {
  none: {},
  pending: { pending_email: address, pending_ttl_s: 14 * 60, pending_age_s: 90 },
  bound: { email: address },
};
const tabs = ['rooms', 'starters', 'skills', 'tokens', 'account'];

// SHOTS=<dir> saves full-page screenshots for the visual check.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png`, fullPage: true }) : null);

// profilePage opens a new tab of page's browser with GET /api/profile patched by patch(profile),
// email on. A new tab: the joining tab's recovery link would change the text.
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
  return [...document.querySelectorAll('button, a.btn')].filter((b) => b.getClientRects().length && !b.closest('[hidden], dialog')
    && getComputedStyle(b).backgroundColor === primary).map((b) => b.textContent.trim());
});

// openTab goes to a tab through its link, as a person does.
async function openTab(page, tab) {
  await unfoldTabs(page.locator('#profile-tabs'));
  await page.locator('#tab-' + tab).click();
  await expect(page.locator('#tab-' + tab)).toHaveAttribute('aria-current', 'page');
  await expect(page.locator('#panel-' + tab)).toBeVisible();
}

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
    await tab.goto('/profile#account');
    await expect(tab.locator('#device'), state).toHaveText(said[state]);
    await tab.close();
  }
});

// 011b-AC2 011b-AC3 011b-AC4 011b-AC9 011b-AC16: the admin badge sits in the meta line, clear of
// a long name; sub-headings rank under their section; a visible title right under the header;
// hints at 70ch at most; the whole card opens the room.
test('profile hierarchy, long names and room cards', async ({ browser, request }) => {
  const { room, pages: [pat] } = await roomWith(browser, request, ['Pat']);
  await appointAdmin(pat, request, room.id);
  const long = 'Weekly retro for the platform team and friends, autumn editi';
  const tab = await profilePage(pat, (p) => {
    p.rooms[0].room_name = long;
    p.name = 'Pat';
    p.rooms[0].name = 'Patricia Review-Longname';
  });
  for (const size of [desktop, phone]) {
    await tab.setViewportSize(size);
    await tab.goto('/profile');
    const card = tab.locator('#rooms li.card').first();
    await expect(card.locator('.room-line')).toHaveText(long + ' as Patricia Review-Longname');
    const badge = card.locator('.badge');
    await expect(badge).toHaveText('Admin');
    expect(overlap(await rect(badge), await rect(card.locator('.room-line'))), `badge over the name at ${size.width}`).toBe(false);
    expect((await rect(card)).height).toBeGreaterThanOrEqual(44);

    const title = tab.getByRole('heading', { level: 1, name: 'Your profile' });
    await expect(title).toBeVisible();
    const header = await rect(tab.locator('.app-header'));
    expect((await rect(title)).top - header.bottom).toBeLessThanOrEqual(24);
  }
  // Not on the link: the card's lower right corner opens the room too.
  const card = await rect(tab.locator('#rooms li.card').first());
  await tab.mouse.click(card.right - 6, card.bottom - 6);
  await expect(tab).toHaveURL(new RegExp(`/r/${room.id}$`));

  await tab.goto('/profile#starters');
  await expect(tab.locator('#starters-title')).toBeVisible();
  const px = (loc) => loc.evaluate((e) => [parseFloat(getComputedStyle(e).fontSize), getComputedStyle(e).fontWeight]);
  const [h2] = await px(tab.locator('#starters-title'));
  const [h3, weight] = await px(tab.locator('#starters h3').first());
  const [hint] = await px(tab.locator('#starters .hint').first());
  expect(h3).toBeLessThan(h2);
  expect(h3).toBeGreaterThan(hint);
  expect(weight).toBe('600');
  await tab.setViewportSize(desktop);
  for (const t of tabs) {
    await openTab(tab, t);
    // Each hint's width in ch of its own font: the runner's fonts differ from a workstation's.
    const wide = await tab.locator('main .hint').evaluateAll((els) => {
      const ctx = document.createElement('canvas').getContext('2d');
      return els.filter((e) => e.getClientRects().length).map((e) => {
        ctx.font = getComputedStyle(e).font;
        return e.getBoundingClientRect().width / ctx.measureText('0').width;
      });
    });
    expect(wide.length, t).toBeGreaterThan(0);
    for (const ch of wide) expect(ch, t).toBeLessThanOrEqual(70.5);
  }
});

// 011b-AC5 011b-AC6 011b-AC7 011b-AC15: one filled button per tab; empty skills and shared
// starters say what goes there; "Add a skill" opens a sheet whose file picker is a button naming
// the chosen file; a skill's row shows its version and scope, and its menu removes it.
test('one primary per tab, empty states and the skill sheet', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const tab = await profilePage(pat);
  // The shared library is the server's, filled by other specs: here it is empty.
  await tab.route('**/api/profile/starters', async (route) => {
    const res = await route.fetch();
    await route.fulfill({ response: res, json: (await res.json()).filter((st) => st.library !== 'instance') });
  });
  await tab.goto('/profile');
  const primary = { rooms: ['+ New room'], starters: [], skills: ['Add a skill'], tokens: ['New token'], account: ['Send me a link'] };
  for (const t of tabs) {
    await openTab(tab, t);
    expect(await filled(tab), t).toEqual(primary[t]);
  }
  await openTab(tab, 'skills');
  const empty = tab.locator('#skill-list .empty');
  await expect(empty).toContainText('No skills yet.');
  await expect(empty.getByRole('button')).toHaveText(['Add a skill']);
  await openTab(tab, 'starters');
  await expect(tab.locator('#starters .empty').nth(1)).toHaveText(/^Nobody has shared a starter yet\./);

  await tab.setViewportSize(phone);
  await openTab(tab, 'skills');
  await empty.getByRole('button', { name: 'Add a skill' }).click();
  const sheet = tab.locator('#skill-sheet');
  await expect(sheet).toBeVisible();
  await expect(tab.locator('input[type=file]:visible')).toHaveCount(0);
  expect((await rect(tab.locator('#skill-pick'))).height).toBeGreaterThanOrEqual(44);
  await expect(tab.locator('#skill-file-name')).toHaveText('No file chosen');
  await tab.locator('#skill-file').setInputFiles({ name: 'meeting-notes.zip', mimeType: 'application/zip', buffer: Buffer.from('PK') });
  await expect(tab.locator('#skill-file-name')).toHaveText('meeting-notes.zip');
  expect(await smallTargets(tab), 'skill sheet').toEqual([]);
  // While the upload runs, its own button is the one held, not the picker.
  let answer;
  const held = new Promise((r) => { answer = r; });
  await tab.route('**/api/profile/skills?*', async (route) => { await held; await route.fulfill({ status: 400, json: { error: 'Not a zip.' } }); });
  const add = sheet.getByRole('button', { name: 'Add skill', exact: true });
  await add.click();
  await expect(add).toBeDisabled();
  await expect(tab.locator('#skill-pick')).toBeEnabled();
  answer();
  await expect(tab.locator('#skill-error')).toHaveText('Not a zip.');
  await expect(add).toBeEnabled();
  await tab.unroute('**/api/profile/skills?*');

  await tab.locator('#skill-file').setInputFiles(skillZip('notes', 'NOTES'));
  await add.click();
  await expect(sheet).toBeHidden();
  await expect(tab.locator('#skill-status')).toHaveText('Added notes v1.');
  const row = tab.locator('#skill-list li[data-skill=notes]');
  await expect(row).toContainText('v1 · Only my rooms');
  await expect(tab.locator('#count-skills')).toHaveText('1');
  expect(await filled(tab)).toEqual(['Add a skill']);
  await row.getByRole('button', { name: 'More actions for notes' }).click();
  await tab.locator('[id^=skill-menu-]:popover-open').getByRole('menuitem', { name: 'Remove' }).click();
  await confirmSheet(tab, 'Remove');
  await expect(tab.locator('#skill-status')).toHaveText('notes removed.');
  await expect(tab.locator('#skill-list .empty')).toBeVisible();
});

// 011b-AC8 011b-AC10: the old wording is gone; a browser with no profile gets the 28rem card.
test('profile wording and the signed-out card', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const tab = await profilePage(pat);
  await tab.goto('/profile');
  let text = '';
  for (const t of tabs) {
    await openTab(tab, t);
    text += await tab.locator('body').innerText();
  }
  for (const old of ['for advanced users', 'Offer it', 'shared library', 'Upload', 'This profile', 'Your rooms', 'Profile without email',
    'Add an email above', 'Use on another device', 'No email on this profile yet', 'None yet.', 'as Pat']) {
    expect(text, old).not.toContain(old);
  }

  const stranger = await (await browser.newContext()).newPage();
  for (const size of [desktop, phone]) {
    await stranger.setViewportSize(size);
    await stranger.goto('/profile');
    const card = stranger.locator('#signed-out');
    await expect(card.getByRole('heading', { name: 'Your profile' })).toBeVisible();
    await expect(card).toContainText("You're not signed in on this browser.");
    const box = await rect(card);
    expect(box.width).toBeLessThanOrEqual(28 * 16 + 0.5);
    // Centred on the screen, as the join page's card.
    expect(Math.abs(box.left + box.width / 2 - size.width / 2)).toBeLessThanOrEqual(2);
    await expect(stranger.locator('#me')).toBeHidden();
  }
});

// 011b-AC11 011b-AC12 011b-AC13 011b-AC17 011b-AC18: at 1440 and 390 px, light and dark, in each
// Email state: the strip shows the state above the fold, nothing overflows, every phone target is
// 44 px; each tab has its deep link; /profile#email opens Account on the field; the content is
// the full width, left-aligned with the tabs; the header has the home link and the user menu.
test('tabs and identity strip at desktop and phone widths', async ({ browser, request }) => {
  test.setTimeout(90_000);
  const { pages: [pat] } = await roomWith(browser, request, ['Pat Review']);
  const strip = {
    none: ['#profile-anon', 'No email yet. Add one to sign in on other devices.'],
    pending: ['#profile-pending', `Link sent to ${address}, 14 min left`],
    bound: ['#profile-email', address],
  };
  for (const [state, patch] of Object.entries(states)) {
    const tab = await profilePage(pat, (p) => Object.assign(p, patch));
    for (const scheme of ['light', 'dark']) {
      await tab.emulateMedia({ colorScheme: scheme });
      for (const [vp, size] of [['desktop', desktop], ['phone', phone]]) {
        const where = `${state} ${scheme} ${vp}`;
        await tab.setViewportSize(size);
        await tab.goto('/profile');
        const [sel, said] = strip[state];
        await expect(tab.locator(sel), where).toContainText(said);
        for (const other of Object.values(strip).map(([s]) => s).filter((s) => s !== sel)) await expect(tab.locator(other), where).toBeHidden();
        expect((await rect(tab.locator(sel))).bottom, `${where}: strip above the fold`).toBeLessThanOrEqual(size.height);
        for (const t of tabs) {
          await tab.goto('/profile#' + t);
          await expect(tab.locator('#panel-' + t), `${where} #${t}`).toBeVisible();
          await expect(tab.locator('#tab-' + t)).toHaveAttribute('aria-current', 'page');
          await expect(tab.locator('.tab-panel:visible')).toHaveCount(1);
          const box = await rect(tab.locator(sel));
          expect(box.top >= 0 && box.bottom <= size.height, `${where} #${t}: strip in view`).toBe(true);
          expect(await noOverflow(tab), `${where} #${t}`).toEqual([]);
          if (size === phone) expect(await smallTargets(tab), `${where} #${t}`).toEqual([]);
          if (state !== 'pending' || t === 'account') await shot(tab, `${state}-${t}-${vp}-${scheme}`);
        }
      }
    }
    await tab.close();
  }

  const tab = await profilePage(pat);
  await tab.setViewportSize(desktop);
  // 011-AC7: the marker's link lands on the field.
  await tab.goto('/profile#email');
  await expect(tab.locator('#panel-account')).toBeVisible();
  await expect(tab.locator('#bind-email')).toBeFocused();
  await expect(tab).toHaveURL(/\/profile#account$/);
  // The strip's button does the same from any tab.
  await openTab(tab, 'starters');
  await tab.locator('#profile-anon').getByRole('link', { name: 'Add an email' }).click();
  await expect(tab.locator('#bind-email')).toBeFocused();

  const main = await rect(tab.locator('main'));
  const bar = await rect(tab.locator('#profile-tabs'));
  expect(main.left).toBe(0);
  expect(main.width).toBe(desktop.width);
  expect(bar.left).toBe(main.left + 16);

  await expect(tab.locator('.app-header a.home-link')).toHaveAttribute('href', '/');
  await expect(tab.locator('#me')).toHaveAttribute('data-initials', 'PR');
  await expect(tab.locator('#me')).toHaveText('Pat Review');
  await tab.locator('#me').click();
  await expect(tab.locator('#me-menu').getByRole('link')).toHaveText(['Your rooms', 'Account and email']);
  await tab.locator('#me-menu').getByRole('link', { name: 'Your rooms' }).click();
  await expect(tab.locator('#me-menu')).toBeHidden();
  await expect(tab.locator('#panel-rooms')).toBeVisible();
  await expect(tab.locator('#count-rooms')).toHaveText('1');
});

// 005-AC48: Cancel and Escape close the address field back to Change.
test('changing the email can be cancelled', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const tab = await profilePage(pat, (p) => Object.assign(p, states.bound));
  await tab.goto('/profile#account');
  const change = tab.locator('#email-change');
  const cancel = tab.locator('#email-cancel');
  for (const close of [() => cancel.click(), () => tab.locator('#bind-email').press('Escape')]) {
    await change.click();
    await expect(tab.locator('#bind-email')).toBeFocused();
    await expect(cancel).toBeVisible();
    await tab.locator('#bind-email').fill('new@example.com');
    await close();
    await expect(tab.locator('#bind')).toBeHidden();
    await expect(change).toBeVisible();
    await expect(change).toBeFocused();
    await expect(tab.locator('#email-state')).toHaveText(address);
    await expect(tab.locator('#bind-email')).toHaveValue('');
  }
  // With no address there is nothing to go back to: the field stays, without Cancel.
  const none = await profilePage(pat);
  await none.goto('/profile#account');
  await expect(none.locator('#bind')).toBeVisible();
  await expect(none.locator('#email-cancel')).toBeHidden();
});

// 005-AC48: the profile's own name, checked as at join, kept over a reload, and where the join page
// starts; the room keeps the name it was joined with.
test('the profile sets its name', async ({ browser, request }) => {
  const { pages: [pat] } = await roomWith(browser, request, ['Pat']);
  const tab = await pat.context().newPage();
  await tab.goto('/profile#account');
  const change = tab.locator('#name-change');
  const input = tab.locator('#name-input');
  await expect(tab.locator('#account-name')).toHaveText('Pat');
  await change.click();
  await expect(input).toBeFocused();
  await expect(input).toHaveValue('Pat');
  await tab.locator('#name-cancel').click();
  await expect(tab.locator('#name-form')).toBeHidden();
  await expect(change).toBeVisible();
  await change.click();
  await input.press('Escape');
  await expect(tab.locator('#name-form')).toBeHidden();

  await change.click();
  await input.fill('   ');
  await tab.getByRole('button', { name: 'Save' }).click();
  await expect(tab.locator('#name-error')).toHaveText('Type the name the others will see, up to 40 characters.');
  await expect(input).toHaveAttribute('aria-invalid', 'true');
  await input.fill('Patricia');
  await tab.getByRole('button', { name: 'Save' }).click();
  await expect(tab.locator('#name-form')).toBeHidden();
  await expect(tab.locator('#account-name')).toHaveText('Patricia');
  await tab.reload();
  await expect(tab.locator('#account-name')).toHaveText('Patricia');
  await expect(tab.locator('#me')).toHaveText('Patricia');
  await openTab(tab, 'rooms');
  await expect(tab.locator('#rooms li .room-line')).toHaveText(['Docs room as Pat']);

  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Next room' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  await tab.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await expect(tab.getByLabel('Your name')).toHaveValue('Patricia');
});
