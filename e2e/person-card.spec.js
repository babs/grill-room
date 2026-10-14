import { test, expect } from './fixtures.js';
import { roomWith, fakeDaemon, appointAdmin } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const row = (page, name) => page.locator('#participants li, #participants-away li').filter({ has: page.locator('.name', { hasText: name }) });

// lines reads the open card's name and lines.
async function lines(page) {
  const card = page.locator('#person-card');
  await expect(card).toBeVisible();
  return [await card.locator('strong').textContent(), ...(await card.locator('li').allTextContents())];
}

// 009-AC36: hovering or focusing a person in People or their name on a chat line shows their
// details card, which the pointer may move onto, closed by Escape. It lists the earlier name, the
// roles, the right to ask the AI, audio, transcription, presence and join time, never an IP or a
// user agent, flips above the last row at the bottom edge and stays clear of the ⋯ column. No native
// title tooltip there. On touch a tap opens it, and ⋯ still opens the menu.
test('the people details card', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
  await appointAdmin(alice, request, room.id);
  const stops = [];
  try {
    stops.push(await fakeDaemon(bob, baseURL, room.id, 'bob-laptop'));
    await expect(row(alice, 'Bob').locator('.host-badge')).toBeVisible();
    const carolID = await carol.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()).then((d) => d.me.id), room.id);
    expect((await request.put(`/api/admin/rooms/${room.id}/participants/${carolID}/name`, { headers: admin, data: { name: 'Caroline' } })).status()).toBe(204);
    await expect(row(alice, 'Caroline')).toBeVisible();

    // The host, on hover.
    const bobName = row(alice, 'Bob').locator('.name');
    await bobName.hover();
    const host = await lines(alice);
    expect(host.slice(0, 7)).toEqual(['Bob', 'Roles: AI host', 'Can ask the AI', 'Audio: not in the audio', 'Not transcribed', host[5], host[6]]);
    expect(host[5]).toMatch(/^Here since /);
    expect(host[6]).toMatch(/^Joined /);
    expect(host).toHaveLength(7);
    // A re-render, here Bob joining the audio from another tab, keeps the open card and refreshes it.
    // Opened by focus: a resting pointer would reopen a closed card and hide the fault.
    await alice.mouse.move(0, 0);
    await expect(alice.locator('#person-card')).toBeHidden();
    await bobName.focus();
    const beat = (body) => bob.evaluate(([id, b]) => fetch(`/api/rooms/${id}/presence`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b),
    }).then((r) => r.status), [room.id, body]);
    expect(await beat({ tab: 'bob-second', audio: true })).toBe(204);
    await expect(alice.locator('#person-card')).toContainText('Audio: listening only');
    await expect(alice.locator('#person-card')).toBeVisible();
    expect(await beat({ tab: 'bob-second', gone: true })).toBe(204);
    await expect(alice.locator('#person-card')).toContainText('Audio: not in the audio');
    await expect(alice.locator('#person-card')).toBeVisible();
    await expect(row(alice, 'Bob').locator('.name')).toHaveAttribute('aria-describedby', 'person-card');
    await bobName.hover();
    await expect(bobName).toHaveAttribute('aria-describedby', 'person-card');
    // The pointer can travel onto the card to read it (WCAG 1.4.13), across the gap under the name,
    // and leaving it closes it.
    const cb = await alice.locator('#person-card').boundingBox();
    await alice.mouse.move(cb.x + cb.width / 2, cb.y + cb.height / 2, { steps: 10 });
    await expect(alice.locator('#person-card')).toBeVisible();
    await expect(alice.locator('#person-card strong')).toHaveText('Bob');
    await expect(bobName).toHaveAttribute('aria-describedby', 'person-card');
    // The same for a badge's tip.
    await alice.mouse.move(0, 0);
    await expect(alice.locator('#person-card')).toBeHidden();
    const hostTag = row(alice, 'Bob').locator('.role-host');
    await hostTag.hover();
    await expect(alice.locator('#person-tip')).toBeVisible();
    const tb = await alice.locator('#person-tip').boundingBox();
    await alice.mouse.move(tb.x + tb.width / 2, tb.y + tb.height / 2, { steps: 10 });
    await expect(alice.locator('#person-tip')).toBeVisible();
    await expect(alice.locator('#person-tip')).toHaveText(/AI host/);
    await alice.mouse.move(0, 0);
    await expect(alice.locator('#person-tip')).toBeHidden();
    await expect(alice.locator('#person-card')).toBeHidden();
    await bobName.hover();
    await expect(alice.locator('#person-card')).toBeVisible();
    const card = await alice.locator('#person-card').boundingBox();
    const more = await row(alice, 'Bob').locator('.person-more').boundingBox();
    expect(card.x + card.width).toBeLessThanOrEqual(more.x);
    await alice.keyboard.press('Escape');
    await expect(alice.locator('#person-card')).toBeHidden();
    await expect(bobName).not.toHaveAttribute('aria-describedby', 'person-card');

    // A member renamed by the admin, on focus.
    await row(alice, 'Caroline').locator('.name').focus();
    const member = await lines(alice);
    expect(member.slice(0, 5)).toEqual(['Caroline', 'Earlier name: Carol', 'Roles: member', 'Audio: not in the audio', 'Not transcribed']);
    await alice.keyboard.press('Escape');

    // The room admin herself, then as a standby host once her app connects.
    await row(alice, 'Alice').locator('.name').focus();
    expect((await lines(alice))[1]).toBe('Roles: you, room admin');
    await alice.keyboard.press('Escape');
    stops.push(await fakeDaemon(alice, baseURL, room.id, 'alice-laptop'));
    await expect.poll(async () => {
      await row(carol, 'Alice').locator('.name').focus();
      const l = await lines(carol);
      await carol.keyboard.press('Escape');
      return l[1];
    }).toBe('Roles: room admin, standby host');

    // A chat line's name.
    await bob.locator('#body').fill('hello');
    await bob.locator('#body').press('Enter');
    const line = alice.locator('#messages li.msg', { hasText: 'hello' }).locator('.sender');
    await line.hover();
    expect((await lines(alice))[0]).toBe('Bob');
    await expect(alice.locator('#person-card')).not.toContainText(/\d+\.\d+\.\d+\.\d+|Mozilla|Chrome|HeadlessChrome|127\.0\.0\.1/);
    await alice.mouse.move(0, 0);
    await expect(alice.locator('#person-card')).toBeHidden();
    await line.focus();
    expect((await lines(alice))[0]).toBe('Bob');
    await expect(line).toHaveAttribute('aria-describedby', 'person-card');
    await alice.keyboard.press('Escape');
    // No native tooltip nor help cursor in People or on author names: the card and the tip say it.
    await expect(alice.locator('#participants [title], #participants-away [title], #messages .sender[title]')).toHaveCount(0);
    expect(await alice.locator('#participants .tag:not(.blank)').first().evaluate((el) => getComputedStyle(el).cursor)).not.toBe('help');

    // The last row at the bottom edge: the card flips above it, still clear of the ⋯ column.
    const last = alice.locator('#participants li').last();
    const lb = await last.boundingBox();
    await alice.setViewportSize({ width: 1280, height: Math.ceil(lb.y + lb.height + 12) });
    await last.locator('.name').hover();
    await expect(alice.locator('#person-card')).toBeVisible();
    const flipped = await alice.locator('#person-card').boundingBox();
    expect(flipped.y + flipped.height).toBeLessThanOrEqual((await last.locator('.name').boundingBox()).y);
    expect(flipped.y).toBeGreaterThanOrEqual(0);
    expect(flipped.x + flipped.width).toBeLessThanOrEqual((await last.locator('.person-more').boundingBox()).x);
    await alice.mouse.move(0, 0);

    // Touch: a tap opens the card; ⋯ stays its own target, opens the menu and closes the card.
    const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
    const phone = await (await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true })).newPage();
    await phone.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
    await phone.getByLabel('Your name').fill('Dana');
    await phone.getByRole('button', { name: 'Join the room' }).click();
    await expect(phone.locator('#me')).toHaveText('Dana');
    // A room admin's ⋯ on Bob holds Rename.
    await appointAdmin(phone, request, room.id);
    await expect(phone.locator('#me')).toHaveText('Dana');
    await phone.locator('#tabbar button[data-tab=people]').tap();
    await row(phone, 'Bob').locator('.name').tap();
    expect((await lines(phone)).slice(0, 2)).toEqual(['Bob', 'Roles: AI host']);
    // On a narrow screen it stays inside the viewport, left of the ⋯ column.
    const narrow = await phone.locator('#person-card').boundingBox();
    expect(narrow.x).toBeGreaterThanOrEqual(0);
    expect(narrow.x + narrow.width).toBeLessThanOrEqual((await row(phone, 'Bob').locator('.person-more').boundingBox()).x);
    await row(phone, 'Bob').locator('.person-more').tap();
    await expect(phone.locator('#person-menu')).toBeVisible();
    await expect(phone.locator('#person-card')).toBeHidden();
  } finally {
    for (const stop of stops) stop();
  }
});
