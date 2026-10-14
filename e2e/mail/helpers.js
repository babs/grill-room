import { expect } from '@playwright/test';
import { adminToken, mailpitAPI } from './playwright.config.js';

export const admin = { Authorization: `Bearer ${adminToken}` };

// nextMail waits for mail number n (from 1) sent to `to`, read through the mailpit API, and returns
// its subject, text and the magic link it carries.
export async function nextMail(request, to, n) {
  for (let i = 0; i < 50; i++) {
    const found = await (await request.get(`${mailpitAPI}/api/v1/search?query=${encodeURIComponent('to:' + to)}`)).json();
    if (found.messages.length >= n) {
      // Newest first.
      const id = found.messages[found.messages.length - n].ID;
      const msg = await (await request.get(`${mailpitAPI}/api/v1/message/${id}`)).json();
      const link = msg.Text.match(/http\S+\/profile#(magic|bind)=\S+/);
      expect(link, msg.Text).not.toBeNull();
      return { subject: msg.Subject, text: msg.Text, link: link[0] };
    }
    await new Promise((r) => setTimeout(r, 200)); // polls mailpit, bounded by 50 rounds
  }
  throw new Error(`no mail ${n} to ${to}`);
}

// newContext is a browser context seen as a client of its own: the server sits on loopback and
// limits emailed-link requests per client IP (002-AC4), which every spec would otherwise share.
// 198.18.0.0/15 is a benchmarking range the rate-limit spec does not use.
let clients = 0;
export const newContext = (browser) => {
  clients++;
  return browser.newContext({ extraHTTPHeaders: { 'X-Forwarded-For': `198.18.${clients >> 8}.${clients & 255}` } });
};

// joinRoom joins roomID as name in a fresh browser context, which makes a new profile.
export async function joinRoom(browser, request, roomID, name) {
  const inv = await (await request.post(`/api/rooms/${roomID}/invites`, { headers: admin })).json();
  const page = await (await newContext(browser)).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

// durationText words a link lifetime in seconds as the server's mail does: "1 minute", "N minutes",
// else Go's time.Duration form ("4s").
export function durationText(s) {
  if (s === 60) return '1 minute';
  if (s % 60 === 0) return s / 60 + ' minutes';
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
  return (h ? h + 'h' : '') + (h || m ? m + 'm' : '') + (s % 60) + 's';
}

// linkPath is a mailed or admin link as a path on the server under test.
export const linkPath = (url) => new URL(url).pathname + new URL(url).hash;

// ownProfile is what GET /api/profile answers page's browser.
export const ownProfile = async (page) => (await (await page.request.get('/api/profile')).json()).profile;
