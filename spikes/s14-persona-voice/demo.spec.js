import { test, expect, chromium } from '@playwright/test';
import { readFileSync, writeFileSync } from 'node:fs';

// Spike S14 demo: two personas speak in the call through the voice bot, a human cuts one off, and
// the STT worker transcribes the human only. Needs the loopback stack spikes/S14-persona-voice.md starts: a server with
// GRILL_ROOM_PERSONA_VOICE=on, LiveKit, an STT worker and voicebot.py, all on loopback.
const env = (k) => {
  if (!process.env[k]) throw new Error(`${k} is not set`);
  return process.env[k];
};
const base = env('S14_BASE');
const admin = { Authorization: `Bearer ${env('S14_ADMIN_TOKEN')}` };
const json = { 'Content-Type': 'application/json' };

const lines = {
  Grill: "J'ai ajouté une section sur l'idempotence à la spec de paiement. Stripe ne garantit pas l'unicité des événements. "
    + "On dédoublonne donc sur l'identifiant de l'événement, gardé sept jours. Au sujet de la note d'Alice sur le budget, "
    + "j'ai répondu que les trois cents euros couvrent les trois environnements. Bob, que fait-on si la commande est déjà annulée ?",
  Secretary: 'I recorded two decisions in the minutes. Releases move to Thursdays, and the staging freeze is dropped. '
    + 'Raj writes the rollback runbook by the twenty-fourth.',
};

function launch(wav) {
  return chromium.launch({ channel: 'chrome', args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
    '--autoplay-policy=no-user-gesture-required', `--use-file-for-fake-audio-capture=${wav}%noloop`] });
}

async function join(browser, roomID, name) {
  const inv = await (await fetch(`${base}/api/rooms/${roomID}/invites`, { method: 'POST', headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill(name);
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText(name);
  return page;
}

const joinAudio = async (page) => {
  await page.getByRole('button', { name: 'Join audio' }).click();
  await expect(page.getByRole('button', { name: 'Audio options' })).toBeVisible();
};

// fakeDaemon pairs a host daemon that answers every turn with the persona's canned line as
// spoken.md: the turn result path is the real one, only claude is left out.
async function fakeDaemon(page, roomID) {
  const room = `${base}/api/rooms/${roomID}`;
  const p = await (await fetch(`${room}/pairings`, { method: 'POST', headers: json, body: JSON.stringify({ name: 's14' }) })).json();
  expect(await page.evaluate(([r, id]) => fetch(`/api/rooms/${r}/pairings/${id}`, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ approve: true }) }).then((x) => x.status), [roomID, p.id])).toBe(200);
  const { token } = await (await fetch(`${base}/api/pairings/${p.id}`, { headers: { Authorization: 'Bearer ' + p.secret } })).json();
  const auth = { Authorization: 'Bearer ' + token };
  const stop = new AbortController();
  const prompts = [];
  const sessions = {};
  const events = await fetch(`${room}/daemon/events`, { headers: auth, signal: stop.signal });
  (async () => {
    const dec = new TextDecoder();
    let buf = '';
    for await (const chunk of events.body) {
      buf += dec.decode(chunk);
      for (let i; (i = buf.indexOf('\n\n')) >= 0; buf = buf.slice(i + 2)) {
        const ev = buf.slice(0, i);
        if (!ev.includes('event: turn')) continue;
        const id = JSON.parse(ev.split('data: ')[1]).id;
        const job = await (await fetch(`${room}/daemon/turns/${id}`, { headers: auth })).json();
        prompts.push(job.prompt);
        sessions[job.persona] ??= crypto.randomUUID();
        const res = await fetch(`${room}/daemon/turns/${id}`, { method: 'POST', headers: { ...auth, ...json }, body: JSON.stringify({
          synthesis: `${job.persona}: see the call.`, session_id: sessions[job.persona], session_offset: 0, session: btoa('s14\n'),
          changes: [{ path: 'spoken.md', content: btoa(String.fromCharCode(...new TextEncoder().encode(lines[job.persona]))) }],
        }) });
        expect(res.status).toBe(200);
      }
    }
  })().catch(() => {});
  expect((await fetch(`${room}/daemon/status`, { method: 'POST', headers: { ...auth, ...json }, body: JSON.stringify({ can_host: true }) })).status).toBe(204);
  return { stop: () => stop.abort(), prompts };
}

// record mixes every remote audio track Alice hears into one recording, the way she hears it.
const startRecording = (page) => page.evaluate(() => {
  const ctx = new AudioContext();
  const dest = ctx.createMediaStreamDestination();
  const seen = new Set();
  const add = () => {
    for (const p of voice.remoteParticipants.values()) {
      for (const pub of p.audioTrackPublications.values()) {
        const t = pub.track?.mediaStreamTrack;
        if (t && !seen.has(t.id)) {
          seen.add(t.id);
          ctx.createMediaStreamSource(new MediaStream([t])).connect(dest);
        }
      }
    }
  };
  add();
  window.s14Timer = setInterval(add, 200);
  window.s14Chunks = [];
  window.s14Rec = new MediaRecorder(dest.stream, { mimeType: 'audio/webm;codecs=opus' });
  window.s14Rec.ondataavailable = (e) => window.s14Chunks.push(e.data);
  window.s14Rec.start(1000);
});

const stopRecording = (page) => page.evaluate(async () => {
  clearInterval(window.s14Timer);
  await new Promise((r) => { window.s14Rec.onstop = r; window.s14Rec.stop(); });
  const buf = new Uint8Array(await new Blob(window.s14Chunks, { type: 'audio/webm' }).arrayBuffer());
  let s = '';
  for (let i = 0; i < buf.length; i += 0x8000) s += String.fromCharCode(...buf.subarray(i, i + 0x8000));
  return btoa(s);
});

// The bot's log lines about this room only: an earlier run's lines must not satisfy a check.
const botLog = (room) => () => readFileSync(env('S14_BOT_LOG'), 'utf8').split('\n').filter((l) => l.includes(room)).join('\n');

test('two personas speak, a human cuts in, the STT worker hears only the human', async () => {
  test.setTimeout(300_000);
  const room = await (await fetch(`${base}/api/rooms`, { method: 'POST', headers: { ...admin, ...json }, body: JSON.stringify({ name: 'S14 persona voice' }) })).json();
  const quiet = await launch(env('S14_SILENT_WAV'));
  const talker = await launch(env('S14_HUMAN_WAV'));
  try {
    const alice = await join(quiet, room.id, 'Alice');
    const bob = await join(talker, room.id, 'Bob');
    const daemon = await fakeDaemon(alice, room.id);
    const agent = async () => (await alice.request.get(`/api/rooms/${room.id}/agent`)).json();
    await expect.poll(async () => (await agent()).personas?.length ?? 0).toBeGreaterThan(0);
    const lib = await (await alice.request.get(`/api/rooms/${room.id}/templates`)).json();
    expect((await alice.request.post(`/api/rooms/${room.id}/personas`, { data: { template_id: lib.find((t) => t.preset === 'secretary').id } })).status()).toBe(201);
    const personas = (await agent()).personas;
    const id = (name) => personas.find((p) => p.name === name).id;
    await expect(alice.locator('button.persona[data-name="Secretary"] .led')).toHaveAttribute('aria-label', 'Secretary ready', { timeout: 30_000 });

    await joinAudio(alice);
    await startRecording(alice);
    const press = (name) => alice.request.post(`/api/rooms/${room.id}/turns`, { data: { persona_id: id(name) } });
    expect((await press('Grill')).status()).toBe(201);
    await expect.poll(() => daemon.prompts.length).toBe(1);
    expect(daemon.prompts[0]).toContain('Write spoken.md');
    // Queued behind Grill's line: one persona speaks at a time.
    await expect.poll(async () => (await press('Secretary')).status(), { timeout: 30_000 }).toBe(201);

    // Grill's bot joins Alice's call and talks; Bob joins then, and his mic's speech starts 2 s in.
    const grill = `persona-${id('Grill')}`;
    await expect.poll(() => alice.evaluate((g) => voice.remoteParticipants.get(g)?.isSpeaking ?? false, grill), { timeout: 60_000 }).toBe(true);
    await joinAudio(bob);
    await expect.poll(botLog(room.id), { timeout: 60_000 }).toMatch(/interrupted .*"Grill"/);
    await expect.poll(botLog(room.id), { timeout: 60_000 }).toMatch(/resumed .*"Grill"/);
    await expect.poll(botLog(room.id), { timeout: 120_000 }).toMatch(/spoken .*"Secretary"/);
    // The Secretary's line started after Grill's ended.
    const log = botLog(room.id)();
    expect(log.indexOf('spoken {"room": "' + room.id + '", "persona": "Grill"')).toBeLessThan(log.indexOf('speaking {"room": "' + room.id + '", "persona": "Secretary"'));

    // Alice can mute one persona's voice from the audio menu.
    await alice.getByRole('button', { name: 'Audio options' }).click();
    await expect(alice.locator('#voice-menu .persona-voice', { hasText: 'Hear Grill' })).toBeVisible();
    if (process.env.S14_SHOT) await alice.locator('#voice-menu').screenshot({ path: process.env.S14_SHOT });
    await alice.locator('#voice-menu .persona-voice', { hasText: 'Hear Grill' }).locator('input').uncheck();
    expect(await alice.evaluate((g) => [...document.querySelectorAll('#voice-audio audio')].filter((a) => a.dataset.pid === g).every((a) => a.volume === 0), grill)).toBe(true);
    await alice.keyboard.press('Escape');

    writeFileSync(env('S14_RECORDING'), Buffer.from(await stopRecording(alice), 'base64'));
    daemon.stop();
    // The STT worker transcribed Bob's line, and no persona's: each voice line in the chat is his.
    const voiceLines = alice.locator('#messages li.msg.voice');
    await expect(voiceLines.first()).toBeVisible({ timeout: 30_000 });
    expect(await voiceLines.allTextContents()).toEqual(expect.arrayContaining([expect.stringContaining('Bob')]));
    for (const t of await voiceLines.allTextContents()) expect(t).toContain('Bob');
  } finally {
    await quiet.close();
    await talker.close();
  }
});
