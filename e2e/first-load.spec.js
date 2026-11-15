import { test, expect } from './fixtures.js';
import { roomWith } from './helpers.js';

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

// A read the browser drops once, here the persona picker's library, is retried: the picker lists it.
test('a dropped read is retried', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  let failed = 0;
  await alice.route(new RegExp(`/api/rooms/${room.id}/templates$`), (r) => (failed++ ? r.continue() : r.abort('internetdisconnected')));
  // "+" reads the library once: only the retry can fill the picker.
  await alice.getByRole('button', { name: 'Add a persona', exact: true }).click();
  await expect(alice.locator('#persona-picker').getByRole('radio', { name: /^Secretary, v/ })).toHaveCount(1);
  expect(failed).toBeGreaterThanOrEqual(2);
});
