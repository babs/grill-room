import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';
import { roomWith, upload } from './helpers.js';

// The script tries everything a malicious upload would, then prints what it got.
const probe = `<!doctype html><html><body><pre id="result">pending</pre><script>
(async () => {
  const out = {};
  const room = location.pathname.split('/')[3];
  const attempt = async (key, fn) => { try { out[key] = await fn(); } catch (e) { out[key] = 'blocked:' + e.name; } };
  await attempt('cookie', () => document.cookie);
  await attempt('parent', () => parent.document.cookie);
  await attempt('get', () => fetch('/api/rooms/' + room, { credentials: 'include' }).then((r) => 'status:' + r.status));
  await attempt('post', () => fetch('/api/rooms/' + room + '/messages', {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' }, body: '{"body":"pwned-fetch"}',
  }).then((r) => 'status:' + r.status));
  await attempt('nocors', () => fetch('/api/rooms/' + room + '/messages', {
    method: 'POST', mode: 'no-cors', credentials: 'include', body: '{"body":"pwned-nocors"}',
  }).then((r) => 'type:' + r.type));
  document.getElementById('result').textContent = JSON.stringify(out);
})();
</script></body></html>`;

async function probeResult(frame) {
  const result = frame.locator('#result');
  await expect(result).not.toHaveText('pending');
  return JSON.parse(await result.textContent());
}

function expectConfined(out) {
  expect(out.cookie).toBe('blocked:SecurityError');
  expect(out.parent).toBe('blocked:SecurityError');
  for (const key of ['get', 'post', 'nocors']) expect(out[key], key).toMatch(/^blocked:/);
}

// 001-AC11: a script in an uploaded HTML document can neither read the room cookie nor call the
// room API, in the room's iframe or opened on its own.
test('uploaded HTML is sandboxed away from the cookie and the API', async ({ browser, request }) => {
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);

  // Control: the room page itself, holding the cookie, does reach the API.
  expect(await alice.evaluate((id) => fetch('/api/rooms/' + id).then((r) => r.status), room.id)).toBe(200);

  await upload(alice, 'probe.html', 'text/html', probe);
  const frame = alice.locator('#doc-frame iframe');
  await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
  expectConfined(await probeResult(alice.frameLocator('#doc-frame iframe')));

  // Bob gets the document live and runs the same probe.
  await bob.locator('#documents').getByRole('button', { name: 'probe.html' }).click();
  expectConfined(await probeResult(bob.frameLocator('#doc-frame iframe')));

  // Opened as a page of its own, the CSP sandbox still holds, without the iframe attribute.
  const src = await frame.getAttribute('src');
  const top = await alice.context().newPage();
  const resp = await top.goto(src);
  expect(resp.headers()['content-security-policy']).toContain('sandbox allow-scripts');
  expectConfined(await probeResult(top));

  // Nothing got through: no message was posted.
  await alice.getByLabel('Message').fill('still here');
  await alice.getByLabel('Message').press('Enter');
  await expect(bob.locator('#messages')).toContainText('still here');
  await expect(bob.locator('#messages')).not.toContainText('pwned');
  await expect(alice.locator('#messages')).not.toContainText('pwned');
});

const markdown = `# Rollout plan

| Step | Owner |
|------|-------|
| Freeze | Alice |

\`\`\`go
func main() {}
\`\`\`

\`\`\`mermaid
graph TD
  A[Plan] --> B[Grill]
\`\`\`
`;

// 1x1 transparent PNG.
const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=', 'base64');

// 001-AC11 001-AC12 001-AC13: each upload kind shows in the room, live for every participant.
test('uploads render for everyone', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);

  await upload(alice, 'plan.md', 'text/markdown', markdown);
  await expect(bob.locator('#documents')).toContainText('plan.md');
  const doc = alice.frameLocator('#doc-frame iframe');
  await expect(doc.locator('h1')).toHaveText('Rollout plan');
  await expect(doc.locator('table td').first()).toHaveText('Freeze');
  await expect(doc.locator('.chroma')).toBeVisible();
  // Mermaid ran inside the sandbox: the source became a diagram.
  const diagram = doc.locator('.mermaid-viewport > svg');
  await expect(diagram).toBeVisible();
  await expect(diagram).toContainText('Grill');

  await upload(alice, 'plan.docx', 'application/octet-stream', readFileSync('../internal/convert/testdata/fixture.docx'));
  await expect(alice.locator('#doc-meta')).toContainText('Markdown copy');
  await expect(doc.locator('h1')).toHaveText('Grill plan');
  await expect(alice.locator('#doc-original')).toBeVisible();
  await expect(bob.locator('#documents')).toContainText('plan.docx');

  await upload(alice, 'shot.png', 'image/png', png);
  const img = alice.locator('#doc-frame img');
  await expect(img).toBeVisible();
  expect(await img.evaluate((el) => el.complete && el.naturalWidth)).toBe(1);

  await upload(alice, 'notes.txt', 'text/plain', 'plain text');
  await expect(alice.locator('#uploads li', { hasText: 'notes.txt' }).locator('.why')).toHaveText("This file type isn't supported. Use PDF, Word, slides, images, Markdown or HTML.");
  await expect(bob.locator('#documents li')).toHaveCount(3);
});
