import { readFileSync } from 'node:fs';
import { test, expect } from '../fixtures.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };
const tts = { Authorization: `Bearer ${process.env.TTS_TOKEN}` };
const shots = 'test-results/persona-voice';

// silentMic: a silent microphone, so the call holds a listener and nothing to transcribe.
function silentMic() {
  navigator.mediaDevices.getUserMedia = async () => {
    const ctx = new AudioContext();
    const dst = ctx.createMediaStreamDestination();
    ctx.createGain().connect(dst);
    return dst.stream;
  };
}

async function alice(browser, request, viewport) {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Call UI' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const ctx = await browser.newContext({ permissions: ['microphone'], viewport, hasTouch: viewport.width < 600 });
  ctx.setDefaultTimeout(15_000);
  await ctx.addInitScript(snoozePush, snoozeKey);
  await ctx.addInitScript(silentMic);
  const page = await ctx.newPage();
  const url = new URL(inv.url);
  await page.goto(url.pathname + url.hash);
  await page.getByLabel('Your name').fill('Alice');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Alice');
  return { room, ctx, page };
}

// joinAudio joins the call and returns once the server knows: the beat that says so got its answer.
async function joinAudio(page) {
  const inAudio = page.waitForResponse((r) => r.url().endsWith('/presence') && r.request().postDataJSON().audio === true);
  await page.getByRole('button', { name: 'Join audio' }).click();
  await expect(page.getByRole('button', { name: 'Audio options' })).toBeVisible();
  expect((await inAudio).status()).toBe(204);
}

const pill = (page, name) => page.locator(`.persona-pill[data-name="${name}"]`);
async function speak(request, room, persona, spoken) {
  const r = await request.post('/api/admin/test/speak', { headers: admin, data: { room, persona, spoken } });
  expect(r.status(), await r.text()).toBe(204);
}

for (const [label, viewport] of [['desktop', { width: 1440, height: 900 }], ['phone', { width: 390, height: 844 }]]) {
  // 013-AC26, AC27, AC28, AC29, AC37, AC39: the pill opens the card, ▶ asks, the speaker mutes the
  // persona for this listener across a reload; the sheet's Identity tab sets whether it speaks, its
  // voice, with a sample and the credit, and its rate; the pill shows speaking and waiting to speak.
  test(`persona in the call, ${label}`, async ({ browser, request }) => {
    const { room, ctx, page } = await alice(browser, request, viewport);
    try {
      // The pod hands its samples over at start; this stack's fake engine has none: hand one over.
      expect((await request.put('/api/tts/samples/alba/en', { headers: tts, data: readFileSync(process.env.PERSONA_VOICE_SAMPLE) })).status()).toBe(204);
      const templates = await (await page.request.get(`/api/rooms/${room.id}/templates`)).json();
      const sec = templates.find((t) => t.name === 'Secretary');
      expect((await page.request.post(`/api/rooms/${room.id}/personas`, { data: { template_id: sec.id } })).status()).toBe(201);
      await joinAudio(page);
      await expect(page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(2, { timeout: 30_000 });

      // The pill: a click opens the card; ▶ asks, and with no AI host says why.
      const grill = pill(page, 'Grill');
      await expect(grill.locator('.persona-run')).toHaveAttribute('aria-label', 'Ask Grill now');
      await expect(grill.locator('.persona-hear')).toBeVisible();
      await grill.locator('button.persona').click();
      const card = page.locator('#persona-card');
      await expect(card).toBeVisible();
      await expect(card.locator('dl')).toContainText('In the call');
      await expect(card.getByRole('checkbox', { name: 'Mute for me' })).not.toBeChecked();
      await page.screenshot({ path: `${shots}/card-${label}.png` });
      await card.getByRole('button', { name: 'Ask Grill now' }).click();
      await expect(page.locator('#why')).toBeVisible();
      await page.locator('#why').getByRole('button', { name: 'OK' }).click();

      // Mute for me: Grill's track goes, Secretary's stays, and it holds across a reload.
      const grillID = await grill.getAttribute('data-id');
      await grill.locator('.persona-hear').click();
      await expect(grill.locator('.persona-hear')).toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator(`#voice-audio audio[data-pid="persona-${grillID}"]`)).toHaveCount(0);
      await expect(page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(1);
      // Unsubscribed, not only hidden: no audio of Grill reaches this page (013-AC28).
      const subscribed = (id) => page.evaluate((i) => window.grillDiag.subscribed(i), id);
      await expect.poll(() => subscribed(`persona-${grillID}`)).toBe(false);
      await page.reload();
      await joinAudio(page);
      await expect(pill(page, 'Grill').locator('.persona-hear')).toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator('#voice-audio audio[data-pid^="persona-"]')).toHaveCount(1, { timeout: 30_000 });
      await expect(page.locator(`#voice-audio audio[data-pid="persona-${grillID}"]`)).toHaveCount(0);
      await expect.poll(() => subscribed(`persona-${grillID}`)).toBe(false);
      await pill(page, 'Grill').locator('button.persona').click();
      await page.locator('#persona-card').getByRole('checkbox', { name: 'Mute for me' }).uncheck();
      await expect(page.locator(`#voice-audio audio[data-pid="persona-${grillID}"]`)).toHaveCount(1, { timeout: 30_000 });
      expect(await subscribed(`persona-${grillID}`)).toBe(true);
      // The checkbox kept the focus through the card's redraw.
      await expect(page.locator('#persona-card-mute')).toBeFocused();
      await page.keyboard.press('Escape');

      // Speaking, then waiting to speak: an icon and, on a desktop, the word.
      await speak(request, room.id, 'Grill', Array(40).fill('word').join(' ') + '.');
      await speak(request, room.id, 'Secretary', 'Noted.');
      await expect(pill(page, 'Grill').locator('.vstate.speaking svg')).toBeVisible({ timeout: 30_000 });
      await expect(pill(page, 'Secretary').locator('.vstate.speaking-wait svg')).toBeVisible();
      await expect(pill(page, 'Grill').locator('button.persona')).toHaveAttribute('aria-label', /Grill is speaking in the call/);
      await expect(pill(page, 'Secretary').locator('button.persona')).toHaveAttribute('data-tip', 'Secretary is waiting to speak in the call');
      if (label === 'desktop') {
        await expect(pill(page, 'Grill').locator('.pstate')).toHaveText('speaking');
        await expect(pill(page, 'Secretary').locator('.pstate')).toHaveText('waiting to speak');
      }
      await page.screenshot({ path: `${shots}/pills-${label}.png` });

      // The sheet's Identity tab: speaks, voice with its sample and credit, rate.
      await pill(page, 'Grill').locator('button.persona').click();
      await page.locator('#persona-card').getByRole('button', { name: 'Edit Grill' }).click();
      const sheet = page.locator('#persona-sheet');
      await expect(sheet.getByRole('checkbox', { name: 'Speaks in the call' })).toBeChecked();
      const voices = sheet.getByRole('radiogroup', { name: 'Voice of Grill' });
      await expect(voices.getByRole('radio', { name: /^Alba/ })).toBeChecked();
      // Secretary took Charles by default: the row says so, and it may still be picked (013-AC27).
      await expect(voices.locator('.voice-opt', { hasText: 'Charles' })).toContainText('Secretary has it');
      const sample = page.waitForResponse((r) => r.url().endsWith('/voices/alba/en') && [200, 206].includes(r.status()));
      await voices.getByRole('button', { name: 'Play a sample of Alba' }).click();
      await sample;
      const credit = voices.getByRole('button', { name: 'Credit for Alba' });
      if (label === 'phone') await credit.tap();
      else await credit.hover();
      await expect(page.locator('#person-tip')).toContainText('Alba MacKenna');
      await page.screenshot({ path: `${shots}/voices-${label}.png` });
      await voices.getByRole('radio', { name: /^Charles/ }).check();
      await sheet.getByLabel('Speaking rate').fill('1.2');
      await sheet.getByRole('button', { name: 'Save' }).click();
      await expect(page.locator('#persona-status')).toContainText('Grill saved');
      const ps = await (await page.request.get(`/api/rooms/${room.id}/agent`)).json();
      const g = ps.personas.find((p) => p.name === 'Grill');
      expect([g.voice, g.voice_rate, g.speaks]).toEqual(['charles', 1.2, true]);
    } finally {
      await ctx.close();
    }
  });
}
