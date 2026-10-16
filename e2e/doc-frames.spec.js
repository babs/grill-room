import { test, expect } from './fixtures.js';
import { roomWith, upload } from './helpers.js';

const pdf = `%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 100]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
4 0 obj<</Length 36>>stream
BT /F1 18 Tf 20 40 Td (Framed) Tj ET
endstream endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
trailer<</Root 1 0 R>>
%%EOF
`;

// expectRendered waits for the Markdown and the PDF to show in page's document frame. Chrome draws
// a PDF in a plugin no script can read: there the frame must end on the PDF, not an error page.
async function expectRendered(page, browserName) {
  await page.locator('#documents button.doc', { hasText: 'framed.md' }).click();
  await expect(page.frameLocator('#doc-frame iframe').getByRole('heading', { name: 'Framed notes' })).toBeVisible();
  await page.locator('#documents button.doc', { hasText: 'framed.pdf' }).click();
  await expect(page.locator('#doc-frame iframe')).toHaveAttribute('src', /\/original$/);
  if (browserName === 'firefox') {
    await expect(page.frameLocator('#doc-frame iframe').getByText('Framed')).toBeVisible();
  } else {
    await expect.poll(() => page.frames().map((f) => f.url()).find((u) => u.endsWith('/original'))).toBeTruthy();
  }
}

// Firefox refused the sandboxed Markdown frame under the room page's COEP from v0.0.21 on, while
// Chrome loaded it: the room page and the docs window render both formats in either browser.
test('documents render in the room and the docs window', async ({ browser, request, browserName }) => {
  const { room, pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const blocked = [];
  alice.context().on('console', (m) => { if (/blocked|Cross-Origin/i.test(m.text())) blocked.push(m.text()); });
  expect(await alice.evaluate(() => crossOriginIsolated)).toBe(true);
  await upload(alice, 'framed.md', 'text/markdown', '# Framed notes\n\nBody text.\n');
  await expect(alice.locator('#documents button.doc', { hasText: 'framed.md' })).toBeVisible();
  await upload(alice, 'framed.pdf', 'application/pdf', pdf);
  await expect(alice.locator('#documents button.doc', { hasText: 'framed.pdf' })).toBeVisible();
  await expectRendered(alice, browserName);

  await alice.goto(`/r/${room.id}/docs`);
  await expectRendered(alice, browserName);
  expect(blocked).toEqual([]);
});
