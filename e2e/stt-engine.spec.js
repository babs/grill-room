import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, menuItem, playSTT, playEvents, patchJSON } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// 007-AC14: the glossary works only while a worker reads it; the stored glossary stays. 038-AC12:
// without one, the tab stays and says "Not available on this server". 007-AC13: the audio menu credits parakeet-ultra while a worker runs it.
test('glossary tab and model credit follow the workers', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const me = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  const tabs = (page) => page.locator('#manage [id^="tab-"]');

  // A worker that reads it: every member gets the tab, and saves a glossary.
  await playSTT(bob, room.id, { engines: ['whisper'], glossary: true });
  await menuItem(bob, 'Room settings');
  await bob.locator('#manage #tab-glossary').click();
  // Loaded first: a fill before the glossary arrives is overwritten by it.
  await expect(bob.locator('#glossary-used')).toHaveText('3 / ~70 terms used');
  await bob.locator('#stt-glossary').getByLabel('Words to recognise').fill('Kubernetes, Helm');
  await bob.locator('#stt-glossary').getByRole('button', { name: 'Save', exact: true }).click();
  await expect(bob.locator('#manage .said[role="status"]')).toHaveText('Saved: it applies from the next utterance.');
  // The credit sits in the audio menu, which needs audio: check its state, not its pixels.
  await expect(bob.locator('#stt-credit')).toHaveAttribute('hidden', '');

  // parakeet reads none: the tab says so for everyone, and the credit. A plain member keeps Room settings
  // for their invites (011-AC27), the plugins (009-AC5), the websites and connectors (014), the links (023-AC1)
  // and the glossary's place (038-AC12).
  for (const st of [{ engines: ['parakeet'] }, { engines: [], available: false }]) {
    await playSTT(bob, room.id, st);
    await expect(bob.locator('#room-menu #manage-open')).not.toHaveAttribute('hidden');
    await bob.goto(`/r/${room.id}#manage/glossary`);
    await expect(bob.locator('#manage #tab-glossary')).toHaveAttribute('aria-current', 'page');
    await expect(tabs(bob)).toHaveCount(5);
    await expect(bob.locator('#stt-glossary')).toHaveCount(0);
    await expect(bob.locator('#manage .unavailable')).toHaveText('Not available on this server');
    if (st.engines.includes('parakeet')) await expect(bob.locator('#stt-credit')).not.toHaveAttribute('hidden');
    else await expect(bob.locator('#stt-credit')).toHaveAttribute('hidden', '');

    await playSTT(alice, room.id, st);
    await alice.goto(`/r/${room.id}#manage/glossary`);
    await expect(alice.locator('#manage #room-panel')).toBeVisible();
    await expect(tabs(alice)).toHaveCount(12);
    await expect(alice.locator('#tab-glossary')).toHaveAttribute('aria-current', 'page');
    await expect(alice.locator('#manage .unavailable')).toHaveText('Not available on this server');
    // The e2e server has no speech: Call voices keeps its tab and says so.
    await alice.goto(`/r/${room.id}#manage/voices`);
    await expect(alice.locator('#tab-voices')).toHaveAttribute('aria-current', 'page');
    await expect(alice.locator('#manage .unavailable')).toHaveText('Not available on this server');
    await alice.goto(`/r/${room.id}`);
  }
  // 038-AC12: on a server with voice, the tab waits for the stream's state, then says the worker is
  // down in 007's words; "Not available" is for a server without transcription.
  await patchJSON(bob, `**/api/rooms/${room.id}`, (json) => ({ ...json, voice: true }));
  await playEvents(bob, room.id, []);
  await bob.goto(`/r/${room.id}#manage/glossary`);
  await expect(bob.locator('#manage .tab-panel')).toHaveText('Loading…');
  await playSTT(bob, room.id, { available: false });
  await bob.goto(`/r/${room.id}#manage/glossary`);
  await expect(bob.locator('#manage .unavailable')).toHaveText('Voice-to-text is off');
  await playSTT(bob, room.id, { engines: ['parakeet'] });
  await bob.goto(`/r/${room.id}#manage/glossary`);
  await expect(bob.locator('#manage .unavailable')).toHaveText('Not available on this server');
  // The data stayed: with a reading worker back, the glossary is there.
  await playSTT(alice, room.id, { engines: ['parakeet', 'whisper'], glossary: true });
  await alice.goto(`/r/${room.id}#manage/glossary`);
  await expect(tabs(alice)).toHaveCount(12);
  await expect(alice.locator('#stt-glossary').getByLabel('Words to recognise')).toHaveValue('Kubernetes, Helm');
  expect(await bob.evaluate((id) => fetch(`/api/rooms/${id}/glossary`).then((r) => r.json()), room.id)).toMatchObject({ glossary: 'Kubernetes, Helm' });
});
