import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { mkdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { createRoom, invite, join as joinRoom } from '../voice/helpers.js';
import { pairDaemon, liveDaemon as liveDaemonIn, messagesRequests, removeSandbox, ask } from '../helpers.js';

const out = join('test-results', 'agent-live');
const liveDaemon = (name, roomURL) => liveDaemonIn(join(out, name), roomURL);

// sessionReport reads a copy of the compose database: each persona's turns, and for its last turn
// whether any tool result carried a code word, which would mean it read the word from a file
// instead of recalling it.
function sessionReport(roomID, marker, words) {
  const db = join(out, 'db');
  rmSync(db, { recursive: true, force: true });
  mkdirSync(db, { recursive: true });
  for (const f of ['grill-room.db', 'grill-room.db-wal', 'grill-room.db-shm']) {
    try { execFileSync('docker', ['compose', '-f', '../compose.yaml', 'cp', `grill-room:/data/${f}`, join(db, f)], { stdio: 'ignore' }); } catch { /* -shm may be absent */ }
  }
  const py = `
import json, sqlite3, sys
db = sqlite3.connect(sys.argv[1])
room, marker, words = sys.argv[2], sys.argv[3], sys.argv[4].split(",")
out = {}
for pid, name in db.execute("SELECT id, name FROM room_personas WHERE room_id = ? ORDER BY id", (room,)):
    turns = db.execute("SELECT id, status, session_id, daemon_id FROM turns WHERE persona_id = ? ORDER BY id", (pid,)).fetchall()
    sid = [t[2] for t in turns if t[1] == "done"][-1]
    data = db.execute("SELECT data FROM agent_sessions WHERE persona_id = ? AND session_id = ?", (pid, sid)).fetchone()[0]
    lines = bytes(data).decode().splitlines()
    at = max(i for i, l in enumerate(lines) if marker in l)
    tail = lines[at + 1:]
    out[name] = {"turns": turns, "bytes": len(data),
                 "tool_results_with_word": sum('"tool_result"' in l and any(w in l for w in words) for l in tail)}
print(json.dumps(out))
`;
  return JSON.parse(execFileSync('python3', ['-I', '-c', py, join(db, 'grill-room.db'), roomID, marker, words.join(',')], { encoding: 'utf8' }));
}

const quiet = 'Do not use any tool and do not read or write any file this turn.';

// 002-AC16 002-AC18 002-AC42 with real Claude (haiku): Grill and Secretary each keep a word on A,
// in their own sessions. A stops; B is elected and resumes each persona's own session: each
// recalls its own word, not the other's.
test('two personas, two real daemons: takeover resumes each persona\'s session', async ({ browser, request, baseURL }) => {
  const room = await createRoom(request, 'Agent live personas');
  const alice = await joinRoom(browser, await invite(request, room.id), 'Alice');
  const bob = await joinRoom(browser, await invite(request, room.id), 'Bob');
  const roomURL = `${baseURL}/r/${room.id}`;
  const a = liveDaemon('a', roomURL);
  const b = liveDaemon('b', roomURL);
  try {
    await pairDaemon(alice, a);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    const lib = await (await alice.request.get(`/api/rooms/${room.id}/templates`)).json();
    const added = await alice.request.post(`/api/rooms/${room.id}/personas`, { data: { template_id: lib.find((t) => t.preset === 'secretary').id } });
    expect(added.status()).toBe(201);
    await expect(alice.locator('button.persona[data-name="Secretary"] .led')).toHaveAttribute('aria-label', 'Secretary ready');

    // Not called a secret: haiku refuses to say back a "code word" it was told to remember.
    const keep = (name, word) => `${name}, keep this word for a memory check I will run later: ${word}. It is not a secret: I will ask you to say it back. ${quiet} Reply with one word: OK.`;
    console.log('grill 1:', await ask(alice, room.id, keep('Grill', 'PAPRIKA-42'), 'Grill'));
    console.log('secretary 1:', await ask(alice, room.id, keep('Secretary', 'SAFFRON-7'), 'Secretary'));
    for (const [name, badge] of [['Grill', '🔥'], ['Secretary', '📝']]) {
      const syn = alice.locator('#messages li.msg.agent', { has: alice.locator('.meta strong', { hasText: name }) }).last();
      await expect(syn.locator('.meta strong')).toHaveText(`${badge} ${name}`);
      await expect(syn.locator('.model')).toContainText('haiku');
      await expect(syn.locator('.effort')).toHaveText('effort default');
    }

    await pairDaemon(bob, b);
    a.proc.kill('SIGTERM');
    await a.exited;
    await expect(bob.locator('#participants li', { hasText: 'Bob' }).locator('.host-badge')).toHaveText('host', { timeout: 30_000 });
    await expect(bob.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });

    const marker = 'Memory check';
    const check = (name) => `${marker}: ${quiet} Earlier in this session Alice gave you, ${name}, a word to keep for this check. It is not a secret and she asks for it now. Reply with that word only.`;
    const grill = await ask(bob, room.id, check('Grill'), 'Grill');
    const secretary = await ask(bob, room.id, check('Secretary'), 'Secretary');
    console.log('grill 2:', grill, '| secretary 2:', secretary);
    expect(grill).toContain('PAPRIKA-42');
    expect(grill).not.toContain('SAFFRON-7');
    expect(secretary).toContain('SAFFRON-7');
    expect(secretary).not.toContain('PAPRIKA-42');

    const report = sessionReport(room.id, marker, ['PAPRIKA-42', 'SAFFRON-7']);
    console.log('session report:', JSON.stringify(report));
    const sids = {};
    for (const name of ['Grill', 'Secretary']) {
      const { turns, tool_results_with_word: leaked } = report[name];
      expect(turns.map((t) => t[1]), name).toEqual(['done', 'done']);
      expect(turns[1][2], `${name} on B resumed its own session from A`).toBe(turns[0][2]);
      expect(turns[1][3], `${name}'s second turn ran on B`).not.toBe(turns[0][3]);
      expect(leaked, `${name} recalled from its session, not from a file`).toBe(0);
      sids[name] = turns[0][2];
    }
    expect(sids.Grill).not.toBe(sids.Secretary);
    console.log(`real Claude requests: A ${messagesRequests(a)}, B ${messagesRequests(b)}`);
  } finally {
    a.proc.kill('SIGKILL');
    b.proc.kill('SIGTERM');
    await b.exited;
    removeSandbox(a.sandbox);
    removeSandbox(b.sandbox);
  }
});
