import { test, expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, openSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join as joinPath } from 'node:path';
import { createRoom, invite, join, joinAudio, launch, admin } from '../../e2e/voice/helpers.js';
import { upload, annotateSelection, pairDaemon, messagesRequests, logLines, removeSandbox, agentState } from '../../e2e/helpers.js';

const here = new URL('.', import.meta.url).pathname;
const repo = joinPath(here, '..', '..');
const scratch = joinPath(repo, '.scratchpad', 's5');
const audioDir = process.env.S5_AUDIO || joinPath(scratch, 'audio');
const runDir = joinPath(scratch, 'run');
const micDir = joinPath(runDir, 'mic');
const session = JSON.parse(readFileSync(joinPath(here, 'session.json')));
const plan = readFileSync(joinPath(here, 'plan.md'), 'utf8');
const names = Object.keys(session.speakers);
const host = names[0];
// S5_AGENT=0 plays both rounds without a daemon: no Claude request.
const withAgent = process.env.S5_AGENT !== '0';
const model = process.env.S5_MODEL || 'opus';
const maxRequests = 14;
const nudges = [
  'Agent : fais-le en un minimum d\'étapes, lis tout en parallèle puis écris le plan révisé et les réponses en une fois.',
  'Agent : réponds à Emma sur son annotation et marque-la terminée si c\'est réglé, ouvre une annotation sur le plan pour ce qui reste à trancher. Un minimum d\'étapes, tout en une fois.',
];

// Each browser plays <name>.wav as its microphone; pointing the link at the next round's file and
// reopening the microphone plays that round, since Chrome reads the file each time capture starts.
function point(name, round) {
  const link = joinPath(micDir, `${name}.wav`);
  rmSync(link, { force: true });
  symlinkSync(joinPath(audioDir, `${name}-r${round}.wav`), link);
}

// The fake microphone starts its file when capture opens: that instant is the round's time zero.
function micClock() {
  const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia = async (...args) => {
    const stream = await gum(...args);
    window.s5MicOpenedAt = Date.now();
    return stream;
  };
}

// The real daemon with this machine's Claude Code login. e2e's liveDaemon pins haiku; S5 sets the
// model the only way the room allows today, the daemon's GRILL_ROOM_AGENT_MODEL.
function daemonFor(roomURL) {
  const dir = joinPath(scratch, 'daemon');
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const stateDir = joinPath(dir, 'state');
  const log = joinPath(dir, 'daemon.log');
  const proc = spawn(joinPath(repo, 'bin', 'grill-room-daemon'), [roomURL], {
    env: { ...process.env, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none', GRILL_ROOM_AGENT_MODEL: model, LOG_LEVEL: 'info' },
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
  return { proc, exited, link, log, sandbox };
}

const messages = (page) => page.locator('#messages li.msg').evaluateAll((els) => els.map((li) => ({
  kind: [...li.classList].find((c) => c !== 'msg'),
  sender: li.querySelector('strong')?.textContent,
  body: li.querySelector('.body')?.textContent,
  model: li.querySelector('.model')?.textContent,
  ts: Date.parse(li.querySelector('time')?.dateTime),
})));

async function playRound(pages, n) {
  const rnd = session.rounds[n - 1];
  for (const name of names) point(name, n);
  if (n === 1) {
    await Promise.all(names.map((name) => joinAudio(pages[name])));
  } else {
    // One at a time, as people do: the audio room never empties, so the worker stays in it.
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
  // The worker can lag far behind speech: wait until no line has landed for 30 s, or 5 minutes.
  const voiceCount = () => pages[host].locator('#messages li.msg.voice').count();
  let count = await voiceCount();
  for (let quiet = Date.now(), cap = Date.now() + 300_000; Date.now() - quiet < 30_000 && Date.now() < cap;) {
    await pages[host].waitForTimeout(2000);
    const c = await voiceCount();
    if (c !== count) [count, quiet] = [c, Date.now()];
  }
  return { round: n, micAt, drainedAt: Date.now() };
}

async function agentTurn(page, roomID, daemon, nudge) {
  await page.locator('#body').fill(nudge);
  await page.locator('#body').press('Enter');
  await expect(page.locator('#messages')).toContainText(nudge.slice(0, 40));
  const before = await page.locator('#messages li.msg.agent').count();
  const pressed = Date.now();
  await page.getByRole('button', { name: 'Ask the agent' }).click();
  await expect(page.locator('#messages li.msg.agent')).toHaveCount(before + 1, { timeout: 900_000 });
  const synthesisMs = Date.now() - pressed;
  const { turn } = await agentState(page, roomID);
  const started = logLines(daemon).filter((l) => l.msg === 'turn_started').at(-1);
  return {
    synthesisMs, turn, resume: started?.resume,
    synthesis: await page.locator('#messages li.msg.agent').last().locator('.body').textContent(),
    requestsSoFar: messagesRequests(daemon),
  };
}

// Documents with every revision as text, and annotation threads, read through the room API.
const roomState = (page, roomID) => page.evaluate(async (id) => {
  const get = (p) => fetch(`/api/rooms/${id}${p}`).then((r) => r.json());
  const text = (html) => new DOMParser().parseFromString(html, 'text/html').body.innerText;
  const docs = [];
  for (const d of await get('/documents')) {
    const { revisions } = await get(`/documents/${d.id}`);
    const texts = [];
    for (const r of revisions ?? []) {
      texts.push({ n: r.n, author: r.author, text: text(await fetch(`/api/rooms/${id}/documents/${d.id}/view?rev=${r.n}`).then((x) => x.text())) });
    }
    docs.push({ ...d, texts, annotations: await get(`/documents/${d.id}/annotations`) });
  }
  return docs;
}, roomID);

test('simulated grill session with synthetic voices', async ({ request, baseURL }) => {
  rmSync(runDir, { recursive: true, force: true });
  mkdirSync(micDir, { recursive: true });
  for (const name of names) point(name, 1);
  const browsers = {};
  for (const name of names) browsers[name] = await launch(joinPath(micDir, `${name}.wav`));
  const room = await createRoom(request, 'S5 dark mode grill');
  const daemon = withAgent ? daemonFor(`${baseURL}/r/${room.id}`) : null;
  const result = { room: room.id, model: withAgent ? model : null, rounds: [], turns: [] };
  try {
    const pages = {};
    for (const name of names) pages[name] = await join(browsers[name], await invite(request, room.id), name, micClock);
    const hostPage = pages[host];

    await upload(hostPage, 'plan.md', 'text/markdown', plan);
    await expect(hostPage.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Dark mode for the room UI');

    if (withAgent) {
      const pairedAt = Date.now();
      await pairDaemon(hostPage, daemon);
      await expect(hostPage.locator('#agent-led')).toHaveAttribute('aria-label', 'Agent ready', { timeout: 300_000 });
      result.agentReadyMs = Date.now() - pairedAt;
    }

    result.rounds.push(await playRound(pages, 1));

    const { by, quote, comment } = session.annotation;
    await pages[by].locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    await annotateSelection(pages[by], quote, comment);
    await expect(hostPage.locator('#annot-list')).toContainText(comment);

    // 001-AC39 loop: Emma's thread gets an agent reply, she answers it, the agent answers again,
    // closes a thread and opens its own annotation; a participant reply then reopens the closed one.
    const reply = async (page, text, body) => {
      const thread = page.locator('#annot-list li.annot', { hasText: text });
      await thread.getByLabel('Reply').fill(body);
      await thread.getByRole('button', { name: 'Reply' }).click();
      await expect(thread).toContainText(body);
    };
    if (withAgent) {
      result.turns.push(await agentTurn(hostPage, room.id, daemon, nudges[0]));
      await expect.soft(pages[by].locator('#annot-list li.annot', { hasText: comment })).toContainText('Agent');
      await reply(pages[by], comment, session.annotation.followup);
    }
    result.rounds.push(await playRound(pages, 2));
    // A turn spends 3 requests at least (read, write, answer): stay within the budget.
    if (withAgent && result.turns[0].requestsSoFar <= maxRequests - 4) result.turns.push(await agentTurn(hostPage, room.id, daemon, nudges[1]));
    if (withAgent) {
      result.threadsAfterTurns = (await roomState(hostPage, room.id)).flatMap((d) => (d.annotations ?? []).map((a) => ({
        doc: d.name, status: a.status, opener: a.messages[0].author, messages: a.messages.map((m) => m.author),
      })));
      const closed = result.threadsAfterTurns.find((a) => a.status === 'done');
      if (closed) {
        const opening = (await roomState(hostPage, room.id)).flatMap((d) => d.annotations ?? []).find((a) => a.status === 'done').messages[0].body;
        await pages[by].locator('#documents').getByRole('button', { name: closed.doc }).first().click();
        await reply(pages[by], opening.slice(0, 40), session.annotation.reopen);
        await expect(pages[by].locator('#annot-list li.annot', { hasText: opening.slice(0, 40) }).locator('.annot-badge')).toHaveText('reopened');
      }
    }

    result.messages = await messages(hostPage);
    result.documents = await roomState(hostPage, room.id);
    if (withAgent) {
      result.requests = messagesRequests(daemon);
      for (const [i, t] of result.turns.entries()) {
        expect(t.turn.status, `turn ${i + 1}`).toBe('done');
        expect(t.synthesis.trim(), `turn ${i + 1} synthesis`).not.toBe('');
      }
      expect(result.turns[1]?.resume, 'second turn resumes the session').toBe(true);
      expect(result.messages.filter((m) => m.kind === 'agent').every((m) => m.model), 'every synthesis names its model').toBe(true);
      expect(result.threadsAfterTurns.some((a) => a.opener === 'Agent'), 'the agent opened an annotation').toBe(true);
      expect(result.threadsAfterTurns.some((a) => a.status === 'done'), 'the agent closed a thread').toBe(true);
      expect(result.documents.some((d) => d.texts.some((r) => r.author === 'Agent')), 'an agent document or revision').toBe(true);
      const thread = result.documents.find((d) => d.name === 'plan.md').annotations[0];
      expect(thread.messages.some((m) => m.author === 'Agent'), 'agent reply on the annotation').toBe(true);
      expect(result.requests).toBeLessThanOrEqual(maxRequests);
    }
  } finally {
    writeFileSync(joinPath(runDir, 'result.json'), JSON.stringify(result, null, 1) + '\n');
    if (daemon) {
      daemon.proc.kill('SIGTERM');
      await daemon.exited;
      removeSandbox(daemon.sandbox);
    }
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    for (const b of Object.values(browsers)) await b.close();
  }
});
