import { test, expect } from '@playwright/test';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, rmSync, openSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createRoom, invite, join as joinRoom } from '../voice/helpers.js';
import { pairDaemon } from '../helpers.js';

const out = join('test-results', 'agent-live');

// liveDaemon runs the real daemon with this machine's Claude Code login (read only), the real
// sandbox image and haiku. Its log goes to a file: the proxy logs one upstream_response per request.
function liveDaemon(name, roomURL) {
  const dir = join(out, name);
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const stateDir = join(dir, 'state');
  const log = join(dir, 'daemon.log');
  const proc = spawn('../bin/grill-room-daemon', [roomURL], {
    env: { ...process.env, GRILL_ROOM_DAEMON_STATE_DIR: stateDir, GRILL_ROOM_DAEMON_BROWSER: 'none', GRILL_ROOM_AGENT_MODEL: 'haiku', LOG_LEVEL: 'info' },
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
  // Named as cmd/grill-room-daemon names it: a killed daemon leaves its sandbox behind.
  const sandbox = createHash('sha256').update(stateDir + '\0' + roomURL).digest('hex').slice(0, 12);
  return { proc, exited, link, log, sandbox };
}

const logLines = (d) => readFileSync(d.log, 'utf8').split('\n').filter((l) => l.startsWith('{')).map((l) => JSON.parse(l));
const messagesRequests = (d) => logLines(d).filter((l) => l.msg === 'upstream_response' && l.path.startsWith('/v1/messages')).length;

function removeSandbox(name) {
  const docker = (...args) => { try { return execFileSync('docker', args, { encoding: 'utf8' }); } catch { return ''; } };
  const label = `label=grill-room-daemon=${name}`;
  for (const id of docker('ps', '-aq', '--filter', label).split('\n').filter(Boolean)) docker('rm', '-f', '-v', id);
  for (const id of docker('network', 'ls', '-q', '--filter', label).split('\n').filter(Boolean)) docker('network', 'rm', id);
  for (const id of docker('volume', 'ls', '-q', '--filter', label).split('\n').filter(Boolean)) docker('volume', 'rm', '-f', id);
}

const agentState = (page, roomID) => page.evaluate((id) => fetch(`/api/rooms/${id}/agent`).then((r) => r.json()), roomID);

async function ask(page, roomID, text) {
  const before = await page.locator('#messages li.msg.agent').count();
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
  await expect(page.locator('#messages')).toContainText(text.slice(0, 40));
  await page.getByRole('button', { name: 'Ask the agent' }).click();
  await expect(page.locator('#messages li.msg.agent')).toHaveCount(before + 1, { timeout: 240_000 });
  const st = await agentState(page, roomID);
  expect(st.turn.status).toBe('done');
  return page.locator('#messages li.msg.agent').last().locator('.body').textContent();
}

// The session the room stores, read from a copy of the compose database: turn statuses, and the
// tool results of the last turn that carry the code word, which would mean it read the word from a
// file instead of recalling it.
function sessionReport(roomID, marker, word) {
  const db = join(out, 'db');
  rmSync(db, { recursive: true, force: true });
  mkdirSync(db, { recursive: true });
  for (const f of ['grill-room.db', 'grill-room.db-wal', 'grill-room.db-shm']) {
    try { execFileSync('docker', ['compose', '-f', '../compose.yaml', 'cp', `grill-room:/data/${f}`, join(db, f)], { stdio: 'ignore' }); } catch { /* -shm may be absent */ }
  }
  const py = `
import json, sqlite3, sys
db = sqlite3.connect(sys.argv[1])
room, marker, word = sys.argv[2], sys.argv[3], sys.argv[4]
turns = db.execute("SELECT id, status, session_id, error FROM turns WHERE room_id = ? ORDER BY id", (room,)).fetchall()
sid = [t[2] for t in turns if t[1] == "done"][-1]
data = db.execute("SELECT data FROM agent_sessions WHERE room_id = ? AND session_id = ?", (room, sid)).fetchone()[0]
lines = bytes(data).decode().splitlines()
at = max(i for i, l in enumerate(lines) if marker in l)
tail = lines[at + 1:]
print(json.dumps({"turns": turns, "bytes": len(data), "tool_results": sum('"tool_result"' in l for l in tail),
                  "tool_results_with_word": sum('"tool_result"' in l and word in l for l in tail)}))
`;
  return JSON.parse(execFileSync('python3', ['-I', '-c', py, join(db, 'grill-room.db'), roomID, marker, word], { encoding: 'utf8' }));
}

// 001-AC32 001-AC33 001-AC34 with real Claude: a turn on A takes a code word and writes a document
// (revision and synthesis); A is killed mid-turn (turn failed, no partial revision); B is elected
// and its turn resumes the room's copy of the session and recalls the code word from memory.
test('two real daemons: turn, crash, takeover with --resume', async ({ browser, request, baseURL }) => {
  const room = await createRoom(request, 'Agent live');
  const alice = await joinRoom(browser, await invite(request, room.id), 'Alice');
  const bob = await joinRoom(browser, await invite(request, room.id), 'Bob');
  const roomURL = `${baseURL}/r/${room.id}`;
  const a = liveDaemon('a', roomURL);
  const b = liveDaemon('b', roomURL);
  try {
    await pairDaemon(alice, a);
    await expect(alice.locator('#agent-led')).toHaveAttribute('aria-label', 'Agent ready', { timeout: 30_000 });

    const first = await ask(alice, room.id, 'The code word is PAPRIKA-42. Remember it. '
      + 'Create the file docs/plan.md containing exactly one line: Ship on Friday. Then reply in one short sentence.');
    console.log('turn 1 synthesis:', first);
    const docs = await (await alice.request.get(`/api/rooms/${room.id}/documents`)).json();
    const plan = docs.find((d) => d.name === 'plan.md');
    expect(plan, 'docs/plan.md created').toBeTruthy();
    expect(plan.revisions).toBe(1);
    expect(plan.author).toBe('Agent');

    await pairDaemon(bob, b);
    await alice.locator('#body').fill('Rewrite docs/plan.md as three bullet points about the rollout.');
    await alice.locator('#body').press('Enter');
    await alice.getByRole('button', { name: 'Ask the agent' }).click();
    // Kill A once its turn reached the sandbox, as a crash would.
    await expect.poll(() => logLines(a).filter((l) => l.msg === 'turn_started').length, { timeout: 60_000 }).toBe(2);
    await new Promise((r) => setTimeout(r, 1500));
    a.proc.kill('SIGKILL');
    await a.exited;
    await expect.poll(async () => {
      const st = await agentState(bob, room.id);
      return `${st.turn.status} ${st.online}`;
    }, { timeout: 30_000 }).toBe('failed true');
    const failed = await agentState(bob, room.id);
    expect(failed.turn.error).toContain('disconnected');
    const after = await (await alice.request.get(`/api/rooms/${room.id}/documents/${plan.id}`)).json();
    expect(after.revisions, 'no partial revision kept').toHaveLength(1);
    await expect(bob.locator('#participants li', { hasText: 'Bob' }).locator('.host-badge')).toHaveText('host');

    const marker = 'Answer from memory';
    const recalled = await ask(bob, room.id, `${marker}, without using any tool or reading any file: what is the code word I gave you earlier? Reply with the code word only.`);
    console.log('turn 3 synthesis:', recalled);
    expect(recalled).toContain('PAPRIKA-42');

    const report = sessionReport(room.id, marker, 'PAPRIKA-42');
    console.log('session report:', JSON.stringify(report));
    const done = report.turns.filter((t) => t[1] === 'done');
    expect(done).toHaveLength(2);
    expect(done[1][2], 'B resumed the session A started').toBe(done[0][2]);
    expect(report.turns.map((t) => t[1])).toEqual(['done', 'failed', 'done']);
    // The failed turn's request comes back in this prompt, so tools may run; none may hand it the word.
    expect(report.tool_results_with_word, 'recalled from the session, not read from a file').toBe(0);
    console.log(`real Claude requests: A ${messagesRequests(a)}, B ${messagesRequests(b)}`);
  } finally {
    a.proc.kill('SIGKILL');
    b.proc.kill('SIGTERM');
    await b.exited;
    removeSandbox(a.sandbox);
    removeSandbox(b.sandbox);
  }
});
