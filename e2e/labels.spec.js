import { test, expect } from '@playwright/test';
import { roomWith } from './helpers.js';

// 005-AC9: the personas panel offers explicit models and efforts only, never a default, and says
// what a persona runs on in full words.
test('persona panel: no default model or effort', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.getByRole('button', { name: 'Personas' }).click();
  const grill = alice.locator('#personas .persona-form', { hasText: 'Grill' });
  await expect(grill.getByLabel('Model').locator('option')).toHaveText(['haiku', 'sonnet', 'opus', 'fable']);
  await expect(grill.getByLabel('Effort').locator('option')).toHaveText(['low', 'medium', 'high', 'xhigh', 'max']);
  await expect(grill.getByLabel('Model')).toHaveValue('sonnet');
  await expect(grill.getByLabel('Effort')).toHaveValue('medium');
  await expect(grill.locator('.hint', { hasText: 'Runs on' })).toContainText('Runs on sonnet · effort medium.');
  await expect(alice.locator('#personas')).not.toContainText('default');
});

// 005-AC8: the room counts the voices the STT worker transcribes, and tells a viewer only about
// their own voice. The stt event is played to the page as the server would send it.
test('transcription label counts voices', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const me = async (page) => (await page.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id)).me.id;
  const aliceID = await me(alice);
  const play = async (page, st) => {
    await page.route(`**/api/rooms/${room.id}/events*`, (r) => r.fulfill({
      status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: `event: stt\ndata: ${JSON.stringify(st)}\n\n`,
    }));
    await page.reload();
  };
  const label = (page) => page.locator('#stt-status');
  await play(alice, { available: true, listening: true, transcribed: [aliceID, aliceID + 1000] });
  await expect(label(alice)).toHaveText('2 voices transcribed · your voice is being transcribed');
  await play(bob, { available: true, listening: true, transcribed: [aliceID] });
  await expect(label(bob)).toHaveText('1 voice transcribed');
  await play(bob, { available: true, listening: true, transcribed: [] });
  await expect(label(bob)).toHaveText('');
  await play(bob, { available: false, listening: false, transcribed: [] });
  await expect(label(bob)).toHaveText('Transcription unavailable');
});
