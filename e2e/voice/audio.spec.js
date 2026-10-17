import { test, expect } from '@playwright/test';
import { createRoom, invite, join, joinAudio, inboundAudio, launch, admin, fixture, audioItem } from './helpers.js';
import { personMenu } from '../helpers.js';

// 001-AC6: two participants in a room hear each other, through LiveKit behind traefik. Two browser
// profiles: Alice's microphone plays recorded speech, Bob's a beep.
test('two participants exchange audio', async ({ request }) => {
  const aliceBrowser = await launch(fixture.wav);
  const bobBrowser = await launch();
  try {
    const room = await createRoom(request, 'Voice room');
    const aliceInv = await invite(request, room.id);
    const alice = await join(aliceBrowser, aliceInv, 'Alice');
    const bob = await join(bobBrowser, await invite(request, room.id), 'Bob');
    await joinAudio(alice);
    await joinAudio(bob);

    const aliceID = await alice.evaluate(() => myID);
    const aliceRow = bob.locator(`#participants li[data-pid="${aliceID}"]`);
    await expect(aliceRow).toHaveClass(/in-voice/);
    await expect((await personMenu(bob, 'Alice')).getByRole('slider', { name: 'Volume' })).toBeVisible();
    await bob.keyboard.press('Escape');

    // Audio flows both ways and keeps flowing.
    for (const page of [alice, bob]) {
      await expect.poll(() => inboundAudio(page), { timeout: 30_000 }).toBeGreaterThan(0);
      const before = await inboundAudio(page);
      await expect.poll(() => inboundAudio(page), { timeout: 30_000 }).toBeGreaterThan(before + 2000);
    }
    // Alice's recorded speech starts a few seconds in: Bob sees her speaking indicator light up.
    await expect(aliceRow).toHaveClass(/speaking/, { timeout: 20_000 });

    await alice.getByRole('button', { name: 'Mute' }).click();
    await expect(aliceRow).toHaveClass(/muted/);
    await expect(aliceRow).not.toHaveClass(/speaking/);
    await alice.getByRole('button', { name: 'Unmute' }).click();
    await expect(aliceRow).not.toHaveClass(/muted/);

    // Revoking Alice's invite ends her session and takes her out of Bob's audio.
    expect((await request.delete(`/api/rooms/${room.id}/invites/${aliceInv.id}?cut=1`, { headers: admin })).status()).toBe(200);
    await expect(alice.locator('#banner')).toHaveText('Your access to this room has ended.');
    await expect.poll(() => bob.evaluate((id) => voice.remoteParticipants.has(id), aliceID)).toBe(false);

    await audioItem(bob, 'Leave audio');
    await expect(bob.getByRole('button', { name: 'Join audio' })).toBeVisible();
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    await aliceBrowser.close();
    await bobBrowser.close();
  }
});
