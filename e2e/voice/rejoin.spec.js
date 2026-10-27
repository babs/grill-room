import { test, expect } from '@playwright/test';
import { createRoom, invite, join, joinAudio, launch, admin, fixture, audioItem } from './helpers.js';

// 005-AC18: everyone leaves the audio, LiveKit closes the empty room, Alice comes back: the STT
// worker joins the new LiveKit room of the same name and her lines resume, with no restart.
test('transcription resumes after LiveKit closes the empty room', async ({ request }) => {
  test.setTimeout(300_000);
  const browser = await launch(fixture.wav);
  try {
    const room = await createRoom(request, 'Rejoin room');
    const alice = await join(browser, await invite(request, room.id), 'Alice');
    const label = alice.locator('#stt-status');
    const lines = alice.locator('.msg.voice');
    // At the default 1280 px, voice lines show in the Transcription pane (020-AC8).
    await joinAudio(alice);
    await expect(label).toHaveText(/^1 voice is being written down/, { timeout: 60_000 });
    await expect(lines.first()).toBeVisible({ timeout: 60_000 });

    await audioItem(alice, 'Leave audio');
    await expect(label).toHaveText('', { timeout: 60_000 });
    // Past LiveKit's departure timeout (20 s by default): the room is closed. LiveKit runs in its
    // own container with no clock to move: this waits for it, it asserts nothing.
    await alice.waitForTimeout(45_000);
    const before = await lines.count();

    await joinAudio(alice);
    await expect(label).toHaveText(/^1 voice is being written down/, { timeout: 60_000 });
    await expect.poll(() => lines.count(), { timeout: 90_000 }).toBeGreaterThan(before);
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    await browser.close();
  }
});
