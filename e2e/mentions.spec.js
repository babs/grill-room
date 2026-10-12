import { test, expect } from '@playwright/test';
import { roomWith, startDaemon, pairDaemon } from './helpers.js';

async function say(page, text) {
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
}

// line is the chat line of page holding text.
const line = (page, text) => page.locator('#messages li.msg', { hasText: text }).last();

// 005-AC2: "@" suggests the room's personas, then the people, filtered as you type; arrows and
// Enter pick with the keyboard, a tap picks on a phone. 005-AC3: a mention that cannot run tells
// its author why, and nobody else sees it.
test('mentions: autocomplete and notices without a host', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob Smith']);
  const body = alice.locator('#body');
  const list = alice.locator('#mention-list');
  await expect(alice.locator('#participants')).toContainText('Bob Smith');
  await body.pressSequentially('Hi @');
  await expect(list.getByRole('option')).toHaveText(['🔥 Grillagent', 'Bob Smithperson']);
  await expect(body).toHaveAttribute('aria-expanded', 'true');
  await body.pressSequentially('b');
  await expect(list.getByRole('option')).toHaveText(['Bob Smithperson']);
  await body.press('Enter');
  await expect(body).toHaveValue('Hi @Bob_Smith ');
  await expect(list).toBeHidden();
  await body.pressSequentially('and @');
  await body.press('ArrowDown');
  await expect(list.getByRole('option').nth(1)).toHaveAttribute('aria-selected', 'true');
  await body.press('ArrowUp');
  await body.press('Tab');
  await expect(body).toHaveValue('Hi @Bob_Smith and @Grill ');
  await body.pressSequentially('@x');
  await expect(list).toBeHidden();
  await body.fill('');

  // On a phone, a tap picks.
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: { Authorization: 'Bearer e2e-admin-token' } })).json();
  const phone = await (await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true })).newPage();
  await phone.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await phone.getByLabel('Your name').fill('Carol');
  await phone.getByRole('button', { name: 'Join the room' }).click();
  await expect(phone.getByRole('button', { name: 'Ask Grill' })).toBeVisible();
  await phone.locator('#body').pressSequentially('@gr');
  await phone.locator('#mention-list').getByRole('option', { name: /Grill/ }).tap();
  await expect(phone.locator('#body')).toHaveValue('@Grill ');
  await expect(phone.locator('#body')).toBeFocused();

  // Bob may not trigger agents; @nobody names nobody; @Alice names a person. Only Bob reads why.
  await say(bob, '@grill @nobody what does @Alice think?');
  const mine = line(bob, 'what does');
  await expect(mine.locator('.notice')).toHaveText([
    '@nobody: no persona or participant has that name. Only you see this.',
    '@Grill: you may not trigger agents in this room: the host can allow you. Only you see this.',
  ]);
  await expect(line(alice, 'what does')).toBeVisible();
  await expect(line(alice, 'what does').locator('.mentions')).toHaveCount(0);
});

// 005-AC3 005-AC4 005-AC5 with the real daemon, a Docker sandbox and the fake claude: a mention
// shows its turn done, linked to the synthesis. A running one is stopped from the room with nothing
// applied, and the next turn resumes the session as the last done turn left it. One that fails
// shows failed. A persona waiting for approval, and an offline host, give notices.
test('mentions: turn states, stop, pending and offline', async ({ browser, request, baseURL }) => {
  test.setTimeout(240_000);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const admin = await pairDaemon(alice, daemon);
    await say(alice, '@grill the code word is ALPHA');
    const first = line(alice, 'code word is ALPHA');
    await expect(first.locator('.mention-turn')).toHaveText('→ @Grill · done', { timeout: 120_000 });
    await expect(line(bob, 'code word is ALPHA').locator('.mention-turn')).toHaveText('→ @Grill · done');
    await first.locator('.mention-turn').click();
    await expect(alice.locator('#messages li.msg.agent.flash')).toHaveText(/Wrote docs\/notes\.md, turn 1\./);
    const notes = async () => (await (await alice.request.get(`/api/rooms/${room.id}/documents`)).json()).find((d) => d.name === 'notes.md');
    expect((await notes()).revisions).toBe(1);

    // Stopped mid-run: the fake writes docs/notes.md and the session, then hangs.
    await say(alice, '@grill FAKE_SLEEP and the code word is BRAVO');
    const sleeping = line(alice, 'code word is BRAVO');
    await expect(sleeping.locator('.mention-turn')).toHaveText('→ @Grill · running', { timeout: 60_000 });
    // Bob neither started it, hosts nor administers the room.
    await expect(bob.locator('#running-list')).toContainText('Grill working');
    await expect(bob.locator('#running-list').getByRole('button', { name: 'Stop' })).toHaveCount(0);
    await alice.waitForTimeout(3000);
    await alice.locator('#running-list').getByRole('button', { name: 'Stop the turn of Grill' }).click();
    await expect(sleeping.locator('.mention-turn')).toHaveText('→ @Grill · stopped');
    await expect(alice.locator('#messages')).toContainText('Grill turn stopped by Alice.');
    await expect(alice.locator('#running')).toBeHidden();
    expect((await notes()).revisions).toBe(1);

    await say(alice, '@grill FAKE_RECALL');
    await expect(line(alice, 'FAKE_RECALL').locator('.mention-turn')).toHaveText('→ @Grill · done', { timeout: 60_000 });
    await expect(alice.locator('#messages li.msg.agent').last().locator('.body')).toHaveText('The code word was ALPHA.');

    // No upload to edit: the turn fails, linked to the line saying so.
    await say(alice, '@grill FAKE_EDIT_UPLOAD');
    const failed = line(alice, 'FAKE_EDIT_UPLOAD').locator('.mention-turn');
    await expect(failed).toHaveText('→ @Grill · failed', { timeout: 60_000 });
    await failed.click();
    await expect(alice.locator('#messages li.msg.system.flash')).toContainText('Grill turn failed: claude');

    // With "approve everything", Bob's new persona waits for the host.
    await admin.getByLabel('Approve every new persona and persona change').check();
    await admin.getByRole('button', { name: 'Save' }).click();
    await expect(admin.locator('#settings-status')).toHaveText('Saved.');
    await bob.getByRole('button', { name: 'Personas' }).click();
    await bob.locator('#persona-template').selectOption({ label: '🔥 Default v1' });
    await bob.locator('#personas').getByRole('button', { name: 'Add', exact: true }).click();
    await expect(bob.locator('#personas-status')).toHaveText('Grill-2 added. It waits for the host\'s approval.');
    await say(alice, '@Grill-2 are you there?');
    await expect(line(alice, 'are you there').locator('.notice')).toHaveText('@Grill-2: it waits for the host\'s approval. Only you see this.');

    daemon.cleanup();
    await expect(alice.locator('#agent-status')).toHaveText('No host daemon is connected.', { timeout: 30_000 });
    await say(alice, '@grill still there?');
    await expect(line(alice, 'still there').locator('.notice')).toHaveText('@Grill: no host is online to run it. Only you see this.');
  } finally {
    daemon.cleanup();
  }
});
