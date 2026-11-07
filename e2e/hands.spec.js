import { test, expect } from './fixtures.js';
import { roomWith } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const phone = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };

const row = (page, name) => page.locator('#participants li', { has: page.locator('.name', { hasText: name }) });
const hand = (page, name) => row(page, name).locator('.tag-hand:not(.blank)');

// lowerFrom opens name's ⋯ on page and picks "Lower hand".
async function lowerFrom(page, name) {
  await row(page, name).locator('.person-more').click();
  await page.locator('#person-menu').getByRole('button', { name: 'Lower hand' }).click();
}

// 031-AC3 031-AC4 031-AC5 031-AC6: A and B raise in turn and both see the ranks and the count; B
// lowers; only the admin and the owner get "Lower hand"; the admin lowers A's; a reload keeps it.
test('raise, rank, lower and keep a hand', async ({ browser, request }) => {
  const { room, pages: [alice, bob, carol] } = await roomWith(browser, request, ['Alice', 'Bob', 'Carol']);
  const carolID = await carol.evaluate(() => myID);
  expect((await request.put(`/api/rooms/${room.id}/admins/${carolID}`, { headers: admin })).status()).toBe(204);

  const raise = (page) => page.getByRole('button', { name: 'Raise my hand' });
  await expect(raise(alice)).toHaveAttribute('aria-pressed', 'false');
  await raise(alice).click();
  await expect(alice.getByRole('button', { name: 'Lower my hand' })).toHaveAttribute('aria-pressed', 'true');
  await expect(hand(bob, 'Alice')).toHaveAttribute('aria-label', 'Hand raised, 1st');
  await raise(bob).click();
  for (const page of [alice, bob]) {
    await expect(hand(page, 'Alice')).toHaveText('1');
    await expect(hand(page, 'Bob')).toHaveText('2');
    await expect(hand(page, 'Bob')).toHaveAttribute('aria-label', 'Hand raised, 2nd');
    await expect(page.locator('#people-hands')).toHaveText('2');
    await expect(page.locator('#people-hands')).toHaveAttribute('aria-label', '2 hands raised');
  }
  // The list keeps its order.
  await expect(alice.locator('#participants li .name')).toHaveText(['Alice', 'Bob', 'Carol']);

  // 031-AC5: Bob has "Lower hand" on his own row, and no action at all on Alice's.
  await expect(row(bob, 'Alice').locator('.person-more')).toBeDisabled();
  await lowerFrom(bob, 'Bob');
  await expect(hand(alice, 'Bob')).toHaveCount(0);
  await expect(bob.getByRole('button', { name: 'Raise my hand' })).toHaveAttribute('aria-pressed', 'false');
  await expect(alice.locator('#people-hands')).toHaveText('1');

  // 031-AC6: a reload within the grace keeps the hand.
  await alice.reload();
  await expect(alice.getByRole('button', { name: 'Lower my hand' })).toHaveAttribute('aria-pressed', 'true');
  await expect(hand(carol, 'Alice')).toHaveText('1');

  // The room admin lowers Alice's.
  await lowerFrom(carol, 'Alice');
  await expect(hand(alice, 'Alice')).toHaveCount(0);
  await expect(alice.getByRole('button', { name: 'Raise my hand' })).toHaveAttribute('aria-pressed', 'false');
  await expect(alice.locator('#people-hands')).toBeHidden();
});

// 031-AC3: quick clicks reach the server in order, and the button ends on what the server holds.
test('quick clicks on the hand button end where the server is', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const b = alice.locator('#hand-raise');
  for (let i = 0; i < 3; i++) await b.click();
  await expect(hand(bob, 'Alice')).toHaveText('1');
  await expect(b).toHaveAttribute('aria-pressed', 'true');
  for (let i = 0; i < 3; i++) await b.click();
  await expect(b).toHaveAttribute('aria-pressed', 'false');
  // Bob's page reads the server: Alice's hand stays down once every request settled.
  await expect(hand(bob, 'Alice')).toHaveCount(0);
  await bob.reload();
  await expect(row(bob, 'Alice')).toBeVisible();
  await expect(hand(bob, 'Alice')).toHaveCount(0);
  await expect(b).toHaveAttribute('aria-pressed', 'false');
});

// 031-AC3 031-AC4 at 390 px: the button is 44 × 44 by the composer, and the People tab counts hands.
test('a phone raises a hand by the composer and its People tab counts hands', async ({ browser, request }) => {
  const { room } = await roomWith(browser, request, ['Alice']);
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext(phone)).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Dan');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Dan');

  const b = page.getByRole('button', { name: 'Raise my hand' });
  const box = await b.boundingBox();
  expect([Math.round(box.width), Math.round(box.height)]).toEqual([44, 44]);
  expect(await b.evaluate((el) => el.closest('#send') !== null)).toBe(true);
  await expect(page.locator('#tab-people-hands')).toBeHidden();
  await b.click();
  await expect(page.locator('#tab-people-hands')).toHaveText('1');
  await expect(page.locator('#tab-people-hands')).toHaveAttribute('aria-label', '1 hand raised');
  await expect(page.locator('#tab-people-hands')).toBeVisible();
  await page.getByRole('button', { name: 'Lower my hand' }).click();
  await expect(page.locator('#tab-people-hands')).toBeHidden();
});
