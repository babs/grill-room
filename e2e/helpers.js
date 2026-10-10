import { expect } from '@playwright/test';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

export async function roomWith(browser, request, names) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Docs room' } })).json();
  const pages = [];
  for (const name of names) {
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const page = await (await browser.newContext()).newPage();
    await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join the room' }).click();
    await expect(page.locator('#me')).toHaveText(name);
    pages.push(page);
  }
  return { room, pages };
}

export function upload(page, name, mimeType, content) {
  return page.locator('#upload-input').setInputFiles({ name, mimeType, buffer: Buffer.from(content) });
}

// docFrame returns the document frame on show, once its page has loaded.
export async function docFrame(page) {
  const handle = await page.locator('#doc-frame iframe').elementHandle();
  const frame = await handle.contentFrame();
  await frame.waitForLoadState();
  return frame;
}
