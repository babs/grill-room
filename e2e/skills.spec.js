import { test, expect } from './fixtures.js';
import { roomWith, startDaemon, pairDaemon, ask, skillZip, personaSheet, closePersonas } from './helpers.js';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

async function uploadSkill(page, zip, scope, status) {
  // Skills fold under "Skills" on the profile page (005-AC41).
  await page.locator('#skills').evaluate((d) => { d.open = true; });
  await page.locator('#skill-file').setInputFiles(zip);
  await page.locator('#skill-scope').selectOption(scope);
  await page.getByRole('button', { name: 'Add skill', exact: true }).click();
  await expect(page.locator('#skill-status')).toHaveText(status);
}

// openPanel opens Grill's Capabilities tab, where its skills sit (009-AC2).
const openPanel = (page) => personaSheet(page, 'Grill', 'Capabilities');

// 002-AC29 002-AC30 002-AC31 002-AC32: Alice uploads a personal skill from her profile page (a
// zip without frontmatter is refused, and says why) and Bob shares one; in the personas panel
// Alice sees the shared skill and her own, not Bob's personal one. Her first activation shows the
// disclaimer, her next ones do not, in this room or another. A turn (real daemon and sandbox,
// fake claude) runs both skills' scripts; a newer upload is offered, the next turn still runs the
// pinned version, and the update applies once taken. The panel and the profile page fit a phone.
test('skills: upload, disclaimer once, pinned through an update', async ({ browser, request, baseURL }) => {
  test.setTimeout(180_000);
  // A shared skill's name is the instance's: one per run, as --repeat-each reuses the server.
  const pdf = 'pdf-' + Date.now().toString(36);
  const { room, pages: [alice, bob] } = await roomWith(browser, request, ['Alice', 'Bob']);
  const aliceProfile = await alice.context().newPage();
  await aliceProfile.goto('/profile');
  await uploadSkill(aliceProfile, skillZip('broken', 'X', false), 'personal', /SKILL.md must start with a --- frontmatter block/);
  await uploadSkill(aliceProfile, skillZip('stamp', 'STAMP-v1'), 'personal', 'Added stamp v1.');
  await expect(aliceProfile.locator('#skill-list li')).toHaveText(['stamp v1 · Only my rooms · Prints STAMP-v1. Remove']);
  const bobProfile = await bob.context().newPage();
  await bobProfile.goto('/profile');
  await uploadSkill(bobProfile, skillZip(pdf, 'PDF-v1'), 'shared', `Added ${pdf} v1.`);
  await uploadSkill(bobProfile, skillZip('bobs', 'BOB'), 'personal', 'Added bobs v1.');

  const grill = await openPanel(alice);
  const pick = grill.getByLabel('Skill to turn on for Grill');
  // Earlier runs' shared skills are on offer too; Bob's personal one never is.
  await expect(pick.locator('option', { hasText: pdf })).toHaveText([`${pdf} v1`]);
  await expect(pick.locator('option', { hasText: 'personal' })).toHaveText(['stamp v1 (personal)']);
  await expect(pick.locator('option', { hasText: 'bobs' })).toHaveCount(0);
  await pick.selectOption({ label: 'stamp v1 (personal)' });
  await grill.getByRole('button', { name: 'Turn on the chosen skill for Grill' }).click();
  const disclaimer = alice.locator('#skills-disclaimer');
  await expect(disclaimer).toBeVisible();
  await expect(disclaimer).toContainText('scripts run in the AI host\'s sandbox');
  await expect(disclaimer).toContainText('ask the AI to print a skill\'s files');
  await expect(disclaimer).toContainText('allowed website that accepts uploads');
  await expect(disclaimer).toContainText('only through the AI host\'s list of allowed websites');
  await disclaimer.getByRole('button', { name: 'Cancel' }).click();
  await expect(disclaimer).toBeHidden();
  await expect(grill.locator('.persona-skills li')).toHaveText(['None on.']);
  await grill.getByRole('button', { name: 'Turn on the chosen skill for Grill' }).click();
  await disclaimer.getByRole('button', { name: 'I understand, turn it on' }).click();
  await expect(alice.locator('#persona-status')).toHaveText('stamp v1 is on for Grill.');
  await expect(bob.locator('#messages')).toContainText('Alice changed 🔥 Grill: skill stamp v1 on.');
  // Once per profile: the next activation goes straight through.
  await pick.selectOption({ label: `${pdf} v1` });
  await grill.getByRole('button', { name: 'Turn on the chosen skill for Grill' }).click();
  await expect(alice.locator('#persona-status')).toHaveText(`${pdf} v1 is on for Grill.`);
  await expect(disclaimer).toBeHidden();
  await expect(grill.locator('.persona-skills li[data-skill]')).toHaveText([new RegExp(`^${pdf} v1`), /^stamp v1 \(personal\)/]);

  // Phone width: the panel's skill controls stay on screen and the dialog scrolls.
  await alice.setViewportSize({ width: 390, height: 844 });
  const off = grill.getByRole('button', { name: `Turn off skill ${pdf} for Grill` });
  await off.scrollIntoViewIfNeeded();
  await expect(off).toBeInViewport();
  expect(await alice.locator('#persona-sheet').evaluate((d) => d.scrollWidth <= d.clientWidth)).toBe(true);
  await alice.setViewportSize({ width: 1280, height: 720 });
  await closePersonas(alice);

  const daemon = startDaemon(`${baseURL}/r/${room.id}`);
  try {
    await pairDaemon(alice, daemon);
    expect((await ask(alice, room.id, 'FAKE_SKILLS first')).trim()).toBe(`Skills used: ${pdf}: PDF-v1; stamp: STAMP-v1.`);
    await uploadSkill(aliceProfile, skillZip('stamp', 'STAMP-v2'), 'personal', 'Added stamp v2.');
    const panel = await openPanel(alice);
    const update = panel.getByRole('button', { name: 'Update skill stamp of Grill to version 2' });
    await expect(update).toBeVisible();
    await closePersonas(alice);
    // Bob sees the offer of Alice's personal skill nowhere: it is hers to take.
    const bobPanel = await openPanel(bob);
    await expect(bobPanel.locator('.persona-skills li[data-skill="stamp"]')).toHaveText(/^stamp v1 \(personal\)Turn off$/);
    await closePersonas(bob);
    expect((await ask(alice, room.id, 'FAKE_SKILLS pinned')).trim()).toBe(`Skills used: ${pdf}: PDF-v1; stamp: STAMP-v1.`);
    await openPanel(alice);
    await update.click();
    await expect(alice.locator('#persona-status')).toHaveText('stamp v2 is on for Grill.');
    await closePersonas(alice);
    expect((await ask(alice, room.id, 'FAKE_SKILLS updated')).trim()).toBe(`Skills used: ${pdf}: PDF-v1; stamp: STAMP-v2.`);
  } finally {
    daemon.cleanup();
  }

  // Another room, same profile: no disclaimer again.
  const other = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Second room' } })).json();
  const inv = await (await request.post(`/api/rooms/${other.id}/invites`, { headers: admin })).json();
  await alice.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await alice.getByLabel('Your name').fill('Alice');
  await alice.getByRole('button', { name: 'Join the room' }).click();
  const second = await openPanel(alice);
  await second.getByLabel('Skill to turn on for Grill').selectOption({ label: `${pdf} v1` });
  await second.getByRole('button', { name: 'Turn on the chosen skill for Grill' }).click();
  await expect(alice.locator('#persona-status')).toHaveText(`${pdf} v1 is on for Grill.`);
  await expect(disclaimer).toBeHidden();

  // The profile page fits a phone too.
  await aliceProfile.setViewportSize({ width: 390, height: 844 });
  await aliceProfile.reload();
  await expect(aliceProfile.locator('#skill-list li')).toHaveText([/^stamp v2 · Only my rooms/]);
  await aliceProfile.locator('#skills summary').click();
  await expect(aliceProfile.getByRole('button', { name: 'Add skill', exact: true })).toBeVisible();
  expect(await aliceProfile.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
