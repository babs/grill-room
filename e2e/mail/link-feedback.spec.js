import { test, expect } from '../fixtures.js';
import { admin, joinRoom, newContext, durationText, ownProfile } from './helpers.js';
import { magicLinkTTL } from './playwright.config.js';

const widths = [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }];
const mailFailed = 'The mail could not be sent. Try again in a few minutes; if it keeps failing, tell the admin.';

const made = { rooms: [], profiles: [] };
test.afterAll(async ({ request }) => {
  for (const id of made.rooms) await request.delete(`/api/rooms/${id}`, { headers: admin });
  for (const id of made.profiles) await request.delete(`/api/profiles/${id}`, { headers: admin });
});

// hold makes the next requests to path wait until the returned release is called: the busy state
// is then on screen for as long as the test looks at it.
async function hold(page, path) {
  let release, passed;
  const gate = new Promise((r) => { release = r; });
  const through = new Promise((r) => { passed = r; });
  await page.route('**' + path, async (route) => {
    await gate;
    await route.continue();
    passed();
  });
  // The route goes once the held request went on: removed earlier, it would answer that request itself.
  return async () => {
    release();
    await through;
    await page.unroute('**' + path);
  };
}

// feedback walks one email form through 011-AC28: busy, the sent panel in its place, Resend with its
// countdown then "Sent again", "Use another address", then each error in place under the field.
async function feedback(page, { box, form, input, path, address, heading, vp }) {
  const submit = form.locator('button[type=submit], button:not([type])');
  const panel = box.locator('.link-sent');
  await input.fill(address);
  const release = await hold(page, path);
  await submit.click();
  await expect(submit).toHaveAttribute('aria-busy', 'true');
  await expect(submit).toHaveText('Sending…');
  await expect(submit).toBeDisabled();
  await release();

  await expect(panel).toBeVisible();
  await expect(form).toBeHidden();
  await expect(panel).toHaveAttribute('role', 'status');
  await expect(panel.locator('h3')).toHaveText(heading);
  await expect(panel.locator('h3')).toBeFocused();
  await expect(panel).toContainText(`The link works once, within ${durationText(magicLinkTTL)}.`);
  await expect(panel).toContainText('Check your spam folder if it does not arrive.');
  const resend = panel.getByRole('button', { name: /^Resend/ });
  await expect(resend).toHaveText(/^Resend \(\d+ s\)$/);
  await expect(resend).toBeDisabled();
  await expect(panel.locator('.again')).toBeHidden();
  // The actions start where the text does: the kit's .actions alone would push them right.
  const [h3Box, resendBox] = [await panel.locator('h3').boundingBox(), await resend.boundingBox()];
  expect(Math.abs(resendBox.x - h3Box.x)).toBeLessThan(1);
  if (vp.width < 400) {
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(vp.width);
  }
  // The countdown runs a minute: a new zero-second one ends it, as the next call replaces it.
  await resend.evaluate((b) => window.grillCooldown(b, 0));
  await expect(resend).toBeEnabled();
  await resend.click();
  await expect(panel.locator('.again')).toHaveText('Sent again');
  await expect(panel.locator('.again')).toBeFocused();
  await expect(resend).toHaveText(/^Resend \(\d+ s\)$/);

  await panel.getByRole('button', { name: 'Use another address' }).click();
  await expect(form).toBeVisible();
  await expect(panel).toBeHidden();
  await expect(input).toHaveValue(address);
  await expect(input).toBeFocused();

  const error = form.getByRole('alert');
  const answers = [
    [{ status: 502, json: { reason: 'mail_failed', error: mailFailed } }, mailFailed],
    [{ status: 429, json: { reason: 'rate_limited', error: 'too many requests', retry_after_s: 290 } }, 'Too many requests. Try again in 5 min.'],
    [{ status: 400, json: { reason: 'invalid_email', error: 'invalid email' } }, 'Check the address: it does not look like an email.'],
  ];
  for (const [answer, text] of answers) {
    await page.route('**' + path, (route) => route.fulfill(answer));
    await submit.click();
    await expect(error).toHaveText(text);
    await expect(input).toBeFocused();
    await expect(form).toBeVisible();
    await expect(panel).toBeHidden();
    await expect(submit).toBeEnabled();
    await page.unroute('**' + path);
  }
  await page.route('**' + path, (route) => route.abort('internetdisconnected'));
  await submit.click();
  await expect(error).toHaveText('No connection. Check your network and try again.');
  await expect(input).toBeFocused();
  await page.unroute('**' + path);
  if (vp.width < 400) {
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(vp.width);
  }
}

for (const vp of widths) {
  // 011-AC28: the join page's email recovery answers in place.
  test(`sign-in link feedback, ${vp.name}`, async ({ browser }) => {
    const page = await (await newContext(browser)).newPage();
    await page.setViewportSize(vp);
    await page.goto('/');
    await page.getByRole('button', { name: 'Recover my profile by email' }).click();
    await feedback(page, {
      box: page.locator('#recover'), form: page.locator('#recover-form'), input: page.locator('#recover-email'),
      path: '/api/profile/signin', address: `ac28-signin-${vp.name}-${Date.now()}@example.com`,
      heading: 'If a profile uses this address, a link is on its way', vp,
    });
  });

  // 011-AC28: /profile's Email section answers in place, and a reload shows the sent panel again.
  test(`bind link feedback, ${vp.name}`, async ({ browser, request }) => {
    const room = await (await request.post('/api/rooms', { headers: admin, data: { name: `AC28 ${vp.name} ${Date.now()}` } })).json();
    made.rooms.push(room.id);
    const page = await joinRoom(browser, request, room.id, 'Fay');
    made.profiles.push((await ownProfile(page)).id);
    await page.setViewportSize(vp);
    await page.goto('/profile');
    const address = `ac28-bind-${vp.name}-${Date.now()}@example.com`;
    await feedback(page, {
      box: page.locator('#email'), form: page.locator('#bind'), input: page.locator('#bind-email'),
      path: '/api/profile/email', address, heading: 'Link sent to ' + address, vp,
    });
    // A reload shows what is left of the link's life and of the Resend wait, as the server dates them.
    // Those seconds are set here: the config's 4 s links would expire under a slow run.
    for (const [sentAgo, resendWaits] of [[20, true], [120, false]]) {
      await page.route('**/api/profile', async (route) => {
        const body = await (await route.fetch()).json();
        body.profile.pending_email = address;
        body.profile.pending_age_s = sentAgo;
        body.profile.pending_ttl_s = 10 * 60 - 1;
        await route.fulfill({ json: body });
      });
      await page.reload();
      const panel = page.locator('#email .link-sent');
      await expect(panel.locator('h3')).toHaveText('Link sent to ' + address);
      await expect(panel).toContainText('The link works once, within 10 minutes.');
      await expect(page.locator('#bind')).toBeHidden();
      const resend = panel.getByRole('button', { name: /^Resend/ });
      if (resendWaits) {
        await expect(resend).toHaveText(/^Resend \(\d+ s\)$/);
        await expect(resend).toBeDisabled();
      } else {
        await expect(resend).toHaveText('Resend');
        await expect(resend).toBeEnabled();
      }
      await page.unroute('**/api/profile');
    }
  });
}
