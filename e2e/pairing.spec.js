import { test, expect } from './fixtures.js';
import { roomWith } from './helpers.js';

// 001-AC22: a pairing request is approved with one click in the participant's room session; the
// daemon's poll then receives its token, which the page never shows.
test('approve a daemon pairing in the room', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const res = await request.post(`/api/rooms/${room.id}/pairings`, { data: { name: 'alice-laptop' } });
  expect(res.status()).toBe(201);
  const pairing = await res.json();
  const poll = async () => (await request.get(`/api/pairings/${pairing.id}`, { headers: { Authorization: `Bearer ${pairing.secret}` } })).json();
  expect((await poll()).status).toBe('pending');

  await alice.goto(new URL(pairing.approve_url).pathname + new URL(pairing.approve_url).search);
  // 003-AC17: the name a daemon sends, a machine name before spec 003, never reaches the room.
  await expect(alice.locator('#pairing-text')).toContainText('An agent daemon asks to run the agent for you');
  await expect(alice.locator('body')).not.toContainText('alice-laptop');
  await expect(alice.locator('#pairing-text')).toContainText(pairing.code);
  await expect(alice).toHaveURL(`/r/${room.id}`);
  // 008-AC17: one of Alice's apps is already connected; the notice waits for one more.
  await alice.evaluate(() => { agent.daemons = [{ participant_id: Number(myID), can_host: true }]; renderAgent(); });
  await alice.getByRole('button', { name: 'Approve' }).click();
  await expect(alice.locator('#pairing-text')).toHaveText('Daemon paired. Its page shows it connected within a few seconds.');
  await expect(alice.locator('#pairing')).toBeVisible();

  const answer = await poll();
  expect(answer.status).toBe('approved');
  expect(answer.token).toBeTruthy();
  await expect(alice.locator('body')).not.toContainText(answer.token);
  // The approved app connects: the notice goes. A new one is dismissed with ✕.
  await alice.evaluate(() => { agent.daemons = [...agent.daemons, { participant_id: Number(myID), can_host: true }]; renderAgent(); });
  await expect(alice.locator('#pairing')).toBeHidden();
  await alice.evaluate(() => { $('pairing').hidden = false; });
  await alice.locator('#pairing').getByRole('button', { name: 'Close' }).click();
  await expect(alice.locator('#pairing')).toBeHidden();
});

test('a stale pairing link says so', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.goto(`/r/${room.id}?pair=nope`);
  await expect(alice.locator('#pairing-text')).toHaveText('this pairing request expired or was already answered');
  await expect(alice.getByRole('button', { name: 'Approve' })).toBeHidden();
});
