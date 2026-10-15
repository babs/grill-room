import { test, expect } from './fixtures.js';
import { roomWith, upload } from './helpers.js';

const pdf = `%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 100]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
4 0 obj<</Length 41>>stream
BT /F1 18 Tf 20 40 Td (Isolated) Tj ET
endstream endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
trailer<</Root 1 0 R>>
%%EOF
`;

// frameURL is the URL the document frame ended on: a frame COEP refuses ends on Chrome's error page.
async function frameURL(page) {
  const frame = await (await page.locator('#doc-frame iframe').elementHandle()).contentFrame();
  await frame.waitForLoadState();
  return frame.url();
}

// 010-AC4: the room page is cross-origin isolated, and the sandboxed document frame, its
// /static/doc/ files, a PDF and the docs window still load under COEP.
test('the room page is isolated and its frames still load', async ({ browser, request }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const failed = [];
  const docFiles = [];
  alice.on('requestfailed', (r) => failed.push(r.url() + ' ' + r.failure()?.errorText));
  alice.on('response', (r) => { if (r.url().includes('/static/doc/')) docFiles.push(r.status()); });
  expect(await alice.evaluate(() => crossOriginIsolated)).toBe(true);

  await upload(alice, 'notes.md', 'text/markdown', '# Isolated notes\n\nBody text.\n');
  const doc = alice.frameLocator('#doc-frame iframe');
  await expect(doc.getByRole('heading', { name: 'Isolated notes' })).toBeVisible();
  expect(await frameURL(alice)).toContain('/view');
  // The frame's opaque origin loads /static/doc/ cross-origin: COEP must not refuse it.
  await expect.poll(() => docFiles.length).toBeGreaterThan(0);
  expect(docFiles.every((s) => s === 200)).toBe(true);

  await upload(alice, 'isolated.pdf', 'application/pdf', pdf);
  await expect(alice.locator('#doc-frame iframe')).toHaveAttribute('src', /\/original$/);
  await expect.poll(() => frameURL(alice)).toMatch(/\/original$/);

  const [docs] = await Promise.all([alice.waitForEvent('popup'), alice.evaluate((id) => window.open('/r/' + id + '/docs', 'grill-docs'), room.id)]);
  await docs.waitForLoadState();
  expect(await docs.evaluate(() => crossOriginIsolated)).toBe(true);
  // Same origin, same COOP: the room page keeps its handle on the window it opened.
  expect(await docs.evaluate(() => window.opener !== null)).toBe(true);
  expect(failed).toEqual([]);
});
