import { test, expect } from '@playwright/test';
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
