import { test, expect } from '@playwright/test';
import { createRoom, invite, join, joinAudio, inboundAudio, launch, admin, audioItem } from './helpers.js';

// 005-AC22: deafen stops Bob hearing Alice and mutes his mic; undeafen restores both. With "keep my
// mic on" his mic stays live while deafened. A reload in the same session comes back deafened.
// Alice sees each state in her list through the server's presence.
test('deafen', async ({ request }) => {
  const browser = await launch();
  try {
    const room = await createRoom(request, 'Deafen room');
    const alice = await join(browser, await invite(request, room.id), 'Alice');
    const bob = await join(browser, await invite(request, room.id), 'Bob');
    await joinAudio(alice);
    await joinAudio(bob);
    const bobID = await bob.evaluate(() => myID);
    const bobRow = alice.locator(`#participants li[data-pid="${bobID}"]`);
    const state = bobRow.locator('.audio');
    await expect(state).toHaveAttribute('data-state', 'live');
    await expect.poll(() => inboundAudio(bob), { timeout: 30_000 }).toBeGreaterThan(0);

    const deafen = { click: () => audioItem(bob, 'Mute the room') };
    await deafen.click();
    await expect(bob.locator('#voice-deaf')).toHaveText('Hear the room again');
    await expect(bob.locator('#voice-deaf')).toHaveAttribute('aria-pressed', 'true');
    await expect(bobRow).toHaveClass(/deaf/);
    await expect(state).toHaveAttribute('data-state', 'muted');
    await expect.poll(() => bob.evaluate(() => [...voice.remoteParticipants.values()]
      .flatMap((p) => [...p.audioTrackPublications.values()]).some((pub) => pub.isSubscribed))).toBe(false);

    await audioItem(bob, 'Hear the room again');
    await expect(bobRow).not.toHaveClass(/deaf/);
    await expect(state).toHaveAttribute('data-state', 'live');
    const before = await inboundAudio(bob);
    await expect.poll(() => inboundAudio(bob), { timeout: 30_000 }).toBeGreaterThan(before);

    // Undeafen restores the mic as it was: muted before, muted after.
    await bob.getByRole('button', { name: 'Mute' }).click();
    await deafen.click();
    await audioItem(bob, 'Hear the room again');
    await expect(state).toHaveAttribute('data-state', 'muted');
    await bob.getByRole('button', { name: 'Unmute' }).click();

    // Keep my mic on: deafened, still live, so still transcribed.
    await bob.getByRole('button', { name: 'Audio options' }).click();
    await bob.getByLabel('Keep my mic on while the room is muted').check();
    await bob.keyboard.press('Escape');
    await deafen.click();
    await expect(bobRow).toHaveClass(/deaf/);
    await expect(state).toHaveAttribute('data-state', 'live');

    // A reload comes back deafened once the audio is joined again.
    await bob.reload();
    await joinAudio(bob);
    await expect(bob.locator('#voice-deaf')).toHaveText('Hear the room again');
    await expect(bobRow).toHaveClass(/deaf/);
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    await browser.close();
  }
});
