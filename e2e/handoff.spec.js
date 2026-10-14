import { test, expect } from './fixtures.js';
import { roomWith, startDaemon, pairDaemon, ask, confirmSheet, personaSheet, closePersonas } from './helpers.js';

// 003-AC24 003-AC25: Alice sets Grill's autocompact, which reaches claude -p. A session past 80% of
// its context window shows the share in the personas panel and a banner offering the host a new
// session with handoff. The handoff's last turn writes docs/_handoff-Grill.md, then the new session
// reads it: it knows the code word only from the note. Bob, a guest, sees the banner without the
// button. Real daemon and Docker sandbox, fake claude.
test('handoff: autocompact, context banner, new session from the note', async ({ browser, request, baseURL }) => {
  test.setTimeout(240_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 30_000 });

    // 009-AC2: the memory size sits on the Model tab.
    const grill = await personaSheet(alice, 'Grill', 'Model');
    await grill.getByLabel('Memory size (k tokens)').fill('300');
    await grill.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(bob.locator('#messages')).toContainText('Alice changed 🔥 Grill: autocompact auto → 300k.');
    await closePersonas(alice);
    expect((await ask(alice, room.id, 'FAKE_AUTOCOMPACT, and the code word is OSPREY.')).trim()).toBe('autocompact 300000');

    expect(await ask(alice, room.id, 'FAKE_CONTEXT_FULL')).toContain('Wrote docs/notes.md');
    const banner = alice.locator('#approvals');
    await expect(banner).toContainText('🔥 Grill\'s session holds 85% of its context window.');
    await expect(bob.locator('#approvals')).toContainText('The AI host can start it fresh, keeping a summary.');
    await expect(bob.getByRole('button', { name: 'Start fresh with a summary for Grill' })).toHaveCount(0);
    await personaSheet(alice, 'Grill', 'Model');
    await expect(grill.locator('section[data-tab=model] .hint')).toContainText('its session holds 85% of its context window.');
    await closePersonas(alice);

    await banner.getByRole('button', { name: 'Start fresh with a summary for Grill' }).click();
    await confirmSheet(alice, 'Start fresh');
    const read = alice.locator('#messages li.msg.agent', { hasText: 'Read docs/_handoff-Grill.md' });
    await expect(read.locator('.body')).toHaveText('Read docs/_handoff-Grill.md: The code word was OSPREY.', { timeout: 120_000 });
    await expect(alice.locator('#messages li.msg.agent', { hasText: 'Handoff note written.' })).toHaveCount(1);
    await expect(alice.locator('#documents')).toContainText('_handoff-Grill.md');
    await expect(banner).toBeHidden();
    // The next turn resumes the new session, which holds no code word of its own.
    expect((await ask(alice, room.id, 'FAKE_RECALL')).trim()).toBe('The code word was nothing.');
  } finally {
    daemon.cleanup();
  }
});
