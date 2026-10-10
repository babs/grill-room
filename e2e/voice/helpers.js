import { expect, chromium } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// FR/EN read speech from FLEURS (CC-BY-4.0): source, license and timings are in fr-en.json.
export const fixture = {
  wav: fileURLToPath(new URL('fixtures/fr-en.wav', import.meta.url)),
  ...JSON.parse(readFileSync(new URL('fixtures/fr-en.json', import.meta.url))),
};

export const admin = { Authorization: `Bearer ${process.env.GRILL_ROOM_ADMIN_TOKEN}` };

// launch starts the installed Chrome with a fake microphone: a beep, or the WAV at audioFile.
export function launch(audioFile) {
  const args = ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'];
  // %noloop: the file plays once, so each utterance has one known end time.
  if (audioFile) args.push(`--use-file-for-fake-audio-capture=${audioFile}%noloop`);
  return chromium.launch({ channel: 'chrome', args });
}

export async function createRoom(request, name) {
  const res = await request.post('/api/rooms', { headers: admin, data: { name } });
  expect(res.status()).toBe(201);
  return res.json();
}

export async function invite(request, roomID) {
  const res = await request.post(`/api/rooms/${roomID}/invites`, { headers: admin });
  expect(res.status()).toBe(201);
  return res.json();
}

// join opens the invite in a fresh context of browser (its own cookie jar) and enters the room.
export async function join(browser, inv, name) {
  const page = await (await browser.newContext()).newPage();
  const url = new URL(inv.url);
  await page.goto(url.pathname + url.hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

export async function joinAudio(page) {
  await page.getByRole('button', { name: 'Join audio' }).click();
  await expect(page.getByRole('button', { name: 'Leave audio' })).toBeVisible();
}

// inboundAudio sums bytesReceived over the page's subscribed audio tracks, from getStats().
export const inboundAudio = (page) => page.evaluate(async () => {
  let bytes = 0;
  for (const p of voice.remoteParticipants.values()) {
    for (const pub of p.audioTrackPublications.values()) {
      const report = await pub.track?.getRTCStatsReport();
      for (const s of report?.values() ?? []) if (s.type === 'inbound-rtp' && s.kind === 'audio') bytes += s.bytesReceived;
    }
  }
  return bytes;
});
