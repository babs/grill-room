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

    // One toggle (005-AC49): pressed while deafened, pressed again to hear the room.
    const deafen = { click: () => audioItem(bob, 'Mute the room') };
    const deaf = bob.locator('#voice-deaf');
    await deafen.click();
    await expect(deaf).toHaveAttribute('aria-pressed', 'true');
    await expect(deaf).toHaveAttribute('data-tip', /Press to hear them again/);
    await expect(bobRow).toHaveClass(/deaf/);
    await expect(state).toHaveAttribute('data-state', 'muted');
    await expect.poll(() => bob.evaluate(() => [...voice.remoteParticipants.values()]
      .flatMap((p) => [...p.audioTrackPublications.values()]).some((pub) => pub.isSubscribed))).toBe(false);

    await deafen.click();
    await expect(deaf).toHaveAttribute('aria-pressed', 'false');
    await expect(bobRow).not.toHaveClass(/deaf/);
    await expect(state).toHaveAttribute('data-state', 'live');
    const before = await inboundAudio(bob);
    await expect.poll(() => inboundAudio(bob), { timeout: 30_000 }).toBeGreaterThan(before);

    // Undeafen restores the mic as it was: muted before, muted after.
    await audioItem(bob, 'Mute my mic');
    await deafen.click();
    await deafen.click();
    await expect(state).toHaveAttribute('data-state', 'muted');
    await audioItem(bob, 'Mute my mic');

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
    await expect(deaf).toHaveAttribute('aria-pressed', 'true');
    await expect(bobRow).toHaveClass(/deaf/);
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    await browser.close();
  }
});
