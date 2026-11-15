import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, fakeCall, appointAdmin, agentState, patchPersona, noOverflow, smallTargets, editPersona } from './helpers.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const absent = 'Not available on this server: the server admin turns it on.';
// The catalogue a server with persona voices hands the voice picker.
const catalogue = { lang: 'fr', voices: [{ id: 'estelle', name: 'Estelle', lang: 'fr', samples: [] }, { id: 'cml-2114', name: 'Camille', lang: 'fr', samples: [] }] };

const sheet = (page) => page.locator('#call-overview');
const row = (page, list, id) => page.locator(`#ov-${list} [data-row="${id}"]`);
const lines = (page, list) => page.locator(`#ov-${list} [data-row]`).evaluateAll((els) => els.map((e) => [...e.querySelectorAll('.main > div')].map((d) => d.textContent)));

// openFrom opens the overview from the room menu or the Audio options menu.
async function openFrom(page, which = 'room') {
  await page.locator(which === 'room' ? '#room-open' : '#voice-more').click();
  await page.locator(which === 'room' ? '#room-menu' : '#voice-menu').getByRole('link', { name: 'Call and voice-to-text…' }).click();
  await expect(sheet(page)).toBeVisible();
  await expect(page).toHaveURL(/#call-overview$/);
  await expect(sheet(page).getByRole('heading', { name: 'Call and voice-to-text' })).toBeVisible();
}

// callRoom: Alice, a room admin, and Bob, a plain member, on a server with a call, voice-to-text
// and persona voices.
async function callRoom(browser, request) {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await appointAdmin(alice, request, room.id);
  for (const p of [alice, bob]) {
    await p.context().route(`**/api/rooms/${room.id}/voices?*`, (r) => r.fulfill({ json: catalogue }));
    await fakeCall(p, room.id, { tts: true });
    await expect(p.locator('#voice-join')).toBeVisible();
  }
  return { room, alice, bob };
}

// stt plays the transcription state the server sends from its workers, with mine among the voices.
const stt = (page, available, mine, more = {}) => page.evaluate(([available, mine, more]) => showSTT({ available, listening: available, glossary: available,
  transcribed: mine ? [Number(myID)] : [], engines: [], ...more }), [available, mine, more]);

// 042-AC1 042-AC4 042-AC5: on a server without a call the overview still opens from the room menu,
// every row saying the setting is not there and who turns it on; an ended room, the docs and the
// chat windows open nothing and drop the hash.
test('call overview: a server without a call', async ({ browser, request }) => {
  const { room, pages: [bob] } = await roomWith(browser, request, ['Bob']);
  await expect(bob.locator('#voice')).toBeHidden();
  await openFrom(bob);
  expect(await lines(bob, 'you')).toEqual([[absent]]);
  const grill = (await agentState(bob, room.id)).personas[0];
  expect(await lines(bob, 'room')).toEqual([['Words to recognise', absent], ['Call voices', absent], [grill.badge + ' ' + grill.name, absent]]);
  expect(await lines(bob, 'server')).toEqual([['Voice-to-text', absent], ['Persona voices', absent], ['Pronunciation', absent]]);
  await expect(sheet(bob).locator('a[href^="/admin"], a[href^="#"]')).toHaveCount(0);
  await bob.keyboard.press('Escape');
  await expect(sheet(bob)).toBeHidden();
  await expect(bob).not.toHaveURL(/#/);
  await expect(bob.locator('#room-open')).toBeFocused();

  for (const path of [`/r/${room.id}/docs#call-overview`, `/r/${room.id}/chat#call-overview`]) {
    const win = await bob.context().newPage();
    await win.goto(path);
    await expect(win).not.toHaveURL(/#call-overview/);
    await expect(sheet(win)).toBeHidden();
    // Its room name links back to the room: no room menu, so no entry.
    await expect(win.locator('#room-name a')).toBeVisible();
    await expect(win.locator('#room-open')).toHaveCount(0);
    await win.close();
  }

  await bob.evaluate(() => end('This room was deleted.'));
  await expect(bob.locator('#overview-open')).toBeHidden();
  await bob.evaluate(() => { location.hash = '#call-overview'; });
  await expect(bob).not.toHaveURL(/#call-overview/);
  await expect(sheet(bob)).toBeHidden();
});

// 042-AC2 042-AC3 042-AC4 042-AC6: the "You" lines follow the audio bar as it changes under the
// open sheet; the room's rows follow a glossary and a persona's voice changed in another session;
// Call voices links for a room admin only.
test('call overview: the viewer, the room and the server, live', async ({ browser, request }) => {
  const { room, alice, bob } = await callRoom(browser, request);
  await openFrom(alice);
  // Before the first voice-to-text state the glossary's count still shows; only the server's row waits.
  await expect(row(alice, 'room', 'glossary').locator('.meta').first()).toHaveText('0 words');
  await expect(row(alice, 'server', 'stt').locator('.meta').first()).toHaveText('Loading…');
  // Section headings name their lists, not a landmark each: one announcement per heading.
  await expect(sheet(alice).getByRole('region')).toHaveCount(0);
  await expect(sheet(alice).getByRole('list', { name: 'This room' })).toBeVisible();
  expect(await lines(alice, 'you')).toEqual([['Not in the call. Join it from the audio bar.']]);
  // The audio bar sits under the modal sheet: its buttons are pressed from the page.
  const press = (id) => alice.evaluate((id) => document.getElementById(id).click(), id);
  const you = (n) => row(alice, 'you', n).locator('.name');
  await press('voice-join');
  await expect(you('call')).toHaveText('In the call · mic on · hearing the call');
  await expect(you('stt')).toHaveText('Loading…');
  await stt(alice, true, false);
  expect(await lines(alice, 'server')).toEqual([['Voice-to-text', 'Available', 'Who: the server admin'], ['Persona voices', 'Available', 'Who: the server admin'],
    ['Pronunciation', 'Set by the server admin, on the server\'s admin page.']]);
  await expect(row(alice, 'room', 'glossary').locator('.meta').first()).toHaveText('0 words');
  await expect(row(alice, 'room', 'glossary').locator('.ai-who')).toHaveText('Who: any member');
  await expect(row(alice, 'room', 'voices').locator('.meta').first()).toHaveText('pause 1 s, resume 20 s, queue 40 s, up to 60 words');
  await expect(row(alice, 'room', 'voices').locator('.ai-who')).toHaveText('Who: room admins');
  await expect(row(alice, 'room', 'voices').getByRole('link', { name: 'Open Call voices' })).toHaveAttribute('href', '#manage/voices');

  // No worker on the voice, the server's voice-to-text up: not "off", which "This server" would contradict.
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is in the call but not writing down your voice');
  await expect(row(alice, 'server', 'stt').locator('.meta').first()).toHaveText('Available');
  await stt(alice, true, false, { listening: false });
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is not in the call');
  // The device engine's self-test, after its download.
  await alice.evaluate(() => {
    window.realDeviceSTT = window.grillDeviceSTT;
    window.grillDeviceSTT = { state: 'testing', info: { offered: false }, pipeline: {} };
    dispatchEvent(new CustomEvent('grill:device-stt'));
  });
  await expect(you('stt')).toHaveText('Transcription is on, waiting: this device is testing the model');
  await alice.evaluate(() => {
    window.grillDeviceSTT = window.realDeviceSTT;
    dispatchEvent(new CustomEvent('grill:device-stt'));
  });
  await stt(alice, true, true);
  await expect(you('stt')).toHaveText('Your voice is written down now');
  await press('voice-mute');
  await expect(you('call')).toHaveText('In the call · muted · hearing the call');
  await expect(you('stt')).toHaveText('Transcription is on, waiting: your mic is muted');
  await press('voice-mute');
  await expect(you('stt')).toHaveText('Your voice is written down now');
  await stt(alice, false, false);
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is off');
  await expect(row(alice, 'server', 'stt').locator('.meta').first()).toHaveText('Voice-to-text is off');
  // No worker reads the glossary: the row gives its tab's reason, no link.
  const glossary = row(alice, 'room', 'glossary');
  expect(await lines(alice, 'room').then((l) => l[0])).toEqual(['Words to recognise', 'Voice-to-text is off']);
  await expect(glossary.locator('a')).toHaveCount(0);
  await stt(alice, true, true, { glossary: false });
  expect(await lines(alice, 'room').then((l) => l[0])).toEqual(['Words to recognise', absent]);
  await expect(glossary.locator('a')).toHaveCount(0);
  await stt(alice, true, true);
  await expect(glossary.getByRole('link', { name: 'Open Words to recognise' })).toBeVisible();
  await press('voice-deaf');
  await expect(you('call')).toHaveText('In the call · muted · stopped hearing the call');
  await press('voice-deaf');
  await expect(you('call')).toHaveText('In the call · mic on · hearing the call');
  await press('voice-stt');
  await expect(you('stt')).toHaveText('Your voice is not written down');
  await press('voice-stt');
  await expect(you('stt')).toHaveText('Your voice is written down now');
  // No device offer on this browser: no line on where it runs.
  await expect(row(alice, 'you', 'where')).toHaveCount(0);

  // Another session edits the glossary and a persona's voice.
  expect(await bob.evaluate((id) => fetch(`/api/rooms/${id}/glossary`, { method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ glossary: 'Kubernetes\nArgo CD, Helm' }) }).then((r) => r.status), room.id)).toBe(200);
  await expect(row(alice, 'room', 'glossary').locator('.meta').first()).toHaveText('3 words');
  const grill = (await agentState(bob, room.id)).personas[0];
  const persona = row(alice, 'room', 'persona-' + grill.id);
  await expect(persona.getByRole('link', { name: 'Open ' + grill.name })).toHaveAttribute('href', `#personas/${grill.id}/identity`);
  await expect(persona.locator('.ai-who')).toHaveText('Who: whoever may edit the persona');
  await patchPersona(bob, room.id, grill.id, { speaks: false });
  await expect(persona.locator('.meta').first()).toHaveText('stays silent in the call');
  await patchPersona(bob, room.id, grill.id, { speaks: true, voice: 'cml-2114', voice_rate: 1.1 });
  await expect(persona.locator('.name')).toHaveText(grill.badge + ' ' + grill.name);
  await expect(persona.locator('.meta').first()).toHaveText('speaks in the call with Camille (French), rate 1.1');

  // Bob, a plain member, has no Call voices tab: no link; the glossary is his to change too.
  await stt(bob, true, false);
  await openFrom(bob);
  await expect(row(bob, 'room', 'voices').locator('.meta').first()).toHaveText('pause 1 s, resume 20 s, queue 40 s, up to 60 words');
  await expect(row(bob, 'room', 'voices').locator('a')).toHaveCount(0);
  await expect(row(bob, 'room', 'glossary').getByRole('link', { name: 'Open Words to recognise' })).toHaveAttribute('href', '#manage/glossary');

  // 042-AC1: the entry follows "Personas" in the room menu and the toggles in Audio options.
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeHidden();
  await alice.locator('#room-open').click();
  await expect(alice.locator('#room-menu > :is(a, button):visible')).toHaveText(['Invite people', 'Room settings', 'Link a room…', 'Personas', 'Call and voice-to-text…', 'AI in this room',
    'Open documents in a new window', 'Open chat in a new window', 'Run the AI on my computer', 'Switch room…']);
  await alice.keyboard.press('Escape');
  // In markup order: the phone-only and the device toggles count, hidden or not.
  await alice.locator('#voice-more').click();
  await expect(alice.locator('#voice-menu > :is(a, button, label)')).toHaveText(['Transcribe my voice', 'Keep my mic on while I stop hearing the call',
    'Transcribe on this device', 'Call and voice-to-text…', 'Leave the call']);
  await alice.keyboard.press('Escape');
});

// 042-AC3 042-AC4 042-AC5: a call with no persona voices: Call voices, Persona voices and each
// persona's row stay and say why.
test('call overview: no persona voices', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await fakeCall(alice, room.id);
  await stt(alice, true, false);
  await openFrom(alice);
  const grill = (await agentState(alice, room.id)).personas[0];
  // The word count loads as the sheet opens.
  await expect.poll(() => lines(alice, 'room')).toEqual([['Words to recognise', '0 words', 'Who: any member'], ['Call voices', absent],
    [grill.badge + ' ' + grill.name, absent]]);
  await expect(row(alice, 'room', 'persona-' + grill.id).locator('a')).toHaveCount(0);
  await expect(row(alice, 'server', 'tts').locator('.meta')).toHaveText(absent);
  await expect(row(alice, 'server', 'pronunciation').locator('.meta')).toHaveText(absent);
});

// Stand-ins for stt/engine.js and stt/pipeline.js around the real stt/device.js: the engine is ready
// once the test says so; the pipeline, like the real one, reports nothing before its first mode,
// then the mode the test sends it.
const stubEngine = `onmessage = (e) => {
  if (e.data.cmd === 'load') new BroadcastChannel('stub-engine').onmessage = () => postMessage({ ev: 'ready', backend: 'wasm', isolated: true, ms: {} });
};`;
const stubPipeline = `const stats = (mode) => postMessage({ ev: 'stats', mode });
onmessage = (e) => {
  if (e.data.cmd === 'stop') postMessage({ ev: 'stopped' });
  if (e.data.cmd !== 'start') return;
  new BroadcastChannel('stub-pipeline').onmessage = (m) => {
    postMessage({ ev: 'mode', mode: m.data });
    stats(m.data);
  };
};`;

// 042-AC2 042-AC6, 010-AC17: voice-to-text on this device, through stt/device.js itself. The line
// on where it runs comes with the offer, the sheet already open; it waits for the model, then says
// the device holds the voice only while the pipeline does: after a fall back the server writes it
// down, or nobody when the server has no worker.
test('call overview: voice-to-text on this device and its fall back', async ({ browser, request, browserName }) => {
  test.skip(browserName !== 'chromium', 'the device option is checked on Chrome (010-AC1)');
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const ctx = alice.context();
  await ctx.addInitScript(() => {
    globalThis.grillReloadDelayMs = 0;
    for (const [k, v] of Object.entries({ userAgentData: { mobile: false }, hardwareConcurrency: 8, deviceMemory: 8 })) {
      Object.defineProperty(Navigator.prototype, k, { get: () => v, configurable: true });
    }
    // Passed earlier: no self-test.
    localStorage.setItem('grill-room-device-stt-passed:rev-042', 'wasm');
    window.roomReads = 0;
    addEventListener('grill:room', () => window.roomReads++);
  });
  for (const [path, body] of [['engine', stubEngine], ['pipeline', stubPipeline]]) {
    // The real answer's headers: the isolated page refuses a worker script without them.
    await ctx.route(`**/static/stt/${path}.js`, async (r) => r.fulfill({ response: await r.fetch(), body }));
  }
  await fakeCall(alice, room.id);
  const offer = { device: { revision: 'rev-042', bytes: 432e6, files: [] } };
  await alice.locator('#voice-join').click();
  await openFrom(alice, 'voice');
  const you = (n) => row(alice, 'you', n).locator('.name');
  const said = alice.locator('#device-stt-status');
  const send = (channel, data) => alice.evaluate(([channel, data]) => new BroadcastChannel(channel).postMessage(data), [channel, data]);
  // A worker just started may not listen yet: the engine and the pipeline are told until they answer.
  const ready = () => expect.poll(async () => {
    await send('stub-engine', 'ready');
    return alice.evaluate(() => window.grillDeviceSTT.state);
  }).toBe('ready');
  const onDevice = () => expect.poll(async () => {
    await send('stub-pipeline', 'device');
    return alice.evaluate(() => window.grillDeviceSTT.pipeline.mode);
  }).toBe('device');

  // The replayed "joined" line reloads the room once more, which redraws the sheet: past it, only
  // stt/device.js's word on the offer draws the line.
  await expect.poll(() => alice.evaluate(() => window.roomReads)).toBeGreaterThanOrEqual(2);
  await stt(alice, true, false, offer);
  await expect(you('where')).toHaveText('Voice-to-text runs on the server');
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is in the call but not writing down your voice');
  // The toggle sits in Audio options, under the sheet.
  await alice.evaluate(() => document.querySelector('#voice-device input').click());
  await expect(you('stt')).toHaveText('Transcription is on, waiting: the model is downloading');
  await expect(you('where')).toHaveText('Voice-to-text runs on the server');
  await ready();
  expect(await alice.evaluate(() => window.grillDeviceSTT.info.live)).toBe(true);
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is in the call but not writing down your voice');

  await onDevice();
  await expect(said).toHaveText('Transcribed on this device');
  await expect(you('stt')).toHaveText('Your voice is written down now');
  await expect(you('where')).toHaveText('Voice-to-text runs on this device');

  // Too slow: the server's worker takes the voice back.
  await send('stub-pipeline', 'slow');
  await expect(said).toHaveText('Transcribed by the server: this device is too slow right now');
  await expect(you('where')).toHaveText('Voice-to-text runs on the server');
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is in the call but not writing down your voice');
  await stt(alice, true, true, offer);
  await expect(you('stt')).toHaveText('Your voice is written down now');
  // No worker on the server: nobody writes it down.
  await stt(alice, false, false, offer);
  await expect(you('stt')).toHaveText('Transcription is on, waiting: voice-to-text is off');
  await expect(you('where')).toHaveText('Voice-to-text runs on the server');
  await expect(row(alice, 'server', 'stt').locator('.meta').first()).toHaveText('Voice-to-text is off');
  // Fast again: back on the device.
  await send('stub-pipeline', 'device');
  await expect(you('stt')).toHaveText('Your voice is written down now');
  await expect(you('where')).toHaveText('Voice-to-text runs on this device');
  // Transcription off and on again: a new engine, a new pipeline, whose first word is "device".
  const press = (id) => alice.evaluate((id) => document.getElementById(id).click(), id);
  await press('voice-stt');
  await expect(you('stt')).toHaveText('Your voice is not written down');
  await expect(you('where')).toHaveText('Voice-to-text runs on the server');
  await press('voice-stt');
  await ready();
  await onDevice();
  await expect(you('stt')).toHaveText('Your voice is written down now');
  await expect(you('where')).toHaveText('Voice-to-text runs on this device');
});

// 042-AC7: a link out opens its target as a new entry and closes the overview; closing the target
// by Escape, ✕ or Back returns to the overview with the focus on the row that linked, and Forward
// reopens the target. A tab switch inside the target still returns. A row gone meanwhile hands the
// focus to the title. A target loaded directly closes as before. Closing the overview gives the
// focus to its opener, or to "Join the call" once leaving the call took Audio options away.
test('call overview: links out and back', async ({ browser, request }) => {
  const { room, alice, bob } = await callRoom(browser, request);
  await stt(alice, true, false);
  const glossaryLink = row(alice, 'room', 'glossary').getByRole('link', { name: 'Open Words to recognise' });
  await openFrom(alice);
  await glossaryLink.click();
  await expect(alice.locator('#manage')).toBeVisible();
  await expect(sheet(alice)).toBeHidden();
  await expect(alice).toHaveURL(/#manage\/glossary$/);
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeVisible();
  await expect(alice).toHaveURL(/#call-overview$/);
  await expect(glossaryLink).toBeFocused();
  await alice.goForward();
  await expect(alice.locator('#manage')).toBeVisible();
  await expect(sheet(alice)).toBeHidden();
  await alice.goBack();
  await expect(sheet(alice)).toBeVisible();
  await expect(alice.locator('#manage')).toBeHidden();
  await expect(glossaryLink).toBeFocused();

  // Call voices, then the Invites tab inside Room settings, then ✕.
  const voicesLink = row(alice, 'room', 'voices').getByRole('link', { name: 'Open Call voices' });
  await voicesLink.click();
  await expect(alice).toHaveURL(/#manage\/voices$/);
  await alice.locator('#manage').getByRole('link', { name: 'Invites' }).click();
  await expect(alice).toHaveURL(/#manage\/invites$/);
  await expect(alice.locator('#manage #tab-invites')).toHaveAttribute('aria-current', 'page');
  await alice.locator('#manage-close').click();
  await expect(sheet(alice)).toBeVisible();
  await expect(voicesLink).toBeFocused();

  // A reload on Room settings keeps the way back, through a tab switch after it.
  await voicesLink.click();
  await expect(alice.locator('#manage')).toBeVisible();
  await alice.reload();
  await expect(alice.locator('#manage')).toBeVisible();
  await alice.locator('#manage').getByRole('link', { name: 'Invites' }).click();
  await expect(alice.locator('#manage #tab-invites')).toHaveAttribute('aria-current', 'page');
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeVisible();
  await expect(voicesLink).toBeFocused();

  // Room settings › Personas › a persona: its close returns to Room settings, whose close returns here.
  await voicesLink.click();
  await alice.locator('#manage').getByRole('link', { name: 'Personas' }).click();
  await expect(alice.locator('#manage #tab-personas')).toHaveAttribute('aria-current', 'page');
  await alice.locator('#manage-personas a').first().click();
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#manage')).toBeVisible();
  await expect(alice).toHaveURL(/#manage\/personas$/);
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeVisible();
  await expect(voicesLink).toBeFocused();

  // Room settings hands over to the Personas sheet: that one closes to the room, not here.
  await voicesLink.click();
  await alice.locator('#manage').getByRole('link', { name: 'Personas' }).click();
  await alice.locator('#manage').getByRole('button', { name: 'Open the Personas sheet' }).click();
  await expect(alice.locator('#personas')).toBeVisible();
  await expect(alice).not.toHaveURL(/#/);
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#personas')).toBeHidden();
  await expect.poll(() => alice.evaluate(() => [location.hash, history.state])).toEqual(['', null]);
  await expect(sheet(alice)).toBeHidden();
  await openFrom(alice);

  // A persona's sheet, closed by ✕ after a tab switch.
  const grill = (await agentState(alice, room.id)).personas[0];
  const grillLink = row(alice, 'room', 'persona-' + grill.id).getByRole('link', { name: 'Open ' + grill.name });
  await grillLink.click();
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  await expect(alice).toHaveURL(new RegExp(`#personas/${grill.id}/identity$`));
  await alice.locator('#persona-tabs').getByRole('link', { name: 'Model' }).click();
  await expect(alice).toHaveURL(new RegExp(`#personas/${grill.id}/model$`));
  await alice.locator('#persona-sheet-close').click();
  await expect(sheet(alice)).toBeVisible();
  await expect(grillLink).toBeFocused();

  // A Save that jumps to the tab of an invalid field keeps the way back.
  await grillLink.click();
  const ps = alice.locator('#persona-sheet');
  await editPersona(ps, () => ps.getByLabel('Name').fill(''));
  await ps.getByRole('link', { name: 'Model', exact: true }).click();
  await ps.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(alice).toHaveURL(new RegExp(`#personas/${grill.id}/identity$`));
  await expect(ps.getByLabel('Name')).toBeFocused();
  await ps.getByLabel('Name').fill(grill.name);
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeVisible();
  await expect(grillLink).toBeFocused();

  // A second persona, removed in another session while its sheet is open: the focus goes to the title.
  const tpls = await alice.evaluate((id) => fetch(`/api/rooms/${id}/templates`).then((r) => r.json()), room.id);
  const added = await alice.evaluate(([id, t]) => fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ template_id: t }) }).then((r) => r.json()), [room.id, tpls.find((t) => t.name !== grill.name).id]);
  const addedLink = row(alice, 'room', 'persona-' + added.id).locator('a');
  // The sheet's own read of the agent state, taken before the removal, lands after it: stale.
  let fetched;
  const read = new Promise((r) => { fetched = r; });
  let release;
  const held = new Promise((r) => { release = r; });
  await alice.route(`**/api/rooms/${room.id}/agent`, async (r) => {
    const res = await r.fetch();
    fetched();
    await held;
    await r.fulfill({ response: res }).catch(() => {});
  });
  await addedLink.click();
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  await read;
  expect(await bob.evaluate(([id, p]) => fetch(`/api/rooms/${id}/personas/${p}`, { method: 'DELETE' }).then((r) => r.status), [room.id, added.id])).toBe(204);
  await expect(sheet(alice)).toBeVisible();
  await expect(alice.locator('#call-overview-title')).toBeFocused();
  const stale = alice.waitForResponse((r) => r.url().endsWith(`/api/rooms/${room.id}/agent`));
  release();
  // Its body in, loadSkills applies it or not within the frame.
  await (await stale).finished();
  await alice.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  await alice.unroute(`**/api/rooms/${room.id}/agent`);
  await expect(row(alice, 'room', 'persona-' + added.id)).toHaveCount(0);
  await expect(alice.locator('#call-overview-title')).toBeFocused();

  // Closed, the overview hands the focus to the room menu's button.
  await alice.keyboard.press('Escape');
  await expect(sheet(alice)).toBeHidden();
  await expect(alice.locator('#room-open')).toBeFocused();

  // From Audio options, in the call: leaving the call under the sheet takes ⋯ away.
  await alice.locator('#voice-join').click();
  await openFrom(alice, 'voice');
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#voice-more')).toBeFocused();
  await openFrom(alice, 'voice');
  await alice.evaluate(() => document.getElementById('voice-leave').click());
  await expect(row(alice, 'you', 'call').locator('.name')).toHaveText('Not in the call. Join it from the audio bar.');
  await alice.keyboard.press('Escape');
  await expect(alice.locator('#voice-join')).toBeFocused();

  // Loaded directly, Room settings has no origin: closing it drops the hash, as before.
  await alice.goto(`/r/${room.id}#manage/glossary`);
  await expect(alice.locator('#manage')).toBeVisible();
  await alice.keyboard.press('Escape');
  await expect(alice).not.toHaveURL(/#/);
  await expect(sheet(alice)).toBeHidden();
});

// 042-AC6: the persona sheet's own read of the agent state is sent before a stream event lands and
// answered after it, but read on the server after it: the newer read is not dropped. The read
// carries an update offer, as a library upload leaves one without waking the stream.
test('persona sheet: a read newer than the event that landed during it', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const grill = (await agentState(alice, room.id)).personas[0];
  let first = true;
  await alice.route(`**/api/rooms/${room.id}/agent`, async (r) => {
    if (first) {
      first = false;
      expect(await patchPersona(bob, room.id, grill.id, { name: 'Grilly' })).toBeLessThan(300);
      await expect.poll(() => alice.evaluate(() => agent.personas[0].name)).toBe('Grilly');
    }
    const res = await r.fetch();
    const json = await res.json();
    json.personas[0].update_version = 99;
    await r.fulfill({ response: res, json });
  });
  await alice.evaluate((id) => { location.hash = `#personas/${id}/identity`; }, grill.id);
  await expect(alice.locator('#persona-sheet')).toBeVisible();
  await expect.poll(() => alice.evaluate(() => agent.personas[0].update_version)).toBe(99);
  expect(await alice.evaluate(() => agent.personas[0].name)).toBe('Grilly');
});

// 042-AC1: full screen on a phone, nothing sticking out and 44 px targets, at 390 and 1440 px and
// with the text at 125 %.
test('call overview: phone, desktop and large text', async ({ browser, request }, testInfo) => {
  const { room, alice } = await callRoom(browser, request);
  await stt(alice, true, true);
  await alice.locator('#voice-join').click();
  await alice.evaluate((id) => fetch(`/api/rooms/${id}/glossary`, { method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ glossary: 'Kubernetes, Helm' }) }), room.id);
  await openFrom(alice, 'voice');
  for (const big of [false, true]) {
    await alice.evaluate((big) => { document.documentElement.style.fontSize = big ? '125%' : ''; }, big);
    for (const vp of [{ width: 390, height: 844 }, { width: 1440, height: 900 }]) {
      await alice.setViewportSize(vp);
      if (vp.width === 390) {
        const box = await sheet(alice).boundingBox();
        expect(box.width).toBeGreaterThanOrEqual(389);
        expect(await noOverflow(alice)).toEqual([]);
        expect(await smallTargets(alice)).toEqual([]);
      }
      await alice.screenshot({ path: testInfo.outputPath(`call-overview-${vp.width}${big ? '-xl' : ''}.png`) });
    }
  }
});
