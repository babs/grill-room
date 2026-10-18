import { test, expect } from '@playwright/test';
import { join } from 'node:path';
import { createRoom, invite, join as joinRoom, composeSQL } from '../voice/helpers.js';
import { pairDaemon, liveDaemon as liveDaemonIn, messagesRequests, removeSandbox, ask } from '../helpers.js';

const out = join('test-results', 'agent-live');
const liveDaemon = (name, roomURL) => liveDaemonIn(join(out, name), roomURL);

// sessionReport reads the compose database: each persona's turns, and for its last turn whether any
// tool result carried a code word, which would mean it read the word from a file instead of
// recalling it.
function sessionReport(roomID, marker, words) {
  const personas = JSON.parse(composeSQL(`
SELECT coalesce(json_object_agg(p.name, json_build_object(
  'turns', (SELECT coalesce(json_agg(json_build_array(t.id, t.status, t.session_id, t.daemon_id) ORDER BY t.id), '[]')
            FROM turns t WHERE t.persona_id = p.id),
  'session', (SELECT json_build_object('bytes', octet_length(s.data), 'text', convert_from(s.data, 'UTF8'))
              FROM agent_sessions s WHERE s.persona_id = p.id AND s.session_id = (SELECT t.session_id FROM turns t
                WHERE t.persona_id = p.id AND t.status = 'done' ORDER BY t.id DESC LIMIT 1))) ORDER BY p.id), '{}')
FROM room_personas p WHERE p.room_id = :'room';
`, { room: roomID }));
  const out = {};
  for (const [name, { turns, session }] of Object.entries(personas)) {
    if (!session) throw new Error(`${name}: no stored session for its last done turn`);
    const { bytes, text } = session;
    const lines = text.split(/\r\n|\r|\n/);
    const at = lines.findLastIndex((l) => l.includes(marker));
    if (at < 0) throw new Error(`${name}: no line of its session holds ${marker}`);
    const tail = lines.slice(at + 1);
    out[name] = { turns, bytes, tool_results_with_word: tail.filter((l) => l.includes('"tool_result"') && words.some((w) => l.includes(w))).length };
  }
  return out;
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
      await expect(syn.locator('.effort')).toHaveText('effort medium');
    }

    await pairDaemon(bob, b);
    a.proc.kill('SIGTERM');
    await a.exited;
    await expect(bob.locator('#participants li', { hasText: 'Bob' }).locator('.host-badge')).toHaveAttribute('aria-label', 'host', { timeout: 30_000 });
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
