import { test, expect } from '@playwright/test';
import { roomWith } from './helpers.js';

// 005-AC23: any member edits the transcription glossary in Manage, sees how many terms the prompt
// holds, and the room says who changed it.
test('a member edits the transcription glossary', async ({ browser, request }) => {
  const { pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  await bob.getByRole('button', { name: 'Manage' }).click();
  const form = bob.locator('#stt-glossary');
  // Alice, Bob and the room's Grill persona.
  await expect(form.locator('#glossary-used')).toHaveText('3 / ~70 terms used');
  await form.getByLabel('Transcription glossary').fill('Kubernetes\nArgo CD, Helm');
  await form.getByRole('button', { name: 'Save glossary' }).click();
  await expect(form.locator('#glossary-used')).toHaveText('6 / ~70 terms used');
  await expect(form.locator('#glossary-used')).toHaveAttribute('title', 'Avec Alice, Bob et Grill, on parle de Kubernetes, Argo CD et Helm.');
  await expect(alice.locator('.msg.system', { hasText: 'Bob changed the transcription glossary' })).toHaveCount(1);
  await form.getByLabel('Transcription glossary').fill('x'.repeat(61));
  await form.getByRole('button', { name: 'Save glossary' }).click();
  await expect(bob.locator('#manage .said')).toContainText('at most 60 characters');
});
