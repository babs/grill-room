import { test, expect } from '@playwright/test';
import { magicLinkTTL, lab } from './playwright.config.js';
import { admin, nextMail, joinRoom, newContext, durationText } from './helpers.js';

// 002-AC4: bind an email, sign in from other devices with emailed links, each link once. The room
// and the profile, which holds the email, are deleted at the end: on the lab they would pile up.
test('magic links: bind, sign in elsewhere, single use', async ({ browser, request }) => {
  const email = `alice-${Date.now()}@example.com`;
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Mail room' } })).json();
  let profile = 0;
  try {
    const laptop = await joinRoom(browser, request, room.id, 'Alice');
    profile = (await (await laptop.request.get('/api/profile')).json()).profile.id;

    await laptop.goto('/profile');
    await expect(laptop.locator('#email-state')).toHaveText('No email on this profile yet.');
    await laptop.getByLabel('Add an email').fill(email);
    await laptop.getByRole('button', { name: 'Send me a link' }).click();
    await expect(laptop.locator('#email .link-sent h3')).toHaveText('Link sent to ' + email);
    const bind = await nextMail(request, email, 1);
    // 011-AC24: the subject says what the link does.
    expect(bind.subject).toBe('Add this email to your grill-room profile');

    // 011-AC23: a bind link opened elsewhere asks first, names the profile, and signs nobody in.
    const tablet = await (await newContext(browser)).newPage();
    await tablet.goto(bind.link);
    await expect(tablet.locator('#confirm-text')).toContainText(`adds ${email} to Alice's profile`);
    await tablet.getByRole('button', { name: 'Cancel' }).click();
    await expect(tablet.locator('#status')).toHaveText('Link not used.');
    await expect(tablet.locator('#signed-out')).toBeVisible();
    await laptop.goto(bind.link);
    await expect(laptop.locator('#status')).toHaveText(`Email added to your profile: ${email}.`);
    await expect(laptop.locator('#email-state')).toHaveText(email);
    // The secret leaves the address bar once read.
    expect(new URL(laptop.url()).hash).toBe('');
    await laptop.goto(bind.link);
    await expect(laptop.locator('#status')).toHaveText('This link was already used. Your profile is below.');

    // Sign in on a phone with the bound address.
    const phone = await (await newContext(browser)).newPage();
    await phone.goto('/profile');
    await phone.getByLabel('Or sign in with the email').fill(email);
    await phone.getByRole('button', { name: 'Email me a sign-in link' }).click();
    await expect(phone.locator('#signed-out .link-sent h3')).toHaveText('If a profile uses this address, a link is on its way');
    await expect(phone.locator('#signed-out .link-sent')).toContainText(`The link works once, within ${durationText(magicLinkTTL)}.`);
    const signin = await nextMail(request, email, 2);
    expect(signin.subject).toBe('Sign in to grill-room');
    await phone.goto(signin.link);
    await phone.getByRole('button', { name: 'Sign in to this profile' }).click();
    await expect(phone.locator('#status')).toHaveText('Signed in as Alice on this device.');
    await expect(phone.locator('#email-state')).toHaveText(email);
    await phone.getByRole('link', { name: 'Mail room' }).click();
    await expect(phone.locator('#me')).toHaveText('Alice');
  } finally {
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    if (profile) await request.delete(`/api/profiles/${profile}`, { headers: admin });
  }
  const ids = async (path) => (await (await request.get(path, { headers: admin })).json()).map((x) => x.id);
  expect(await ids('/api/profiles'), 'the profile is gone').not.toContain(profile);
  expect(await ids('/api/rooms'), 'the room is gone').not.toContain(room.id);
});

// 002-AC4 011-AC23: a link opened after its lifetime is refused, binds nothing, and offers a new one.
test('an expired magic link is refused', async ({ browser, request }) => {
  test.skip(magicLinkTTL > 30, 'links live 15 minutes here; the local run covers expiry');
  const email = `late-${Date.now()}@example.com`;
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Late room' } })).json();
  const page = await joinRoom(browser, request, room.id, 'Late');
  await page.goto('/profile');
  await page.getByLabel('Add an email').fill(email);
  await page.getByRole('button', { name: 'Send me a link' }).click();
  const expired = await nextMail(request, email, 1);
  // The server is a process of its own, with no clock to move: this waits out the short TTL the
  // config gives it, until the profile no longer reports the link pending. TestMagicLinkExpires
  // checks the rule itself on an injected clock.
  await expect.poll(async () => (await (await page.request.get('/api/profile')).json()).profile.pending_email,
    { timeout: (magicLinkTTL + 5) * 1000 }).toBeUndefined();
  await page.goto(expired.link);
  await expect(page.locator('#status')).toHaveText('This link has expired.');
  await expect(page.locator('#email-state')).toHaveText('No email on this profile yet.');
  await page.getByRole('button', { name: 'Send a new link' }).click();
  await expect(page.locator('#bind-email')).toBeFocused();
});

// 002-AC4: rate limits per address and per client IP. The server sits on loopback, so it reads the
// client from X-Forwarded-For, as behind the lab's proxy.
test('magic link requests are rate-limited per address and per IP', async ({ request }) => {
  test.skip(lab, 'behind the lab proxy every request counts against this machine, whatever X-Forwarded-For says');
  const ask = (email, ip) => request.post('/api/profile/signin', { data: { email }, headers: { 'X-Forwarded-For': ip } }).then((r) => r.status());
  const addr = `limited-${Date.now()}@example.com`;
  for (let i = 0; i < 3; i++) expect(await ask(addr, '198.51.100.1')).toBe(202);
  expect(await ask(addr, '198.51.100.1')).toBe(429);
  // Another client can still ask for that address: one person cannot lock its owner out.
  expect(await ask(addr, '198.51.100.2')).toBe(202);
  for (let i = 0; i < 10; i++) expect(await ask(`ip-${i}-${Date.now()}@example.com`, '203.0.113.50')).toBe(202);
  expect(await ask(`ip-x-${Date.now()}@example.com`, '203.0.113.50')).toBe(429);
});
