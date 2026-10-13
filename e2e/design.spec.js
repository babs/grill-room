import { test, expect } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, upload, startDaemon, smallTargets, menuItem } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
// SHOTS=<dir> keeps a screenshot of each screen checked: the before and after of the UX audit.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : null);

// dropFiles drops files on target, as a file manager does: a DataTransfer built in the page.
async function dropFiles(page, target, files) {
  const dt = await page.evaluateHandle((files) => {
    const dt = new DataTransfer();
    for (const f of files) dt.items.add(new File([f.body], f.name, { type: f.type }));
    return dt;
  }, files);
  for (const type of ['dragenter', 'dragover', 'drop']) await page.locator(target).dispatchEvent(type, { dataTransfer: dt });
}

// 005-AC30: at 1440 px the room header's controls share one height.
test('header controls share one height', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await expect(alice.locator('button.persona').first()).toBeVisible();
  const heights = await alice.locator('header button:visible').evaluateAll((els) => els.map((e) => [e.id || e.textContent.trim(), Math.round(e.getBoundingClientRect().height)]));
  expect(heights.length).toBeGreaterThan(2);
  expect(new Set(heights.map(([, h]) => h)), JSON.stringify(heights)).toEqual(new Set([36]));
  // 005-AC32: 8 px between the controls of a group, 24 px between groups; the auto-turn counter
  // hides at 0.
  const gap = (sel) => alice.locator(sel).evaluate((e) => getComputedStyle(e).columnGap);
  expect(await gap('.zone-me')).toBe('8px');
  expect(await gap('#persona-buttons')).toBe('8px');
  expect(await gap('.room-header')).toBe('24px');
  await expect(alice.locator('#chain-count')).toBeHidden();
  await shot(alice, 'after-desktop-room');
  // 005-AC28: dark tokens follow the system's scheme.
  await alice.emulateMedia({ colorScheme: 'dark' });
  await expect(alice.locator('body')).toHaveCSS('background-color', 'rgb(22, 26, 44)');
  await shot(alice, 'after-desktop-room-dark');
});

// 005-AC30: every control is a 44 px target at 390 px, on every page.
test('touch targets at phone width', async ({ browser, request, baseURL }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  await expect(alice.locator('button.persona').first()).toBeVisible();
  expect(await smallTargets(alice), 'room').toEqual([]);
  await shot(alice, 'after-phone-room');

  await alice.locator('#me').click();
  expect(await smallTargets(alice), 'user menu').toEqual([]);
  await alice.keyboard.press('Escape');
  await alice.locator('#more-open').click();
  expect(await smallTargets(alice), 'more menu').toEqual([]);
  await shot(alice, 'after-phone-more-menu');
  await alice.keyboard.press('Escape');

  await alice.locator('#tabbar').getByRole('button', { name: /People/ }).click();
  expect(await smallTargets(alice), 'people tab').toEqual([]);
  await shot(alice, 'after-phone-people');
  await alice.locator('#tabbar').getByRole('button', { name: /Chat/ }).click();

  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n\nShip it.\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  expect(await smallTargets(alice), 'document open').toEqual([]);
  await shot(alice, 'after-phone-doc');
  // 008-AC12: the document's ⋯ menu and its Notes pane.
  await alice.locator('#doc-more').click();
  expect(await smallTargets(alice), 'document menu').toEqual([]);
  await alice.keyboard.press('Escape');
  await alice.getByRole('button', { name: /Notes/ }).click();
  expect(await smallTargets(alice), 'notes pane').toEqual([]);
  await alice.getByRole('button', { name: '← Back to the document' }).click();
  // 008-AC12: the push explainer.
  await menuItem(alice, 'Notifications', 'me');
  await alice.locator('#push-toggle').click();
  await expect(alice.locator('#push-explain')).toBeVisible();
  expect(await smallTargets(alice), 'push explainer').toEqual([]);
  await alice.keyboard.press('Escape');
  await alice.keyboard.press('Escape');
  await alice.locator('#doc-close').click();

  // Room settings, as a room admin.
  const me = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  await alice.reload();
  for (const tab of ['general', 'invites', 'admins', 'glossary']) {
    await alice.goto(`/r/${room.id}#manage/${tab}`);
    await expect(alice.locator('#manage #room-panel')).toBeVisible();
    expect(await smallTargets(alice), 'settings ' + tab).toEqual([]);
    await shot(alice, 'after-phone-settings-' + tab);
  }

  const docs = await alice.context().newPage();
  await docs.setViewportSize(phone);
  await docs.goto(`/r/${room.id}/docs`);
  await expect(docs.locator('#documents')).toContainText('plan.md');
  expect(await smallTargets(docs), 'docs window').toEqual([]);

  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const join = await (await browser.newContext({ viewport: phone })).newPage();
  await join.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await expect(join.getByLabel('Your name')).toBeVisible();
  expect(await smallTargets(join), 'join').toEqual([]);
  await shot(join, 'after-phone-join');

  await alice.goto('/profile');
  await expect(alice.locator('#rooms li')).toHaveCount(1);
  expect(await smallTargets(alice), 'profile').toEqual([]);
  await shot(alice, 'after-phone-profile');

  const adm = await (await browser.newContext({ viewport: phone })).newPage();
  await adm.goto('/admin');
  await expect(adm.getByLabel('Admin token')).toBeVisible();
  expect(await smallTargets(adm), 'admin sign-in').toEqual([]);
  expect(await signIn(adm, adminToken, 'rooms')).toBe(201);
  await expect(adm.locator(`#room-${room.id}`)).toBeVisible();
  expect(await smallTargets(adm), 'admin rooms').toEqual([]);
  await shot(adm, 'after-phone-admin');
  for (const view of ['profiles', 'libraries', 'instance', 'sessions', 'audit']) {
    await adm.goto('/admin#' + view);
    await expect(adm.locator('main h2').first()).toBeVisible();
    expect(await smallTargets(adm), 'admin ' + view).toEqual([]);
  }

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const page = await (await browser.newContext({ viewport: phone })).newPage();
    await page.goto(await daemon.link);
    await expect(page.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
    for (const tab of ['rooms', 'instances', 'accounts', 'settings', 'monitoring', 'update']) {
      await page.locator('#tab-' + tab).click();
      expect(await smallTargets(page), 'daemon ' + tab).toEqual([]);
    }
    await shot(page, 'after-phone-daemon-update');
  } finally {
    daemon.cleanup();
  }
});

// 005-AC31: several files dropped at once, each with its row: ✓, or ✗ with a reason and Retry.
test('drag and drop upload, in the room and the docs window', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await expect(alice.locator('#docs-empty')).toBeVisible();
  await expect(alice.locator('#docs-empty')).toContainText('No documents yet. Drop files here or press +.');
  await alice.locator('#docs-pane').dispatchEvent('dragenter', { dataTransfer: await alice.evaluateHandle(() => { const d = new DataTransfer(); d.items.add(new File(['x'], 'x.md')); return d; }) });
  await expect(alice.locator('#docs-pane .drop-overlay')).toBeVisible();
  await expect(alice.locator('#docs-pane .drop-overlay')).toHaveText('Drop to add to the room');
  await shot(alice, 'after-desktop-dropping');
  await alice.locator('#docs-pane').dispatchEvent('dragleave', { dataTransfer: await alice.evaluateHandle(() => { const d = new DataTransfer(); d.items.add(new File(['x'], 'x.md')); return d; }) });
  await expect(alice.locator('#docs-pane .drop-overlay')).toBeHidden();

  await dropFiles(alice, '#docs-pane', [
    { name: 'brief.md', type: 'text/markdown', body: '# Brief\n' },
    { name: 'tool.exe', type: 'application/octet-stream', body: 'MZ' },
    { name: 'fake.pdf', type: 'application/pdf', body: 'not a pdf at all' },
  ]);
  const rows = alice.locator('#uploads li');
  await expect(rows.filter({ hasText: 'brief.md' }).locator('.state')).toHaveText('✓');
  await expect(alice.locator('#documents')).toContainText('brief.md');
  const exe = rows.filter({ hasText: 'tool.exe' });
  await expect(exe.locator('.why')).toHaveText("This file type isn't supported. Use PDF, Word, slides, images, Markdown or HTML.");
  const pdf = rows.filter({ hasText: 'fake.pdf' });
  await expect(pdf.locator('.state')).toHaveText('✗');
  await expect(pdf.locator('.why')).not.toBeEmpty();
  await shot(alice, 'after-desktop-uploads');
  // Retry sends it again: the server refuses it again, the row stays.
  const [again] = await Promise.all([alice.waitForResponse((r) => r.url().endsWith('/documents') && r.request().method() === 'POST'),
    pdf.getByRole('button', { name: 'Retry fake.pdf' }).click()]);
  expect(again.status()).toBe(415);
  await expect(rows.filter({ hasText: 'fake.pdf' }).locator('.state')).toHaveText('✗');
  await exe.getByRole('button', { name: 'Dismiss tool.exe' }).click();
  await expect(rows.filter({ hasText: 'tool.exe' })).toHaveCount(0);
  // A finished row clears itself; a failed one stays.
  await expect(rows.filter({ hasText: 'brief.md' })).toHaveCount(0, { timeout: 8000 });
  await expect(rows.filter({ hasText: 'fake.pdf' })).toHaveCount(1);
  await expect(alice.locator('#docs-empty')).toBeHidden();

  // The + button picks several files.
  await alice.locator('#upload-input').setInputFiles([
    { name: 'a.md', mimeType: 'text/markdown', buffer: Buffer.from('# A\n') },
    { name: 'b.md', mimeType: 'text/markdown', buffer: Buffer.from('# B\n') },
  ]);
  await expect(alice.locator('#documents')).toContainText('a.md');
  await expect(alice.locator('#documents')).toContainText('b.md');
  await expect(alice.getByRole('button', { name: 'Add documents' })).toBeVisible();

  // The docs window takes a drop anywhere.
  const docs = await alice.context().newPage();
  await docs.setViewportSize(desktop);
  await docs.goto(`/r/${room.id}/docs`);
  await expect(docs.locator('#documents')).toContainText('brief.md');
  await dropFiles(docs, 'body', [{ name: 'notes.md', type: 'text/markdown', body: '# Notes\n' }]);
  await expect(docs.locator('#documents')).toContainText('notes.md');
  await expect(alice.locator('#documents')).toContainText('notes.md');
});

// 005-AC32, 005-AC33, 005-AC34: the header's menus, the plain words, the empty states.
test('header menus, wording and empty states', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await expect(alice.locator('#me')).toHaveAttribute('data-initials', 'A');
  await expect(alice.locator('#chat-empty')).toBeVisible();
  await expect(alice.locator('#chat-empty')).toContainText('Invite people to start.');
  await alice.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await alice.getByRole('button', { name: 'Copy invite link' }).click();
  await expect(alice.locator('#chat-invite-status')).toHaveText('Copied. Paste it to the people you invite.');
  expect(await alice.evaluate(() => navigator.clipboard.readText())).toMatch(/#[A-Za-z0-9_-]+$/);

  await alice.locator('#me').click();
  await expect(alice.locator('#me-menu').getByRole('button')).toHaveText(['Change my name', 'Notifications', 'Use on another device']);
  await expect(alice.locator('#me-menu').getByRole('link')).toHaveText('My rooms and profile');
  await shot(alice, 'after-desktop-user-menu');
  await alice.keyboard.press('Escape');
  await alice.locator('#more-open').click();
  await expect(alice.locator('#more-menu').getByRole('button', { name: 'Invite people' })).toBeVisible();
  await expect(alice.locator('#more-menu').getByRole('button', { name: 'Personas' })).toBeVisible();
  await expect(alice.locator('#more-menu').getByRole('link', { name: 'Open documents in a new window' })).toBeVisible();
  await alice.keyboard.press('Escape');

  // No jargon in the room's own words: the header, its menus, the side panel and the composer.
  const words = await alice.evaluate(() => [...document.querySelectorAll('header, .menu, #side, #send, #chat-empty, #viewer, dialog')]
    .map((e) => e.innerText + ' ' + [...e.querySelectorAll('[placeholder], [aria-label], [title]')]
      .map((x) => [x.getAttribute('placeholder'), x.getAttribute('aria-label'), x.getAttribute('title')].join(' ')).join(' ')).join(' '));
  for (const bad of [/\bchain\b/i, /\bManage\b/, /Transcribing/, /\bDeafen\b/, /Upload a document/, /Download original/, /\bVolume\b/, /holds the agent/, /(?<!AI )\bhost\b/]) {
    expect(words, String(bad)).not.toMatch(bad);
  }
  await expect(alice.locator('#body')).toHaveAttribute('placeholder', 'Message the room · @ to mention');
  await alice.locator('#body').focus();
  await expect(alice.locator('#send-hint')).toHaveText('Enter sends, Shift+Enter adds a line.');

  // A room just created from the profile opens on its invite card.
  await alice.goto('/profile');
  await alice.getByRole('button', { name: 'New room' }).click();
  await alice.getByLabel('Room name').fill('Launch plan');
  await alice.getByRole('button', { name: 'Create the room' }).click();
  await expect(alice.locator('#invites')).toBeVisible();
  await expect(alice).toHaveURL(/\/r\/[^?]+$/);
  await alice.locator('#invites').getByRole('button', { name: 'Close' }).click();

  // Room settings: no raw id, one storage line, the delete button a danger outline from the start.
  await alice.locator('#more-open').click();
  await alice.locator('#more-menu').getByRole('button', { name: 'Room settings' }).click();
  const panel = alice.locator('#manage #room-panel');
  await expect(panel.locator('h2')).toHaveText('Launch plan');
  await expect(panel.locator('#storage')).toHaveText(/^Storage: 0 B of \d+ MB/);
  await expect(panel.getByRole('button', { name: 'Delete the room' })).toHaveClass('btn-danger-outline');
  await expect(panel.getByRole('button', { name: 'Copy room id' })).toBeVisible();
  await shot(alice, 'after-desktop-settings');
  void room;
});

// 005-AC35: a pill that cannot ask says why; the phone's People sheet; the viewer's one toolbar.
test('locked pill and viewer toolbar', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  const pill = alice.locator('button.persona').first();
  await expect(pill).toBeDisabled();
  await pill.click({ force: true });
  await expect(alice.locator('#why')).toBeVisible();
  await expect(alice.locator('#why-text')).toContainText('No one runs the AI for this room yet');
  await shot(alice, 'after-phone-why');
  await alice.locator('#why').getByRole('button', { name: 'OK' }).click();

  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await alice.setViewportSize(desktop);
  await expect(alice.locator('#viewer')).toBeVisible();
  const tools = await alice.locator('.viewer-head :is(button, a):visible').evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().height)));
  expect(new Set(tools)).toEqual(new Set([36]));
  await expect(alice.locator('.viewer-head > :first-child')).toHaveId('doc-close');
});

// 005-AC36: a phone shows one tab at a time under a bar; each keeps its scroll position, and a
// document picked opens in the Docs tab.
test('phone tab bar', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  await alice.evaluate(async (id) => {
    for (let i = 0; i < 40; i++) await fetch(`/api/rooms/${id}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body: 'line ' + i }) });
  }, room.id);
  await expect(alice.locator('.msg.text')).toHaveCount(40);
  const bar = alice.locator('#tabbar');
  await expect(bar).toBeVisible();
  const box = await bar.boundingBox();
  expect(Math.round(box.height)).toBe(56);
  expect(Math.round(box.y + box.height)).toBe(phone.height);
  await expect(alice.locator('.chat')).toBeVisible();
  await expect(alice.locator('#side')).toBeHidden();
  // A scroll right after the viewport resize above counts as the layout's: repeat it until it unpins.
  await expect.poll(() => alice.evaluate(async () => {
    messages.scrollTop = 200;
    await new Promise((r) => requestAnimationFrame(() => setTimeout(r)));
    return pinned;
  })).toBe(false);
  await bar.getByRole('button', { name: /Docs/ }).click();
  await expect(alice.locator('#docs-pane')).toBeVisible();
  await expect(alice.locator('#people-pane')).toBeHidden();
  await expect(alice.locator('.chat')).toBeHidden();
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  await expect(bar.getByRole('button', { name: /Docs/ })).toHaveAttribute('aria-pressed', 'true');
  await expect(alice.locator('#tab-docs-count')).toHaveText('1');
  await shot(alice, 'after-phone-docs-tab');
  await bar.getByRole('button', { name: /People/ }).click();
  await expect(alice.locator('#people-pane')).toBeVisible();
  await expect(alice.locator('#viewer')).toBeHidden();
  await bar.getByRole('button', { name: /Chat/ }).click();
  await expect(alice.locator('.chat')).toBeVisible();
  expect(await alice.locator('#messages').evaluate((m) => m.scrollTop)).toBe(200);
  await expect(alice.locator('#people-open')).toHaveCount(0);
});

// 005-AC37: annotations toggle from the viewer, closed by default below 100rem and remembered; the
// chat folds into a 56 px rail.
test('annotations panel and chat rail', async ({ browser, request }) => {
  const { room } = await roomWith(browser, request, ['Bob']);
  // A context of its own: roomWith's pages start with the panel open.
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const alice = await (await browser.newContext({ viewport: desktop })).newPage();
  await alice.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await alice.getByLabel('Your name').fill('Alice');
  await alice.getByRole('button', { name: 'Join the room' }).click();
  await expect(alice.locator('#me')).toHaveText('Alice');
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#annotations')).toBeHidden();
  await alice.getByRole('button', { name: /Notes/ }).click();
  await expect(alice.locator('#annotations')).toBeVisible();
  await alice.reload();
  await alice.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(alice.locator('#annotations')).toBeVisible();
  await alice.setViewportSize({ width: 1700, height: 900 });
  await alice.getByRole('button', { name: 'Fold the chat away' }).click();
  const rail = await alice.locator('.chat').boundingBox();
  expect(Math.round(rail.width)).toBe(56);
  await expect(alice.locator('#body')).toBeHidden();
  await shot(alice, 'after-desktop-chat-rail');
  await alice.getByRole('button', { name: 'Show the chat' }).first().click();
  await expect(alice.locator('#body')).toBeVisible();
});

// 005-AC38: one strip shows the most urgent state and "+N more"; feedback goes to toasts.
test('status strip and toasts', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await alice.evaluate(() => {
    showNotice('A notice.');
    document.getElementById('reconnecting').hidden = false;
  });
  await expect(alice.locator('#reconnecting')).toBeVisible();
  await expect(alice.locator('#notice')).toBeHidden();
  await expect(alice.locator('#strip-more')).toHaveText('+1 more');
  await alice.locator('#strip-more').click();
  await expect(alice.locator('#notice')).toBeVisible();
  await expect(alice.locator('#strip-more')).toHaveText('Show less');
  await alice.evaluate(() => { document.getElementById('reconnecting').hidden = true; });
  await expect(alice.locator('#strip-more')).toBeHidden();
  await expect(alice.locator('#notice')).toBeVisible();

  await alice.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await menuItem(alice, 'Invite people');
  await alice.getByRole('button', { name: 'Create an invite link' }).click();
  await alice.getByRole('button', { name: 'Copy invite', exact: true }).click();
  await expect(alice.locator('#toasts .toast')).toHaveText('Copied');
  await expect(alice.locator('#toast-live')).toHaveText('Copied');
  await expect(alice.locator('#toast-live')).toHaveAttribute('role', 'status');
  await expect(alice.locator('#toasts .toast')).toHaveCount(0, { timeout: 8000 });
});

// 005-AC39, 005-AC40: the Personas sheet: one primary Save, Remove a danger outline, the rest folded;
// a right-click on a pill opens it; a removal asks in the shared confirm sheet.
test('personas sheet and confirm sheet', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await alice.locator('button.persona').first().click({ button: 'right', force: true });
  const form = alice.locator('#personas .persona-form').first();
  await expect(form).toBeVisible();
  await expect(form.locator('button.btn-primary, button[type=submit]')).toHaveText(['Save']);
  await expect(form.getByRole('button', { name: /^Remove/ })).toHaveClass('btn-danger-outline');
  await expect(form.getByLabel('Speaks up every N answers')).toBeHidden();
  await form.locator('details.advanced summary').click();
  await expect(form.getByLabel('Speaks up every N answers')).toBeVisible();
  await expect(form.getByRole('button', { name: /library/ })).toBeHidden();
  await form.locator('details.more summary').click();
  await expect(form.getByRole('button', { name: /library/ })).toBeVisible();
  await shot(alice, 'after-desktop-personas');
  await form.getByRole('button', { name: /^Remove/ }).click();
  const sheet = alice.locator('dialog.confirm');
  await expect(sheet.locator('h2')).toHaveText('Remove Grill?');
  await sheet.getByRole('button', { name: 'Cancel' }).click();
  await expect(sheet).toHaveCount(0);
  await expect(alice.locator('button.persona')).toHaveCount(1);
  await form.getByRole('button', { name: /^Remove/ }).click();
  await alice.locator('dialog.confirm').getByRole('button', { name: 'Remove' }).click();
  await expect(alice.locator('button.persona')).toHaveCount(0);
});

// 005-AC41: the profile is a home page: room cards with people, activity and the admin badge; New
// room in a sheet.
test('profile home page', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await alice.goto('/profile');
  const card = alice.locator('#rooms li.card').first();
  await expect(card.locator('.room-line')).toHaveText('Docs room as Alice');
  await expect(card).toContainText('1 person · active');
  await expect(card.locator('time')).toHaveAttribute('title', /\d/);
  await expect(alice.locator('#profile-name')).toHaveText('Alice');
  await expect(alice.locator('#skills')).not.toHaveAttribute('open', '');
  await alice.getByRole('button', { name: 'New room' }).click();
  await expect(alice.locator('#room-create-sheet')).toBeVisible();
  await expect(alice.getByLabel('Your name in the room')).toHaveValue('Alice');
  await shot(alice, 'after-desktop-profile-new');
  await alice.getByLabel('Room name').fill('Launch plan');
  await alice.getByRole('button', { name: 'Create the room' }).click();
  await expect(alice.locator('#invites')).toBeVisible();
  await alice.goto('/profile');
  await expect(alice.locator('#rooms li.card', { hasText: 'Launch plan' }).locator('.badge')).toHaveText('admin');
  await shot(alice, 'after-desktop-profile');
  await alice.setViewportSize(phone);
  expect(await smallTargets(alice), 'profile').toEqual([]);
});

// 005-AC43: /admin's rooms: a search box, relative dates, the import in a sheet, cards on a phone.
test('admin rooms list', async ({ browser, request }) => {
  const { room } = await roomWith(browser, request, ['Alice']);
  const adm = await (await browser.newContext({ viewport: desktop })).newPage();
  expect(await signIn(adm, adminToken, 'rooms')).toBe(201);
  const row = adm.locator(`#room-${room.id}`);
  await expect(row.locator('time').first()).toHaveAttribute('title', /\d/);
  await expect(row.locator('time').first()).toHaveText(/ago|just now/);
  await adm.getByLabel('Search rooms').fill('no such room');
  await expect(row).toHaveCount(0);
  await adm.getByLabel('Search rooms').fill('docs');
  await expect(row).toBeVisible();
  await adm.getByRole('button', { name: 'Import a room' }).click();
  await expect(adm.locator('#import-sheet')).toBeVisible();
  await shot(adm, 'after-desktop-admin-import');
  await adm.keyboard.press('Escape');
  await adm.setViewportSize(phone);
  await expect(row.locator('td').first()).toHaveCSS('display', 'block');
  expect(await smallTargets(adm), 'admin rooms').toEqual([]);
});

// 005-AC44: the join page names its action and shows a missing name under the field.
test('join page', async ({ browser, request }) => {
  const r = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Join room' } })).json();
  const inv = await (await request.post(`/api/rooms/${r.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext({ viewport: phone })).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await expect(page).toHaveTitle('Join the room · grill-room');
  await expect(page.locator('main h1')).toHaveText('Join the room');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#name-error')).toHaveText('Type the name the others will see, up to 40 characters.');
  await expect(page.getByLabel(/Your name/)).toHaveAttribute('aria-invalid', 'true');
  await shot(page, 'after-phone-join-error');
  await page.getByLabel(/Your name/).fill('Zoe');
  await expect(page.locator('#name-error')).toBeEmpty();
});

// 005-AC35: a long name ends with an ellipsis instead of breaking.
test('participant names never break mid-word, roles stay two badges', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alexandrina-Wilhelmina']);
  await alice.setViewportSize(desktop);
  const me = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  await alice.reload();
  const row = alice.locator('#participants li').first();
  await expect(row.locator('.tag:not(.blank):is(.role-you, .role-admin)')).toHaveCount(2);
  const name = row.locator('.name');
  const [lines, clipped] = await name.evaluate((e) => [Math.round(e.getBoundingClientRect().height / parseFloat(getComputedStyle(e).lineHeight)), e.scrollWidth > e.clientWidth]);
  expect(lines).toBe(1);
  expect(clipped).toBe(true);
  await expect(name).toHaveCSS('text-overflow', 'ellipsis');
  await menuItem(alice, 'Personas');
  await expect(alice.locator('#personas')).toBeVisible();
});

// 008-AC12: the new controls at 1440 px share the one control height, the handles show the focus
// ring, and nothing new moves under reduced motion.
test('docs panes follow the design rules', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await upload(alice, 'plan.md', 'text/markdown', '# Plan\n');
  await expect(alice.locator('#viewer')).toBeVisible();
  const heights = await alice.locator('.viewer-head button:visible').evaluateAll((els) => els.map((e) => [e.id, Math.round(e.getBoundingClientRect().height)]));
  expect(heights.map(([id]) => id)).toEqual(expect.arrayContaining(['doc-more', 'doc-width']));
  expect(new Set(heights.map(([, h]) => h)), JSON.stringify(heights)).toEqual(new Set([36]));
  // The drawer headers' small actions: one 24 px size (008-AC14).
  const heads = await alice.locator('.pane-head :is(button, a):visible').evaluateAll((els) => els.map((e) => [e.id, Math.round(e.getBoundingClientRect().height)]));
  expect(heads.map(([id]) => id)).toEqual(['people-fold', 'docs-fold', 'docs-window', 'upload-open']);
  expect(new Set(heads.map(([, h]) => h)), JSON.stringify(heads)).toEqual(new Set([24]));
  await alice.emulateMedia({ reducedMotion: 'reduce' });
  for (const id of ['side-handle', 'notes-handle']) {
    await alice.locator('#' + id).focus();
    const s = await alice.locator('#' + id).evaluate((e) => { const c = getComputedStyle(e); return [c.outlineStyle, c.outlineWidth, c.transitionDuration]; });
    expect(s, id).toEqual(['solid', '2px', '0s']);
  }
  await alice.locator('#people-fold').click();
  await alice.locator('#docs-fold').click();
  expect(await alice.locator('#side').evaluate((e) => getComputedStyle(e).transitionDuration)).toBe('0s');
  await shot(alice, 'after-desktop-rail');
  await menuItem(alice, 'Notifications', 'me');
  await alice.locator('#push-toggle').click();
  await expect(alice.locator('#push-explain')).toBeVisible();
  const sheet = await alice.locator('#push-explain button:visible').evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().height)));
  expect(new Set(sheet)).toEqual(new Set([36]));
});
