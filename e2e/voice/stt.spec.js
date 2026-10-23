import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createRoom, invite, join, joinAudio, inboundAudio, launch, admin, fixture, audioItem, composeSQL } from './helpers.js';

const repo = new URL('../../', import.meta.url).pathname;
const compose = (...args) => execFileSync('docker', ['compose', ...args], { cwd: repo, encoding: 'utf8' });

const words = (s) => new Set(s.toLowerCase().replaceAll('’', "'").match(/[\p{L}\p{N}']+/gu) ?? []);
const overlap = (a, b) => [...a].filter((w) => b.has(w)).length;

// voiceLines reads the voice lines on show: sender, text, and the time the server stored them.
const voiceLines = (page) => page.locator('.msg.voice').evaluateAll((els) => els.map((li) => ({
  sender: li.querySelector('strong').textContent,
  body: li.querySelector('.body').textContent,
  ts: Date.parse(li.querySelector('time').dateTime),
})));

// A listener in the audio, so the room is active and the worker joins it.
async function listener(browser, request, room) {
  const page = await join(browser, await invite(request, room.id), 'Bob');
  await joinAudio(page);
  await expect(page.locator('#stt-status')).toHaveText(/^\d+ voices? (is|are) being written down/, { timeout: 30_000 });
  return page;
}

// 001-AC7 001-AC8 001-AC9: Alice's recorded FR and EN sentences reach Bob as voice lines with her
// name, each in the language spoken and within 10 s of the end of the sentence; no audio is kept.
test('speech becomes voice lines in the language spoken', async ({ request }) => {
  const bobBrowser = await launch();
  const aliceBrowser = await launch(fixture.wav);
  try {
    const room = await createRoom(request, 'STT room');
    const bob = await listener(bobBrowser, request, room);
    const alice = await join(aliceBrowser, await invite(request, room.id), 'Alice');
    // The fixture starts playing when the page opens the microphone, after this instant: every
    // latency below is measured from a sentence end at least this early, so it can only be overstated.
    const start = Date.now();
    await joinAudio(alice);

    const refs = fixture.utterances.map((u) => ({ ...u, words: words(u.text) }));
    const last = refs.at(-1);
    await expect.poll(async () => (await voiceLines(bob)).filter((l) => overlap(words(l.body), last.words) > 0).length, {
      timeout: (last.end + 15) * 1000,
    }).toBeGreaterThan(0);
    const lines = await voiceLines(bob);
    console.log(JSON.stringify(lines.map((l) => ({ ...l, ts: l.ts - start })), null, 1));

    const byRef = refs.map(() => []);
    for (const l of lines) {
      expect(l.sender).toBe('Alice');
      const w = words(l.body);
      const scores = refs.map((r) => overlap(w, r.words) / w.size);
      const best = scores.indexOf(Math.max(...scores));
      // Most of the line's words come from one sentence: transcribed, not translated.
      expect(scores[best], `line "${l.body}"`).toBeGreaterThanOrEqual(0.5);
      byRef[best].push(l);
    }
    refs.forEach((r, i) => {
      const got = byRef[i];
      expect(got.length, `no line for the ${r.lang} sentence "${r.text}"`).toBeGreaterThan(0);
      const covered = new Set(got.flatMap((l) => [...words(l.body)]));
      expect(overlap(r.words, covered) / r.words.size, `${r.lang}: "${r.text}"`).toBeGreaterThanOrEqual(0.5);
      const latency = Math.max(...got.map((l) => l.ts)) - (start + r.end * 1000);
      console.log(`${r.lang} sentence ${i + 1}: line ${latency} ms after the end of speech`);
      expect(latency).toBeLessThanOrEqual(10_000);
    });

    // No audio file in the worker's container, and no audio blob in the room's database.
    expect(compose('exec', '-T', '-u', '0', 'stt', 'find', '/', '-xdev', '(', '-name', '*.wav', '-o', '-name', '*.opus', '-o', '-name', '*.pcm', ')')).toBe('');
    // RIFF (WAV), OggS (Opus), fLaC, ID3 (MP3), and WebM's EBML header, in any bytea column.
    const blobs = composeSQL(`
SELECT format($q$SELECT %L || ': ' || count(*) FROM %I.%I
  WHERE substr(%I, 1, 4) IN ('\\x52494646', '\\x4f676753', '\\x664c6143', '\\x1a45dfa3') OR substr(%I, 1, 3) = '\\x494433'
  HAVING count(*) > 0$q$, table_name || '.' || column_name, table_schema, table_name, column_name, column_name)
FROM information_schema.columns WHERE table_schema = 'public' AND data_type = 'bytea' ORDER BY 1 \\gexec
`).split('\n').filter(Boolean);
    expect(blobs).toEqual([]);
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    await aliceBrowser.close();
    await bobBrowser.close();
  }
});

// sttTracks reads the worker's grill_stt_tracks gauge: the audio tracks it listens to, all rooms.
const sttTracks = async () => {
  const res = await fetch(`http://127.0.0.1:${process.env.STT_METRICS_PORT || 7682}/metrics`);
  return Number((await res.text()).match(/^grill_stt_tracks (\S+)$/m)[1]);
};

// 001-AC7: LiveKit hands a left participant's freed transceiver to the next track it sends the
// worker. A listener kept open on the old track then transcribes the newcomer under the old name.
test('leaving and rejoining the audio keeps every line under its speaker', async ({ request }) => {
  const quietBrowser = await launch();
  const aliceBrowser = await launch(fixture.wav);
  try {
    const room = await createRoom(request, 'STT rejoin');
    const bob = await listener(quietBrowser, request, room);
    const carol = await join(quietBrowser, await invite(request, room.id), 'Carol');
    await joinAudio(carol);
    await expect.poll(sttTracks, { timeout: 15_000 }).toBe(2);
    // Carol's track goes away and its transceiver is free for the next one.
    await audioItem(carol, 'Leave audio');
    await expect.poll(sttTracks, { timeout: 15_000 }).toBe(1);

    const alice = await join(aliceBrowser, await invite(request, room.id), 'Alice');
    await joinAudio(alice);
    await expect.poll(sttTracks, { timeout: 15_000 }).toBe(2);
    const first = fixture.utterances[0];
    await expect.poll(async () => (await voiceLines(bob)).some((l) => overlap(words(l.body), words(first.text)) > 2), {
      timeout: (first.end + 15) * 1000,
    }).toBe(true);

    // Alice leaves and rejoins: her microphone restarts the fixture on a new track.
    await audioItem(alice, 'Leave audio');
    await expect.poll(sttTracks, { timeout: 15_000 }).toBe(1);
    await joinAudio(carol);
    await joinAudio(alice);
    await expect.poll(sttTracks, { timeout: 15_000 }).toBe(3);
    await expect.poll(async () => (await voiceLines(bob)).filter((l) => overlap(words(l.body), words(first.text)) > 2).length, {
      timeout: (first.end + 15) * 1000,
    }).toBe(2);
    // The second sentence lands too.
    await expect.poll(async () => (await voiceLines(bob)).length, { timeout: 30_000 }).toBeGreaterThan(2);
    const lines = await voiceLines(bob);
    console.log(JSON.stringify(lines, null, 1));
    expect(lines.length).toBeGreaterThan(2);
    for (const l of lines) expect(l.sender, `line "${l.body}"`).toBe('Alice');
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    await expect.poll(sttTracks, { timeout: 15_000 }).toBe(0);
  } finally {
    await aliceBrowser.close();
    await quietBrowser.close();
  }
});

// 010-AC23: Alice turns the transcription of her voice off: Bob sees her "transcribed" badge go, and
// none of her speech after it becomes a line; turned back on, the badge returns.
test('a participant turns the transcription of their voice off', async ({ request }) => {
  const bobBrowser = await launch();
  const aliceBrowser = await launch(fixture.wav);
  try {
    const room = await createRoom(request, 'STT opt-out');
    const bob = await listener(bobBrowser, request, room);
    const alice = await join(aliceBrowser, await invite(request, room.id), 'Alice');
    const start = Date.now();
    await joinAudio(alice);
    const aliceID = await alice.evaluate(() => myID);
    const badge = bob.locator(`#participants li[data-pid="${aliceID}"] .tag-transcribed`);
    await expect(badge).toHaveClass(/(^| )transcribed( |$)/, { timeout: 30_000 });

    await audioItem(alice, 'Transcribe my voice');
    await expect(alice.locator('#voice-stt')).toHaveAttribute('aria-pressed', 'false');
    const off = Date.now();
    await expect(badge).not.toHaveClass(/(^| )transcribed( |$)/, { timeout: 15_000 });
    // Every sentence of the fixture is spoken meanwhile: none lands after the switch.
    await bob.waitForTimeout(Math.max(0, start + (fixture.utterances.at(-1).end + 10) * 1000 - Date.now()));
    const late = (await voiceLines(bob)).filter((l) => l.ts > off + 1000);
    expect(late).toEqual([]);

    await audioItem(alice, 'Transcribe my voice');
    await expect(badge).toHaveClass(/(^| )transcribed( |$)/, { timeout: 15_000 });
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    await aliceBrowser.close();
    await bobBrowser.close();
  }
});

// 001-AC10: with the worker down the room shows it and keeps audio and text; lines resume after.
test('transcription down shows a banner, and lines resume when it is back', async ({ request }) => {
  const bobBrowser = await launch();
  let aliceBrowser;
  try {
    const room = await createRoom(request, 'STT outage');
    const bob = await listener(bobBrowser, request, room);
    const carol = await join(bobBrowser, await invite(request, room.id), 'Carol');
    await joinAudio(carol);

    compose('kill', 'stt');
    await expect(bob.locator('#stt-status')).toHaveText('Voice-to-text is off', { timeout: 30_000 });
    const before = await inboundAudio(bob);
    await expect.poll(() => inboundAudio(bob), { timeout: 30_000 }).toBeGreaterThan(before);
    await carol.getByLabel('Message').fill('still here');
    await carol.getByLabel('Message').press('Enter');
    await expect(bob.locator('.msg.text').last()).toContainText('still here');

    compose('start', 'stt');
    await expect(bob.locator('#stt-status')).toHaveText(/^\d+ voices? (is|are) being written down/, { timeout: 120_000 });
    aliceBrowser = await launch(fixture.wav);
    const alice = await join(aliceBrowser, await invite(request, room.id), 'Alice');
    await joinAudio(alice);
    const first = fixture.utterances[0];
    await expect.poll(async () => (await voiceLines(bob)).some((l) => l.sender === 'Alice' && overlap(words(l.body), words(first.text)) > 2), {
      timeout: (first.end + 15) * 1000,
    }).toBe(true);
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
  } finally {
    // Leave the worker running for the next test, even after a failure here.
    compose('start', 'stt');
    await aliceBrowser?.close();
    await bobBrowser.close();
  }
});
