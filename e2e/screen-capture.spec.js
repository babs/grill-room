import { test, expect } from './fixtures.js';
import { roomWith, smallTargets } from './helpers.js';

// Headless Chrome cannot capture a screen, even with --auto-select-desktop-capture-source and
// --use-fake-ui-for-media-stream (NotReadableError): getDisplayMedia is stubbed with a canvas stream.
// mode: 'ok' shares a red 64 × 48 canvas, 'frameless' a canvas that never sends a frame, 'ends' one
// whose share ends before its first frame, 'none' removes getDisplayMedia, any other mode is the
// name of the DOMException it rejects with. Every track shared stays in window.shared, the last
// options in window.captureOptions, unhandled rejections in window.unhandled, the focus behaviours
// asked of a CaptureController in window.focusBehaviors.
const stubCapture = async (page, mode) => {
  await page.context().addInitScript((mode) => {
    window.shared = [];
    window.captureCalls = 0;
    window.unhandled = [];
    addEventListener('unhandledrejection', (e) => window.unhandled.push(String(e.reason)));
    window.focusBehaviors = [];
    if (window.CaptureController) CaptureController.prototype.setFocusBehavior = (b) => { window.focusBehaviors.push(b); };
    if (mode === 'none') {
      delete MediaDevices.prototype.getDisplayMedia;
      return;
    }
    MediaDevices.prototype.getDisplayMedia = async function (options) {
      window.captureCalls++;
      window.captureOptions = options;
      // A real picker takes a moment: a second click lands while the first waits.
      await new Promise((r) => setTimeout(r, 200));
      if (!['ok', 'frameless', 'ends'].includes(mode)) throw new DOMException('No screen to share', mode);
      const c = Object.assign(document.createElement('canvas'), { width: 64, height: 48 });
      if (mode !== 'ok') {
        // A rate of 0 sends a frame only on requestFrame(), never called here.
        const stream = c.captureStream(0);
        window.shared.push(...stream.getTracks());
        // stop() fires no ended event: the share ending on its own does.
        if (mode === 'ends') setTimeout(() => { for (const t of stream.getTracks()) { t.dispatchEvent(new Event('ended')); t.stop(); } }, 300);
        return stream;
      }
      const g = c.getContext('2d');
      const paint = () => { g.fillStyle = '#c33'; g.fillRect(0, 0, 64, 48); };
      paint();
      const stream = c.captureStream();
      // A canvas stream sends a frame when the canvas is painted.
      setInterval(paint, 50);
      window.shared.push(...stream.getTracks());
      return stream;
    };
  }, mode);
  await page.reload();
  await expect(page.locator('#me')).not.toBeEmpty();
  await expect.poll(() => page.evaluate(() => !!window.grillRoom?.upload)).toBe(true);
};

const pending = (page) => page.getByRole('list', { name: 'Files to send' }).locator('li');
const capture = (page) => page.getByRole('button', { name: 'Capture screen' });
const now = (page) => page.evaluate(() => Date.now());
// stampOf reads a capture name's local time, in the page's own time zone.
const stampOf = (page, name) => page.evaluate((n) => {
  const [, y, mo, d, h, mi, s] = n.match(/^capture-(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})\.png$/).map(Number);
  return new Date(y, mo - 1, d, h, mi, s).getTime();
}, name);

// 029-AC15: one click asks for the screen once, adds its frame as a PNG named by the local clock,
// stops the share, and the message carries it.
test('a screen capture is a pending PNG and goes with the message', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await stubCapture(alice, 'ok');
  await expect(capture(alice)).toBeVisible();
  // Next to the paperclip.
  expect(await capture(alice).evaluate((b) => b.previousElementSibling.previousElementSibling.id)).toBe('attach-open');

  const before = await now(alice);
  await capture(alice).dblclick();
  await expect(pending(alice)).toHaveCount(1);
  const after = await now(alice);
  const name = await pending(alice).locator('.file-name').textContent();
  expect(name).toMatch(/^capture-\d{8}-\d{6}\.png$/);
  const at = await stampOf(alice, name);
  expect(at).toBeGreaterThanOrEqual(Math.floor(before / 1000) * 1000);
  expect(at).toBeLessThanOrEqual(after);
  // With a CaptureController, the shared tab is told not to take the focus.
  expect(await alice.evaluate(() => [!!window.captureOptions.controller, window.focusBehaviors])).toEqual(
    await alice.evaluate(() => (window.CaptureController ? [true, ['no-focus-change']] : [false, []])));
  await expect.poll(() => pending(alice).locator('img').evaluate((i) => i.naturalWidth)).toBe(64);
  // The double click asked once; every track of the share ended.
  expect(await alice.evaluate(() => window.captureCalls)).toBe(1);
  expect(await alice.evaluate(() => window.shared.map((t) => t.readyState))).toEqual(['ended']);
  await expect(alice.locator('#send-error')).toBeEmpty();

  await alice.locator('#body').fill('My screen');
  await alice.getByRole('button', { name: 'Send' }).click();
  await expect(pending(alice)).toHaveCount(0);
  const thumb = bob.locator('.msg.text', { hasText: 'My screen' }).getByRole('button', { name });
  await expect(thumb.locator('img')).toBeVisible();
  await expect.poll(() => thumb.locator('img').evaluate((i) => i.naturalWidth)).toBe(64);
});

// 029-AC15: a capture follows the limit of the other pending files.
test('a capture past the limit is refused', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await stubCapture(alice, 'ok');
  const rules = await alice.evaluate(() => window.grillRoom.upload);
  // Mouse clicks: each new chip lands under the still pointer, its name's tip over the button.
  for (let i = 1; i <= rules.max_files; i++) {
    await capture(alice).click();
    await expect(pending(alice)).toHaveCount(i);
  }
  await capture(alice).click();
  await expect(alice.locator('#send-error')).toHaveText(rules.too_many[0].toUpperCase() + rules.too_many.slice(1));
  await expect(pending(alice)).toHaveCount(rules.max_files);
  // Refused before the picker opens.
  expect(await alice.evaluate(() => window.captureCalls)).toBe(rules.max_files);
  expect(await alice.evaluate(() => window.shared.every((t) => t.readyState === 'ended'))).toBe(true);
});

// 029-AC15: a narrow window with getDisplayMedia keeps a 44 px target.
test('the capture button at 390 px', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 390, height: 844 });
  await stubCapture(alice, 'ok');
  const box = await capture(alice).boundingBox();
  expect(Math.min(box.width, box.height)).toBeGreaterThanOrEqual(44);
  expect(await smallTargets(alice), 'composer').toEqual([]);
});

// 029-AC16: a touch phone that defines getDisplayMedia (Android refuses it) gets no button either.
test('no capture button on a touch phone', async ({ browser, browserName, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const phone = await (await browser.newContext({ storageState: await alice.context().storageState(),
    viewport: { width: 390, height: 844 }, isMobile: browserName !== 'firefox', hasTouch: true })).newPage();
  await phone.goto(alice.url());
  await stubCapture(phone, 'ok');
  expect(await phone.evaluate(() => typeof navigator.mediaDevices.getDisplayMedia)).toBe('function');
  await expect(phone.getByRole('button', { name: 'Attach files' })).toBeVisible();
  await expect(capture(phone)).toHaveCount(0);
});

// 029-AC16: no getDisplayMedia (a phone), no button.
test('no capture button without getDisplayMedia', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  await alice.setViewportSize({ width: 390, height: 844 });
  await stubCapture(alice, 'none');
  await expect(alice.getByRole('button', { name: 'Attach files' })).toBeVisible();
  await expect(capture(alice)).toHaveCount(0);
  await expect(alice.locator('#capture-open')).toBeHidden();
});

// 029-AC16: a refused or cancelled picker is no failure; any other one says so in the composer.
for (const [mode, shown] of [['NotAllowedError', ''], ['AbortError', ''], ['NotReadableError', 'Screen capture failed: No screen to share.']]) {
  test(`a picker rejecting with ${mode}`, async ({ browser, request }) => {
    const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await stubCapture(alice, mode);
    // A failure shown before goes once the picker opens.
    await alice.evaluate(() => sendError('An earlier failure.'));
    await capture(alice).click();
    // capturing is room.js's guard: false again once the rejection is handled.
    await expect.poll(() => alice.evaluate(() => window.captureCalls === 1 && !capturing)).toBe(true);
    await expect(alice.locator('#send-error')).toHaveText(shown);
    await expect(pending(alice)).toHaveCount(0);
  });
}

// 029-AC15: a share that never sends a frame, or ends before its first, is a failure in bounded
// time: the share stops and the button works again.
for (const [mode, shown] of [['frameless', 'Screen capture failed: The screen sent no picture.'], ['ends', 'Screen capture failed: The share ended before its first frame.']]) {
  test(`a share that ${mode === 'ends' ? 'ends before' : 'never sends'} its first frame`, async ({ browser, browserName, request }) => {
    test.skip(mode === 'ends' && browserName === 'firefox', 'Firefox calls no listener for an ended event dispatched on a track');
    const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
    await stubCapture(alice, mode);
    await capture(alice).click();
    await expect(alice.locator('#send-error')).toHaveText(shown, { timeout: 10_000 });
    expect(await alice.evaluate(() => [capturing, window.shared.map((t) => t.readyState), window.unhandled])).toEqual([false, ['ended'], []]);
    await expect(pending(alice)).toHaveCount(0);
    await capture(alice).click();
    await expect.poll(() => alice.evaluate(() => window.captureCalls)).toBe(2);
  });
}
