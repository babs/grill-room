import { test, expect } from './fixtures.js';
import { roomWith, menuItem } from './helpers.js';

// A room page whose first room request fails, as Chrome fails one on ERR_NETWORK_CHANGED, loads the
// room once a retry answers instead of staying empty.
test('the room page retries a failed first load', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  let failed = 0;
  await alice.route(new RegExp(`/api/rooms/${room.id}$`), (r) => (failed++ ? r.continue() : r.abort('internetdisconnected')));
  await alice.reload();
  await expect(alice.locator('#me')).toHaveText('Alice');
  expect(failed).toBeGreaterThanOrEqual(2);
});

// A read the browser drops once, here the personas panel's templates, is retried: the panel lists them.
test('a dropped read is retried', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  let failed = 0;
  await alice.route(new RegExp(`/api/rooms/${room.id}/templates$`), (r) => (failed++ ? r.continue() : r.abort('internetdisconnected')));
  await menuItem(alice, 'Personas');
  await expect(alice.locator('#personas #persona-template option', { hasText: 'Secretary' })).toHaveCount(1);
  expect(failed).toBeGreaterThanOrEqual(2);
});
