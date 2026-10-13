import { test, expect } from '@playwright/test';
import { adminToken, mailpitAPI, magicLinkTTL, lab } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// nextMail waits for mail number n (from 1) sent to `to`, read through the mailpit API, and returns
// its subject and the magic link it carries.
async function nextMail(request, to, n) {
  for (let i = 0; i < 50; i++) {
    const found = await (await request.get(`${mailpitAPI}/api/v1/search?query=${encodeURIComponent('to:' + to)}`)).json();
    if (found.messages.length >= n) {
      // Newest first.
      const id = found.messages[found.messages.length - n].ID;
      const msg = await (await request.get(`${mailpitAPI}/api/v1/message/${id}`)).json();
      const link = msg.Text.match(/http\S+\/profile#(magic|bind)=\S+/);
      expect(link, msg.Text).not.toBeNull();
      return { subject: msg.Subject, link: link[0] };
    }
    await new Promise((r) => setTimeout(r, 200)); // polls mailpit, bounded by 50 rounds
  }
  throw new Error(`no mail ${n} to ${to}`);
}

async function joinRoom(browser, request, roomID, name) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

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
    await expect(laptop.locator('#email-state')).toHaveText('No email bound.');
    await laptop.getByLabel('Bind an email').fill(email);
    await laptop.getByRole('button', { name: 'Send me a link' }).click();
    await expect(laptop.locator('#status')).toContainText('Check your mailbox');
    const bind = await nextMail(request, email, 1);
    expect(bind.subject).toBe('Confirm your email for grill-room');

    // A bind link works only in the browser that asked for it, and signs nobody in.
    const tablet = await (await browser.newContext()).newPage();
    await tablet.goto(bind.link);
    await expect(tablet.locator('#status')).toHaveText('open this link in the browser where you asked for it');
    await expect(tablet.locator('#signed-out')).toBeVisible();
    await laptop.goto(bind.link);
    await expect(laptop.locator('#status')).toHaveText('Email bound.');
    await expect(laptop.locator('#email-state')).toHaveText(`Bound to ${email}.`);
    // The secret leaves the address bar once read.
    expect(new URL(laptop.url()).hash).toBe('');
    await laptop.goto(bind.link);
    await expect(laptop.locator('#status')).toHaveText('this link is invalid, already used or expired');

    // Sign in on a phone with the bound address.
    const phone = await (await browser.newContext()).newPage();
    await phone.goto('/profile');
    await phone.getByLabel('Or sign in with the email').fill(email);
    await phone.getByRole('button', { name: 'Email me a sign-in link' }).click();
    await expect(phone.locator('#status')).toContainText('a sign-in link is on its way');
    const signin = await nextMail(request, email, 2);
    expect(signin.subject).toBe('Sign in to grill-room');
    await phone.goto(signin.link);
    await phone.getByRole('button', { name: 'Sign in to this profile' }).click();
    await expect(phone.locator('#status')).toHaveText('Signed in on this device.');
    await expect(phone.locator('#email-state')).toHaveText(`Bound to ${email}.`);
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

// 002-AC4: a link opened after its lifetime is refused and binds nothing.
test('an expired magic link is refused', async ({ browser, request }) => {
  test.skip(magicLinkTTL > 30, 'links live 15 minutes here; the local run covers expiry');
  const email = `late-${Date.now()}@example.com`;
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Late room' } })).json();
  const page = await joinRoom(browser, request, room.id, 'Late');
  await page.goto('/profile');
  await page.getByLabel('Bind an email').fill(email);
  await page.getByRole('button', { name: 'Send me a link' }).click();
  const expired = await nextMail(request, email, 1);
  // The server is a process of its own, with no clock to move: this waits out the short TTL the
  // config gives it. TestMagicLinkExpires checks the rule itself on an injected clock.
  await page.waitForTimeout((magicLinkTTL + 1) * 1000);
  await page.goto(expired.link);
  await expect(page.locator('#status')).toHaveText('this link is invalid, already used or expired');
  await expect(page.locator('#email-state')).toHaveText('No email bound.');
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
