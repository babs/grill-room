import { test, expect } from './fixtures.js';
import { roomWith, startDaemon, pairDaemon, menuItem, sandboxHas, openStrip, addSecretary, upload, commentOnDocument, sandboxPrompts, approveEverything, messageAction } from './helpers.js';

async function say(page, text) {
  await page.locator('#body').fill(text);
  await page.locator('#body').press('Enter');
}

// line is the chat line of page holding text.
const line = (page, text) => page.locator('#messages li.msg', { hasText: text }).last();

// 005-AC2: "@" suggests the room's personas, then the people, filtered as you type; arrows and
// Enter pick with the keyboard, a tap picks on a phone. 005-AC3: a mention that cannot run tells
// its author why, and nobody else sees it.
// The specs that start a daemon run in the sandbox project only: the firefox project runs beside others.
const noSandbox = (browserName) => test.skip(browserName === 'firefox', 'agent turns run in the sandbox project, in Chrome');

// phoneContext is a touch phone; Firefox has no isMobile.
const phoneContext = (browser, browserName) =>
  browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: browserName !== 'firefox', hasTouch: true });

test('mentions: autocomplete and notices without a host', async ({ browser, request, browserName }) => {
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
  const phone = await (await phoneContext(browser, browserName)).newPage();
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

  // 016-AC21: an edit that names a persona tells its author why it will not run, as a send does.
  await say(bob, 'Second thought');
  await messageAction(line(bob, 'Second thought'), 'Edit');
  const box = bob.getByRole('textbox', { name: 'New text' });
  await box.press('End');
  await box.pressSequentially(' @grill now');
  await box.press('Enter');
  await expect(line(bob, 'Second thought').locator('.notice')).toHaveText(
    '@Grill: you may not trigger agents in this room: the host can allow you. Only you see this.');
  await expect(line(alice, 'Second thought')).toBeVisible();
  await expect(line(alice, 'Second thought').locator('.mentions')).toHaveCount(0);
});

// 005-AC2 in the edit box: editing a line offers the same @ list as the send box, keyboard and
// tap; Enter picks while the list is open and saves once it is closed, and the saved line shows
// its chips (005-AC27).
test('mentions: autocomplete in the edit box', async ({ browser, request, browserName }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice', 'Bob Smith']);
  await expect(alice.locator('#participants')).toContainText('Bob Smith');
  await say(alice, 'Hi there');
  await messageAction(line(alice, 'Hi there'), 'Edit');
  const box = alice.getByRole('textbox', { name: 'New text' });
  // The line's text sits in the box while it is edited: the edit form finds it.
  const list = alice.locator('#messages form.edit').getByRole('listbox', { name: 'Mention' });
  await box.press('End');
  await box.pressSequentially(' @');
  await expect(list.getByRole('option')).toHaveText(['🔥 Grillagent', 'Bob Smithperson']);
  await expect(box).toHaveAttribute('aria-expanded', 'true');
  // Inside the #messages live region, the list is not read out on each keystroke.
  await expect(list).toHaveAttribute('aria-live', 'off');
  await box.press('ArrowDown');
  await expect(list.getByRole('option').nth(1)).toHaveAttribute('aria-selected', 'true');
  await box.press('Enter');
  await expect(box).toHaveValue('Hi there @Bob_Smith ');
  await expect(list).toBeHidden();
  await box.pressSequentially('and @g');
  await expect(list.getByRole('option')).toHaveText(['🔥 Grillagent']);
  // Escape closes the list and keeps the edit open.
  await box.press('Escape');
  await expect(list).toBeHidden();
  await expect(box).toBeVisible();
  await box.pressSequentially('r');
  await box.press('Tab');
  await expect(box).toHaveValue('Hi there @Bob_Smith and @Grill ');
  await box.press('Enter');
  const saved = line(alice, 'Hi there').locator('mark.mention-chip');
  await expect(saved).toHaveText(['@Bob_Smith', '@Grill']);
  await expect(saved.nth(1)).toHaveClass(/persona/);
  // The send box's list stayed shut throughout.
  await expect(alice.locator('#mention-list')).toBeHidden();

  // On a phone, a tap picks in the edit box too.
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: { Authorization: 'Bearer e2e-admin-token' } })).json();
  const phone = await (await phoneContext(browser, browserName)).newPage();
  await phone.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await phone.getByLabel('Your name').fill('Carol');
  await phone.getByRole('button', { name: 'Join the room' }).click();
  await say(phone, 'Carol here');
  await messageAction(line(phone, 'Carol here'), 'Edit');
  const pbox = phone.getByRole('textbox', { name: 'New text' });
  await pbox.press('End');
  await pbox.pressSequentially(' @bo');
  await phone.locator('#messages form.edit').getByRole('option', { name: /Bob Smith/ }).tap();
  await expect(pbox).toHaveValue('Carol here @Bob_Smith ');
  await expect(pbox).toBeFocused();
});

// 005-AC3 005-AC4 005-AC5 with the real daemon, a Docker sandbox and the fake claude: a mention
// shows its turn done, linked to the synthesis. A running one is stopped from the room with nothing
// applied, and the next turn resumes the session as the last done turn left it. One that fails
// shows failed. A persona waiting for approval, and an offline host, give notices.
test('mentions: turn states, stop, pending and offline', async ({ browser, request, baseURL, browserName }) => {
  noSandbox(browserName);
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

    // 016-AC21: an edit naming @Grill queues it as a send would, and the line shows the chip. Its
    // turn writes docs/notes.md, as the first did.
    await say(alice, 'Before the edit');
    await messageAction(line(alice, 'Before the edit'), 'Edit');
    await alice.getByRole('textbox', { name: 'New text' }).fill('After the edit, @grill please');
    await alice.getByRole('textbox', { name: 'New text' }).press('Enter');
    await expect(line(alice, 'After the edit').locator('.mention-turn')).toHaveText(/→ @Grill · (queued|running|done)/);
    await expect(line(alice, 'After the edit').locator('.mention-turn')).toHaveText('→ @Grill · done', { timeout: 60_000 });
    expect((await notes()).revisions).toBe(2);

    // Stopped mid-run: the fake writes docs/notes.md and the session, then hangs.
    await say(alice, '@grill FAKE_SLEEP and the code word is BRAVO');
    const sleeping = line(alice, 'code word is BRAVO');
    await expect(sleeping.locator('.mention-turn')).toHaveText('→ @Grill · running', { timeout: 60_000 });
    // Bob neither started it, hosts nor administers the room.
    await expect(bob.locator('#running-list')).toContainText('Grill answering');
    await expect(bob.locator('#running-list').getByRole('button', { name: 'Stop' })).toHaveCount(0);
    // The fake has written its notes in the sandbox: the stop lands mid-run.
    await expect.poll(() => sandboxHas(daemon, 'docs/notes.md', 'never ended'), { timeout: 30_000 }).toBe(true);
    await openStrip(alice);
    await alice.locator('#running-list').getByRole('button', { name: 'Stop the turn of Grill' }).click();
    await expect(sleeping.locator('.mention-turn')).toHaveText('→ @Grill · stopped');
    await expect(alice.locator('#messages')).toContainText('Grill turn stopped by Alice.');
    await expect(alice.locator('#running')).toBeHidden();
    expect((await notes()).revisions).toBe(2);

    await say(alice, '@grill FAKE_RECALL');
    await expect(line(alice, 'FAKE_RECALL').locator('.mention-turn')).toHaveText('→ @Grill · done', { timeout: 60_000 });
    await expect(alice.locator('#messages li.msg.agent').last().locator('.body')).toHaveText('The code word was ALPHA.');

    // No upload to edit: the turn fails, linked to the line saying so.
    await say(alice, '@grill FAKE_EDIT_UPLOAD');
    const failed = line(alice, 'FAKE_EDIT_UPLOAD').locator('.mention-turn');
    await expect(failed).toHaveText('→ @Grill · failed', { timeout: 60_000 });
    await failed.click();
    await expect(alice.locator('#messages li.msg.system.flash')).toContainText('Grill turn failed: claude');
    // 005-AC4: the chips are stored with their lines: a reload shows them all again.
    const again = await bob.context().newPage();
    await again.goto(bob.url());
    for (const [text, state] of [['code word is ALPHA', 'done'], ['FAKE_RECALL', 'done'], ['FAKE_EDIT_UPLOAD', 'failed']]) {
      await expect(line(again, text).locator('.mention-turn')).toHaveText('→ @Grill · ' + state);
    }
    await again.close();

    // With "approve everything", Bob's new persona waits for the host.
    await approveEverything(admin);
    await menuItem(bob, 'Personas');
    await bob.locator('#persona-template').selectOption({ label: '🔥 Default v1' });
    await bob.locator('#personas').getByRole('button', { name: 'Add', exact: true }).click();
    await expect(bob.locator('#personas-status')).toHaveText('Grill-2 added. It waits for the AI host\'s approval.');
    await say(alice, '@Grill-2 are you there?');
    await expect(line(alice, 'are you there').locator('.notice')).toHaveText('@Grill-2: it waits for the host\'s approval. Only you see this.');

    daemon.cleanup();
    await expect(alice.locator('#agent-status')).toHaveText('No one runs the AI for this room right now.', { timeout: 30_000 });
    await say(alice, '@grill still there?');
    await expect(line(alice, 'still there').locator('.notice')).toHaveText('@Grill: no host is online to run it. Only you see this.');
  } finally {
    daemon.cleanup();
  }
});

// 009-AC29 009-AC30 with the real daemon and the fake claude: a note naming @Secretary queues it,
// and the note message shows the turn's chip, kept after a reload. The thread is the Secretary's:
// Grill's prompt leaves it out of the new threads, and Grill's reply to it (the fake's FAKE_REPLY
// answers the first thread of annotations.md) is refused and reported in the chat.
test('mentions: a note names the persona that owns its thread', async ({ browser, request, baseURL, browserName }) => {
  noSandbox(browserName);
  test.setTimeout(240_000);
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const panel = await addSecretary(alice);
  await panel.getByRole('button', { name: 'Close' }).click();
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    await expect(alice.locator('button.persona[data-name="Secretary"] .led')).toHaveAttribute('aria-label', 'Secretary ready', { timeout: 30_000 });
    await upload(alice, 'plan.md', 'text/markdown', '# Rollout plan\n\nShip behind a flag.\n');
    await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
    await commentOnDocument(alice, '@Secretary minute the rollout');
    const chip = alice.locator('#annot-list .annot-msg', { hasText: 'minute the rollout' }).locator('.mention-turn');
    await expect(chip).toHaveText('→ @Secretary · done', { timeout: 120_000 });
    await alice.reload();
    await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    await expect(chip).toHaveText('→ @Secretary · done');

    await commentOnDocument(alice, 'Is the flag named?');
    await say(alice, 'FAKE_REPLY to the first note');
    await alice.getByRole('button', { name: 'Ask Grill' }).click();
    const docs = await (await alice.request.get(`/api/rooms/${room.id}/documents`)).json();
    const notes = await (await alice.request.get(`/api/rooms/${room.id}/documents/${docs.find((d) => d.name === 'plan.md').id}/annotations`)).json();
    const [owned, free] = notes.map((a) => a.id);
    await expect(alice.locator('#messages')).toContainText(`ignored: reply to annotation ${owned}: it belongs to @Secretary`, { timeout: 120_000 });
    const grill = sandboxPrompts(daemon).filter((p) => p.includes('You: @Grill')).at(-1);
    expect(grill).toContain(`### Annotation ${free} `);
    expect(grill).not.toContain(`### Annotation ${owned} `);
  } finally {
    daemon.cleanup();
  }
});
