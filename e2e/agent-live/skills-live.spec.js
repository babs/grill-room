import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createRoom, invite, join as joinRoom } from '../voice/helpers.js';
import { pairDaemon, liveDaemon, messagesRequests, removeSandbox, ask } from '../helpers.js';

const out = join('test-results', 'skills-live');

// The script prints a stamp haiku cannot guess: only running it gives the right one.
const stamp = 'STAMP-' + createHash('sha256').update('grill-room').digest('hex').slice(0, 12);

function stampZip() {
  mkdirSync(out, { recursive: true });
  const zip = join(out, 'stamp.zip');
  const md = `---
name: stamp
description: Writes the room's verification stamp into docs/stamp.md. Use whenever someone asks for the stamp.
---
Run this one command, with <base> being this skill's base directory, and nothing else:

    sh <base>/scripts/stamp.sh > /workspace/docs/stamp.md
`;
  execFileSync('python3', ['-I', '-c', `import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr("stamp/SKILL.md", sys.argv[2])
    z.writestr("stamp/scripts/stamp.sh", 'printf "# Stamp\\\\n\\\\nSTAMP-%s\\\\n" "$(printf grill-room | sha256sum | cut -c1-12)"\\n')`, zip, md]);
  return readFileSync(zip);
}

// 002-AC31 with real Claude (haiku): Alice's personal skill holds a script; on Grill, a real
// `claude -p` turn loads it through --plugin-dir, runs the script in the sandbox, and its output
// lands in a document of the room. At most four requests.
test('a skill script runs in a real turn and its output lands in a doc', async ({ browser, request, baseURL }) => {
  const room = await createRoom(request, 'Skills live');
  const alice = await joinRoom(browser, await invite(request, room.id), 'Alice');
  const api = `/api/rooms/${room.id}`;
  const up = await alice.request.post('/api/profile/skills', { headers: { 'Content-Type': 'application/zip' }, data: stampZip() });
  expect(up.status(), await up.text()).toBe(201);
  const skill = await up.json();
  const st = await (await alice.request.get(`${api}/agent`)).json();
  const grill = st.personas[0];
  // A bare persona: the Grill text asks for a synthesis document, which costs requests.
  const edit = await alice.request.patch(`${api}/personas/${grill.id}`,
    { data: { text: 'You are Grill. Do exactly what the room asks, with as few tool calls as possible, and nothing else.' } });
  expect(edit.status()).toBe(200);
  const on = await alice.request.put(`${api}/personas/${grill.id}/skills/${skill.id}`, { data: { accept_disclaimer: true } });
  expect(on.status(), await on.text()).toBe(200);

  const d = liveDaemon(join(out, 'daemon'), `${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, d);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });
    const synthesis = await ask(alice, room.id, 'Grill, use your stamp skill now. Then reply with the single word: stamped.');
    console.log('synthesis:', synthesis);
    const docs = await (await alice.request.get(`${api}/documents`)).json();
    const doc = docs.find((x) => x.name === 'stamp.md');
    expect(doc, JSON.stringify(docs)).toBeTruthy();
    const page = await (await alice.request.get(`${api}/documents/${doc.id}/view`)).text();
    expect(page).toContain(stamp);
    const used = messagesRequests(d);
    console.log(`real Claude requests: ${used}`);
    expect(used).toBeLessThanOrEqual(4);
  } finally {
    d.proc.kill('SIGTERM');
    await d.exited;
    removeSandbox(d.sandbox);
  }
});
