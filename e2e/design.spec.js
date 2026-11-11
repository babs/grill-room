import { test, expect } from './fixtures.js';
import { mkdirSync } from 'node:fs';
import { adminToken } from './playwright.config.js';
import { roomWith, signIn, upload, startDaemon, openDaemonPage, smallTargets, menuItem, playSTT, newRoomEditor, personaAction, unfoldTabs, dropFiles } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };
// SHOTS=<dir> keeps a screenshot of each screen checked: the before and after of the UX audit.
const shots = process.env.SHOTS;
if (shots) mkdirSync(shots, { recursive: true });
const shot = (page, name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : null);

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
  // The pane's voice lines move back once the resize is observed: measure after it.
  await expect(alice.locator('body:not(.transcript-room)')).toBeAttached();
  expect(await smallTargets(alice), 'room').toEqual([]);
  await shot(alice, 'after-phone-room');

  await alice.locator('#me').click();
  expect(await smallTargets(alice), 'user menu').toEqual([]);
  await alice.keyboard.press('Escape');
  await alice.locator('#room-open').click();
  expect(await smallTargets(alice), 'room menu').toEqual([]);
  await shot(alice, 'after-phone-room-menu');
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
  await alice.getByRole('button', { name: 'Back to the document' }).click();
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
  await playSTT(alice, room.id, { engines: ['whisper'], glossary: true });
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
    await openDaemonPage(page, await daemon.link);
    await expect(page.locator('#pairing')).toHaveText('Waiting for your approval in the room.', { timeout: 30_000 });
    for (const tab of ['rooms', 'accounts', 'general', 'network', 'monitoring', 'update']) {
      await unfoldTabs(page.locator('#tabs'));
      await page.locator('#tab-' + tab).click();
      expect(await smallTargets(page), 'daemon ' + tab).toEqual([]);
    }
    await shot(page, 'after-phone-daemon-update');
  } finally {
    daemon.cleanup();
  }
});

// 005-AC17: the daemon's tab bar looks like tabs at both widths. Tabs are found by the nav's
// role, not by the .tabs class: v0.0.10 to v0.0.15 shipped the nav without that class.
test('daemon tab bar keeps its tab styling', async ({ browser, request, baseURL }) => {
  const { room } = await roomWith(browser, request, ['Alice']);
  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    const page = await (await browser.newContext()).newPage();
    await openDaemonPage(page, await daemon.link);
    await page.locator('#tab-accounts').click();
    for (const viewport of [phone, desktop]) {
      await page.setViewportSize(viewport);
      const at = `${viewport.width}px`;
      // A phone folds the bar into the header's ☰, named after the current tab.
      const menu = page.locator('.app-header > .tabs-menu');
      if (viewport === phone) {
        await expect(menu).toHaveAccessibleName('AI accounts');
        expect((await menu.boundingBox()).height, at).toBeGreaterThanOrEqual(43.5);
        await expect(page.locator('nav[aria-label=Sections]')).toBeHidden();
      } else {
        await expect(menu).toBeHidden();
        const tabs = await page.locator('nav[aria-label=Sections] a').evaluateAll((els) => els.map((e) => {
          const r = e.getBoundingClientRect();
          const s = getComputedStyle(e);
          return { name: e.id, current: e.getAttribute('aria-current') === 'page', left: r.left, right: r.right, height: r.height,
            pad: Math.min(parseFloat(s.paddingLeft), parseFloat(s.paddingRight)), underline: s.textDecorationLine.includes('underline'),
            border: parseFloat(s.borderBottomWidth) >= 2 && s.borderBottomStyle !== 'none' ? s.borderBottomColor : 'none' };
        }));
        const why = `${at} ${JSON.stringify(tabs)}`;
        expect(tabs.length, why).toBe(6);
        expect(tabs.filter((t) => t.height < 43.5).map((t) => t.name), why).toEqual([]);
        expect(tabs.filter((t) => t.underline).map((t) => t.name), why).toEqual([]);
        const cramped = tabs.slice(1).filter((t, i) => {
          const gap = t.left - tabs[i].right;
          return gap < 8 && !(gap >= 0 && t.pad >= 8 && tabs[i].pad >= 8);
        });
        expect(cramped.map((t) => t.name), why).toEqual([]);
        const [current, ...others] = [...tabs.filter((t) => t.current), ...tabs.filter((t) => !t.current)];
        expect(current.name, why).toBe('tab-accounts');
        expect(current.border, why).not.toMatch(/^none$|rgba\(0, 0, 0, 0\)/);
        expect(others.filter((t) => t.border === current.border).map((t) => t.name), why).toEqual([]);
      }
      // Stuck at the top, the bar keeps 8 px between itself and the panel's first content.
      await page.setViewportSize({ width: viewport.width, height: 400 });
      const stuck = await page.evaluate(() => {
        const nav = /** @type {HTMLElement} */ (document.querySelector('#tabs'));
        // A short panel cannot scroll the bar into its stuck place: room below the content lets it.
        document.body.style.paddingBottom = '100vh';
        scrollTo(0, nav.offsetTop);
        const first = [...document.querySelectorAll('.panel:not([hidden]) *')].find((e) => e.getBoundingClientRect().height > 0);
        const ns = getComputedStyle(nav);
        return { edge: ns.boxShadow !== 'none' || parseFloat(ns.borderBottomWidth) > 0, top: nav.getBoundingClientRect().top, gap: (first?.getBoundingClientRect().top ?? 0) - nav.getBoundingClientRect().bottom };
      });
      expect(stuck.top, at).toBeLessThan(1);
      expect(stuck.edge, 'the stuck bar has a bottom border or shadow').toBe(true);
      expect(stuck.gap, at).toBeGreaterThanOrEqual(8);
      await page.evaluate(() => { document.body.style.paddingBottom = ''; scrollTo(0, 0); });
    }
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
  await expect(rows.filter({ hasText: 'brief.md' }).locator('.state')).toHaveAttribute('aria-label', 'Added');
  await expect(alice.locator('#documents')).toContainText('brief.md');
  const exe = rows.filter({ hasText: 'tool.exe' });
  await expect(exe.locator('.why')).toHaveText("This file type isn't supported. Use PDF, Word, slides, images, Markdown or HTML.");
  const pdf = rows.filter({ hasText: 'fake.pdf' });
  await expect(pdf.locator('.state')).toHaveAttribute('aria-label', 'Not added');
  await expect(pdf.locator('.why')).not.toBeEmpty();
  await shot(alice, 'after-desktop-uploads');
  // Retry sends it again: the server refuses it again, the row stays.
  const [again] = await Promise.all([alice.waitForResponse((r) => r.url().endsWith('/documents') && r.request().method() === 'POST'),
    pdf.getByRole('button', { name: 'Retry fake.pdf' }).click()]);
  expect(again.status()).toBe(415);
  await expect(rows.filter({ hasText: 'fake.pdf' }).locator('.state')).toHaveAttribute('aria-label', 'Not added');
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
  await expect(alice.locator('#me-menu').getByRole('button')).toHaveText(['Change my name', 'Notifications', 'Text size', 'Use on another device', 'Diagnostics']);
  await expect(alice.locator('#me-menu').getByRole('link')).toHaveText('My rooms and profile');
  await shot(alice, 'after-desktop-user-menu');
  await alice.keyboard.press('Escape');
  await alice.locator('#room-open').click();
  await expect(alice.locator('#room-menu').getByRole('button', { name: 'Invite people' })).toBeVisible();
  await expect(alice.locator('#room-menu').getByRole('button', { name: 'Personas' })).toBeVisible();
  await expect(alice.locator('#room-menu').getByRole('link', { name: 'Open documents in a new window' })).toBeVisible();
  // The room menu hangs under the room's name, on the left (005-AC32).
  expect((await alice.locator('#room-menu').boundingBox()).x).toBeLessThan(16);
  await alice.keyboard.press('Escape');

  // No jargon in the room's own words: the header, its menus, the side panel and the composer.
  const words = await alice.evaluate(() => [...document.querySelectorAll('header, .menu, #side, #send, #chat-empty, #viewer, dialog')]
    .map((e) => e.innerText + ' ' + [...e.querySelectorAll('[placeholder], [aria-label], [data-tip]')]
      .map((x) => [x.getAttribute('placeholder'), x.getAttribute('aria-label'), x.getAttribute('data-tip')].join(' ')).join(' ')).join(' '));
  for (const bad of [/\bchain\b/i, /\bManage\b/, /Transcribing/, /\bDeafen\b/, /Upload a document/, /Download original/, /\bVolume\b/, /holds the agent/, /(?<!AI )\bhost\b/]) {
    expect(words, String(bad)).not.toMatch(bad);
  }
  await expect(alice.locator('#body')).toHaveAttribute('placeholder', 'Message the room · @ to mention');
  await alice.locator('#body').focus();
  await expect(alice.locator('#send-hint')).toHaveText('Enter sends, Shift+Enter adds a line.');

  // A room just created from the profile opens on its invite card.
  await alice.goto('/profile');
  await newRoomEditor(alice);
  await alice.getByLabel('Room name').fill('Launch plan');
  await alice.getByRole('button', { name: 'Create room' }).click();
  await expect(alice.locator('#invites')).toBeVisible();
  await expect(alice).toHaveURL(/\/r\/[^?]+$/);
  await alice.locator('#invites').getByRole('button', { name: 'Close' }).click();

  // Room settings: no raw id, one storage line, the delete button a danger outline from the start.
  await alice.locator('#room-open').click();
  await alice.locator('#room-menu').getByRole('button', { name: 'Room settings' }).click();
  const panel = alice.locator('#manage #room-panel');
  // One heading names the room (004b-AC11); the storage is said once (AD5).
  await expect(alice.locator('#manage-title')).toHaveText('Room settings · Launch plan');
  await expect(panel.locator('h2')).toHaveCount(0);
  await expect(panel.locator('#storage')).toHaveText(/^\s*0 B of \d+ MB/);
  await expect(panel.getByRole('button', { name: 'Delete the room' })).toHaveClass('btn-danger-outline');
  await expect(panel.getByRole('button', { name: 'Copy room id' })).toBeVisible();
  await shot(alice, 'after-desktop-settings');
  void room;
});

// 005-AC35, 013-AC39: a pill's ▶ that cannot ask says why; the phone's People sheet; the viewer's one toolbar.
test('locked pill and viewer toolbar', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(phone);
  const pill = alice.locator('.persona-pill .persona-run').first();
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
  // The document's head, not the Video stage's (030), which has its own.
  await expect(alice.locator('#viewer > .viewer-head > :first-child')).toHaveId('doc-close');
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
    await new Promise((r) => requestAnimationFrame(() => setTimeout(r))); // one frame, then the scroll event's task
    return isPinned(messages);
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
  await alice.locator('#invites').getByRole('button', { name: 'Copy', exact: true }).click();
  await expect(alice.locator('#toasts .toast')).toHaveText('Copied');
  await expect(alice.locator('#toast-live')).toHaveText('Copied');
  await expect(alice.locator('#toast-live')).toHaveAttribute('role', 'status');
  // Each message is a node of its own: atomic, a status region would read them all again each time.
  await expect(alice.locator('#toast-live')).toHaveAttribute('aria-atomic', 'false');
  await expect(alice.locator('#toasts .toast')).toHaveCount(0, { timeout: 8000 });
});

// 005-AC39, 005-AC40, 009-AC3, 009-AC4: a right-click on a pill opens that persona's sheet on
// Identity; one primary Save; the rare actions in ⋯; a removal asks in the shared confirm sheet.
test('personas sheet and confirm sheet', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await alice.locator('button.persona').first().click({ button: 'right', force: true });
  const sheet = alice.locator('#persona-sheet');
  await expect(sheet.locator('#persona-sheet-title')).toHaveText('🔥 Grill');
  await expect(alice).toHaveURL(/#personas\/\d+\/identity$/);
  await expect(sheet.locator('.btn-primary, button[type=submit]')).toHaveText(['Save']);
  await expect(sheet.getByLabel('Speaks up')).toBeHidden();
  await sheet.getByRole('link', { name: 'Triggers', exact: true }).click();
  await expect(sheet.getByLabel('Speaks up')).toBeVisible();
  await expect(sheet.getByRole('button', { name: /library|^Remove/ })).toHaveCount(0);
  await sheet.locator('#persona-more').click();
  const menu = alice.locator('#persona-menu');
  await expect(menu.getByRole('button')).toHaveText(['Save to the library', 'Remove']);
  await expect(menu.getByRole('button', { name: 'Remove Grill' })).toHaveClass('danger');
  await shot(alice, 'after-desktop-personas');
  await menu.getByRole('button', { name: 'Remove Grill' }).click();
  const confirm = alice.locator('dialog.confirm');
  await expect(confirm.locator('h2')).toHaveText('Remove Grill?');
  await confirm.getByRole('button', { name: 'Cancel' }).click();
  await expect(confirm).toHaveCount(0);
  await expect(alice.locator('button.persona')).toHaveCount(1);
  await personaAction(alice, 'Remove Grill');
  await alice.locator('dialog.confirm').getByRole('button', { name: 'Remove' }).click();
  await expect(alice.locator('button.persona')).toHaveCount(0);
  // A removed persona's sheet closes, and its deep link goes with it.
  await expect(sheet).toBeHidden();
  await expect(alice).not.toHaveURL(/#personas/);
});

// 005-AC41: the profile is a home page: room cards with people, activity and the admin badge; New
// room in a sheet.
test('profile home page', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize(desktop);
  await alice.goto('/profile');
  const card = alice.locator('#rooms li.card').first();
  await expect(card.locator('.room-line')).toHaveText('Docs room');
  await expect(card).toContainText('1 person · active');
  await expect(card.locator('time')).toHaveAttribute('data-tip', /\d/);
  await expect(alice.locator('#profile-name')).toHaveText('Alice');
  await newRoomEditor(alice);
  await expect(alice.locator('#room-create-sheet')).toBeVisible();
  await expect(alice.getByLabel('Your name in the room')).toHaveValue('Alice');
  await shot(alice, 'after-desktop-profile-new');
  await alice.getByLabel('Room name').fill('Launch plan');
  await alice.getByRole('button', { name: 'Create room' }).click();
  await expect(alice.locator('#invites')).toBeVisible();
  await alice.goto('/profile');
  await expect(alice.locator('#rooms li.card', { hasText: 'Launch plan' }).locator('.badge')).toHaveText('Admin');
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
  await expect(row.locator('time').first()).toHaveAttribute('data-tip', /\d/);
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
  // A row turns into a card: its values on one wrapped line under the name (004b-AC5).
  await expect(row.locator('.cells')).toHaveCSS('display', 'flex');
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
test('participant names never break mid-word', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alexandrina-Wilhelmina']);
  await alice.setViewportSize(desktop);
  const me = await alice.evaluate((id) => fetch(`/api/rooms/${id}`).then((r) => r.json()), room.id);
  expect((await request.put(`/api/rooms/${room.id}/admins/${me.me.id}`, { headers: admin })).status()).toBe(204);
  await alice.reload();
  const row = alice.locator('#participants li').first();
  await expect(row.locator('.sigil')).toHaveText('@');
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
  // The drawer headers' small actions: one 24 px size (008-AC14), the Transcription pane's fold too (020-AC12).
  const heads = await alice.locator('.pane-head :is(button, a):visible').evaluateAll((els) => els.map((e) => [e.id, Math.round(e.getBoundingClientRect().height)]));
  expect(heads.map(([id]) => id)).toEqual(['people-fold', 'docs-fold', 'docs-window', 'upload-open', 'transcript-fold']);
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

// overlaps names what crosses in the agents bar: two pills, as far as their scrolling bar shows
// them; the auto-turn count, the header warning and the status words; whatever else is drawn at
// their centre; the count or the warning cut; and a pill drawn past its bar's edges, which the bar
// no longer clips.
const overlaps = (page) => page.evaluate(() => {
  const bar = document.getElementById('persona-buttons').getBoundingClientRect();
  const pills = [...document.querySelectorAll('.zone-agents .persona-pill')].map((e) => {
    const r = e.getBoundingClientRect();
    const [left, right] = [Math.max(r.left, bar.left), Math.min(r.right, bar.right)];
    return [e.textContent.trim().slice(0, 12), { left, right, width: right - left }];
  });
  const out = [];
  // Only the status may end in an ellipsis; a count shrunk to nothing is cut too.
  for (const e of document.querySelectorAll('#chain-count:not([hidden]), #agent-warn:not([hidden])')) {
    if (e.scrollWidth > e.clientWidth) out.push(`${e.id} cut to ${e.clientWidth} of ${e.scrollWidth} px`);
  }
  const own = [...document.querySelectorAll('#chain-count:not([hidden]), #agent-warn, #agent-status:not(.sr-only)')].filter((e) => e.getBoundingClientRect().width > 1);
  for (const group of [pills, own.map((e) => [e.id, e.getBoundingClientRect()])]) {
    const shown = group.filter(([, r]) => r.width > 0);
    for (const [i, [a, r]] of shown.entries()) for (const [b, s] of shown.slice(i + 1)) {
      if (r.left < s.right - 0.5 && s.left < r.right - 0.5) out.push(`${a} [${Math.round(r.left)}, ${Math.round(r.right)}] × ${b} [${Math.round(s.left)}, ${Math.round(s.right)}]`);
    }
  }
  for (const e of own) {
    const r = e.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    if (!e.contains(hit)) out.push(`${e.id} drawn over by ${hit?.closest('.persona-pill')?.textContent.trim().slice(0, 12) || hit?.tagName}`);
  }
  for (const e of document.querySelectorAll('.zone-agents .persona-pill')) {
    const r = e.getBoundingClientRect();
    for (const [from, to] of [[r.left, Math.min(r.right, bar.left)], [Math.max(r.left, bar.right), r.right]]) {
      for (let x = from + 1; x < to - 1; x += 4) {
        if (e.contains(document.elementFromPoint(x, r.top + r.height / 2))) {
          out.push(`${e.textContent.trim().slice(0, 12)} drawn past its bar at ${Math.round(x)}`);
          break;
        }
      }
    }
  }
  return out;
});

// The header warning: with nobody running the AI, the agents bar shows an alert icon, its message
// in the styled tip and its name, never the words over the pills. A long room name squeezes the
// bar as the audio controls do: the pills keep their width, nothing crosses at 1024, 1280, 1440 px,
// a persona answering included. A failed action shows its message there, in the notice on a phone.
test('the header warning keeps the agents bar apart', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  expect((await request.patch(`/api/rooms/${room.id}`, { headers: admin, data: { name: 'A room name long enough to take the header from the personas' } })).status()).toBe(200);
  const added = await alice.evaluate(async (id) => {
    const tpls = await (await fetch(`/api/rooms/${id}/templates`)).json();
    const out = [];
    for (const n of ['Secretary', 'Rodin']) {
      const r = await fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ template_id: tpls.find((t) => t.name === n).id }) });
      out.push((await r.json()).id);
    }
    return out;
  }, room.id);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(3);
  const warn = alice.locator('#agent-warn');
  const status = alice.locator('#agent-status');
  const offline = 'No one runs the AI for this room right now.';
  for (const width of [1024, 1280, 1440]) {
    await alice.setViewportSize({ width, height: 800 });
    // 64rem and under the bar has no status: the pills alone.
    await expect(warn).toBeVisible({ visible: width > 1024 });
    await expect(status).toHaveText(offline);
    if (width > 1024) expect((await status.boundingBox()).width).toBeLessThanOrEqual(1);
    expect(await overlaps(alice), `${width} px`).toEqual([]);
  }
  await expect(warn).toHaveAccessibleName('AI warning');
  await expect(warn).toHaveAccessibleDescription(offline);
  await warn.hover();
  await expect(alice.locator('#person-tip')).toHaveText(offline);
  await alice.mouse.move(0, 400);
  await expect(alice.locator('#person-tip')).toBeHidden();
  // The keyboard reaches it after the personas and gets the tip at once.
  await alice.locator('.persona-pill').last().locator('.persona-run').focus();
  await alice.keyboard.press('Tab');
  await expect(warn).toBeFocused();
  await expect(alice.locator('#person-tip')).toHaveText(offline);

  // A persona answering stays words, never drawn over the pills: whole, or cut with an ellipsis
  // at no less than 6rem, the pills scrolling for it, whatever the font's metrics.
  await alice.evaluate((ids) => { agent = { ...agent, online: true, running: ids.map((id) => ({ persona_id: id })) }; renderAgent(); }, added);
  await expect(status).toHaveText('Secretary answering, Rodin answering…');
  await expect(warn).toBeHidden();
  for (const width of [1025, 1200, 1280, 1440, 1920]) {
    await alice.setViewportSize({ width, height: 800 });
    await expect(status, `${width} px`).toBeVisible();
    // The floor takes from the pills, never from the page: the header keeps the window's width.
    expect(await alice.evaluate(() => [document.documentElement.scrollWidth, innerWidth]), `${width} px`).toEqual([width, width]);
    expect(await alice.locator('.room-header').evaluate((h) => h.scrollWidth - h.clientWidth), `${width} px`).toBe(0);
    const s = await status.evaluate((e) => ({ shown: e.clientWidth, whole: e.scrollWidth, cut: getComputedStyle(e).textOverflow,
      floor: 6 * parseFloat(getComputedStyle(document.documentElement).fontSize) }));
    expect(s.shown, `${width} px`).toBeGreaterThanOrEqual(Math.min(s.floor, s.whole));
    if (s.whole > s.shown) expect(s.cut, `${width} px`).toBe('ellipsis');
    expect(await overlaps(alice), `${width} px`).toEqual([]);
  }

  // A failed action on the AI: the warning carries its message.
  await alice.evaluate(() => agentError('This answer already stopped.'));
  await expect(warn).toBeVisible();
  await expect(warn).toHaveAccessibleDescription('This answer already stopped.');
  await expect(alice.locator('#notice')).toBeHidden();
  await alice.setViewportSize(phone);
  await alice.evaluate(() => agentError('This answer already stopped.'));
  await expect(alice.locator('#notice-text')).toHaveText('This answer already stopped.');
});

// The header warning stays in the agents bar's box while the pills scroll under it, so a failed
// action always reaches it; where the bar has no room for it, the notice says it instead.
test('the header warning stays in sight past many personas', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.evaluate(async (id) => {
    const tpls = await (await fetch(`/api/rooms/${id}/templates`)).json();
    // Compact pills take half the width (034-AC1): eight more make the bar overflow.
    for (const n of ['Secretary', 'Rodin', 'Secretary', 'Rodin', 'Secretary', 'Rodin', 'Secretary', 'Rodin']) {
      await fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ template_id: tpls.find((t) => t.name === n).id }) });
    }
  }, room.id);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(9);
  const warn = alice.locator('#agent-warn');
  const inZone = () => alice.evaluate(() => {
    const r = document.getElementById('agent-warn').getBoundingClientRect();
    const z = document.getElementById('agent').getBoundingClientRect();
    return r.width > 0 && r.left >= z.left - 0.5 && r.right <= z.right + 0.5;
  });
  for (const width of [1025, 1200]) {
    await alice.setViewportSize({ width, height: 800 });
    await expect(warn).toBeVisible();
    // The pills overflow their bar: it scrolls, the warning stays beside it.
    expect(await alice.locator('#persona-buttons').evaluate((z) => z.scrollWidth > z.clientWidth), `${width} px`).toBe(true);
    expect(await overlaps(alice), `${width} px`).toEqual([]);
    expect(await inZone(), `${width} px`).toBe(true);
    await alice.locator('#persona-buttons').evaluate((z) => { z.scrollLeft = z.scrollWidth; });
    expect(await inZone(), `${width} px, scrolled to the end`).toBe(true);
    await alice.evaluate((w) => agentError('Could not stop it at ' + w), width);
    await expect(warn).toHaveAccessibleDescription('Could not stop it at ' + width);
    await expect(alice.locator('#notice')).toBeHidden();
  }
  // The auto-turn count beside the overflowing pills keeps its words whole.
  await alice.setViewportSize({ width: 1200, height: 800 });
  await alice.evaluate(() => { agent = { ...agent, chain: { ...agent.chain, count: 3, budget: 25 } }; renderChain(); });
  await expect(alice.locator('#chain-count')).toHaveText('auto 3/25');
  expect(await overlaps(alice)).toEqual([]);
  await alice.setViewportSize({ width: 1024, height: 800 });
  await alice.evaluate(() => agentError('Could not stop it here'));
  await expect(alice.locator('#notice-text')).toHaveText('Could not stop it here');
});

// addPersonas adds n personas to the room from its templates, in turn; a name taken gets "-2".
const addPersonas = (page, id, n) => page.evaluate(async ([id, n]) => {
  const tpls = await (await fetch(`/api/rooms/${id}/templates`)).json();
  for (let i = 0; i < n; i++) {
    await fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ template_id: tpls[i % tpls.length].id }) });
  }
}, [id, n]);

// barState reads the persona bar: its mode on each pill, its scroll, and each "+N" with the pills
// the fades leave out of the clear part of the bar (034-AC4).
const barState = (page) => page.evaluate(() => {
  const bar = document.getElementById('persona-buttons');
  const r = bar.getBoundingClientRect();
  const edge = (side) => document.querySelector('.persona-more.' + side);
  const shown = (side) => !edge(side).hidden && edge(side).getBoundingClientRect().width > 0;
  const [left, right] = [shown('left') ? r.left + 44 : r.left, shown('right') ? r.right - 44 : r.right];
  const pills = [...bar.children].map((e) => e.getBoundingClientRect());
  return {
    modes: [...bar.children].map((e) => getComputedStyle(e.querySelector('.pname')).display === 'none'),
    scroll: bar.scrollLeft, max: bar.scrollWidth - bar.clientWidth,
    hiddenLeft: pills.filter((p) => p.left < left - 0.5).length,
    hiddenRight: pills.filter((p) => p.right > right + 0.5).length,
    width: bar.clientWidth,
    left: shown('left') ? edge('left').getAttribute('aria-label') : null,
    right: shown('right') ? edge('right').getAttribute('aria-label') : null,
  };
});
// bandHits names the pills that take a pointer where an edge fades, as far as its mask's ramp
// reaches, read from the drawn mask: "transparent <a>, opaque <b>, opaque calc(100% - <c>)" (034-AC4).
const bandHits = (page) => page.evaluate(() => {
  const el = document.getElementById('persona-buttons');
  const bar = el.getBoundingClientRect();
  const stops = [...getComputedStyle(el).maskImage.matchAll(/\)\s+(?:calc\(100% - )?([\d.]+)px/g)].map((m) => parseFloat(m[1]));
  const ramp = { left: stops[1] || 0, right: stops[2] || 0 };
  const y = bar.top + bar.height / 2;
  const out = [];
  for (const side of ['left', 'right']) {
    if (document.querySelector('.persona-more.' + side).hidden) continue;
    if (!ramp[side]) out.push(`${side}: no fade`);
    for (let d = 1; d < ramp[side]; d += 2) {
      const pill = document.elementFromPoint(side === 'left' ? bar.left + d : bar.right - d, y)?.closest('.persona-pill');
      if (pill) out.push(`${side} +${d}: ${pill.dataset.name}`);
    }
  }
  return out;
});
const settle = (page) => page.evaluate(() => new Promise((ok) => requestAnimationFrame(() => requestAnimationFrame(ok))));
const more = (n, side) => `${n} more persona${n > 1 ? 's' : ''}, scroll ${side}`;

// 034-AC1 to 034-AC7: nine personas. Every pill turns compact together where the full ones do not
// fit; the bar scrolls with a "+N" on each edge that hides pills, carrying the strongest signal it
// hides; the room's name keeps 6rem and nothing crosses in the agents bar.
test('the persona bar turns compact and shows what it hides', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  // A press scrolls at once: its distance reads without waiting out a smooth scroll.
  await alice.emulateMedia({ reducedMotion: 'reduce' });
  await addPersonas(alice, room.id, 8);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(9);
  const bar = alice.locator('#persona-buttons');
  const rem = await alice.evaluate(() => parseFloat(getComputedStyle(document.documentElement).fontSize));
  // 034-AC6: the room's name shows at least 6rem, or whole when shorter, past what its heading
  // clips: what is shown over that floor.
  const nameShown = () => alice.locator('#room-open .name').evaluate((e) => {
    const [n, h] = [e.getBoundingClientRect(), e.closest('h1').getBoundingClientRect()];
    const floor = Math.min(6 * parseFloat(getComputedStyle(document.documentElement).fontSize), e.scrollWidth);
    return Math.min(n.right, h.right) - Math.max(n.left, h.left) - floor;
  });
  for (const width of [390, 768, 1024, 1280, 1600]) {
    await alice.setViewportSize({ width, height: 900 });
    expect(await nameShown(), `${width} px, short name`).toBeGreaterThanOrEqual(-0.5);
  }
  expect((await request.patch(`/api/rooms/${room.id}`, { headers: admin, data: { name: 'A room name long enough to take the header from the personas' } })).status()).toBe(200);
  await expect(alice.locator('#room-open .name')).toHaveText(/long enough/);
  for (const width of [390, 768, 1024, 1280, 1600]) {
    await alice.setViewportSize({ width, height: 900 });
    await bar.evaluate((b) => { b.scrollLeft = 0; });
    await settle(alice);
    let s = await barState(alice);
    // 034-AC1: nine full pills fit no width here: all compact, never a mix.
    expect(s.modes, `${width} px`).toEqual(Array(9).fill(true));
    await expect(bar).toHaveClass(/compact/);
    // 034-AC3: no signal 112 px at most, 156 px with the mute button; ▶ keeps its target.
    const widths = await alice.locator('.persona-pill').evaluateAll((els) => els.map((e) => ({
      plain: !e.querySelector(':is(.watch, .drift, .vstate):not([hidden])'), width: e.getBoundingClientRect().width,
      run: Math.min(e.querySelector('.persona-run').getBoundingClientRect().width, e.querySelector('.persona-run').getBoundingClientRect().height),
      h: parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--control-h')) })));
    for (const w of widths) {
      if (w.plain) expect(w.width, `${width} px`).toBeLessThanOrEqual(112);
      expect(w.run, `${width} px`).toBeGreaterThanOrEqual(w.h - 0.5);
    }
    const heard = await alice.locator('.persona-pill').first().evaluate((e) => {
      e.querySelector('.persona-hear').hidden = false;
      const w = e.getBoundingClientRect().width;
      e.querySelector('.persona-hear').hidden = true;
      return w;
    });
    expect(heard, `${width} px`).toBeLessThanOrEqual(156);
    // Every "+N" is a 44 px target, the phone's every control too.
    expect((await smallTargets(alice)).filter((t) => t.includes('more persona')), `${width} px`).toEqual([]);
    if (width === 390) expect(await smallTargets(alice), '390 px').toEqual([]);
    expect(await nameShown(), `${width} px`).toBeGreaterThanOrEqual(-0.5);
    // 034-AC4: at the start, only the right edge hides pills; in the middle both; at the end the left.
    expect(s.max, `${width} px`).toBeGreaterThan(0);
    expect([s.left, s.right], `${width} px, start`).toEqual([null, more(s.hiddenRight, 'right')]);
    expect(s.hiddenLeft).toBe(0);
    expect(await overlaps(alice), `${width} px, start`).toEqual([]);
    await bar.evaluate((b) => { b.scrollLeft = (b.scrollWidth - b.clientWidth) / 2; });
    await settle(alice);
    s = await barState(alice);
    expect([s.left, s.right], `${width} px, middle`).toEqual([more(s.hiddenLeft, 'left'), more(s.hiddenRight, 'right')]);
    expect(s.hiddenLeft + s.hiddenRight, `${width} px, middle`).toBeLessThan(9);
    expect(await overlaps(alice), `${width} px, middle`).toEqual([]);
    await shot(alice, `compact-${width}-middle`);
    // A faded pill takes no click, its ▶ beside the "+N" included; brought in whole, it takes one again.
    expect(await bandHits(alice), `${width} px, middle`).toEqual([]);
    const back = alice.locator(`.persona-pill[data-id="${await alice.locator('.persona-pill.edge-hidden').last().getAttribute('data-id')}"]`);
    await back.evaluate((e) => e.scrollIntoView({ inline: 'center' }));
    await settle(alice);
    await expect(back).not.toHaveClass(/edge-hidden/);
    expect(await back.locator('.persona-run').evaluate((e) => {
      const r = e.getBoundingClientRect();
      return e.contains(document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2));
    }), `${width} px, brought in`).toBe(true);
    await bar.evaluate((b) => { b.scrollLeft = (b.scrollWidth - b.clientWidth) / 2; });
    await settle(alice);
    s = await barState(alice);
    // A press scrolls toward its side by the visible width less the two 44 px edge bands, or as
    // far as that side goes.
    const step = s.width - 2 * 44;
    const lands = (to) => expect.poll(async () => Math.abs((await barState(alice)).scroll - to)).toBeLessThanOrEqual(1);
    await alice.locator('.persona-more.right').click();
    await lands(s.scroll + Math.min(step, s.max - s.scroll));
    const pressed = (await barState(alice)).scroll;
    await alice.locator('.persona-more.left').click();
    await lands(pressed - Math.min(step, pressed));
    await bar.evaluate((b) => { b.scrollLeft = b.scrollWidth; });
    await settle(alice);
    s = await barState(alice);
    expect([s.left, s.right], `${width} px, end`).toEqual([more(s.hiddenLeft, 'left'), null]);
    expect(s.hiddenRight).toBe(0);
    expect(await overlaps(alice), `${width} px, end`).toEqual([]);
  }

  // 034-AC3: the focus ring of a compact pill draws inside the bar.
  await alice.setViewportSize({ width: 1280, height: 900 });
  await alice.locator('.persona-pill').first().locator('button.persona').focus();
  await alice.keyboard.press('Tab');
  await alice.keyboard.press('Tab');
  const ring = await alice.evaluate(() => {
    const e = document.activeElement;
    const [r, b] = [e.getBoundingClientRect(), document.getElementById('persona-buttons').getBoundingClientRect()];
    return { pill: e.matches('button.persona'), offset: getComputedStyle(e).outlineOffset, inside: r.top >= b.top - 0.5 && r.bottom <= b.bottom + 0.5 };
  });
  expect(ring).toEqual({ pill: true, offset: '-2px', inside: true });

  // 034-AC5: at the start, the right "+N" carries the drift of a hidden time keeper over the
  // speech bubble of another, then the bubble, then an answer; the pills keep their order.
  await bar.evaluate((b) => { b.scrollLeft = 0; });
  await settle(alice);
  const order = await alice.locator('.persona-pill').evaluateAll((els) => els.map((e) => e.dataset.id));
  const [last, before, third] = await alice.evaluate(() => agent.personas.slice(-3).reverse().map((p) => ({ id: p.id, name: p.name })));
  const right = alice.locator('.persona-more.right');
  await alice.evaluate(([off, waits]) => {
    ttsOn = true;
    ttsLive = { pod: true, personas: { [waits]: { state: 'waiting' } } };
    agent = { ...agent, personas: agent.personas.map((p) => (p.id === off ? { ...p, time_keeper: true, drift_level: 'high', drift_reason: 'Ten minutes on one point.' } : p)) };
    renderAgent();
  }, [last.id, before.id]);
  const n = (await barState(alice)).hiddenRight;
  await expect(right).toHaveAccessibleName(`${more(n, 'right')}. ${last.name} is off track`);
  await expect(right.locator('.sig.off-track svg')).toBeVisible();
  await alice.evaluate((off) => { agent = { ...agent, personas: agent.personas.map((p) => (p.id === off ? { ...p, drift_level: 'low' } : p)) }; renderAgent(); }, last.id);
  await expect(right).toHaveAccessibleName(`${more(n, 'right')}. ${before.name} is waiting to speak`);
  await expect(right.locator('.sig.waiting svg')).toBeVisible();
  await alice.evaluate((id) => { ttsLive = { pod: true, personas: {} }; agent = { ...agent, online: true, running: [{ persona_id: id }] }; renderAgent(); }, third.id);
  await expect(right).toHaveAccessibleName(`${more(n, 'right')}. ${third.name} is answering`);
  await expect(right.locator('.sig.active')).toBeVisible();
  // Nothing hidden to the left: no fade, no button, a signal or not.
  await expect(alice.locator('.persona-more.left')).toBeHidden();
  await expect(bar).not.toHaveClass(/fade-left/);
  expect(await alice.locator('.persona-pill').evaluateAll((els) => els.map((e) => e.dataset.id))).toEqual(order);
});

// 034-AC1, 034-AC2: four personas keep their full pills on a wide window; a window narrowed past
// the fit and widened back switches the mode once each way, never back and forth between frames.
test('the persona bar switches mode once per crossing', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await addPersonas(alice, room.id, 3);
  await alice.reload();
  await expect(alice.locator('.persona-pill')).toHaveCount(4);
  await alice.setViewportSize({ width: 1600, height: 900 });
  await settle(alice);
  const bar = alice.locator('#persona-buttons');
  await expect(bar).not.toHaveClass(/compact/);
  expect((await barState(alice)).modes).toEqual([false, false, false, false]);
  await expect(alice.locator('.persona-more')).toHaveCount(2);
  for (const e of await alice.locator('.persona-more').all()) await expect(e).toBeHidden();
  // Each frame's mode, as painted.
  await alice.evaluate(() => {
    window.modes = [];
    const bar = document.getElementById('persona-buttons');
    const tick = () => {
      const c = bar.classList.contains('compact');
      if (window.modes.at(-1) !== c) window.modes.push(c);
      requestAnimationFrame(tick);
    };
    tick();
  });
  // Coarse steps find the threshold, then single pixels cross it each way, past it and back.
  const sweep = async (from, to, step) => {
    let crossed = null;
    for (let w = from; step < 0 ? w >= to : w <= to; w += step) {
      await alice.setViewportSize({ width: w, height: 900 });
      await settle(alice);
      if (crossed === null && await bar.evaluate((b, c) => b.classList.contains('compact') === c, step < 0)) crossed = w;
    }
    return crossed;
  };
  // Above 64rem only: the warning's icon leaves the bar there, a second threshold.
  const at = await sweep(1600, 1040, -40);
  expect(at, 'the status shows past 64rem').toBeGreaterThan(1024 + 40);
  let up = null;
  for (const [from, to, step] of [[at, at + 40, 1], [at + 40, at - 40, -1], [at - 40, at + 40, 1]]) {
    up = await sweep(from, to, step);
    expect(up).not.toBeNull();
  }
  expect(await alice.evaluate(() => window.modes)).toEqual([false, true, false, true, false]);

  // 034-AC2: a pixel over the threshold, the mode follows a persona added, removed and renamed.
  await alice.setViewportSize({ width: up, height: 900 });
  await settle(alice);
  await expect(bar).not.toHaveClass(/compact/);
  await alice.evaluate(() => { agent = { ...agent, personas: [...agent.personas, { ...agent.personas[0], id: 1e6, name: 'Extra' }] }; renderAgent(); });
  await expect.soft(bar, 'persona added').toHaveClass(/compact/);
  await alice.evaluate(() => { agent = { ...agent, personas: agent.personas.filter((p) => p.id !== 1e6) }; renderAgent(); });
  await expect.soft(bar, 'persona removed').not.toHaveClass(/compact/);
  const first = await alice.evaluate(() => agent.personas[0].name);
  await alice.evaluate(() => { agent = { ...agent, personas: agent.personas.map((p, i) => (i ? p : { ...p, name: 'A persona with a long name' })) }; renderAgent(); });
  await expect.soft(bar, 'persona renamed').toHaveClass(/compact/);
  await alice.evaluate((name) => { agent = { ...agent, personas: agent.personas.map((p, i) => (i ? p : { ...p, name })) }; renderAgent(); }, first);
  await expect.soft(bar, 'name back').not.toHaveClass(/compact/);
  // The status in words keeps 6rem beside the bar where the warning took one icon: the pills turn
  // compact for it, and back to full when a warning takes its place again.
  await alice.evaluate(() => { agent = { ...agent, online: true, running: [{ persona_id: agent.personas[1].id }] }; renderAgent(); });
  await expect(alice.locator('#agent-status')).toBeVisible();
  await expect.soft(bar, 'status shown').toHaveClass(/compact/);
  await alice.evaluate(() => agentError('This answer already stopped.'));
  await expect(alice.locator('#agent-warn')).toBeVisible();
  await expect.soft(bar, 'warning instead').not.toHaveClass(/compact/);
});
