import { readFileSync } from 'node:fs';
import { test, expect } from '@playwright/test';
import { roomWith, upload, docFrame, annotateSelection } from './helpers.js';

// 1x1 transparent PNG.
const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=', 'base64');

const plan = '# Rollout plan\n\nThe rollout needs a rollback step before the freeze.\n\nOwners sign off.\n';

// marked lists the text under the frame's open annotation marks.
const marked = (frame) => frame.evaluate(() => [...(CSS.highlights.get('gr-annot') || [])].map((r) => r.toString()));

// 001-AC18 001-AC13: a mark on selected text and its thread reach the other participant live, on
// a Markdown upload and on the Markdown copy of a DOCX.
test('text annotation appears live in the other browser', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await upload(alice, 'plan.md', 'text/markdown', plan);
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  await bob.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
  await expect(bob.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  const bobFrame = await docFrame(bob);
  // Record what the room page tells Bob's frame: anchors and statuses, never thread text.
  await bobFrame.evaluate(() => {
    window.seen = [];
    addEventListener('message', (e) => window.seen.push(JSON.stringify(e.data)));
  });

  await annotateSelection(alice, 'rollback step', 'How long does a rollback take?');
  const item = bob.locator('#annot-list li.annot');
  await expect(item).toContainText('How long does a rollback take?');
  await expect(item).toContainText('"rollback step"');
  await expect.poll(() => marked(bobFrame)).toEqual(['rollback step']);

  // A click on the mark in the frame opens its thread in the room page.
  await bob.frameLocator('#doc-frame iframe').getByText('rollback step').click();
  await expect(item).toHaveClass(/active/);
  await item.getByLabel('Reply').fill('Ten minutes.');
  await item.getByRole('button', { name: 'Reply' }).click();
  await expect(alice.locator('#annot-list')).toContainText('Ten minutes.');
  await alice.locator('#annot-list').getByRole('button', { name: 'Mark done' }).click();
  await expect(item.locator('.annot-badge')).toHaveText('done');
  await expect.poll(() => marked(bobFrame)).toEqual([]);

  const seen = (await bobFrame.evaluate(() => window.seen)).join('\n');
  expect(seen).toContain('rollback step');
  expect(seen).not.toContain('Ten minutes');
  expect(seen).not.toContain('Alice');

  await upload(alice, 'plan.docx', 'application/octet-stream', readFileSync('../internal/convert/testdata/fixture.docx'));
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Grill plan');
  await annotateSelection(alice, 'Freeze the schema', 'Which schema?');
  await bob.locator('#documents').getByRole('button', { name: 'plan.docx' }).click();
  await expect(bob.locator('#annot-list')).toContainText('Which schema?');
  await expect(bob.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Grill plan');
  await expect.poll(async () => marked(await docFrame(bob))).toEqual(['Freeze the schema']);
});

const board = `<!doctype html><html><head><title>Board</title></head><body>
<h1>Launch board</h1>
<ul id="steps"><li>Freeze the schema</li><li>Ship behind a flag</li></ul>
<button id="rerender" type="button">Re-render</button>
<script>
// Rebuilt from data, as a framework would: no attribute of the old nodes survives.
const steps = [...document.querySelectorAll('#steps li')].map((li) => li.textContent);
document.getElementById('rerender').addEventListener('click', () => {
  steps.unshift('Back up the database');
  document.getElementById('steps').replaceChildren(...steps.map((t) => Object.assign(document.createElement('li'), { textContent: t })));
});
</script>
</body></html>`;

// 001-AC19: an element picked in an HTML document is marked for the other participant, and the
// mark follows the element when a script re-renders the list around it.
test('element annotation follows a re-render', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await upload(alice, 'board.html', 'text/html', board);
  const aliceDoc = alice.frameLocator('#doc-frame iframe');
  await expect(aliceDoc.locator('h1')).toHaveText('Launch board');
  await alice.getByRole('button', { name: 'Annotate an element' }).click();
  await aliceDoc.getByText('Ship behind a flag').click();
  await expect(alice.locator('#annot-target')).toHaveText('<li> element: Ship behind a flag');
  await alice.getByRole('textbox', { name: 'Annotation', exact: true }).fill('Which flag?');
  await alice.locator('#annot-new').getByRole('button', { name: 'Save' }).click();

  await bob.locator('#documents').getByRole('button', { name: 'board.html' }).click();
  const bobDoc = bob.frameLocator('#doc-frame iframe');
  await expect(bob.locator('#annot-list')).toContainText('Which flag?');
  const mark = bobDoc.locator('[data-gr-annot]');
  await expect(mark).toHaveText('Ship behind a flag');
  await expect(mark).toHaveCount(1);

  // The document's own script rebuilds every <li>: the mark lands on the new node, one place down.
  const before = await (await docFrame(bob)).evaluate(() => document.querySelector('[data-gr-annot]').outerHTML);
  await bobDoc.getByRole('button', { name: 'Re-render' }).click();
  await expect(bobDoc.locator('li')).toHaveCount(3);
  await expect(bobDoc.locator('li:nth-of-type(3)')).toHaveAttribute('data-gr-annot', /\d+/);
  await expect(mark).toHaveCount(1);
  await expect(mark).toHaveText('Ship behind a flag');
  expect(before).toContain('data-gr-annot');
});

// 001-AC20: an image attached to an annotation message reaches the other participant.
test('image attachment appears in the other browser', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await upload(alice, 'plan.md', 'text/markdown', plan);
  await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
  await bob.locator('#documents').getByRole('button', { name: 'plan.md' }).click();

  await alice.getByRole('button', { name: 'Comment on the document' }).click();
  await expect(alice.locator('#annot-target')).toHaveText('Whole document');
  await alice.getByRole('textbox', { name: 'Annotation', exact: true }).fill('See the chart');
  await alice.locator('#annot-files').setInputFiles({ name: 'chart.png', mimeType: 'image/png', buffer: png });
  await alice.locator('#annot-new').getByRole('button', { name: 'Save' }).click();

  const img = bob.locator('#annot-list img');
  await expect(img).toBeVisible();
  await expect.poll(() => img.evaluate((el) => el.complete && el.naturalWidth)).toBe(1);
  const resp = await bob.context().request.get(await img.getAttribute('src'));
  expect(resp.status()).toBe(200);
  expect(resp.headers()['content-type']).toBe('image/png');

  // A non-image is refused with the reason shown, and nothing is posted.
  await alice.getByRole('button', { name: 'Comment on the document' }).click();
  await alice.getByRole('textbox', { name: 'Annotation', exact: true }).fill('Not an image');
  await alice.locator('#annot-files').setInputFiles({ name: 'x.png', mimeType: 'image/png', buffer: Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"></svg>') });
  await alice.locator('#annot-new').getByRole('button', { name: 'Save' }).click();
  await expect(alice.locator('#annot-status')).toContainText('must be PNG, JPEG, GIF or WebP');
  await expect(bob.locator('#annot-list li.annot')).toHaveCount(1);
});

// What a hostile document posts to the room page: every message must be dropped but the last.
const hostile = `<!doctype html><html><body><p>valid quote here</p><script>
const bad = [
  'string', null, [1], 42,
  { type: 'gr:anchor' },
  { type: 'gr:anchor', anchor: { kind: 'text', exact: 'x'.repeat(4001) } },
  { type: 'gr:anchor', anchor: { kind: 'text', exact: 'x'.repeat(1 << 20) } },
  { type: 'gr:anchor', anchor: { kind: 'text', exact: 'ok', prefix: 'p'.repeat(65) } },
  { type: 'gr:anchor', anchor: { kind: 'text', exact: '   ' } },
  { type: 'gr:anchor', anchor: { kind: 'text', exact: 'ok', extra: 1 } },
  { type: 'gr:anchor', anchor: { kind: 'element', path: 'body > p:nth-of-type(1), script', tag: 'p' } },
  { type: 'gr:anchor', anchor: { kind: 'element', path: 'body > div:nth-of-type(1)', tag: 'p' } },
  { type: 'gr:anchor', anchor: { kind: 'element', path: 'body' + ' > div:nth-of-type(1)'.repeat(65), tag: 'div' } },
  { type: 'gr:anchor', anchor: { kind: 'element', path: 'body', tag: 'body', fingerprint: 'f'.repeat(129) } },
  { type: 'gr:open', id: '1' },
  { type: 'gr:open', id: 2 ** 60 },
  { type: 'gr:pick', on: true },
  { type: 'gr:ready', token: 'x' },
  { type: 'gr:steal' },
];
for (const m of bad) parent.postMessage(m, '*');
parent.postMessage({ type: 'gr:anchor', anchor: { kind: 'text', exact: 'valid quote', prefix: '', suffix: ' here' } }, '*');
</script></body></html>`;

// The bridge treats the frame as hostile: malformed, oversized and unknown messages are dropped.
test('postMessage bridge rejects malformed and oversized messages', async ({ browser, request }) => {
  const { pages: [alice] } = await roomWith(browser, request, ['Alice']);
  const rejected = [];
  alice.on('console', (m) => {
    if (m.type() === 'warning' && m.text().startsWith('bridge: rejected')) rejected.push(m.text());
  });
  await upload(alice, 'hostile.html', 'text/html', hostile);
  await expect(alice.locator('#annot-target')).toHaveText('"valid quote"');
  await expect.poll(() => rejected.length).toBe(19);
  expect(rejected.every((t) => t.endsWith('malformed or oversized'))).toBe(true);

  // A message from anything but the document frame is ignored outright.
  await alice.evaluate(() => postMessage({ type: 'gr:anchor', anchor: { kind: 'text', exact: 'from the page' } }, '*'));
  await expect(alice.locator('#annot-target')).toHaveText('"valid quote"');
});
