import { test, expect } from '@playwright/test';
import { execFileSync, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, openSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { join as joinPath } from 'node:path';
import { createRoom, invite, join, joinAudio, launch, admin, dropProfiles } from '../../e2e/voice/helpers.js';
import { upload, pairDaemon, logLines, messagesRequests, removeSandbox, agentState } from '../../e2e/helpers.js';

// 002-AC41, spike S6: the S5 session (voices, script, plan) with two personas. Grill runs on demand at
// opus/high, Secretary every 2 rounds at opus/medium. Turns: Grill (calls @Secretary), Secretary
// (the call), Grill with a shared skill (pressed while Secretary runs: queued), round 2, Grill,
// Secretary (every 2 rounds). Set S6_KEEP=1 to keep the room for reading the stored prompts.
const here = new URL('.', import.meta.url).pathname;
const s5 = joinPath(here, '..', 's5-simulated-session');
const repo = joinPath(here, '..', '..');
const scratch = joinPath(repo, '.scratchpad', 's6');
const audioDir = process.env.S6_AUDIO || joinPath(repo, '.scratchpad', 's5', 'audio');
const runDir = joinPath(scratch, 'run');
const micDir = joinPath(runDir, 'mic');
const session = JSON.parse(readFileSync(joinPath(s5, 'session.json')));
const plan = readFileSync(joinPath(s5, 'plan.md'), 'utf8');
const names = Object.keys(session.speakers);
const host = names[0];
const maxRequests = 15;
const nudges = {
  all: 'Pour les agents : un minimum d\'étapes, lisez tout en parallèle puis écrivez en une seule fois.',
  t1: 'Grill : mets à jour la synthèse en une seule écriture, puis termine ta réponse par une ligne qui mentionne le Secretary (arobase puis son nom, en texte simple) pour qu\'il prenne le compte rendu.',
  t3: 'Grill : utilise ton skill s6-stamp maintenant, puis réponds en une phrase. Rien d\'autre à ce tour, et ne mentionne aucun autre agent.',
  t4: 'Grill : sans aucun outil, en trois phrases au plus, quel est le principal risque qui reste ? Ne mentionne aucun autre agent.',
};
// The skill prints a stamp the model cannot guess: only running its script gives it.
const stamp = 'STAMP-' + createHash('sha256').update('grill-room').digest('hex').slice(0, 12);

function stampZip() {
  const zip = joinPath(runDir, 's6-stamp.zip');
  const md = `---
name: s6-stamp
description: Writes the room's verification stamp into docs/stamp.md. Use whenever someone asks for the stamp.
---
Run this one command, with <base> being this skill's base directory, and nothing else:

    sh <base>/scripts/stamp.sh > /workspace/docs/stamp.md
`;
  execFileSync('python3', ['-I', '-c', `import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr("s6-stamp/SKILL.md", sys.argv[2])
    z.writestr("s6-stamp/scripts/stamp.sh", 'printf "# Stamp\\\\n\\\\nSTAMP-%s\\\\n" "$(printf grill-room | sha256sum | cut -c1-12)"\\n')`, zip, md]);
  return readFileSync(zip);
}

function point(name, round) {
  const link = joinPath(micDir, `${name}.wav`);
  rmSync(link, { force: true });
  symlinkSync(joinPath(audioDir, `${name}-r${round}.wav`), link);
}

// The fake microphone starts its file when capture opens: that instant is the round's time zero.
// The host page also records every chain count and queue line it shows.
function pageProbe() {
  const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (...args) => {
    const stream = await gum(...args);
    window.s5MicOpenedAt = Date.now();
    return stream;
  };
  window.s6Seen = [];
  document.addEventListener('DOMContentLoaded', () => {
    const watch = (id) => new MutationObserver(() => {
      const t = document.getElementById(id).textContent;
      if (t && window.s6Seen.at(-1)?.text !== t) window.s6Seen.push({ id, text: t, ts: Date.now() });
    }).observe(document.getElementById(id), { childList: true, subtree: true, characterData: true });
    watch('chain-count');
    watch('queue-list');
  });
}

// The real daemon with this machine's Claude Code login. No GRILL_ROOM_AGENT_MODEL: the personas
// choose; the host cap is opus/high, which both personas fit under.
function daemonFor(roomURL) {
  const dir = joinPath(scratch, 'daemon');
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const stateDir = joinPath(dir, 'state');
  const log = joinPath(dir, 'daemon.log');
  const proc = spawn(joinPath(repo, 'bin', 'grill-room-daemon'), [roomURL], {
    env: { ...process.env, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none', GRILL_ROOM_MAX_MODEL: 'opus', GRILL_ROOM_MAX_EFFORT: 'high', LOG_LEVEL: 'info' },
    stdio: ['ignore', 'ignore', openSync(log, 'w')],
  });
  const exited = new Promise((resolve) => proc.on('exit', resolve));
  const link = (async () => {
    for (let i = 0; i < 100; i++) {
      const m = readFileSync(log, 'utf8').match(/Admin page: (http:\/\/127\.0\.0\.1:\d+\/s\/\S+)/);
      if (m) return m[1];
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error('no admin link in ' + log);
  })();
  const sandbox = createHash('sha256').update(stateDir + '\0' + roomURL).digest('hex').slice(0, 12);
  return { proc, exited, link, log, sandbox, cache: joinPath(stateDir, 'workspace-cache') };
}

// watchCache notes when each file of the daemon's workspace cache first appears: a file appears
// when a turn fetches it, so the files new during a turn are what that turn transferred.
function watchCache(dir) {
  const seen = new Map();
  const scan = (d) => {
    let entries = [];
    try { entries = readdirSync(d, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const p = joinPath(d, e.name);
      if (e.isDirectory()) scan(p);
      else if (!seen.has(p)) {
        try { seen.set(p, { ts: Date.now(), size: statSync(p).size }); } catch { /* evicted meanwhile */ }
      }
    }
  };
  const timer = setInterval(() => scan(dir), 200);
  return { seen, stop: () => clearInterval(timer) };
}

const messages = (page) => page.locator('#messages li.msg').evaluateAll((els) => els.map((li) => ({
  kind: [...li.classList].find((c) => c !== 'msg'),
  sender: li.querySelector('strong')?.textContent,
  body: li.querySelector('.body')?.textContent,
  model: li.querySelector('.model')?.textContent,
  effort: li.querySelector('.effort')?.textContent,
  ts: Date.parse(li.querySelector('time')?.dateTime),
})));

async function playRound(pages, n) {
  const rnd = session.rounds[n - 1];
  for (const name of names) point(name, n);
  if (n === 1) {
    await Promise.all(names.map((name) => joinAudio(pages[name])));
  } else {
    for (const name of names) {
      await pages[name].getByRole('button', { name: 'Leave audio' }).click();
      await expect(pages[name].getByRole('button', { name: 'Join audio' })).toBeVisible();
      await joinAudio(pages[name]);
    }
  }
  const micAt = {};
  for (const name of names) micAt[name] = await pages[name].evaluate(() => window.s5MicOpenedAt);
  await expect(pages[host].locator('#stt-status')).toHaveText('Transcribing', { timeout: 30_000 });
  const lastEnd = Math.max(...rnd.lines.map((l) => l.end));
  await pages[host].waitForTimeout(Math.max(...Object.values(micAt)) + lastEnd * 1000 - Date.now());
  const voiceCount = () => pages[host].locator('#messages li.msg.voice').count();
  let count = await voiceCount();
  for (let quiet = Date.now(), cap = Date.now() + 120_000; Date.now() - quiet < 15_000 && Date.now() < cap;) {
    await pages[host].waitForTimeout(2000);
    const c = await voiceCount();
    if (c !== count) [count, quiet] = [c, Date.now()];
  }
  return { round: n, micAt, drainedAt: Date.now() };
}

const lines = (page, persona) => page.locator('#messages li.msg.agent').filter({ has: page.locator('.meta strong', { hasText: persona }) });

async function say(page, text) {
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
  await expect(page.locator('#messages')).toContainText(text.slice(0, 40));
}

// waitLine waits for persona's n-th synthesis and returns it with its meta line.
async function waitLine(page, persona, n, from) {
  await expect(lines(page, persona)).toHaveCount(n, { timeout: 900_000 });
  const li = lines(page, persona).nth(n - 1);
  return {
    persona, ms: Date.now() - from,
    who: await li.locator('.meta strong').textContent(),
    model: await li.locator('.model').textContent(),
    effort: await li.locator('.effort').textContent(),
    synthesis: await li.locator('.body').textContent(),
  };
}

async function editPersona(panel, id, fields) {
  const form = panel.locator(`.persona-form[data-id="${id}"]`);
  if (fields.model) await form.getByLabel('Model').selectOption(fields.model);
  if (fields.effort) await form.getByLabel('Effort').selectOption(fields.effort);
  if (fields.everyN) await form.getByLabel('Every N rounds').fill(String(fields.everyN));
  await form.getByRole('button', { name: 'Save' }).click();
  await expect(panel.locator('#personas-status')).toContainText('saved. It applies from its next turn.');
}

test('S6: two personas, a chain, a queued press and a shared skill', async ({ request, baseURL }) => {
  rmSync(runDir, { recursive: true, force: true });
  mkdirSync(micDir, { recursive: true });
  for (const name of names) point(name, 1);
  const browsers = {};
  for (const name of names) browsers[name] = await launch(joinPath(micDir, `${name}.wav`));
  const room = await createRoom(request, 'S6 dark mode grill');
  const api = `/api/rooms/${room.id}`;
  const daemon = daemonFor(`${baseURL}/r/${room.id}`);
  const cache = watchCache(daemon.cache);
  const result = { room: room.id, rounds: [], turns: [], agentTurns: [], lines: [] };
  let skillID = 0;
  const pages = {};
  try {
    for (const name of names) pages[name] = await join(browsers[name], await invite(request, room.id), name, pageProbe);
    const claire = pages[host];
    await upload(claire, 'plan.md', 'text/markdown', plan);
    await expect(claire.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Dark mode for the room UI');

    await pairDaemon(claire, daemon);
    await expect(claire.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 300_000 });

    // Personas, from the panel as a participant would set them.
    await claire.getByRole('button', { name: 'Personas' }).click();
    const panel = claire.locator('#personas');
    await panel.locator('#persona-template').selectOption({ label: '📝 Secretary v1' });
    await panel.getByRole('button', { name: 'Add', exact: true }).click();
    await expect(panel.locator('.persona-form')).toHaveCount(2);
    const st = await agentState(claire, room.id);
    const id = Object.fromEntries(st.personas.map((p) => [p.name, p.id]));
    await editPersona(panel, id.Grill, { model: 'opus', effort: 'high' });
    await editPersona(panel, id.Secretary, { model: 'opus', effort: 'medium', everyN: 2 });
    await panel.getByRole('button', { name: 'Close' }).click();
    await expect(claire.locator('#chain-count')).toHaveText('Chain 0/25');

    const up = await claire.request.post('/api/profile/skills?scope=shared', { headers: { 'Content-Type': 'application/zip' }, data: stampZip() });
    expect(up.status(), await up.text()).toBe(201);
    skillID = (await up.json()).id;

    result.rounds.push(await playRound(pages, 1));

    // T1 Grill, pressed; its synthesis calls @Secretary, a chain link (T2).
    await say(claire, nudges.all);
    await say(claire, nudges.t1);
    let pressed = Date.now();
    await claire.getByRole('button', { name: 'Ask Grill' }).click();
    result.lines.push(await waitLine(claire, 'Grill', 1, pressed));
    expect(result.lines[0].synthesis, 'Grill calls @Secretary').toMatch(/@secretary\b/i);
    await expect(claire.locator('#chain-count')).toHaveText('Chain 1/25');
    const t1At = Date.now();
    await expect.poll(async () => (await agentState(claire, room.id)).turn?.persona_name, { timeout: 60_000 }).toBe('Secretary');

    // While Secretary runs, Grill gets the shared skill and a press: the press waits in the queue.
    const on = await claire.request.put(`${api}/personas/${id.Grill}/skills/${skillID}`, { data: { accept_disclaimer: true } });
    expect(on.status(), await on.text()).toBe(200);
    await say(claire, nudges.t3);
    const queuedAt = Date.now();
    await claire.getByRole('button', { name: 'Ask Grill' }).click();
    await expect(claire.locator('#queue-list')).toContainText('Grill, asked by Claire');
    await expect(claire.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill queued, position 1');
    result.lines.push(await waitLine(claire, 'Secretary', 1, t1At));
    await expect.poll(async () => {
      const t = (await agentState(claire, room.id)).turn;
      return t?.persona_name === 'Grill' && t.status === 'running' ? Date.parse(t.started_at) : 0;
    }, { timeout: 60_000 }).toBeGreaterThan(0);
    result.queue = { pressedWhileBusy: queuedAt, startedAfterMs: Date.now() - queuedAt };
    result.lines.push(await waitLine(claire, 'Grill', 2, queuedAt));
    const docs = await (await claire.request.get(`${api}/documents`)).json();
    const stampDoc = docs.find((d) => d.name === 'stamp.md');
    expect(stampDoc, 'the skill wrote docs/stamp.md').toBeTruthy();
    expect(await (await claire.request.get(`${api}/documents/${stampDoc.id}/view`)).text()).toContain(stamp);
    const off = await claire.request.delete(`${api}/personas/${id.Grill}/skills/${skillID}`);
    expect(off.status(), await off.text()).toBe(200);

    // Round 2, then T4 Grill: a second round since Secretary's own turn, so Secretary runs (T5).
    result.rounds.push(await playRound(pages, 2));
    const left = maxRequests - messagesRequests(daemon);
    expect(left, 'budget for T4 and T5').toBeGreaterThanOrEqual(3);
    await say(claire, nudges.t4);
    pressed = Date.now();
    await claire.getByRole('button', { name: 'Ask Grill' }).click();
    result.lines.push(await waitLine(claire, 'Grill', 3, pressed));
    expect(result.lines.at(-1).synthesis).not.toMatch(/@secretary\b/i);
    await expect(claire.locator('#chain-count')).toHaveText('Chain 1/25');
    result.lines.push(await waitLine(claire, 'Secretary', 2, Date.now()));

    result.messages = await messages(claire);
    result.seen = await claire.evaluate(() => window.s6Seen);
    result.minutes = await claire.evaluate(async (id) => {
      const docs = await fetch(`/api/rooms/${id}/documents`).then((r) => r.json());
      const d = docs.find((x) => x.name === 'minutes.md');
      if (!d) return null;
      const html = await fetch(`/api/rooms/${id}/documents/${d.id}/view`).then((r) => r.text());
      return new DOMParser().parseFromString(html, 'text/html').body.innerText;
    }, room.id);

    for (const l of result.lines) {
      const effort = l.persona === 'Grill' ? 'effort high' : 'effort medium';
      expect(l.who, 'synthesis names its persona').toContain(l.persona);
      expect(l.model, `${l.persona} model`).toMatch(/opus/);
      expect(l.effort, `${l.persona} effort`).toBe(effort);
    }
    expect(result.minutes, 'Secretary wrote docs/minutes.md').toBeTruthy();
    // Secretary writes in the room's language: the headings may be French.
    for (const h of [/Decisions|Décisions/, /Actions/, /Open questions|Questions ouvertes/]) expect(result.minutes).toMatch(h);
  } finally {
    cache.stop();
    const log = logLines(daemon);
    const at = (l) => l.ts * 1000;
    const started = log.filter((l) => l.msg === 'turn_started');
    for (const [i, s] of started.entries()) {
      const done = log.find((l) => l.msg === 'turn_done' && l.turn === s.turn);
      const end = done ? at(done) : Infinity;
      // A file belongs to the last turn started before it appeared: a queued turn starts within
      // milliseconds of the previous one's end, so a window past turn_done would count it twice.
      const next = started[i + 1] ? at(started[i + 1]) : Infinity;
      const files = [...cache.seen.values()].filter((f) => f.ts >= at(s) && f.ts < next);
      result.agentTurns.push({
        turn: s.turn, persona: s.persona, resume: s.resume, ms: done ? Math.round(end - at(s)) : null, error: done?.error,
        requests: log.filter((l) => l.msg === 'upstream_response' && l.path.startsWith('/v1/messages') && at(l) >= at(s) && at(l) <= end).length,
        fetchedFiles: files.length, fetchedBytes: files.reduce((n, f) => n + f.size, 0),
      });
    }
    result.requests = messagesRequests(daemon);
    writeFileSync(joinPath(runDir, 'result.json'), JSON.stringify(result, null, 1) + '\n');
    daemon.proc.kill('SIGTERM');
    await daemon.exited;
    removeSandbox(daemon.sandbox);
    if (process.env.S6_KEEP !== '1') {
      await request.delete(`/api/rooms/${room.id}`, { headers: admin });
      if (skillID) await request.delete(`/api/skills/${skillID}`, { headers: admin });
    }
    await dropProfiles(request, Object.values(pages));
    for (const b of Object.values(browsers)) await b.close();
  }
  expect(result.requests, 'real Claude requests').toBeLessThanOrEqual(maxRequests);
});
