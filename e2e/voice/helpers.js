import { expect, chromium } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { snoozePush, snoozeKey } from '../quiet-push.js';

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
// init, if given, is a script the context runs in every page before the room's own.
export async function join(browser, inv, name, init) {
  const ctx = await browser.newContext();
  await ctx.addInitScript(snoozePush, snoozeKey);
  if (init) await ctx.addInitScript(init);
  const page = await ctx.newPage();
  const url = new URL(inv.url);
  await page.goto(url.pathname + url.hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

export async function joinAudio(page) {
  await page.getByRole('button', { name: 'Join audio' }).click();
  await expect(page.getByRole('button', { name: 'Audio options' })).toBeVisible();
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

// dropProfiles deletes the profiles of pages, which joining gave one each: deleting a room keeps
// its participants' profiles, so a run against a long-lived deployment would leave them behind.
export async function dropProfiles(request, pages) {
  const ids = [];
  for (const page of pages) {
    const me = await page.request.get('/api/profile').then((r) => r.json()).catch(() => null);
    if (me?.profile?.id) ids.push(me.profile.id);
  }
  expect(ids.length, 'a profile per page').toBe(pages.length);
  for (const id of ids) await request.delete(`/api/profiles/${id}`, { headers: admin });
  const left = (await (await request.get('/api/profiles', { headers: admin })).json()).filter((p) => ids.includes(p.id));
  expect(left, 'profiles left behind').toEqual([]);
}

// audioItem picks an item of the audio menu (005-AC32): Mute the room, Hear the room again, Leave audio.
export async function audioItem(page, name) {
  await page.getByRole('button', { name: 'Audio options' }).click();
  await page.locator('#voice-menu').getByRole('button', { name, exact: true }).click();
}
