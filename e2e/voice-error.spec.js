import { test, expect } from './fixtures.js';
import { adminToken } from './playwright.config.js';
import { roomWith, playSTT, noOverflow, menuItem, fakeCall } from './helpers.js';

// Firefox's text for a denied mic: the one the lab showed raw.
const raw = 'The request is not allowed by the user agent or the platform in the current context.';

async function voicePage(browser, request) {
  const { room, pages: [page] } = await roomWith(browser, request, ['Alice']);
  await fakeCall(page, room.id, {}, raw);
  await expect(page.locator('#voice-join')).toBeVisible();
  return page;
}

// 037-AC6 037-AC7: in the audio at 390 × 844 with three personas, the header keeps the pills' row,
// 100 px at most, and the messages keep 65% of the screen.
test('the audio on a phone leaves the messages two thirds of the screen', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const room = await page.evaluate(() => roomID);
  const admin = { Authorization: `Bearer ${adminToken}` };
  const inv = await (await request.post(`/api/rooms/${room}/invites`, { headers: admin })).json();
  expect((await request.post('/api/join', { data: { invite: inv.token, name: 'Bob' } })).status()).toBe(201);
  await page.evaluate(async (id) => {
    const tpls = await (await fetch(`/api/rooms/${id}/templates`)).json();
    for (let i = 0; i < 2; i++) await fetch(`/api/rooms/${id}/personas`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ template_id: tpls[i].id }) });
  }, room);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await expect(page.locator('.persona-pill')).toHaveCount(3);
  await expect(page.locator('#messages')).toContainText('Bob joined');
  await page.locator('#voice-join').click();
  await expect(page.locator('#voice-more')).toBeVisible();
  const height = (sel) => page.locator(sel).evaluate((e) => e.getBoundingClientRect().height);
  expect(await height('.room-header')).toBeLessThanOrEqual(100);
  // 039-AC10's checklist shares the empty chat's area with the messages: the reading area is both.
  expect(await height('#messages') + await height('#chat-ready')).toBeGreaterThanOrEqual(549);
});

// A denied unmute shows a banner; the next unmute that works clears it (the lab's banner stayed
// with the mic live).
test('a mic error clears once the mic is live', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const banner = page.locator('#voice-error');
  await page.locator('#voice-join').click();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');

  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-mute').click();
  await expect(banner).toBeVisible();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'true');

  await page.evaluate(() => { window.denyMic = false; });
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
  await expect(banner).toBeHidden();
});

// Joining with the mic denied: the participant listens, told how to allow it; the banner closes
// with its ✕, and a later unmute that works leaves no banner either.
test('a mic denied at join is dismissible and clears on unmute', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const banner = page.locator('#voice-error');
  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-join').click();
  await expect(banner).toContainText('Allow it in the site settings');
  await expect(banner).toContainText('You can still listen.');
  await expect(banner).not.toContainText(raw);
  await expect(banner.locator('[data-icon=alert] svg')).toHaveCount(1);
  await banner.getByRole('button', { name: 'Dismiss' }).click();
  await expect(banner).toBeHidden();

  await page.evaluate(() => { window.denyMic = true; });
  await page.locator('#voice-mute').click();
  await expect(banner).toContainText('Allow it in the site settings');
  await expect(banner).not.toContainText(raw);
  await page.evaluate(() => { window.denyMic = false; });
  await page.locator('#voice-mute').click();
  await expect(page.locator('#voice-mute')).toHaveAttribute('aria-pressed', 'false');
  await expect(banner).toBeHidden();
});

const bar = ['#voice-mute', '#voice-deaf', '#voice-stt', '#voice-more', '#voice-leave'];

// tipUnder waits for the styled tip to say text, then checks it sits below el.
async function tipUnder(page, el, text) {
  const tip = page.locator('#person-tip');
  await expect(tip).toBeVisible();
  await expect(tip).toHaveText(text);
  const [t, b] = [await tip.boundingBox(), await el.boundingBox()];
  expect(t.y, 'tip below its icon').toBeGreaterThanOrEqual(b.y + b.height);
}

// 005-AC49: once in the audio, the header is a row of icon buttons, each named, each with its styled
// tip below it on hover and on keyboard focus; the toggles carry aria-pressed. ⋯ holds only the
// rare options, as rows aligned with its head.
test('the audio bar: icon toggles with their tips below', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  await page.setViewportSize({ width: 1440, height: 900 });
  const room = await page.evaluate(() => roomID);
  const me = Number(await page.evaluate(() => myID));
  // A worker transcribes Alice: the transcription toggle carries its dot.
  await playSTT(page, room, { listening: true, transcribed: [me], engines: ['parakeet'] });
  // By keyboard: a click would leave the pointer over whichever icon takes Join the call's place.
  await page.locator('#voice-join').focus();
  await page.keyboard.press('Enter');
  for (const sel of bar) {
    const b = page.locator(sel);
    await expect(b).toBeVisible();
    await expect(b).toHaveAttribute('aria-label', /\w/);
    await expect(b).toHaveAttribute('data-tip', /\w/);
    await expect(b.locator('svg')).toHaveCount(1);
    await expect(b).toHaveText('');
  }
  await expect(page.locator('#voice-join')).toBeHidden();
  const mute = page.locator('#voice-mute');
  const deaf = page.locator('#voice-deaf');
  const stt = page.locator('#voice-stt');
  await expect(mute).toHaveAttribute('aria-pressed', 'false');
  await expect(deaf).toHaveAttribute('aria-pressed', 'false');
  await expect(stt).toHaveAttribute('aria-pressed', 'true');
  await expect(stt).toHaveClass(/captured/);

  // Hover shows the tip under the icon, with the words of its state.
  await expect(page.locator('#person-tip')).toBeHidden();
  await mute.hover();
  await tipUnder(page, mute, 'Your mic is on. Press to mute it.');
  await mute.click();
  await expect(mute).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#person-tip')).toHaveText('Your mic is muted. Press to speak again.');
  // Keyboard focus shows it at once. The camera and the screen come between (030-AC2).
  await page.keyboard.press('Tab');
  await expect(page.locator('#voice-cam')).toBeFocused();
  await tipUnder(page, page.locator('#voice-cam'), 'Turn your camera on: the others see you in Video.');
  await page.keyboard.press('Tab');
  await expect(page.locator('#voice-screen')).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(deaf).toBeFocused();
  await tipUnder(page, deaf, 'Stop hearing the call: the others go silent for you.');
  await page.keyboard.press('Enter');
  await expect(deaf).toHaveAttribute('aria-pressed', 'true');
  await expect(deaf).toHaveAttribute('data-tip', /Press to hear the others again/);
  await page.keyboard.press('Enter');
  await expect(deaf).toHaveAttribute('aria-pressed', 'false');
  await page.keyboard.press('Tab');
  await tipUnder(page, stt, /^Your voice is being written down\. Press to stop/);

  // ⋯ holds no button: the checkbox rows start at the head's text, their words in one column.
  await page.locator('#voice-more').click();
  const menu = page.locator('#voice-menu');
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('button')).toHaveCount(0);
  await expect(menu.locator('#stt-status')).toHaveText('1 voice is being written down · yours is one of them');
  const x = await page.evaluate(() => {
    const head = document.getElementById('stt-status');
    const box = document.querySelector('#voice-keepmic input');
    return { head: head.getBoundingClientRect().left + parseFloat(getComputedStyle(head).paddingLeft), box: box.getBoundingClientRect().left };
  });
  expect(Math.abs(x.head - x.box)).toBeLessThanOrEqual(1);
  await page.keyboard.press('Escape');

  await page.locator('#voice-leave').click();
  await expect(page.locator('#voice-join')).toBeVisible();
  for (const sel of bar) await expect(page.locator(sel)).toBeHidden();

  // On a phone the bar fits the header row, every target 44 px; Leave the call and the transcription
  // toggle move into ⋯ so the room's name keeps its first word (030-AC14).
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator('#voice-join').click();
  const phoneBar = bar.filter((sel) => sel !== '#voice-leave' && sel !== '#voice-stt');
  await expect(page.locator('#voice-more')).toBeVisible();
  await expect(page.locator('#voice-leave')).toBeHidden();
  await expect(page.locator('#voice-stt')).toBeHidden();
  for (const sel of phoneBar) {
    const b = await page.locator(sel).boundingBox();
    expect(b.width, sel).toBeGreaterThanOrEqual(44);
    expect(b.height, sel).toBeGreaterThanOrEqual(44);
  }
  expect(await noOverflow(page)).toEqual([]);
  // One row with the room's name: nothing wrapped under it.
  const name = await page.locator('#room-open').boundingBox();
  expect((await page.locator('#room-open .name').boundingBox()).width).toBeGreaterThanOrEqual(80);
  for (const sel of [...phoneBar, '#me']) {
    const b = await page.locator(sel).boundingBox();
    expect(Math.abs(b.y + b.height / 2 - (name.y + name.height / 2)), sel + ' on the name row').toBeLessThanOrEqual(2);
  }
  await page.locator('#voice-more').click();
  await expect(menu.getByRole('checkbox', { name: 'Transcribe my voice', exact: true })).toBeChecked();
  await menu.getByRole('button', { name: 'Leave the call', exact: true }).click();
  await expect(page.locator('#voice-join')).toBeVisible();
});

// 010-AC23: turning the transcription of my voice off holds across a reload, stops this device's
// transcription engine, and another tab of mine in this browser follows at once.
test('the transcription of my voice turns off and stays off', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const room = await page.evaluate(() => roomID);
  const stt = page.locator('#voice-stt');
  const meOff = async () => (await (await page.request.get(`/api/rooms/${room}`)).json()).me.stt_off;
  const other = await page.context().newPage();
  await other.goto(page.url());
  await other.locator('#voice-join').click();
  await expect(other.locator('#voice-stt')).toHaveAttribute('aria-pressed', 'true');
  await page.locator('#voice-join').click();
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(true);
  await stt.click();
  await expect(stt).toHaveAttribute('aria-pressed', 'false');
  await expect(stt).toHaveAttribute('data-tip', /not written down, and the others see it/);
  // The device engine follows: it runs only while my voice is to be transcribed, in each tab.
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(false);
  await expect(other.locator('#voice-stt')).toHaveAttribute('aria-pressed', 'false');
  await expect.poll(() => other.evaluate(() => window.grillVoiceOn)).toBe(false);
  await other.close();
  expect(await meOff()).toBe(true);

  await page.reload();
  await page.locator('#voice-join').click();
  await expect(stt).toHaveAttribute('aria-pressed', 'false');
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(false);
  await stt.click();
  await expect(stt).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(() => page.evaluate(() => window.grillVoiceOn)).toBe(true);
  expect(await meOff()).toBeUndefined();
});

// 010-AC23: a room read sent while the switch is in flight answers with the state before it; landing
// after the switch, it must not turn it back.
test('a room read from before the switch does not undo it', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  const room = await page.evaluate(() => roomID);
  const stt = page.locator('#voice-stt');
  await page.locator('#voice-join').click();
  await expect(stt).toHaveAttribute('aria-pressed', 'true');
  let releasePut;
  const putHeld = new Promise((r) => { releasePut = r; });
  await page.route(`**/api/rooms/${room}/voice/transcribe`, async (r) => { await putHeld; await r.continue(); });
  let releaseRead;
  const readHeld = new Promise((r) => { releaseRead = r; });
  let read;
  const readDone = new Promise((r) => { read = r; });
  let armed = false;
  await page.route(`**/api/rooms/${room}`, async (r) => {
    const mine = armed;
    const res = await r.fetch();
    const json = { ...(await res.json()), voice: true };
    if (mine) read();
    await readHeld;
    await r.fulfill({ response: res, json });
  });
  await stt.click();
  armed = true;
  await page.evaluate(() => { loadRoom().catch(() => {}); });
  await readDone; // served before the switch reached the server: stt_off still unset
  releasePut();
  await expect(stt).toHaveAttribute('aria-pressed', 'false');
  releaseRead();
  await page.waitForTimeout(500);
  await expect(stt).toHaveAttribute('aria-pressed', 'false');
});

// 031-AC10: the chat window asks for no voice token and connects no LiveKit room: with it open and
// used, the room page stays in the call, its one connection the only one.
test('the chat window leaves the call to the room page', async ({ browser, request }) => {
  const page = await voicePage(browser, request);
  await page.setViewportSize({ width: 1440, height: 900 });
  const room = await page.evaluate(() => roomID);
  let tokens = 0;
  page.context().on('request', (r) => { if (r.method() === 'GET' && new URL(r.url()).pathname === `/api/rooms/${room}/voice`) tokens++; });
  await page.locator('#voice-join').click();
  await expect(page.locator('#voice-leave')).toBeVisible();
  expect(tokens).toBe(1);

  const [chat] = await Promise.all([page.context().waitForEvent('page'), menuItem(page, 'Open chat in a new window')]);
  await expect(chat.locator('#me')).toHaveText('Alice');
  await expect(chat.locator('#voice')).toBeHidden();
  await chat.locator('#body').fill('from the chat window');
  await chat.locator('#body').press('Enter');
  await expect(page.locator('#messages')).toContainText('from the chat window');
  await chat.getByRole('button', { name: 'Raise my hand' }).click();
  await expect(page.getByRole('button', { name: 'Lower my hand' })).toBeVisible();
  // A reload of the chat window runs its start-up again: still no token.
  await chat.reload();
  await expect(chat.locator('#me')).toHaveText('Alice');

  expect(tokens).toBe(1);
  expect(await chat.evaluate(() => window.lkConnects ?? 0)).toBe(0);
  expect(await chat.evaluate(() => !!voice)).toBe(false);
  expect(await page.evaluate(() => [window.lkConnects, voice?.state])).toEqual([1, 'connected']);
  await expect(page.locator('#voice-leave')).toBeVisible();
});
