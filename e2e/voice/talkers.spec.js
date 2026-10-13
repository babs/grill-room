import { test, expect } from '@playwright/test';
import { createRoom, invite, join, joinAudio, launch, admin, fixture, dropProfiles } from './helpers.js';

// LINE_WITHIN_S is the AC18 bound, 5 s with parakeet; the rollback drill passes 10 for whisper.
const within = Number(process.env.LINE_WITHIN_S || 5) * 1000;
const words = (s) => new Set(s.toLowerCase().replaceAll('’', "'").match(/[\p{L}\p{N}']+/gu) ?? []);
const overlap = (a, b) => [...a].filter((w) => b.has(w)).length;

// 007-AC18: two people speak at once, each from a fake microphone playing the FR/EN fixture: both
// get every sentence as their own line, each within LINE_WITHIN_S of the end of speech. A listener
// reads them. The latency counts from the instant before the microphones open, so it can only be
// overstated.
test('two people speaking at once both get their lines', async ({ request }) => {
  const browsers = [await launch(), await launch(fixture.wav), await launch(fixture.wav)];
  const pages = [];
  try {
    const room = await createRoom(request, 'Talkers room');
    const [bob, alice, carol] = [
      await join(browsers[0], await invite(request, room.id), 'Bob'),
      await join(browsers[1], await invite(request, room.id), 'Alice'),
      await join(browsers[2], await invite(request, room.id), 'Carol'),
    ];
    pages.push(bob, alice, carol);
    await joinAudio(bob);
    const start = Date.now();
    await Promise.all([joinAudio(alice), joinAudio(carol)]);

    const refs = fixture.utterances.map((u) => ({ ...u, words: words(u.text) }));
    const lines = () => bob.locator('.msg.voice').evaluateAll((els) => els.map((li) => ({
      sender: li.querySelector('strong').textContent,
      body: li.querySelector('.body').textContent,
      ts: Date.parse(li.querySelector('time').dateTime),
    })));
    const last = refs.at(-1);
    for (const who of ['Alice', 'Carol']) {
      await expect.poll(async () => (await lines()).filter((l) => l.sender === who && overlap(words(l.body), last.words) > 0).length,
        { timeout: (last.end + 20) * 1000 }).toBeGreaterThan(0);
    }
    const got = await lines();
    console.log(JSON.stringify(got.map((l) => ({ ...l, ts: l.ts - start })), null, 1));
    for (const who of ['Alice', 'Carol']) {
      const mine = got.filter((l) => l.sender === who);
      refs.forEach((r) => {
        const hits = mine.filter((l) => overlap(words(l.body), r.words) / words(l.body).size >= 0.5);
        expect(hits.length, `${who}: no line for "${r.text}"`).toBeGreaterThan(0);
        const latency = Math.max(...hits.map((l) => l.ts)) - (start + r.end * 1000);
        console.log(`${who} ${r.lang}: line ${latency} ms after the end of speech`);
        expect(latency, `${who}: "${r.text}"`).toBeLessThanOrEqual(within);
      });
    }
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    await dropProfiles(request, pages);
  } finally {
    for (const b of browsers) await b.close();
  }
});
